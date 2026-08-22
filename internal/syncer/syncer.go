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
		s.warning(&summary, "清理过期临时目录失败")
	}
	run, err := staging.NewRun(s.config.StagingRoot)
	if err != nil {
		return summary, err
	}
	defer func() {
		if err := run.Cleanup(); err != nil {
			s.warning(&summary, "清理本次同步的临时目录失败")
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
	s.logger.Info(
		"已获取远端 Release 清单",
		"GitHub Release 数", len(githubReleases),
		"Gitee Release 数", len(giteeBefore),
	)
	branch, err := s.targetBranch(ctx)
	if err != nil {
		return summary, err
	}
	summary.Plan = planner.BuildPlan(githubReleases, giteeBefore, s.config.GiteeRetainReleaseAttachFilesCount)
	s.logger.Info(
		"已生成同步计划",
		"待重建 Release 数", len(summary.Plan.Cleanup),
		"待创建并上传附件的 Release 数", len(summary.Plan.CreateAndUpload),
		"待仅创建元数据的 Release 数", len(summary.Plan.CreateMetadataOnly),
		"待跳过 Release 数", len(summary.Plan.SkippedExisting),
	)
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
			s.logger.Info("跳过已存在的 Gitee Release", "标签", item.Release.TagName)
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
		s.logger.Info("使用指定的 Gitee 目标分支", "分支", s.config.GiteeBranch)
		return s.config.GiteeBranch, nil
	}
	branch, err := s.gitee.DefaultBranch(ctx)
	if err != nil {
		return "", fmt.Errorf("get Gitee default branch: %w", err)
	}
	s.logger.Info("已识别 Gitee 默认分支", "分支", branch)
	return branch, nil
}

func (s *Syncer) cleanRelease(ctx context.Context, release domain.Release, branch string) error {
	s.logger.Info("为清理多余附件，开始重建已有 Gitee Release", "标签", release.TagName, "Gitee Release ID", release.ID)
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
			s.logger.Warn("删除响应丢失，但旧 Release 已不可见；继续重建", "标签", release.TagName, "Gitee Release ID", release.ID)
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
	s.logger.Info("开始重新创建 Gitee Release", "标签", release.TagName, "目标分支", branch)
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
	assetCount := 0
	if retainAssets {
		assetCount = len(release.Assets)
	}
	s.logger.Info(
		"开始同步 Release",
		"标签", release.TagName,
		"附件总数", len(release.Assets),
		"本次同步附件数", assetCount,
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
					s.logger.Warn("清理 Release 临时目录失败", "标签", release.TagName)
				}
			}
		}()
		for index, asset := range release.Assets {
			s.logger.Info(
				"开始下载 GitHub 附件",
				"标签", release.TagName,
				"进度", progress(index+1, len(release.Assets)),
				"文件", asset.Name,
				"预计大小", assetSize(asset.Size),
			)
			var transformAsset staging.TransformFunc
			if asset.Name == "latest.json" && s.config.LatestJSONURLReplace {
				transformAsset = func(content []byte) ([]byte, error) {
					return transform.LatestJSON(content, true, s.transformOpts)
				}
			}
			downloadProgress := newTransferProgress(s.logger, "GitHub 附件下载", release.TagName, asset.Name, index+1, len(release.Assets), asset.Size)
			item, err := stage.PrepareAsset(ctx, asset, func(downloadCtx context.Context, downloadAsset domain.Asset, destination io.Writer) error {
				return s.github.DownloadAsset(downloadCtx, downloadAsset, &progressWriter{writer: destination, progress: downloadProgress})
			}, transformAsset)
			if err != nil {
				return nil, fmt.Errorf("prepare asset %q for release %q: %w", asset.Name, release.TagName, err)
			}
			prepared = append(prepared, item)
			s.logger.Info(
				"GitHub 附件下载完成",
				"标签", release.TagName,
				"进度", progress(index+1, len(release.Assets)),
				"文件", item.OriginalName,
				"实际大小", byteSize(item.Size),
			)
		}
	}

	input := domain.CreateReleaseInput{
		TagName:         release.TagName,
		Name:            release.Name,
		Body:            transform.GitHubBody(release.Body, release.TagName, s.config.ReleaseBodyURLReplace, s.transformOpts),
		Prerelease:      release.Prerelease,
		TargetCommitish: branch,
	}
	s.logger.Info("开始创建 Gitee Release", "标签", release.TagName, "目标分支", branch)
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
		s.logger.Info("Release 同步完成", "标签", release.TagName, "已上传附件数", 0)
		return nil, nil
	}

	confirmed := make([]string, 0, len(prepared))
	for index, item := range prepared {
		file, err := stage.Open(item)
		if err != nil {
			return nil, fmt.Errorf("open staged asset %q: %w", item.OriginalName, err)
		}
		s.logger.Info(
			"开始上传附件到 Gitee",
			"标签", release.TagName,
			"进度", progress(index+1, len(prepared)),
			"文件", item.OriginalName,
			"文件大小", byteSize(item.Size),
		)
		uploadProgress := newTransferProgress(s.logger, "Gitee 附件上传", release.TagName, item.OriginalName, index+1, len(prepared), &item.Size)
		uploadErr := s.gitee.UploadAsset(ctx, created.ID, item.OriginalName, &progressReader{reader: file, progress: uploadProgress})
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
		s.logger.Info(
			"Gitee 附件上传完成",
			"标签", release.TagName,
			"进度", progress(index+1, len(prepared)),
			"文件", item.OriginalName,
		)
	}
	if err := stage.Cleanup(); err != nil {
		s.logger.Warn("清理已完成 Release 的临时目录失败", "标签", release.TagName)
	}
	stage = nil
	s.logger.Info("Release 同步完成", "标签", release.TagName, "已上传附件数", len(confirmed))
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
	s.logger.Info("Gitee Release 已创建并确认", "标签", expectedTag, "Gitee Release ID", created.ID)
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
		s.logger.Warn("上传请求返回错误，但附件已在 Gitee 可见；按成功处理", "标签", tag, "文件", asset.Name, "Gitee Release ID", created.ID)
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

func progress(current, total int) string {
	return fmt.Sprintf("%d/%d", current, total)
}

func assetSize(size *int64) string {
	if size == nil {
		return "未知"
	}
	return byteSize(*size)
}

func byteSize(size int64) string {
	const unit = int64(1024)
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	if size < unit*unit {
		return fmt.Sprintf("%.1f KiB", float64(size)/float64(unit))
	}
	if size < unit*unit*unit {
		return fmt.Sprintf("%.1f MiB", float64(size)/float64(unit*unit))
	}
	return fmt.Sprintf("%.1f GiB", float64(size)/float64(unit*unit*unit))
}

type transferProgress struct {
	logger           *slog.Logger
	operation        string
	tag              string
	file             string
	fileIndex        int
	fileCount        int
	total            int64
	transferred      int64
	nextPercent      int64
	nextUnknownLogAt int64
	startedAt        time.Time
}

func newTransferProgress(logger *slog.Logger, operation, tag, file string, fileIndex, fileCount int, size *int64) *transferProgress {
	total := int64(-1)
	if size != nil {
		total = *size
	}
	return &transferProgress{
		logger:           logger,
		operation:        operation,
		tag:              tag,
		file:             file,
		fileIndex:        fileIndex,
		fileCount:        fileCount,
		total:            total,
		nextPercent:      25,
		nextUnknownLogAt: 1 << 20,
		startedAt:        time.Now(),
	}
}

func (p *transferProgress) Add(count int) {
	if p == nil || count <= 0 {
		return
	}
	p.transferred += int64(count)
	if p.total > 0 {
		percentage := p.transferred * 100 / p.total
		if percentage < p.nextPercent && p.transferred < p.total {
			return
		}
		for p.nextPercent <= percentage {
			p.nextPercent += 25
		}
	} else if p.transferred < p.nextUnknownLogAt {
		return
	} else {
		p.nextUnknownLogAt += 1 << 20
	}
	p.logProgress()
}

func (p *transferProgress) Reset() {
	if p == nil {
		return
	}
	if p.transferred > 0 {
		if p.logger != nil {
			p.logger.Warn(
				"传输响应中断，正在重新传输附件",
				"标签", p.tag,
				"文件", p.file,
				"已丢弃数据", byteSize(p.transferred),
			)
		}
	}
	p.transferred = 0
	p.nextPercent = 25
	p.nextUnknownLogAt = 1 << 20
	p.startedAt = time.Now()
}

func (p *transferProgress) logProgress() {
	if p == nil || p.logger == nil {
		return
	}
	attributes := []any{
		"标签", p.tag,
		"文件序号", progress(p.fileIndex, p.fileCount),
		"文件", p.file,
		"已传输", byteSize(p.transferred),
	}
	if p.total > 0 {
		attributes = append(attributes,
			"总大小", byteSize(p.total),
			"完成比例", fmt.Sprintf("%d%%", min(p.transferred*100/p.total, 100)),
		)
	}
	elapsed := time.Since(p.startedAt)
	if elapsed > 0 && p.transferred > 0 {
		rate := int64(float64(p.transferred) / elapsed.Seconds())
		attributes = append(attributes, "传输速度", byteSize(rate)+"/s")
		if p.total > p.transferred && rate > 0 {
			remaining := time.Duration(float64(p.total-p.transferred)/float64(rate)) * time.Second
			attributes = append(attributes, "预计剩余", remaining.Round(time.Second).String())
		}
	}
	p.logger.Info(p.operation+"进度", attributes...)
}

type progressWriter struct {
	writer   io.Writer
	progress *transferProgress
}

func (w *progressWriter) Write(content []byte) (int, error) {
	count, err := w.writer.Write(content)
	w.progress.Add(count)
	return count, err
}

func (w *progressWriter) Seek(offset int64, whence int) (int64, error) {
	seeker, ok := w.writer.(io.Seeker)
	if !ok {
		return 0, errors.New("progress writer does not support seeking")
	}
	return seeker.Seek(offset, whence)
}

func (w *progressWriter) Truncate(size int64) error {
	truncater, ok := w.writer.(interface{ Truncate(int64) error })
	if !ok {
		return errors.New("progress writer does not support truncation")
	}
	if err := truncater.Truncate(size); err != nil {
		return err
	}
	if size == 0 {
		w.progress.Reset()
	}
	return nil
}

type progressReader struct {
	reader   io.Reader
	progress *transferProgress
}

func (r *progressReader) Read(content []byte) (int, error) {
	count, err := r.reader.Read(content)
	r.progress.Add(count)
	return count, err
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
