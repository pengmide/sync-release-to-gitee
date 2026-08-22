package gitee

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sync-release-to-gitee/internal/domain"
	"sync-release-to-gitee/internal/httpx"
)

func TestListAndDefaultBranchContract(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Authorization"); got != "token gitee-token" {
			t.Errorf("Authorization = %q", got)
		}
		switch request.URL.Path {
		case "/repos/owner/repo/releases":
			if got := request.URL.Query().Get("per_page"); got != "100" {
				t.Errorf("per_page = %q", got)
			}
			if got := request.URL.Query().Get("page"); got != "1" {
				t.Errorf("page = %q", got)
			}
			_, _ = io.WriteString(writer, "[{\"id\":2,\"tag_name\":\"v2\",\"name\":\"v2\",\"body\":\"body\",\"prerelease\":false,\"target_commitish\":\"main\",\"assets\":[]}]")
		case "/repos/owner/repo":
			_, _ = io.WriteString(writer, "{\"default_branch\":\"main\"}")
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	client := New(server.URL, "owner", "repo", "gitee-token", httpx.New(httpx.Options{MetadataDoer: server.Client(), DownloadDoer: server.Client()}))
	releases, err := client.ListReleases(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(releases) != 1 || releases[0].ID != 2 {
		t.Fatalf("releases = %#v", releases)
	}
	branch, err := client.DefaultBranch(context.Background())
	if err != nil || branch != "main" {
		t.Fatalf("DefaultBranch() = %q, %v", branch, err)
	}
}

func TestCreateAndUploadContract(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Authorization"); got != "token gitee-token" {
			t.Errorf("Authorization = %q", got)
		}
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/repos/owner/repo/releases":
			var payload domain.CreateReleaseInput
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Error(err)
				return
			}
			if payload.TargetCommitish != "main" || payload.Body != "notes" {
				t.Errorf("payload = %#v", payload)
			}
			_, _ = io.WriteString(writer, "{\"id\":123,\"tag_name\":\"v1\",\"name\":\"v1\",\"body\":\"notes\",\"prerelease\":false,\"target_commitish\":\"main\",\"assets\":[]}")
		case request.Method == http.MethodPost && request.URL.Path == "/repos/owner/repo/releases/123/attach_files":
			mediaType, params, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
			if err != nil {
				t.Error(err)
				return
			}
			if mediaType != "multipart/form-data" {
				t.Errorf("media type = %q", mediaType)
			}
			reader := multipart.NewReader(request.Body, params["boundary"])
			part, err := reader.NextPart()
			if err != nil {
				t.Error(err)
				return
			}
			if part.FormName() != "file" || part.FileName() != "artifact.zip" {
				t.Errorf("part = form %q filename %q", part.FormName(), part.FileName())
			}
			content, _ := io.ReadAll(part)
			if string(content) != "payload" {
				t.Errorf("content = %q", content)
			}
			writer.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	client := New(server.URL, "owner", "repo", "gitee-token", httpx.New(httpx.Options{MetadataDoer: server.Client(), DownloadDoer: server.Client()}))
	created, err := client.CreateRelease(context.Background(), domain.CreateReleaseInput{TagName: "v1", Name: "v1", Body: "notes", TargetCommitish: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != 123 {
		t.Fatalf("created = %#v", created)
	}
	if err := client.UploadAsset(context.Background(), created.ID, "artifact.zip", bytes.NewBufferString("payload")); err != nil {
		t.Fatal(err)
	}
}

func TestFindReleaseByID(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.WriteString(writer, "[{\"id\":321,\"tag_name\":\"v1\",\"name\":\"v1\",\"body\":\"\",\"prerelease\":false,\"target_commitish\":\"main\",\"assets\":[]}]")
	}))
	defer server.Close()
	client := New(server.URL, "owner", "repo", "gitee-token", httpx.New(httpx.Options{MetadataDoer: server.Client(), DownloadDoer: server.Client()}))
	release, found, err := client.FindReleaseByID(context.Background(), 321)
	if err != nil || !found || release.TagName != "v1" {
		t.Fatalf("FindReleaseByID() = %#v, %t, %v", release, found, err)
	}
	if _, found, err := client.FindReleaseByID(context.Background(), 999); err != nil || found {
		t.Fatalf("missing FindReleaseByID() = found=%t err=%v", found, err)
	}
}

func TestFindReleaseByTagAndUnknownCreateResponse(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet:
			_, _ = io.WriteString(writer, "[{\"id\":321,\"tag_name\":\"v1\",\"name\":\"v1\",\"body\":\"\",\"prerelease\":false,\"target_commitish\":\"main\",\"assets\":[]}]")
		case request.Method == http.MethodPost:
			_, _ = io.WriteString(writer, "not JSON")
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	client := New(server.URL, "owner", "repo", "gitee-token", httpx.New(httpx.Options{MetadataDoer: server.Client(), DownloadDoer: server.Client()}))
	release, found, err := client.FindReleaseByTag(context.Background(), "v1")
	if err != nil || !found || release.ID != 321 {
		t.Fatalf("FindReleaseByTag() = %#v, %t, %v", release, found, err)
	}
	_, err = client.CreateRelease(context.Background(), domain.CreateReleaseInput{TagName: "v2", Name: "v2", Body: "body", TargetCommitish: "main"})
	if err == nil || !httpx.IsUnknown(err) {
		t.Fatalf("CreateRelease() error = %v, want unknown result", err)
	}
}

func TestCreateReleaseRejectsBusinessErrorWithHTTP2xx(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.WriteString(writer, "{\"message\":\"tag does not exist\"}")
	}))
	defer server.Close()
	client := New(server.URL, "owner", "repo", "gitee-token", httpx.New(httpx.Options{MetadataDoer: server.Client(), DownloadDoer: server.Client()}))
	_, err := client.CreateRelease(context.Background(), domain.CreateReleaseInput{TagName: "v1", Name: "v1", Body: "body", TargetCommitish: "main"})
	if err == nil || !strings.Contains(err.Error(), "tag does not exist") {
		t.Fatalf("CreateRelease() error = %v, want business response error", err)
	}
	if httpx.IsUnknown(err) {
		t.Fatalf("business response error should not be unknown: %v", err)
	}
}
