// Package syncer orchestrates the deterministic release synchronization flow.
package syncer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"sync-release-to-gitee/internal/config"
	"sync-release-to-gitee/internal/domain"
	"sync-release-to-gitee/internal/httpx"
	"sync-release-to-gitee/internal/planner"
	"sync-release-to-gitee/internal/staging"
	"sync-release-to-gitee/internal/transform"
)

const defaultStaleStagingAge = 7 * 24 * time.Hour

// GitHubAPI is the read-only source contract needed by the orchestrator.
type GitHubAPI interface {
	ListReleases(context.Context, int) ([]domain.Release, error)
	DownloadAsset(context.Context, domain.Asset, io.Writer) error
}

// GiteeAPI is the target contract needed by the orchestrator.
type GiteeAPI interface {
	ListReleases(context.Context) ([]domain.Release, error)
	DefaultBranch(context.Context) (string, error)
	CreateRelease(context.Context, domain.CreateReleaseInput) (domain.Release, error)
	DeleteRelease(context.Context, int64) error
	UploadAsset(context.Context, int64, string, io.Reader) error
	FindReleaseByID(context.Context, int64) (domain.Release, bool, error)
	FindReleaseByTag(context.Context, string) (domain.Release, bool, error)
}

// RatePolicy preserves the Rust baseline consistency waits while allowing
// deterministic no-sleep tests.
type RatePolicy struct {
	DeleteDelay time.Duration
	CreateDelay time.Duration
	Sleeper     httpx.Sleeper
}

// Summary describes completed or dry-run work without including credentials.
type Summary struct {
	Plan        domain.Plan
	DryRun      bool
	CleanedTags []string
	CreatedTags []string
	SkippedTags []string
	Uploaded    map[string][]string
	Warnings    []string
}

// RemotePartialError means a remote object may require manual recovery.
type RemotePartialError struct {
	Tag             string
	ReleaseID       int64
	ConfirmedAssets []string
	Cause           error
	RecoveryHint    string
}

func (e *RemotePartialError) Error() string {
	if e.ReleaseID > 0 {
		return fmt.Sprintf("remote state may be partial for tag %q (release ID %d): %s", e.Tag, e.ReleaseID, e.RecoveryHint)
	}
	return fmt.Sprintf("remote creation outcome is unknown for tag %q: %s", e.Tag, e.RecoveryHint)
}

func (e *RemotePartialError) Unwrap() error { return e.Cause }

// Syncer coordinates one run.
type Syncer struct {
	config        config.Config
	github        GitHubAPI
	gitee         GiteeAPI
	logger        *slog.Logger
	rate          RatePolicy
	staleAge      time.Duration
	transformOpts transform.Options
	now           func() time.Time
}

// New builds a Syncer with production-safe defaults.
func New(cfg config.Config, github GitHubAPI, gitee GiteeAPI, logger *slog.Logger, rate RatePolicy) *Syncer {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if rate.DeleteDelay <= 0 {
		rate.DeleteDelay = time.Second
	}
	if rate.CreateDelay <= 0 {
		rate.CreateDelay = 3 * time.Second
	}
	if rate.Sleeper == nil {
		rate.Sleeper = httpx.SleepFunc(func(ctx context.Context, delay time.Duration) error {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		})
	}
	return &Syncer{
		config:   cfg,
		github:   github,
		gitee:    gitee,
		logger:   logger,
		rate:     rate,
		staleAge: defaultStaleStagingAge,
		now:      time.Now,
		transformOpts: transform.Options{
			GitHubOwner: cfg.GitHubOwner,
			GitHubRepo:  cfg.GitHubRepo,
			GiteeOwner:  cfg.GiteeOwner,
			GiteeRepo:   cfg.GiteeRepo,
		},
	}
}

// Run follows the plan's read -> plan -> dry-run/cleanup -> refresh -> sync
// sequencing. Any write failure stops subsequent writes.
func (s *Syncer) Run(ctx context.Context) (summary Summary, resultErr error) {
	if err := config.Validate(s.config); err != nil {
		return summary, err
	}
	if s.github == nil || s.gitee == nil {
		return summary, errors.New("syncer dependencies are not initialized")
	}

	if _, err := staging.CleanupExpired(s.config.StagingRoot, s.staleAge, s.now()); err != nil {
		s.warning(&summary, "could not clean expired staging directories")
	}
	run, err := staging.NewRun(s.config.StagingRoot)
	if err != nil {
		return summary, err
	}
	defer func() {
		if err := run.Cleanup(); err != nil {
			s.warning(&summary, "could not clean this run staging directory")
		}
	}()

	githubReleases, err := s.github.ListReleases(ctx, s.config.GitHubLatestReleaseCount)
	if err != nil {
		return summary, fmt.Errorf("list GitHub releases: %w", err)
	}
	giteeBefore, err := s.gitee.ListReleases(ctx)
	if err != nil {
		return summary, fmt.Errorf("list Gitee releases before cleanup: %w", err)
	}
	branch, err := s.targetBranch(ctx)
	if err != nil {
		return summary, err
	}
	summary.Plan = planner.BuildPlan(githubReleases, giteeBefore, s.config.GiteeRetainReleaseAttachFilesCount)
	if s.config.DryRun {
		summary.DryRun = true
		return summary, nil
	}

	for _, cleanup := range summary.Plan.Cleanup {
		if err := s.cleanRelease(ctx, cleanup.Release, branch); err != nil {
			return summary, err
		}
		summary.CleanedTags = append(summary.CleanedTags, cleanup.Release.TagName)
	}

	giteeAfter, err := s.gitee.ListReleases(ctx)
	if err != nil {
		return summary, fmt.Errorf("list Gitee releases after cleanup: %w", err)
	}
	existing := make(map[string]struct{}, len(giteeAfter))
	for _, release := range giteeAfter {
		existing[release.TagName] = struct{}{}
	}
	summary.Uploaded = make(map[string][]string)
	for _, item := range summary.Plan.Sync {
		if _, found := existing[item.Release.TagName]; found {
			summary.SkippedTags = append(summary.SkippedTags, item.Release.TagName)
			s.logger.Info("Gitee release already exists; skipping", "tag", item.Release.TagName)
			continue
		}
		uploaded, err := s.syncRelease(ctx, run, item.Release, item.RetainAssets, branch)
		if err != nil {
			return summary, err
		}
		summary.CreatedTags = append(summary.CreatedTags, item.Release.TagName)
		if len(uploaded) != 0 {
			summary.Uploaded[item.Release.TagName] = uploaded
		}
		existing[item.Release.TagName] = struct{}{}
	}
	return summary, nil
}

func (s *Syncer) targetBranch(ctx context.Context) (string, error) {
	if s.config.GiteeBranch != "" {
		s.logger.Info("using configured Gitee branch", "branch", s.config.GiteeBranch)
		return s.config.GiteeBranch, nil
	}
	branch, err := s.gitee.DefaultBranch(ctx)
	if err != nil {
		return "", fmt.Errorf("get Gitee default branch: %w", err)
	}
	return branch, nil
}

func (s *Syncer) cleanRelease(ctx context.Context, release domain.Release, branch string) error {
	s.logger.Info("deleting old Gitee release to remove extra assets", "tag", release.TagName, "release_id", release.ID)
	if err := s.gitee.DeleteRelease(ctx, release.ID); err != nil {
		if httpx.IsUnknown(err) {
			_, found, inspectErr := s.gitee.FindReleaseByID(ctx, release.ID)
			if inspectErr != nil {
				return &RemotePartialError{
					Tag:          release.TagName,
					ReleaseID:    release.ID,
					Cause:        err,
					RecoveryHint: "delete outcome is unknown and the old release could not be inspected; inspect Gitee before retrying",
				}
			}
			if found {
				return fmt.Errorf("delete Gitee release %q (%d) has an unknown outcome; the release is still visible: %w", release.TagName, release.ID, err)
			}
			s.logger.Warn("delete response was lost but the old release is no longer visible; continuing recreation", "tag", release.TagName, "release_id", release.ID)
		} else {
			return fmt.Errorf("delete Gitee release %q (%d): %w", release.TagName, release.ID, err)
		}
	}
	if err := s.wait(ctx, s.rate.DeleteDelay); err != nil {
		return fmt.Errorf("wait after deleting Gitee release %q: %w", release.TagName, err)
	}
	input := domain.CreateReleaseInput{
		TagName:         release.TagName,
		Name:            release.Name,
		Body:            transform.ExistingBody(release.Body, s.config.ReleaseBodyURLReplace, s.transformOpts),
		Prerelease:      release.Prerelease,
		TargetCommitish: branch,
	}
	created, err := s.gitee.CreateRelease(ctx, input)
	if err != nil {
		if httpx.IsUnknown(err) {
			return s.unknownCreateError(ctx, release.TagName, err)
		}
		return &RemotePartialError{
			Tag:          release.TagName,
			ReleaseID:    release.ID,
			Cause:        err,
			RecoveryHint: "deletion succeeded but recreation failed; inspect Gitee and recreate the release metadata before retrying",
		}
	}
	if err := s.wait(ctx, s.rate.CreateDelay); err != nil {
		return fmt.Errorf("wait after recreating Gitee release %q: %w", release.TagName, err)
	}
	if err := s.confirmCreatedRelease(ctx, created, release.TagName); err != nil {
		return err
	}
	return nil
}

func (s *Syncer) syncRelease(ctx context.Context, run *staging.Run, release domain.Release, retainAssets bool, branch string) ([]string, error) {
	var (
		stage    *staging.ReleaseStage
		prepared []staging.PreparedAsset
	)
	if retainAssets && len(release.Assets) != 0 {
		var err error
		stage, err = run.NewRelease(release.TagName, release.Assets)
		if err != nil {
			return nil, fmt.Errorf("prepare staging for release %q: %w", release.TagName, err)
		}
		defer func() {
			if stage != nil {
				if err := stage.Cleanup(); err != nil {
					s.logger.Warn("could not clean release staging directory", "tag", release.TagName)
				}
			}
		}()
		for _, asset := range release.Assets {
			var transformAsset staging.TransformFunc
			if asset.Name == "latest.json" && s.config.LatestJSONURLReplace {
				transformAsset = func(content []byte) ([]byte, error) {
					return transform.LatestJSON(content, true, s.transformOpts)
				}
			}
			item, err := stage.PrepareAsset(ctx, asset, s.github.DownloadAsset, transformAsset)
			if err != nil {
				return nil, fmt.Errorf("prepare asset %q for release %q: %w", asset.Name, release.TagName, err)
			}
			prepared = append(prepared, item)
		}
	}

	input := domain.CreateReleaseInput{
		TagName:         release.TagName,
		Name:            release.Name,
		Body:            transform.GitHubBody(release.Body, release.TagName, s.config.ReleaseBodyURLReplace, s.transformOpts),
		Prerelease:      release.Prerelease,
		TargetCommitish: branch,
	}
	created, err := s.gitee.CreateRelease(ctx, input)
	if err != nil {
		if httpx.IsUnknown(err) {
			return nil, s.unknownCreateError(ctx, release.TagName, err)
		}
		return nil, fmt.Errorf("create Gitee release %q: %w", release.TagName, err)
	}
	if err := s.wait(ctx, s.rate.CreateDelay); err != nil {
		return nil, fmt.Errorf("wait after creating Gitee release %q: %w", release.TagName, err)
	}
	if err := s.confirmCreatedRelease(ctx, created, release.TagName); err != nil {
		return nil, err
	}
	if stage == nil {
		return nil, nil
	}

	confirmed := make([]string, 0, len(prepared))
	for index, item := range prepared {
		file, err := stage.Open(item)
		if err != nil {
			return nil, fmt.Errorf("open staged asset %q: %w", item.OriginalName, err)
		}
		uploadErr := s.gitee.UploadAsset(ctx, created.ID, item.OriginalName, file)
		closeErr := file.Close()
		if uploadErr == nil && closeErr != nil {
			uploadErr = fmt.Errorf("close staged asset %q: %w", item.OriginalName, closeErr)
		}
		if uploadErr != nil {
			wasConfirmed, resolutionErr := s.reconcileUploadFailure(ctx, release.TagName, created, release.Assets[index], confirmed, uploadErr)
			if resolutionErr != nil {
				return nil, resolutionErr
			}
			if wasConfirmed {
				confirmed = append(confirmed, item.OriginalName)
				continue
			}
			return nil, fmt.Errorf("upload asset %q to Gitee release %q: %w", item.OriginalName, release.TagName, uploadErr)
		}
		confirmed = append(confirmed, item.OriginalName)
	}
	if err := stage.Cleanup(); err != nil {
		s.logger.Warn("could not clean successful release staging directory", "tag", release.TagName)
	}
	stage = nil
	return confirmed, nil
}

func (s *Syncer) confirmCreatedRelease(ctx context.Context, created domain.Release, expectedTag string) error {
	remote, found, err := s.gitee.FindReleaseByID(ctx, created.ID)
	if err != nil {
		return &RemotePartialError{
			Tag:          expectedTag,
			ReleaseID:    created.ID,
			Cause:        err,
			RecoveryHint: "creation response was received but the release could not be verified; inspect Gitee before retrying",
		}
	}
	if !found || remote.TagName != expectedTag {
		return &RemotePartialError{
			Tag:          expectedTag,
			ReleaseID:    created.ID,
			RecoveryHint: "creation response was received but the release is not visible through the Gitee Release API; no attachment was uploaded",
		}
	}
	return nil
}

func (s *Syncer) reconcileUploadFailure(ctx context.Context, tag string, created domain.Release, asset domain.Asset, confirmed []string, cause error) (bool, error) {
	remote, found, inspectErr := s.gitee.FindReleaseByID(ctx, created.ID)
	if inspectErr != nil {
		return false, &RemotePartialError{
			Tag:             tag,
			ReleaseID:       created.ID,
			ConfirmedAssets: append([]string(nil), confirmed...),
			Cause:           cause,
			RecoveryHint:    "upload failed and the release could not be inspected; verify its attachments before retrying",
		}
	}
	if found && remoteHasAsset(remote, asset) {
		s.logger.Warn("upload returned an error but the asset is present remotely", "tag", tag, "asset", asset.Name, "release_id", created.ID)
		return true, nil
	}
	if !found {
		return false, fmt.Errorf("upload asset %q failed and created release %d is no longer visible: %w", asset.Name, created.ID, cause)
	}
	if err := s.gitee.DeleteRelease(ctx, created.ID); err != nil {
		if httpx.IsUnknown(err) {
			_, found, inspectErr := s.gitee.FindReleaseByID(ctx, created.ID)
			if inspectErr == nil && !found {
				return false, fmt.Errorf("upload asset %q failed; rollback delete response was lost but release %d is no longer visible: %w", asset.Name, created.ID, cause)
			}
		}
		return false, &RemotePartialError{
			Tag:             tag,
			ReleaseID:       created.ID,
			ConfirmedAssets: append([]string(nil), confirmed...),
			Cause:           err,
			RecoveryHint:    "upload failed and rollback deletion failed; inspect the existing release before retrying",
		}
	}
	return false, fmt.Errorf("upload asset %q failed; deleted incomplete Gitee release %d: %w", asset.Name, created.ID, cause)
}

func (s *Syncer) unknownCreateError(ctx context.Context, tag string, cause error) error {
	remote, found, inspectErr := s.gitee.FindReleaseByTag(ctx, tag)
	if inspectErr != nil {
		return &RemotePartialError{
			Tag:          tag,
			Cause:        cause,
			RecoveryHint: "creation outcome is unknown and Gitee could not be queried by tag; inspect Gitee before retrying",
		}
	}
	if found {
		return &RemotePartialError{
			Tag:          tag,
			ReleaseID:    remote.ID,
			Cause:        cause,
			RecoveryHint: "creation outcome is unknown; a matching Gitee release exists but is not modified because ownership cannot be proven",
		}
	}
	return &RemotePartialError{
		Tag:          tag,
		Cause:        cause,
		RecoveryHint: "creation outcome is unknown; no matching Gitee release was found on the first page, inspect Gitee before retrying",
	}
}

func remoteHasAsset(release domain.Release, wanted domain.Asset) bool {
	for _, asset := range release.Assets {
		if asset.Name != wanted.Name {
			continue
		}
		if wanted.Size == nil || asset.Size == nil || *wanted.Size == *asset.Size {
			return true
		}
	}
	return false
}

func (s *Syncer) wait(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	return s.rate.Sleeper.Sleep(ctx, delay)
}

func (s *Syncer) warning(summary *Summary, message string) {
	summary.Warnings = append(summary.Warnings, message)
	s.logger.Warn(message)
}

// StablePlanText returns a concise, deterministic listing for human dry-runs.
func StablePlanText(plan domain.Plan) []string {
	lines := make([]string, 0, len(plan.Cleanup)+len(plan.Sync)+1)
	lines = append(lines, "retain assets for: "+joinTags(plan.RetainedAssetTags))
	for _, cleanup := range plan.Cleanup {
		lines = append(lines, fmt.Sprintf("recreate %s (release ID %d): %s", cleanup.Release.TagName, cleanup.Release.ID, cleanup.Reason))
	}
	for _, sync := range plan.Sync {
		lines = append(lines, fmt.Sprintf("%s %s (GitHub Release ID %d): %s", sync.Action, sync.Release.TagName, sync.Release.ID, sync.Reason))
		if sync.Action == domain.ActionCreateAndUpload {
			lines = append(lines, "  assets: "+assetNames(sync.Release.Assets))
		}
	}
	return lines
}

// DryRunText is the user-facing, secret-free plan. It deliberately avoids
// serializing source DTOs such as signed browser download URLs.
func DryRunText(plan domain.Plan, githubPerPage int) []string {
	lines := []string{
		fmt.Sprintf("GitHub releases: page=1 per_page=%d", githubPerPage),
		"Gitee releases: page=1 per_page=100",
	}
	return append(lines, StablePlanText(plan)...)
}

func joinTags(tags []string) string {
	if len(tags) == 0 {
		return "(none)"
	}
	result := tags[0]
	for _, tag := range tags[1:] {
		result += ", " + tag
	}
	return result
}

func assetNames(assets []domain.Asset) string {
	if len(assets) == 0 {
		return "(none)"
	}
	names := make([]string, 0, len(assets))
	for _, asset := range assets {
		names = append(names, asset.Name)
	}
	return joinTags(names)
}
