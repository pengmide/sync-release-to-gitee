package staging

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pengmd/sync-release-to-gitee/internal/domain"
)

func TestPrepareAssetAtomicAndTransform(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	run, err := NewRun(root)
	if err != nil {
		t.Fatal(err)
	}
	defer run.Cleanup()
	size := int64(len("https://github.com/gh/repo/file"))
	asset := domain.Asset{Name: "latest.json", Size: &size}
	stage, err := run.NewRelease("v1.0.0", []domain.Asset{asset})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := stage.PrepareAsset(context.Background(), asset, func(_ context.Context, _ domain.Asset, dst io.Writer) error {
		_, err := io.WriteString(dst, "https://github.com/gh/repo/file")
		return err
	}, func(data []byte) ([]byte, error) {
		return []byte(strings.ReplaceAll(string(data), "github.com/gh/repo", "gitee.com/gt/repo")), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(prepared.Path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(content), "https://gitee.com/gt/repo/file"; got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
	if _, err := os.Stat(prepared.Path + ".part"); !os.IsNotExist(err) {
		t.Fatalf("part file stat err = %v, want absent", err)
	}
}

func TestPrepareAssetSizeMismatchLeavesNoCompletedFile(t *testing.T) {
	t.Parallel()
	run, err := NewRun(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer run.Cleanup()
	size := int64(9)
	asset := domain.Asset{Name: "binary.zip", Size: &size}
	stage, err := run.NewRelease("v1.0.0", []domain.Asset{asset})
	if err != nil {
		t.Fatal(err)
	}
	_, err = stage.PrepareAsset(context.Background(), asset, func(_ context.Context, _ domain.Asset, dst io.Writer) error {
		_, err := io.WriteString(dst, "short")
		return err
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "downloaded size") {
		t.Fatalf("PrepareAsset() error = %v, want size mismatch", err)
	}
	matches, err := filepath.Glob(filepath.Join(stage.path, "asset-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("remaining asset files = %v", matches)
	}
}

func TestValidateAssetNames(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		assets  []domain.Asset
		wantErr bool
	}{
		{"safe", []domain.Asset{{Name: "release.tar.gz"}}, false},
		{"traversal", []domain.Asset{{Name: "../release.tar.gz"}}, true},
		{"separator", []domain.Asset{{Name: "dir/release.tar.gz"}}, true},
		{"reserved", []domain.Asset{{Name: "CON"}}, true},
		{"trailing-dot", []domain.Asset{{Name: "artifact."}}, true},
		{"trailing-space", []domain.Asset{{Name: "artifact "}}, true},
		{"combining-mark", []domain.Asset{{Name: "e\u0301.zip"}}, true},
		{"collision", []domain.Asset{{Name: "A.zip"}, {Name: "a.zip"}}, true},
	}
	for _, test := range tests {
		if err := ValidateAssetNames(test.assets); (err != nil) != test.wantErr {
			t.Errorf("%s: ValidateAssetNames() error = %v, wantErr %t", test.name, err, test.wantErr)
		}
	}
}

func TestCleanupExpiredOnlyProgramMarkedRuns(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	oldRun := filepath.Join(root, "run-old")
	if err := os.Mkdir(oldRun, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldRun, MarkerName), []byte(markerContent), 0o600); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(root, "run-other")
	if err := os.Mkdir(other, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(oldRun, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(other, old, old); err != nil {
		t.Fatal(err)
	}
	wrongMarker := filepath.Join(root, "run-wrong-marker")
	if err := os.Mkdir(wrongMarker, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wrongMarker, MarkerName), []byte("not ours"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(wrongMarker, old, old); err != nil {
		t.Fatal(err)
	}
	removed, err := CleanupExpired(root, time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != oldRun {
		t.Fatalf("removed = %v, want %q", removed, oldRun)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("unmarked directory was removed: %v", err)
	}
	if _, err := os.Stat(wrongMarker); err != nil {
		t.Fatalf("wrong-marker directory was removed: %v", err)
	}
}

func TestPrepareAssetPropagatesDownloadFailure(t *testing.T) {
	t.Parallel()
	run, err := NewRun(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer run.Cleanup()
	asset := domain.Asset{Name: "binary.zip"}
	stage, err := run.NewRelease("v1.0.0", []domain.Asset{asset})
	if err != nil {
		t.Fatal(err)
	}
	_, err = stage.PrepareAsset(context.Background(), asset, func(context.Context, domain.Asset, io.Writer) error {
		return errors.New("network stopped")
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "network stopped") {
		t.Fatalf("PrepareAsset() error = %v", err)
	}
}
