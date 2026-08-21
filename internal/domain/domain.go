// Package domain contains platform-neutral release models.
package domain

// Asset is a downloadable release attachment.
type Asset struct {
	Name        string
	Size        *int64
	DownloadURL string
}

// Release is the normalized subset shared by GitHub and Gitee release APIs.
type Release struct {
	ID              int64
	TagName         string
	Name            string
	Body            *string
	Prerelease      bool
	TargetCommitish string
	Assets          []Asset
}

// CreateReleaseInput is the Gitee create-release payload after all source
// transformations have been applied.
type CreateReleaseInput struct {
	TagName         string `json:"tag_name"`
	Name            string `json:"name"`
	Body            string `json:"body"`
	Prerelease      bool   `json:"prerelease"`
	TargetCommitish string `json:"target_commitish"`
}

// Action describes the action selected for a GitHub release.
type Action string

const (
	ActionSkipExisting       Action = "skip-existing"
	ActionCreateMetadataOnly Action = "create-metadata-only"
	ActionCreateAndUpload    Action = "create-and-upload"
)

// RecreateRelease describes the destructive cleanup operation for one existing
// Gitee release.
type RecreateRelease struct {
	Release Release
	Reason  string
}

// SyncRelease describes one GitHub release in actual execution order.
type SyncRelease struct {
	Release        Release
	Action         Action
	RetainAssets   bool
	Reason         string
	ExistingAtPlan bool
}

// Plan is a deterministic, secret-free description of a synchronization run.
type Plan struct {
	RetainedAssetTags  []string
	Cleanup            []RecreateRelease
	Sync               []SyncRelease
	SkippedExisting    []string
	CreateMetadataOnly []string
	CreateAndUpload    []string
}
