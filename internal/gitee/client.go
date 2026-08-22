// Package gitee implements the Gitee release read/write API contract.
package gitee

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"sync-release-to-gitee/internal/domain"
	"sync-release-to-gitee/internal/httpx"
)

const maxMetadataBytes = 16 << 20

// Client is a repository-scoped Gitee API client.
type Client struct {
	BaseURL string
	Owner   string
	Repo    string
	Token   string
	HTTP    *httpx.Client
}

// New creates a repository-scoped Gitee client.
func New(baseURL, owner, repo, token string, client *httpx.Client) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Owner: owner, Repo: repo, Token: token, HTTP: client}
}

// ListReleases reads only Gitee's first 100-release page in ID-descending
// order, matching the Rust baseline.
func (c *Client) ListReleases(ctx context.Context) ([]domain.Release, error) {
	var payload []releaseDTO
	err := c.HTTP.Read(
		ctx,
		httpx.ReadOptions{RequestOptions: httpx.RequestOptions{Kind: httpx.Metadata, Retry: true, Operation: "list Gitee releases"}},
		func(requestContext context.Context) (*http.Request, error) {
			endpoint, err := c.endpoint("releases")
			if err != nil {
				return nil, err
			}
			query := endpoint.Query()
			query.Set("per_page", "100")
			query.Set("page", "1")
			endpoint.RawQuery = query.Encode()
			return c.request(requestContext, http.MethodGet, endpoint.String(), nil)
		},
		func(response *http.Response) error {
			return json.NewDecoder(io.LimitReader(response.Body, maxMetadataBytes)).Decode(&payload)
		},
	)
	if err != nil {
		return nil, fmt.Errorf("read Gitee releases: %w", err)
	}
	releases := make([]domain.Release, 0, len(payload))
	for _, release := range payload {
		releases = append(releases, release.domain())
	}
	sort.SliceStable(releases, func(i, j int) bool { return releases[i].ID > releases[j].ID })
	return releases, nil
}

// DefaultBranch reads the repository's default branch.
func (c *Client) DefaultBranch(ctx context.Context) (string, error) {
	var payload struct {
		DefaultBranch string `json:"default_branch"`
	}
	err := c.HTTP.Read(
		ctx,
		httpx.ReadOptions{RequestOptions: httpx.RequestOptions{Kind: httpx.Metadata, Retry: true, Operation: "get Gitee default branch"}},
		func(requestContext context.Context) (*http.Request, error) {
			endpoint, err := c.endpoint("")
			if err != nil {
				return nil, err
			}
			return c.request(requestContext, http.MethodGet, endpoint.String(), nil)
		},
		func(response *http.Response) error {
			return json.NewDecoder(io.LimitReader(response.Body, maxMetadataBytes)).Decode(&payload)
		},
	)
	if err != nil {
		return "", fmt.Errorf("read Gitee default branch: %w", err)
	}
	if strings.TrimSpace(payload.DefaultBranch) == "" {
		return "", fmt.Errorf("Gitee repository returned an empty default branch")
	}
	return payload.DefaultBranch, nil
}

// CreateRelease creates metadata only; assets must be uploaded separately.
func (c *Client) CreateRelease(ctx context.Context, input domain.CreateReleaseInput) (domain.Release, error) {
	endpoint, err := c.endpoint("releases")
	if err != nil {
		return domain.Release{}, err
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return domain.Release{}, fmt.Errorf("encode Gitee create release payload: %w", err)
	}
	request, err := c.request(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return domain.Release{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.HTTP.Do(ctx, request, httpx.RequestOptions{Kind: httpx.Metadata, Retry: false, Operation: "create Gitee release"})
	if err != nil {
		return domain.Release{}, err
	}
	defer response.Body.Close()
	if err := c.HTTP.CheckResponse(response, "create Gitee release"); err != nil {
		return domain.Release{}, err
	}
	rawResponse, err := io.ReadAll(io.LimitReader(response.Body, maxMetadataBytes))
	if err != nil {
		return domain.Release{}, &httpx.OpError{
			Operation: "create Gitee release",
			Method:    http.MethodPost,
			URL:       httpx.RedactURL(endpoint),
			Unknown:   true,
			Summary:   "successful create response could not be read",
			Err:       err,
		}
	}
	var release releaseDTO
	if err := json.Unmarshal(rawResponse, &release); err != nil {
		return domain.Release{}, &httpx.OpError{
			Operation: "create Gitee release",
			Method:    http.MethodPost,
			URL:       httpx.RedactURL(endpoint),
			Unknown:   true,
			Summary:   "successful create response could not be decoded",
			Err:       err,
		}
	}
	created := release.domain()
	if created.ID <= 0 || created.TagName == "" {
		return domain.Release{}, businessResponseError(endpoint, "create Gitee release", rawResponse)
	}
	if created.TagName != input.TagName {
		return domain.Release{}, &httpx.OpError{
			Operation: "create Gitee release",
			Method:    http.MethodPost,
			URL:       httpx.RedactURL(endpoint),
			Unknown:   true,
			Summary:   "create response tag does not match the requested tag",
		}
	}
	return created, nil
}

// DeleteRelease deletes exactly one Release ID and never retries the write.
func (c *Client) DeleteRelease(ctx context.Context, releaseID int64) error {
	endpoint, err := c.endpoint("releases", strconv.FormatInt(releaseID, 10))
	if err != nil {
		return err
	}
	request, err := c.request(ctx, http.MethodDelete, endpoint.String(), nil)
	if err != nil {
		return err
	}
	response, err := c.HTTP.Do(ctx, request, httpx.RequestOptions{Kind: httpx.Metadata, Retry: false, Operation: "delete Gitee release"})
	if err != nil {
		return err
	}
	defer response.Body.Close()
	return c.HTTP.CheckResponse(response, "delete Gitee release")
}

// UploadAsset streams a multipart file with the required "file" form field.
// filename is intentionally supplied separately from the local staging path.
func (c *Client) UploadAsset(ctx context.Context, releaseID int64, filename string, content io.Reader) error {
	endpoint, err := c.endpoint("releases", strconv.FormatInt(releaseID, 10), "attach_files")
	if err != nil {
		return err
	}
	reader, writer := io.Pipe()
	form := multipart.NewWriter(writer)
	copyDone := make(chan error, 1)
	go func() {
		part, err := form.CreateFormFile("file", filename)
		if err == nil {
			_, err = io.Copy(part, content)
		}
		if closeErr := form.Close(); err == nil {
			err = closeErr
		}
		copyDone <- err
		_ = writer.CloseWithError(err)
	}()

	request, err := c.request(ctx, http.MethodPost, endpoint.String(), reader)
	if err != nil {
		_ = reader.Close()
		return err
	}
	request.Header.Set("Content-Type", form.FormDataContentType())
	response, err := c.HTTP.Do(ctx, request, httpx.RequestOptions{Kind: httpx.Download, Retry: false, Operation: "upload Gitee asset"})
	if err != nil {
		_ = reader.Close()
		<-copyDone
		return err
	}
	checkErr := c.HTTP.CheckResponse(response, "upload Gitee asset")
	_ = response.Body.Close()
	_ = reader.Close()
	copyErr := <-copyDone
	if checkErr != nil {
		return checkErr
	}
	if copyErr != nil {
		return fmt.Errorf("stream Gitee asset %q: %w", filename, copyErr)
	}
	return nil
}

// FindReleaseByID re-queries the first Release page and returns the exact ID
// used by a previously successful create response.
func (c *Client) FindReleaseByID(ctx context.Context, releaseID int64) (domain.Release, bool, error) {
	releases, err := c.ListReleases(ctx)
	if err != nil {
		return domain.Release{}, false, err
	}
	for _, release := range releases {
		if release.ID == releaseID {
			return release, true, nil
		}
	}
	return domain.Release{}, false, nil
}

// FindReleaseByTag finds one exact tag on the currently supported first page.
// A duplicate tag is treated as unsafe ambiguity rather than an object to alter.
func (c *Client) FindReleaseByTag(ctx context.Context, tag string) (domain.Release, bool, error) {
	releases, err := c.ListReleases(ctx)
	if err != nil {
		return domain.Release{}, false, err
	}
	var match *domain.Release
	for _, release := range releases {
		if release.TagName != tag {
			continue
		}
		if match != nil {
			return domain.Release{}, false, fmt.Errorf("multiple Gitee releases have tag %q", tag)
		}
		copy := release
		match = &copy
	}
	if match == nil {
		return domain.Release{}, false, nil
	}
	return *match, true, nil
}

func (c *Client) request(ctx context.Context, method, rawURL string, body io.Reader) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, fmt.Errorf("build Gitee request: %w", err)
	}
	request.Header.Set("Authorization", "token "+c.Token)
	return request, nil
}

func (c *Client) endpoint(parts ...string) (*url.URL, error) {
	base, err := url.Parse(c.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse Gitee base URL: %w", err)
	}
	path := strings.TrimRight(base.Path, "/") + "/repos/" + url.PathEscape(c.Owner) + "/" + url.PathEscape(c.Repo)
	for _, part := range parts {
		if part != "" {
			path += "/" + url.PathEscape(part)
		}
	}
	base.Path = path
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
	Name string `json:"name"`
	Size *int64 `json:"size"`
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
		release.Assets = append(release.Assets, domain.Asset{Name: asset.Name, Size: asset.Size})
	}
	return release
}

func businessResponseError(endpoint *url.URL, operation string, rawResponse []byte) error {
	var payload struct {
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	_ = json.Unmarshal(rawResponse, &payload)
	summary := "remote API returned a successful HTTP status without a Release object"
	if message := strings.TrimSpace(payload.Message); message != "" {
		summary += ": " + truncateMessage(message, 200)
	} else if message := strings.TrimSpace(payload.Error); message != "" {
		summary += ": " + truncateMessage(message, 200)
	}
	return &httpx.OpError{
		Operation: operation,
		Method:    http.MethodPost,
		URL:       httpx.RedactURL(endpoint),
		Summary:   summary,
	}
}

func truncateMessage(value string, maximum int) string {
	if len(value) <= maximum {
		return value
	}
	return value[:maximum] + "…"
}
