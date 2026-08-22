package planner

import (
	"reflect"
	"testing"

	"sync-release-to-gitee/internal/domain"
)

func TestCompareTagCompatibility(t *testing.T) {
	t.Parallel()
	tests := []struct {
		left, right string
		want        int
	}{
		{"v1.2.3", "1.2.3", 0},
		{"v0.9.11", "v0.9.9", 1},
		{"v1.2.3-beta", "v1.2.3", -1},
		{"release 1.2.alpha", "release 1.2.dev.4", -1},
		{"1.2.dev.4", "1.2.dev.3", 1},
		{"not-a-version", "v1.0.0", 0},
	}
	for _, test := range tests {
		if got := CompareTag(test.left, test.right); sign(got) != test.want {
			t.Errorf("CompareTag(%q, %q) = %d, want sign %d", test.left, test.right, got, test.want)
		}
	}
}

func TestBuildPlanMatchesStableBaseline(t *testing.T) {
	t.Parallel()
	gitee := []domain.Release{
		release(90, "v1.0.0", 3),
		release(80, "v0.9.0", 2),
	}
	github := []domain.Release{
		release(20, "v1.0.0", 1),
		release(10, "v1.1.0", 1),
		release(30, "nightly", 1),
	}

	plan := BuildPlan(github, gitee, 1)
	if got, want := plan.RetainedAssetTags, []string{"v1.1.0"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("RetainedAssetTags = %#v, want %#v", got, want)
	}
	if len(plan.Cleanup) != 1 || plan.Cleanup[0].Release.TagName != "v1.0.0" {
		t.Fatalf("Cleanup = %#v, want v1.0.0", plan.Cleanup)
	}
	if got, want := syncIDs(plan.Sync), []int64{10, 20, 30}; !reflect.DeepEqual(got, want) {
		t.Fatalf("sync order = %#v, want %#v", got, want)
	}
	if plan.Sync[1].Action != domain.ActionSkipExisting {
		t.Fatalf("same tag action = %q, want skip", plan.Sync[1].Action)
	}
	if plan.Sync[0].Action != domain.ActionCreateAndUpload {
		t.Fatalf("retained action = %q, want upload", plan.Sync[0].Action)
	}
	if plan.Sync[2].Action != domain.ActionCreateMetadataOnly {
		t.Fatalf("unparseable tag action = %q, want metadata only", plan.Sync[2].Action)
	}
}

func TestBuildPlanAssetThresholdAndZeroRetain(t *testing.T) {
	t.Parallel()
	gitee := []domain.Release{
		release(1, "v3.0.0", 0),
		release(2, "v2.0.0", 1),
		release(3, "v1.0.0", 2),
		release(4, "v0.1.0", 3),
	}
	plan := BuildPlan(nil, gitee, 0)
	if len(plan.RetainedAssetTags) != 0 {
		t.Fatalf("RetainedAssetTags = %#v, want empty", plan.RetainedAssetTags)
	}
	if len(plan.Cleanup) != 1 || plan.Cleanup[0].Release.TagName != "v0.1.0" {
		t.Fatalf("Cleanup = %#v, want only >2 assets", plan.Cleanup)
	}
}

func release(id int64, tag string, assets int) domain.Release {
	release := domain.Release{ID: id, TagName: tag, Name: tag}
	for index := 0; index < assets; index++ {
		release.Assets = append(release.Assets, domain.Asset{Name: "asset"})
	}
	return release
}

func syncIDs(items []domain.SyncRelease) []int64 {
	ids := make([]int64, len(items))
	for index, item := range items {
		ids[index] = item.Release.ID
	}
	return ids
}

func sign(value int) int {
	switch {
	case value < 0:
		return -1
	case value > 0:
		return 1
	default:
		return 0
	}
}
