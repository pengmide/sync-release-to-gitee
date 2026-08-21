package syncer

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pengmd/sync-release-to-gitee/internal/config"
	"github.com/pengmd/sync-release-to-gitee/internal/domain"
	"github.com/pengmd/sync-release-to-gitee/internal/httpx"
)

func TestRunDryRunNeverWrites(t *testing.T) {
	t.Parallel()
	github := &fakeGitHub{releases: []domain.Release{sourceRelease(2, "v2.0.0", nil)}}
	gitee := &fakeGitee{branch: "main"}
	cfg := testConfig(t)
	cfg.DryRun = true
	runner := New(cfg, github, gitee, nil, noWaitPolicy())

	summary, err := runner.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !summary.DryRun || len(gitee.creates) != 0 || len(gitee.deletes) != 0 || len(gitee.uploads) != 0 {
		t.Fatalf("dry run summary/writes = %#v %#v", summary, gitee)
	}
	if len(summary.Plan.Sync) != 1 || summary.Plan.Sync[0].Action != domain.ActionCreateMetadataOnly {
		t.Fatalf("dry-run plan = %#v", summary.Plan)
	}
}

func TestRunCleansRefreshesThenSyncsOldToNew(t *testing.T) {
	t.Parallel()
	size := int64(len("payload"))
	github := &fakeGitHub{
		releases: []domain.Release{
			sourceRelease(10, "v1.0.0", nil),
			sourceRelease(20, "v2.0.0", []domain.Asset{{Name: "artifact.zip", Size: &size, DownloadURL: "artifact"}}),
		},
		downloads: map[string]string{"artifact": "payload"},
	}
	gitee := &fakeGitee{
		branch: "main",
		nextID: 100,
		releases: []domain.Release{
			targetRelease(11, "v1.0.0", 3),
		},
	}
	var waits []time.Duration
	cfg := testConfig(t)
	cfg.GiteeRetainReleaseAttachFilesCount = 1
	runner := New(cfg, github, gitee, nil, RatePolicy{
		DeleteDelay: time.Second,
		CreateDelay: 3 * time.Second,
		Sleeper: httpx.SleepFunc(func(_ context.Context, delay time.Duration) error {
			waits = append(waits, delay)
			return nil
		}),
	})

	summary, err := runner.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := summary.CleanedTags, []string{"v1.0.0"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("CleanedTags = %#v, want %#v", got, want)
	}
	if got, want := summary.SkippedTags, []string{"v1.0.0"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("SkippedTags = %#v, want %#v", got, want)
	}
	if got, want := summary.CreatedTags, []string{"v2.0.0"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("CreatedTags = %#v, want %#v", got, want)
	}
	if got, want := summary.Uploaded["v2.0.0"], []string{"artifact.zip"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Uploaded = %#v, want %#v", got, want)
	}
	if got, want := gitee.deletes, []int64{11}; !reflect.DeepEqual(got, want) {
		t.Fatalf("deletes = %#v, want %#v", got, want)
	}
	if got, want := uploadNames(gitee.uploads), []string{"artifact.zip"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("uploads = %#v, want %#v", got, want)
	}
	if got, want := waits, []time.Duration{time.Second, 3 * time.Second, 3 * time.Second}; !reflect.DeepEqual(got, want) {
		t.Fatalf("waits = %#v, want %#v", got, want)
	}
}

func TestRunUploadFailureRollsBackOnlyCreatedRelease(t *testing.T) {
	t.Parallel()
	size := int64(len("bad"))
	github := &fakeGitHub{
		releases:  []domain.Release{sourceRelease(20, "v2.0.0", []domain.Asset{{Name: "bad.zip", Size: &size, DownloadURL: "bad"}})},
		downloads: map[string]string{"bad": "bad"},
	}
	gitee := &fakeGitee{
		branch:    "main",
		nextID:    100,
		uploadErr: map[string]error{"bad.zip": errors.New("upload rejected")},
	}
	cfg := testConfig(t)
	cfg.GiteeRetainReleaseAttachFilesCount = 1
	runner := New(cfg, github, gitee, nil, noWaitPolicy())

	_, err := runner.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "deleted incomplete") {
		t.Fatalf("Run() error = %v, want rollback error", err)
	}
	if got, want := gitee.deletes, []int64{101}; !reflect.DeepEqual(got, want) {
		t.Fatalf("deletes = %#v, want %#v", got, want)
	}
	if len(gitee.releases) != 0 {
		t.Fatalf("releases after rollback = %#v, want empty", gitee.releases)
	}
}

func TestRunRollbackFailureReportsPartialWithoutToken(t *testing.T) {
	t.Parallel()
	size := int64(3)
	github := &fakeGitHub{
		releases:  []domain.Release{sourceRelease(20, "v2.0.0", []domain.Asset{{Name: "bad.zip", Size: &size, DownloadURL: "bad"}})},
		downloads: map[string]string{"bad": "bad"},
	}
	gitee := &fakeGitee{
		branch:    "main",
		nextID:    100,
		uploadErr: map[string]error{"bad.zip": errors.New("upload rejected")},
		deleteErr: errors.New("delete rejected"),
	}
	runner := New(testConfig(t), github, gitee, nil, noWaitPolicy())
	_, err := runner.Run(context.Background())
	var partial *RemotePartialError
	if !errors.As(err, &partial) {
		t.Fatalf("Run() error = %T %v, want RemotePartialError", err, err)
	}
	if partial.ReleaseID != 101 || strings.Contains(err.Error(), "gitee-token") {
		t.Fatalf("partial error = %#v", partial)
	}
}

func TestRunUnknownCreateQueriesTagAndDoesNotUpload(t *testing.T) {
	t.Parallel()
	size := int64(3)
	github := &fakeGitHub{
		releases:  []domain.Release{sourceRelease(20, "v2.0.0", []domain.Asset{{Name: "artifact.zip", Size: &size, DownloadURL: "artifact"}})},
		downloads: map[string]string{"artifact": "zip"},
	}
	gitee := &fakeGitee{
		branch:            "main",
		nextID:            100,
		createErr:         &httpx.OpError{Unknown: true, Summary: "transport error"},
		createBeforeError: true,
	}
	runner := New(testConfig(t), github, gitee, nil, noWaitPolicy())
	_, err := runner.Run(context.Background())
	var partial *RemotePartialError
	if !errors.As(err, &partial) {
		t.Fatalf("Run() error = %T %v, want RemotePartialError", err, err)
	}
	if partial.ReleaseID != 101 || len(gitee.uploads) != 0 {
		t.Fatalf("partial=%#v uploads=%#v", partial, gitee.uploads)
	}
}

func TestRunUnknownUploadWithRemoteAssetContinues(t *testing.T) {
	t.Parallel()
	size := int64(3)
	github := &fakeGitHub{
		releases:  []domain.Release{sourceRelease(20, "v2.0.0", []domain.Asset{{Name: "artifact.zip", Size: &size, DownloadURL: "artifact"}})},
		downloads: map[string]string{"artifact": "zip"},
	}
	gitee := &fakeGitee{
		branch:              "main",
		nextID:              100,
		uploadAfterWriteErr: map[string]error{"artifact.zip": &httpx.OpError{Unknown: true, Summary: "transport error"}},
	}
	runner := New(testConfig(t), github, gitee, nil, noWaitPolicy())
	summary, err := runner.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := summary.Uploaded["v2.0.0"], []string{"artifact.zip"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Uploaded = %#v, want %#v", got, want)
	}
	if len(gitee.deletes) != 0 {
		t.Fatalf("deletes = %#v, want none", gitee.deletes)
	}
}

func TestRunDownloadFailureDoesNotCreateRelease(t *testing.T) {
	t.Parallel()
	size := int64(3)
	github := &fakeGitHub{
		releases:    []domain.Release{sourceRelease(20, "v2.0.0", []domain.Asset{{Name: "artifact.zip", Size: &size, DownloadURL: "artifact"}})},
		downloadErr: errors.New("source unavailable"),
	}
	gitee := &fakeGitee{branch: "main"}
	runner := New(testConfig(t), github, gitee, nil, noWaitPolicy())
	_, err := runner.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "source unavailable") {
		t.Fatalf("Run() error = %v", err)
	}
	if len(gitee.creates) != 0 {
		t.Fatalf("creates = %#v, want none", gitee.creates)
	}
}

func TestStablePlanTextPreservesRetainedOrder(t *testing.T) {
	t.Parallel()
	lines := StablePlanText(domain.Plan{RetainedAssetTags: []string{"v10.0.0", "v2.0.0"}})
	if got, want := lines[0], "retain assets for: v10.0.0, v2.0.0"; got != want {
		t.Fatalf("StablePlanText() = %q, want %q", got, want)
	}
}

func TestDryRunTextDoesNotExposeDownloadURL(t *testing.T) {
	t.Parallel()
	plan := domain.Plan{Sync: []domain.SyncRelease{{
		Release: domain.Release{
			ID:      1,
			TagName: "v1.0.0",
			Assets:  []domain.Asset{{Name: "file.zip", DownloadURL: "https://example.test/?X-Amz-Signature=secret"}},
		},
		Action: domain.ActionCreateAndUpload,
		Reason: "retained tag with GitHub assets",
	}}}
	output := strings.Join(DryRunText(plan, 5), "\n")
	if strings.Contains(output, "Signature") || strings.Contains(output, "secret") || !strings.Contains(output, "page=1 per_page=5") || !strings.Contains(output, "assets: file.zip") {
		t.Fatalf("unsafe or incomplete dry-run output: %q", output)
	}
}

type fakeGitHub struct {
	releases    []domain.Release
	downloads   map[string]string
	downloadErr error
}

func (f *fakeGitHub) ListReleases(context.Context, int) ([]domain.Release, error) {
	return cloneReleases(f.releases), nil
}

func (f *fakeGitHub) DownloadAsset(_ context.Context, asset domain.Asset, destination io.Writer) error {
	if f.downloadErr != nil {
		return f.downloadErr
	}
	content, found := f.downloads[asset.DownloadURL]
	if !found {
		return errors.New("download not found")
	}
	_, err := io.WriteString(destination, content)
	return err
}

type fakeUpload struct {
	releaseID int64
	name      string
	content   string
}

type fakeGitee struct {
	releases            []domain.Release
	branch              string
	nextID              int64
	creates             []domain.CreateReleaseInput
	deletes             []int64
	uploads             []fakeUpload
	uploadErr           map[string]error
	uploadAfterWriteErr map[string]error
	createErr           error
	createBeforeError   bool
	deleteErr           error
}

func (f *fakeGitee) ListReleases(context.Context) ([]domain.Release, error) {
	return cloneReleases(f.releases), nil
}

func (f *fakeGitee) DefaultBranch(context.Context) (string, error) {
	if f.branch == "" {
		return "", errors.New("no branch")
	}
	return f.branch, nil
}

func (f *fakeGitee) CreateRelease(_ context.Context, input domain.CreateReleaseInput) (domain.Release, error) {
	if f.createErr != nil && !f.createBeforeError {
		return domain.Release{}, f.createErr
	}
	f.nextID++
	release := domain.Release{
		ID:              f.nextID,
		TagName:         input.TagName,
		Name:            input.Name,
		Body:            stringPtr(input.Body),
		Prerelease:      input.Prerelease,
		TargetCommitish: input.TargetCommitish,
	}
	f.creates = append(f.creates, input)
	f.releases = append(f.releases, release)
	if f.createErr != nil {
		return release, f.createErr
	}
	return release, nil
}

func (f *fakeGitee) DeleteRelease(_ context.Context, releaseID int64) error {
	f.deletes = append(f.deletes, releaseID)
	if f.deleteErr != nil {
		return f.deleteErr
	}
	for index, release := range f.releases {
		if release.ID == releaseID {
			f.releases = append(f.releases[:index], f.releases[index+1:]...)
			return nil
		}
	}
	return errors.New("release not found")
}

func (f *fakeGitee) UploadAsset(_ context.Context, releaseID int64, name string, content io.Reader) error {
	data, err := io.ReadAll(content)
	if err != nil {
		return err
	}
	f.uploads = append(f.uploads, fakeUpload{releaseID: releaseID, name: name, content: string(data)})
	if err := f.uploadErr[name]; err != nil {
		return err
	}
	size := int64(len(data))
	for index := range f.releases {
		if f.releases[index].ID == releaseID {
			f.releases[index].Assets = append(f.releases[index].Assets, domain.Asset{Name: name, Size: &size})
			if err := f.uploadAfterWriteErr[name]; err != nil {
				return err
			}
			return nil
		}
	}
	return errors.New("release not found")
}

func (f *fakeGitee) FindReleaseByID(_ context.Context, releaseID int64) (domain.Release, bool, error) {
	for _, release := range f.releases {
		if release.ID == releaseID {
			return release, true, nil
		}
	}
	return domain.Release{}, false, nil
}

func (f *fakeGitee) FindReleaseByTag(_ context.Context, tag string) (domain.Release, bool, error) {
	for _, release := range f.releases {
		if release.TagName == tag {
			return release, true, nil
		}
	}
	return domain.Release{}, false, nil
}

func testConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Defaults()
	cfg.GitHubOwner = "github-owner"
	cfg.GitHubRepo = "github-repo"
	cfg.GiteeOwner = "gitee-owner"
	cfg.GiteeRepo = "gitee-repo"
	cfg.GiteeToken = "gitee-token"
	cfg.GiteeBranch = "main"
	cfg.StagingRoot = t.TempDir()
	return cfg
}

func noWaitPolicy() RatePolicy {
	return RatePolicy{
		DeleteDelay: time.Nanosecond,
		CreateDelay: time.Nanosecond,
		Sleeper: httpx.SleepFunc(func(context.Context, time.Duration) error {
			return nil
		}),
	}
}

func sourceRelease(id int64, tag string, assets []domain.Asset) domain.Release {
	body := "notes"
	return domain.Release{ID: id, TagName: tag, Name: tag, Body: &body, Assets: assets}
}

func targetRelease(id int64, tag string, assets int) domain.Release {
	release := sourceRelease(id, tag, nil)
	for index := 0; index < assets; index++ {
		release.Assets = append(release.Assets, domain.Asset{Name: "source"})
	}
	return release
}

func cloneReleases(input []domain.Release) []domain.Release {
	output := make([]domain.Release, len(input))
	for index, release := range input {
		output[index] = release
		output[index].Assets = append([]domain.Asset(nil), release.Assets...)
	}
	return output
}

func uploadNames(uploads []fakeUpload) []string {
	names := make([]string, len(uploads))
	for index, upload := range uploads {
		names[index] = upload.name
	}
	return names
}

func stringPtr(value string) *string { return &value }
