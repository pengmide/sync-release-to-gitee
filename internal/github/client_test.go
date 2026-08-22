package github

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"sync-release-to-gitee/internal/domain"
	"sync-release-to-gitee/internal/httpx"
	"sync-release-to-gitee/internal/staging"
)

func TestListReleasesUsesFirstPageAndNormalizesBody(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/repos/owner/repo/releases" {
			t.Errorf("path = %q", request.URL.Path)
		}
		if got := request.URL.Query().Get("per_page"); got != "5" {
			t.Errorf("per_page = %q", got)
		}
		if got := request.URL.Query().Get("page"); got != "1" {
			t.Errorf("page = %q", got)
		}
		if got := request.Header.Get("Authorization"); got != "token git-token" {
			t.Errorf("Authorization = %q", got)
		}
		_, _ = io.WriteString(writer, `[
			{"id":2,"tag_name":"v2.0.0","name":"two","body":null,"prerelease":false,"target_commitish":"main","assets":[]},
			{"id":1,"tag_name":"v1.0.0","name":"one","body":"","prerelease":false,"target_commitish":"main","assets":[]}
		]`)
	}))
	defer server.Close()
	client := New(server.URL, "owner", "repo", "git-token", httpx.New(httpx.Options{MetadataDoer: server.Client(), DownloadDoer: server.Client()}))
	releases, err := client.ListReleases(context.Background(), 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(releases) != 2 || releases[0].ID != 2 || releases[1].ID != 1 {
		t.Fatalf("releases = %#v", releases)
	}
	if releases[0].Body == nil || *releases[0].Body != "v2.0.0" || releases[1].Body == nil || *releases[1].Body != "v1.0.0" {
		t.Fatalf("normalized bodies = %#v", releases)
	}
}

func TestDownloadAssetDoesNotAttachGitHubToken(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Authorization"); got != "" {
			t.Errorf("download Authorization = %q, want empty", got)
		}
		_, _ = io.WriteString(writer, "payload")
	}))
	defer server.Close()
	client := New(server.URL, "owner", "repo", "git-token", httpx.New(httpx.Options{MetadataDoer: server.Client(), DownloadDoer: server.Client()}))
	var target strings.Builder
	if err := client.DownloadAsset(context.Background(), domainAsset("file", server.URL), &target); err != nil {
		t.Fatal(err)
	}
	if target.String() != "payload" {
		t.Fatalf("payload = %q", target.String())
	}
}

func TestDownloadAssetRetriesIntoStagedDestination(t *testing.T) {
	t.Parallel()
	var calls int
	client := New("https://api.example.test", "owner", "repo", "git-token", httpx.New(httpx.Options{
		DownloadDoer: doerFunc(func(request *http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				return &http.Response{
					StatusCode: http.StatusOK,
					Status:     "200 OK",
					Header:     make(http.Header),
					Body:       interruptedBody{},
					Request:    request,
				}, nil
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("complete")),
				Request:    request,
			}, nil
		}),
		Sleeper: httpx.SleepFunc(func(context.Context, time.Duration) error { return nil }),
		Retry:   httpx.RetryPolicy{MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
	}))

	run, err := staging.NewRun(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer run.Cleanup()
	size := int64(len("complete"))
	asset := domain.Asset{Name: "artifact.zip", Size: &size, DownloadURL: "https://downloads.example.test/artifact.zip"}
	stage, err := run.NewRelease("v1.0.0", []domain.Asset{asset})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := stage.PrepareAsset(context.Background(), asset, client.DownloadAsset, nil)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(prepared.Path)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || string(content) != "complete" {
		t.Fatalf("calls=%d content=%q, want 2 and complete", calls, content)
	}
}

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(request *http.Request) (*http.Response, error) { return f(request) }

type interruptedBody struct{}

func (interruptedBody) Read(buffer []byte) (int, error) {
	copy(buffer, "partial")
	return len("partial"), io.ErrUnexpectedEOF
}

func (interruptedBody) Close() error { return nil }

func domainAsset(name, downloadURL string) domain.Asset {
	return domain.Asset{Name: name, DownloadURL: downloadURL}
}
