// Package staging manages one-run, secret-free release attachment staging.
package staging

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"sync-release-to-gitee/internal/domain"
)

const (
	// MarkerName identifies directories that this program is allowed to remove.
	MarkerName              = ".release2gitee-staging-marker"
	markerContent           = "release2gitee-staging-v1\n"
	maxNameRunes            = 255
	maxAssetBytes     int64 = 4 << 30
	maxTransformBytes int64 = 16 << 20
)

// DownloadFunc streams a source asset into dst.
type DownloadFunc func(context.Context, domain.Asset, io.Writer) error

// TransformFunc transforms a fully downloaded asset before completion.
type TransformFunc func([]byte) ([]byte, error)

// Run owns a single program-created staging directory.
type Run struct {
	root string
	path string
}

// ReleaseStage owns the files for one GitHub release.
type ReleaseStage struct {
	path     string
	manifest stageManifest
	assets   map[string]manifestAsset
}

// PreparedAsset is the safe local representation of an original attachment.
type PreparedAsset struct {
	OriginalName string
	Path         string
	Size         int64
}

type stageManifest struct {
	Tag    string          `json:"tag"`
	Assets []manifestAsset `json:"assets"`
}

type manifestAsset struct {
	OriginalName string `json:"original_name"`
	LocalName    string `json:"local_name"`
}

// NewRun creates a private program-marked run directory under root.
func NewRun(root string) (*Run, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("staging root is empty")
	}
	if err := ensureStagingRoot(root); err != nil {
		return nil, err
	}
	path, err := os.MkdirTemp(root, "run-")
	if err != nil {
		return nil, fmt.Errorf("create run staging directory: %w", err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		_ = os.RemoveAll(path)
		return nil, fmt.Errorf("protect run staging directory: %w", err)
	}
	if err := writeMarker(path); err != nil {
		_ = os.RemoveAll(path)
		return nil, fmt.Errorf("write staging marker: %w", err)
	}
	return &Run{root: root, path: path}, nil
}

// Path returns the private run directory. It is intended only for controlled
// warning output after cleanup failure.
func (r *Run) Path() string { return r.path }

// NewRelease validates the source assets and creates an opaque subdirectory.
func (r *Run) NewRelease(tag string, assets []domain.Asset) (*ReleaseStage, error) {
	if r == nil || r.path == "" {
		return nil, errors.New("staging run is not initialized")
	}
	manifest, err := makeManifest(tag, assets)
	if err != nil {
		return nil, err
	}
	path, err := os.MkdirTemp(r.path, "release-")
	if err != nil {
		return nil, fmt.Errorf("create release staging directory: %w", err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		_ = os.RemoveAll(path)
		return nil, fmt.Errorf("protect release staging directory: %w", err)
	}
	if err := writeMarker(path); err != nil {
		_ = os.RemoveAll(path)
		return nil, fmt.Errorf("write release staging marker: %w", err)
	}
	rawManifest, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		_ = os.RemoveAll(path)
		return nil, fmt.Errorf("serialize staging manifest: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(path, "manifest.json"), rawManifest); err != nil {
		_ = os.RemoveAll(path)
		return nil, fmt.Errorf("write staging manifest: %w", err)
	}
	byName := make(map[string]manifestAsset, len(manifest.Assets))
	for _, asset := range manifest.Assets {
		byName[asset.OriginalName] = asset
	}
	return &ReleaseStage{path: path, manifest: manifest, assets: byName}, nil
}

// PrepareAsset downloads one verified source file and atomically makes it
// available for upload. transform is nil for ordinary attachments.
func (s *ReleaseStage) PrepareAsset(ctx context.Context, asset domain.Asset, download DownloadFunc, transform TransformFunc) (PreparedAsset, error) {
	if s == nil {
		return PreparedAsset{}, errors.New("release staging is not initialized")
	}
	if download == nil {
		return PreparedAsset{}, errors.New("asset downloader is nil")
	}
	manifest, ok := s.assets[asset.Name]
	if !ok {
		return PreparedAsset{}, fmt.Errorf("asset %q is not in staging manifest", asset.Name)
	}
	finalPath := filepath.Join(s.path, manifest.LocalName)
	partPath := finalPath + ".part"
	if asset.Size != nil && (*asset.Size < 0 || *asset.Size > maxAssetBytes) {
		return PreparedAsset{}, fmt.Errorf("asset %q exceeds %d-byte staging limit", asset.Name, maxAssetBytes)
	}
	file, err := os.OpenFile(partPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return PreparedAsset{}, fmt.Errorf("create partial asset %q: %w", asset.Name, err)
	}

	downloadErr := download(ctx, asset, &limitWriter{writer: file, remaining: maxAssetBytes})
	syncErr := file.Sync()
	closeErr := file.Close()
	if downloadErr != nil {
		_ = os.Remove(partPath)
		return PreparedAsset{}, fmt.Errorf("download asset %q: %w", asset.Name, downloadErr)
	}
	if syncErr != nil {
		_ = os.Remove(partPath)
		return PreparedAsset{}, fmt.Errorf("sync partial asset %q: %w", asset.Name, syncErr)
	}
	if closeErr != nil {
		_ = os.Remove(partPath)
		return PreparedAsset{}, fmt.Errorf("close partial asset %q: %w", asset.Name, closeErr)
	}

	info, err := os.Stat(partPath)
	if err != nil {
		_ = os.Remove(partPath)
		return PreparedAsset{}, fmt.Errorf("stat partial asset %q: %w", asset.Name, err)
	}
	if asset.Size != nil && info.Size() != *asset.Size {
		_ = os.Remove(partPath)
		return PreparedAsset{}, fmt.Errorf("downloaded size for %q is %d, want %d", asset.Name, info.Size(), *asset.Size)
	}

	if transform == nil {
		if err := os.Rename(partPath, finalPath); err != nil {
			_ = os.Remove(partPath)
			return PreparedAsset{}, fmt.Errorf("finalize asset %q: %w", asset.Name, err)
		}
		return PreparedAsset{OriginalName: asset.Name, Path: finalPath, Size: info.Size()}, nil
	}

	if info.Size() > maxTransformBytes {
		_ = os.Remove(partPath)
		return PreparedAsset{}, fmt.Errorf("asset %q exceeds %d-byte transformation limit", asset.Name, maxTransformBytes)
	}
	content, err := os.ReadFile(partPath)
	if err != nil {
		_ = os.Remove(partPath)
		return PreparedAsset{}, fmt.Errorf("read transform source %q: %w", asset.Name, err)
	}
	if !utf8.Valid(content) {
		_ = os.Remove(partPath)
		return PreparedAsset{}, fmt.Errorf("asset %q is not valid UTF-8", asset.Name)
	}
	transformed, err := transform(content)
	if err != nil {
		_ = os.Remove(partPath)
		return PreparedAsset{}, fmt.Errorf("transform asset %q: %w", asset.Name, err)
	}
	if err := writeFileAtomic(finalPath, transformed); err != nil {
		_ = os.Remove(partPath)
		return PreparedAsset{}, fmt.Errorf("write transformed asset %q: %w", asset.Name, err)
	}
	_ = os.Remove(partPath)
	return PreparedAsset{OriginalName: asset.Name, Path: finalPath, Size: int64(len(transformed))}, nil
}

// Open returns the prepared file for one upload.
func (s *ReleaseStage) Open(asset PreparedAsset) (*os.File, error) {
	return os.Open(asset.Path)
}

// Cleanup deletes only this program-created release directory.
func (s *ReleaseStage) Cleanup() error {
	if s == nil || s.path == "" {
		return nil
	}
	return os.RemoveAll(s.path)
}

// Cleanup deletes the complete run directory.
func (r *Run) Cleanup() error {
	if r == nil || r.path == "" {
		return nil
	}
	return os.RemoveAll(r.path)
}

// CleanupExpired safely removes only direct run children bearing MarkerName and
// older than age. It never walks arbitrary temporary directories.
func CleanupExpired(root string, age time.Duration, now time.Time) ([]string, error) {
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read staging root: %w", err)
	}
	var removed []string
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "run-") {
			continue
		}
		path := filepath.Join(root, entry.Name())
		if !hasProgramMarker(path) {
			continue
		}
		info, err := entry.Info()
		if err != nil || now.Sub(info.ModTime()) < age {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			return removed, fmt.Errorf("remove expired staging run %q: %w", path, err)
		}
		removed = append(removed, path)
	}
	return removed, nil
}

func makeManifest(tag string, assets []domain.Asset) (stageManifest, error) {
	if err := ValidateAssetNames(assets); err != nil {
		return stageManifest{}, err
	}
	manifest := stageManifest{Tag: tag}
	for index, asset := range assets {
		manifest.Assets = append(manifest.Assets, manifestAsset{
			OriginalName: asset.Name,
			LocalName:    fmt.Sprintf("asset-%03d", index+1),
		})
	}
	return manifest, nil
}

// ValidateAssetNames enforces a portable single-file-name contract before any
// local or remote write occurs.
func ValidateAssetNames(assets []domain.Asset) error {
	seen := make(map[string]string, len(assets))
	for _, asset := range assets {
		name := asset.Name
		if strings.TrimSpace(name) == "" {
			return errors.New("asset name is empty")
		}
		if !utf8.ValidString(name) {
			return fmt.Errorf("asset name %q is not valid UTF-8", name)
		}
		if utf8.RuneCountInString(name) > maxNameRunes {
			return fmt.Errorf("asset name %q is too long", name)
		}
		if strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
			return fmt.Errorf("asset name %q has a Windows-ambiguous suffix", name)
		}
		if strings.ContainsRune(name, 0) || strings.ContainsAny(name, "/\\") || filepath.IsAbs(name) || filepath.Base(name) != name || name == "." || name == ".." {
			return fmt.Errorf("asset name %q is not a safe single file name", name)
		}
		for _, char := range name {
			if unicode.Is(unicode.Mn, char) {
				return fmt.Errorf("asset name %q contains a combining mark and is not portable", name)
			}
		}
		if isWindowsReserved(name) {
			return fmt.Errorf("asset name %q is reserved on Windows", name)
		}
		key := strings.ToUpper(name)
		if previous, found := seen[key]; found {
			return fmt.Errorf("asset names %q and %q collide on a case-insensitive filesystem", previous, name)
		}
		seen[key] = name
	}
	return nil
}

func isWindowsReserved(name string) bool {
	trimmed := strings.TrimRight(name, ". ")
	if trimmed == "" {
		return true
	}
	base := strings.ToUpper(strings.SplitN(trimmed, ".", 2)[0])
	switch base {
	case "CON", "PRN", "AUX", "NUL", "COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9", "LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		return true
	default:
		return false
	}
}

func writeFileAtomic(path string, content []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(content); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}

func ensureStagingRoot(root string) error {
	info, err := os.Stat(root)
	switch {
	case err == nil:
		if !info.IsDir() {
			return fmt.Errorf("staging root %q is not a directory", root)
		}
		return nil
	case !os.IsNotExist(err):
		return fmt.Errorf("inspect staging root: %w", err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("create staging root: %w", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return fmt.Errorf("protect newly created staging root: %w", err)
	}
	return nil
}

func writeMarker(directory string) error {
	return os.WriteFile(filepath.Join(directory, MarkerName), []byte(markerContent), 0o600)
}

func hasProgramMarker(directory string) bool {
	markerPath := filepath.Join(directory, MarkerName)
	info, err := os.Lstat(markerPath)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	content, err := os.ReadFile(markerPath)
	return err == nil && string(content) == markerContent
}

type limitWriter struct {
	writer    io.Writer
	remaining int64
}

func (w *limitWriter) Write(content []byte) (int, error) {
	if int64(len(content)) <= w.remaining {
		count, err := w.writer.Write(content)
		w.remaining -= int64(count)
		return count, err
	}
	if w.remaining <= 0 {
		return 0, fmt.Errorf("asset exceeds %d-byte staging limit", maxAssetBytes)
	}
	count, err := w.writer.Write(content[:w.remaining])
	w.remaining -= int64(count)
	if err != nil {
		return count, err
	}
	return count, fmt.Errorf("asset exceeds %d-byte staging limit", maxAssetBytes)
}

// Seek and Truncate let a retried downloader safely discard a partial attempt.
// The wrapped staging file is reset before the next response body is copied.
func (w *limitWriter) Seek(offset int64, whence int) (int64, error) {
	seeker, ok := w.writer.(io.Seeker)
	if !ok {
		return 0, errors.New("staging download target does not support seeking")
	}
	return seeker.Seek(offset, whence)
}

func (w *limitWriter) Truncate(size int64) error {
	truncater, ok := w.writer.(interface{ Truncate(int64) error })
	if !ok {
		return errors.New("staging download target does not support truncation")
	}
	if err := truncater.Truncate(size); err != nil {
		return err
	}
	if size == 0 {
		w.remaining = maxAssetBytes
	}
	return nil
}
