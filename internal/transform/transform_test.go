package transform

import "testing"

func TestGitHubBodyNormalizesOnlyEmpty(t *testing.T) {
	t.Parallel()
	options := Options{GitHubOwner: "gh", GitHubRepo: "repo", GiteeOwner: "gt", GiteeRepo: "mirror"}
	if got := GitHubBody(nil, "v1.0.0", true, options); got != "v1.0.0" {
		t.Fatalf("nil body = %q", got)
	}
	empty := ""
	if got := GitHubBody(&empty, "v1.0.0", true, options); got != "v1.0.0" {
		t.Fatalf("empty body = %q", got)
	}
	space := " "
	if got := GitHubBody(&space, "v1.0.0", true, options); got != " " {
		t.Fatalf("space body = %q", got)
	}
}

func TestReplaceRepoURLIsExact(t *testing.T) {
	t.Parallel()
	options := Options{GitHubOwner: "gh", GitHubRepo: "repo", GiteeOwner: "gt", GiteeRepo: "mirror"}
	input := "https://github.com/gh/repo/a https://github.com/gh/repos"
	if got, want := ReplaceRepoURL(input, options), "https://gitee.com/gt/mirror/a https://gitee.com/gt/mirrors"; got != want {
		t.Fatalf("ReplaceRepoURL() = %q, want %q", got, want)
	}
}

func TestLatestJSONRejectsInvalidUTF8WhenEnabled(t *testing.T) {
	t.Parallel()
	_, err := LatestJSON([]byte{0xff}, true, Options{})
	if err == nil {
		t.Fatal("LatestJSON() error = nil, want invalid UTF-8 error")
	}
	if _, err := LatestJSON([]byte{0xff}, false, Options{}); err != nil {
		t.Fatalf("LatestJSON disabled error = %v", err)
	}
}
