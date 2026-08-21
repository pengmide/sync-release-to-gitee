// Package github implements the read-only GitHub release API contract.
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/pengmd/sync-release-to-gitee/internal/domain"
	"github.com/pengmd/sync-release-to-gitee/internal/httpx"
)

const maxMetadataBytes = 16 << 20

// Client is a repository-scoped GitHub API client.
type Client struct {
	BaseURL string
	Owner   string
	Repo    string
	Token   string
	HTTP    *httpx.Client
}

// New creates a repository-scoped client.
func New(baseURL, owner, repo, token string, client *httpx.Client) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Owner: owner, Repo: repo, Token: token, HTTP: client}
}

// ListReleases retrieves only the first configured GitHub Release page and
// returns it in the Rust baseline's ID-descending order.
func (c *Client) ListReleases(ctx context.Context, perPage int) ([]domain.Release, error) {
	if perPage < 0 {
		return nil, fmt.Errorf("GitHub per_page cannot be negative")
	}
	var payload []releaseDTO
	err := c.HTTP.Read(
		ctx,
		httpx.ReadOptions{RequestOptions: httpx.RequestOptions{Kind: httpx.Metadata, Retry: true, Operation: "list GitHub releases"}},
		func(requestContext context.Context) (*http.Request, error) {
			endpoint, err := c.endpoint("releases")
			if err != nil {
				return nil, err
			}
			query := endpoint.Query()
			query.Set("per_page", fmt.Sprintf("%d", perPage))
			query.Set("page", "1")
			endpoint.RawQuery = query.Encode()
			request, err := http.NewRequestWithContext(requestContext, http.MethodGet, endpoint.String(), nil)
			if err != nil {
				return nil, fmt.Errorf("build GitHub releases request: %w", err)
			}
			if c.Token != "" {
				request.Header.Set("Authorization", "token "+c.Token)
			}
			return request, nil
		},
		func(response *http.Response) error {
			return json.NewDecoder(io.LimitReader(response.Body, maxMetadataBytes)).Decode(&payload)
		},
	)
	if err != nil {
		return nil, fmt.Errorf("read GitHub releases: %w", err)
	}
	releases := make([]domain.Release, 0, len(payload))
	for _, release := range payload {
		normalized := release.domain()
		if normalized.Body == nil || *normalized.Body == "" {
			body := normalized.TagName
			normalized.Body = &body
		}
		releases = append(releases, normalized)
	}
	sort.SliceStable(releases, func(i, j int) bool { return releases[i].ID > releases[j].ID })
	return releases, nil
}

// DownloadAsset streams an unauthenticated browser download URL, preserving the
// Rust baseline's public-release scope.
func (c *Client) DownloadAsset(ctx context.Context, asset domain.Asset, destination io.Writer) error {
	if asset.DownloadURL == "" {
		return fmt.Errorf("asset %q has no download URL", asset.Name)
	}
	return c.HTTP.Read(
		ctx,
		httpx.ReadOptions{
			RequestOptions: httpx.RequestOptions{Kind: httpx.Download, Retry: true, Operation: "download GitHub asset"},
			BeforeAttempt: func(attempt int) error {
				if attempt <= 1 {
					return nil
				}
				return resetDestination(destination)
			},
		},
		func(requestContext context.Context) (*http.Request, error) {
			request, err := http.NewRequestWithContext(requestContext, http.MethodGet, asset.DownloadURL, nil)
			if err != nil {
				return nil, fmt.Errorf("build asset download request: %w", err)
			}
			return request, nil
		},
		func(response *http.Response) error {
			_, err := io.Copy(destination, response.Body)
			return err
		},
	)
}

func (c *Client) endpoint(suffix string) (*url.URL, error) {
	base, err := url.Parse(c.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse GitHub base URL: %w", err)
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/repos/" + url.PathEscape(c.Owner) + "/" + url.PathEscape(c.Repo) + "/" + suffix
	return base, nil
}

type releaseDTO struct {
	ID              int64      `json:"id"`
	TagName         string     `json:"tag_name"`
	Name            string     `json:"name"`
	Body            *string    `json:"body"`
	Prerelease      bool       `json:"prerelease"`
	TargetCommitish string     `json:"target_commitish"`
	Assets          []assetDTO `json:"assets"`
}

type assetDTO struct {
	Name               string `json:"name"`
	Size               *int64 `json:"size"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

func (r releaseDTO) domain() domain.Release {
	release := domain.Release{
		ID:              r.ID,
		TagName:         r.TagName,
		Name:            r.Name,
		Body:            r.Body,
		Prerelease:      r.Prerelease,
		TargetCommitish: r.TargetCommitish,
	}
	for _, asset := range r.Assets {
		release.Assets = append(release.Assets, domain.Asset{
			Name:        asset.Name,
			Size:        asset.Size,
			DownloadURL: asset.BrowserDownloadURL,
		})
	}
	return release
}

func resetDestination(destination io.Writer) error {
	type seekTruncater interface {
		Seek(int64, int) (int64, error)
		Truncate(int64) error
	}
	file, ok := destination.(seekTruncater)
	if !ok {
		return fmt.Errorf("download destination cannot be reset for retry")
	}
	if err := file.Truncate(0); err != nil {
		return err
	}
	_, err := file.Seek(0, io.SeekStart)
	return err
}
