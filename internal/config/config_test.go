package config

import (
	"errors"
	"strings"
	"testing"
)

func TestLoadDefaultsAndCanonicalEnvironment(t *testing.T) {
	t.Parallel()
	env := map[string]string{
		"GITHUB_OWNER": "github-owner",
		"GITHUB_REPO":  "github-repo",
		"GITEE_OWNER":  "gitee-owner",
		"GITEE_REPO":   "gitee-repo",
		"GITEE_TOKEN":  "very-secret-token",
	}
	cfg, warnings, err := Load(nil, mapLookup(env))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
	if cfg.GitHubLatestReleaseCount != 5 || cfg.GiteeRetainReleaseAttachFilesCount != 3 {
		t.Fatalf("defaults = %#v", cfg)
	}
	if !cfg.ReleaseBodyURLReplace || !cfg.LatestJSONURLReplace {
		t.Fatalf("boolean defaults = %#v", cfg)
	}
}

func TestLoadPrecedenceAndAliasWarning(t *testing.T) {
	t.Parallel()
	env := map[string]string{
		"GITHUB_OWNER": "canonical",
		"github_owner": "legacy",
		"GITHUB_REPO":  "github-repo",
		"GITEE_OWNER":  "gitee-owner",
		"GITEE_REPO":   "gitee-repo",
		"GITEE_TOKEN":  "secret",
	}
	cfg, warnings, err := Load([]string{"--github-owner=cli"}, mapLookup(env))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.GitHubOwner != "cli" {
		t.Fatalf("GitHubOwner = %q, want cli", cfg.GitHubOwner)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings with CLI override = %v, want none", warnings)
	}

	cfg, warnings, err = Load(nil, mapLookup(env))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.GitHubOwner != "canonical" {
		t.Fatalf("GitHubOwner = %q, want canonical", cfg.GitHubOwner)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "GITHUB_OWNER") {
		t.Fatalf("warnings = %v, want conflict warning", warnings)
	}
	if strings.Contains(warnings[0], "canonical") || strings.Contains(warnings[0], "legacy") {
		t.Fatalf("warning leaked values: %q", warnings[0])
	}
}

func TestLoadLongEnvironmentAndExplicitFalse(t *testing.T) {
	t.Parallel()
	env := map[string]string{
		"GITHUB_OWNER": "github-owner",
		"GITHUB_REPO":  "github-repo",
		"GITEE_OWNER":  "gitee-owner",
		"GITEE_REPO":   "gitee-repo",
		"GITEE_TOKEN":  "secret",
		"release2gitee__github_latest_release_count":             "8",
		"release2gitee__gitee_retain_release_attach_files_count": "0",
		"release2gitee__release_body_url_replace":                "false",
		"release2gitee__latest_json_url_replace":                 "false",
		"release2gitee__gitee_branch":                            "stable",
	}
	cfg, _, err := Load([]string{"--release-body-url-replace=true"}, mapLookup(env))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.GitHubLatestReleaseCount != 8 || cfg.GiteeRetainReleaseAttachFilesCount != 0 {
		t.Fatalf("counts = %#v", cfg)
	}
	if !cfg.ReleaseBodyURLReplace || cfg.LatestJSONURLReplace {
		t.Fatalf("boolean precedence = %#v", cfg)
	}
	if cfg.GiteeBranch != "stable" {
		t.Fatalf("GiteeBranch = %q", cfg.GiteeBranch)
	}
}

func TestLoadVersionAndHelp(t *testing.T) {
	t.Parallel()
	_, _, err := Load([]string{"--version"}, mapLookup(nil))
	if !errors.Is(err, ErrVersionRequested) {
		t.Fatalf("version error = %v", err)
	}
	_, _, err = Load([]string{"--help"}, mapLookup(nil))
	if !errors.Is(err, ErrHelpRequested) {
		t.Fatalf("help error = %v", err)
	}
}

func TestRedactedSummaryNeverIncludesTokens(t *testing.T) {
	t.Parallel()
	cfg := Defaults()
	cfg.GitHubOwner = "owner"
	cfg.GitHubRepo = "repo"
	cfg.GitHubToken = "github-very-secret"
	cfg.GiteeOwner = "gitee-owner"
	cfg.GiteeRepo = "gitee-repo"
	cfg.GiteeToken = "gitee-very-secret"
	summary := RedactedSummary(cfg)
	for _, secret := range []string{cfg.GitHubToken, cfg.GiteeToken, "github-very", "gitee-very"} {
		if strings.Contains(summary, secret) {
			t.Fatalf("summary leaked %q: %s", secret, summary)
		}
	}
}

func TestValidateRejectsWhitespaceIdentifiers(t *testing.T) {
	t.Parallel()
	cfg := Defaults()
	cfg.GitHubOwner = " owner "
	cfg.GitHubRepo = "repo"
	cfg.GiteeOwner = "gitee"
	cfg.GiteeRepo = "repo"
	cfg.GiteeToken = "token"
	if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), "whitespace") {
		t.Fatalf("Validate() error = %v, want whitespace error", err)
	}
}

func mapLookup(values map[string]string) LookupEnv {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}
