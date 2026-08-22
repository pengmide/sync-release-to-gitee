// Package config parses the backwards-compatible command line and environment.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	DefaultGitHubAPIBaseURL = "https://api.github.com"
	DefaultGiteeAPIBaseURL  = "https://gitee.com/api/v5"
)

var (
	// ErrHelpRequested asks the caller to print Usage and exit successfully.
	ErrHelpRequested = errors.New("help requested")
	// ErrVersionRequested asks the caller to print its version and exit successfully.
	ErrVersionRequested = errors.New("version requested")
)

// Config is the complete configuration for one synchronization run.
type Config struct {
	GitHubOwner string
	GitHubRepo  string
	GitHubToken string

	GiteeOwner string
	GiteeRepo  string
	GiteeToken string

	GitHubLatestReleaseCount           int
	GiteeRetainReleaseAttachFilesCount int
	ReleaseBodyURLReplace              bool
	LatestJSONURLReplace               bool
	GiteeBranch                        string
	Verbosity                          int
	Quiet                              bool
	DryRun                             bool

	GitHubAPIBaseURL string
	GiteeAPIBaseURL  string
	StagingRoot      string
}

// Defaults returns production-safe non-secret defaults.
func Defaults() Config {
	return Config{
		GitHubLatestReleaseCount:           5,
		GiteeRetainReleaseAttachFilesCount: 3,
		ReleaseBodyURLReplace:              true,
		LatestJSONURLReplace:               true,
		GitHubAPIBaseURL:                   DefaultGitHubAPIBaseURL,
		GiteeAPIBaseURL:                    DefaultGiteeAPIBaseURL,
		StagingRoot:                        filepath.Join(os.TempDir(), "release2gitee"),
	}
}

// Usage returns the stable help text shown by the binary.
func Usage() string {
	return `用法：
  sync-release-to-gitee [选项]

必填配置：
  --github-owner, --github-repo, --gitee-owner, --gitee-repo, --gitee-token

选项：
  --github-owner string
  --github-repo string
  --github-token string
  --gitee-owner string
  --gitee-repo string
  --gitee-token string
  --github-latest-release-count int                 （默认 5）
  --gitee-retain-release-attach-files-count int     （默认 3）
  --release-body-url-replace[=true|false]           （默认 true）
  --latest-json-url-replace[=true|false]            （默认 true）
  --gitee-branch string
  --dry-run                                      仅输出计划，不执行写入
  -v, --verbose                                 输出调试日志
  -q, --quiet                                   静默模式
  --version                                     输出版本
  -h, --help                                    显示本帮助
`
}

// LookupEnv allows tests to provide a deterministic environment.
type LookupEnv func(string) (string, bool)

// Load parses args and environment using the documented compatibility order.
func Load(args []string, lookup LookupEnv) (Config, []string, error) {
	if lookup == nil {
		lookup = os.LookupEnv
	}

	cfg := Defaults()
	var warnings []string

	fs := flag.NewFlagSet("sync-release-to-gitee", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var (
		githubOwner   string
		githubRepo    string
		githubToken   string
		giteeOwner    string
		giteeRepo     string
		giteeToken    string
		githubCount   int
		retainCount   int
		bodyReplace   bool
		latestReplace bool
		giteeBranch   string
		dryRun        bool
		version       bool
		verbosity     countFlag
		quiet         bool
	)

	fs.StringVar(&githubOwner, "github-owner", "", "GitHub owner")
	fs.StringVar(&githubRepo, "github-repo", "", "GitHub repository")
	fs.StringVar(&githubToken, "github-token", "", "GitHub token")
	fs.StringVar(&giteeOwner, "gitee-owner", "", "Gitee owner")
	fs.StringVar(&giteeRepo, "gitee-repo", "", "Gitee repository")
	fs.StringVar(&giteeToken, "gitee-token", "", "Gitee token")
	fs.IntVar(&githubCount, "github-latest-release-count", cfg.GitHubLatestReleaseCount, "GitHub release count")
	fs.IntVar(&retainCount, "gitee-retain-release-attach-files-count", cfg.GiteeRetainReleaseAttachFilesCount, "retained asset tag count")
	fs.BoolVar(&bodyReplace, "release-body-url-replace", cfg.ReleaseBodyURLReplace, "replace repository URL in release body")
	fs.BoolVar(&latestReplace, "latest-json-url-replace", cfg.LatestJSONURLReplace, "replace repository URL in latest.json")
	fs.StringVar(&giteeBranch, "gitee-branch", "", "Gitee target branch")
	fs.BoolVar(&dryRun, "dry-run", false, "show the plan without writing")
	fs.BoolVar(&version, "version", false, "show version")
	fs.Var(&verbosity, "v", "increase verbosity")
	fs.Var(&verbosity, "verbose", "increase verbosity")
	fs.BoolVar(&quiet, "q", false, "suppress non-error output")
	fs.BoolVar(&quiet, "quiet", false, "suppress non-error output")

	expanded := expandShortVerbosity(args)
	if err := fs.Parse(expanded); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return Config{}, nil, ErrHelpRequested
		}
		return Config{}, nil, fmt.Errorf("parse flags: %w", err)
	}
	if len(fs.Args()) != 0 {
		return Config{}, nil, fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " "))
	}
	if version {
		return Config{}, nil, ErrVersionRequested
	}

	visited := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) {
		visited[f.Name] = true
	})

	cfg.GitHubOwner, warnings = resolveBase("github owner", githubOwner, visited["github-owner"], "GITHUB_OWNER", "github_owner", "", lookup, warnings)
	cfg.GitHubRepo, warnings = resolveBase("github repo", githubRepo, visited["github-repo"], "GITHUB_REPO", "github_repo", "", lookup, warnings)
	cfg.GitHubToken, warnings = resolveBase("github token", githubToken, visited["github-token"], "GITHUB_TOKEN", "github_token", "", lookup, warnings)
	cfg.GiteeOwner, warnings = resolveBase("gitee owner", giteeOwner, visited["gitee-owner"], "GITEE_OWNER", "gitee_owner", "", lookup, warnings)
	cfg.GiteeRepo, warnings = resolveBase("gitee repo", giteeRepo, visited["gitee-repo"], "GITEE_REPO", "gitee_repo", "", lookup, warnings)
	cfg.GiteeToken, warnings = resolveBase("gitee token", giteeToken, visited["gitee-token"], "GITEE_TOKEN", "gitee_token", "", lookup, warnings)

	var err error
	cfg.GitHubLatestReleaseCount, err = resolveInt(
		"github latest release count",
		githubCount,
		visited["github-latest-release-count"],
		"release2gitee__github_latest_release_count",
		cfg.GitHubLatestReleaseCount,
		lookup,
	)
	if err != nil {
		return Config{}, warnings, err
	}
	cfg.GiteeRetainReleaseAttachFilesCount, err = resolveInt(
		"gitee retain release attach files count",
		retainCount,
		visited["gitee-retain-release-attach-files-count"],
		"release2gitee__gitee_retain_release_attach_files_count",
		cfg.GiteeRetainReleaseAttachFilesCount,
		lookup,
	)
	if err != nil {
		return Config{}, warnings, err
	}
	cfg.ReleaseBodyURLReplace, err = resolveBool(
		"release body URL replace",
		bodyReplace,
		visited["release-body-url-replace"],
		"release2gitee__release_body_url_replace",
		cfg.ReleaseBodyURLReplace,
		lookup,
	)
	if err != nil {
		return Config{}, warnings, err
	}
	cfg.LatestJSONURLReplace, err = resolveBool(
		"latest.json URL replace",
		latestReplace,
		visited["latest-json-url-replace"],
		"release2gitee__latest_json_url_replace",
		cfg.LatestJSONURLReplace,
		lookup,
	)
	if err != nil {
		return Config{}, warnings, err
	}
	cfg.GiteeBranch = resolveNamedString(giteeBranch, visited["gitee-branch"], "release2gitee__gitee_branch", lookup)
	cfg.Verbosity = int(verbosity)
	cfg.Quiet = quiet
	cfg.DryRun = dryRun

	if err := Validate(cfg); err != nil {
		return Config{}, warnings, err
	}
	return cfg, warnings, nil
}

// Validate rejects impossible or unsafe configurations before network work.
func Validate(cfg Config) error {
	required := []struct {
		name  string
		value string
	}{
		{"github owner", cfg.GitHubOwner},
		{"github repo", cfg.GitHubRepo},
		{"gitee owner", cfg.GiteeOwner},
		{"gitee repo", cfg.GiteeRepo},
		{"gitee token", cfg.GiteeToken},
	}
	for _, item := range required {
		trimmed := strings.TrimSpace(item.value)
		if trimmed == "" {
			return fmt.Errorf("%s is required", item.name)
		}
		if trimmed != item.value {
			return fmt.Errorf("%s cannot have leading or trailing whitespace", item.name)
		}
	}
	if cfg.GitHubLatestReleaseCount < 0 {
		return errors.New("github latest release count cannot be negative")
	}
	if cfg.GiteeRetainReleaseAttachFilesCount < 0 {
		return errors.New("gitee retain release attach files count cannot be negative")
	}
	if strings.TrimSpace(cfg.GitHubAPIBaseURL) == "" || strings.TrimSpace(cfg.GiteeAPIBaseURL) == "" {
		return errors.New("API base URLs cannot be empty")
	}
	if strings.TrimSpace(cfg.StagingRoot) == "" {
		return errors.New("staging root cannot be empty")
	}
	return nil
}

// RedactedSummary provides safe configuration observability.
func RedactedSummary(cfg Config) string {
	return fmt.Sprintf(
		"github-owner=%q github-repo=%q github-token=%s gitee-owner=%q gitee-repo=%q gitee-token=%s github-latest-release-count=%d gitee-retain-release-attach-files-count=%d release-body-url-replace=%t latest-json-url-replace=%t gitee-branch=%q dry-run=%t",
		cfg.GitHubOwner,
		cfg.GitHubRepo,
		redacted(cfg.GitHubToken),
		cfg.GiteeOwner,
		cfg.GiteeRepo,
		redacted(cfg.GiteeToken),
		cfg.GitHubLatestReleaseCount,
		cfg.GiteeRetainReleaseAttachFilesCount,
		cfg.ReleaseBodyURLReplace,
		cfg.LatestJSONURLReplace,
		cfg.GiteeBranch,
		cfg.DryRun,
	)
}

func redacted(value string) string {
	if value == "" {
		return "<none>"
	}
	return "<redacted>"
}

type countFlag int

func (c *countFlag) String() string { return strconv.Itoa(int(*c)) }

func (c *countFlag) Set(value string) error {
	enabled, err := strconv.ParseBool(value)
	if err != nil {
		return err
	}
	if enabled {
		*c++
	}
	return nil
}

func (c *countFlag) IsBoolFlag() bool { return true }

func expandShortVerbosity(args []string) []string {
	expanded := make([]string, 0, len(args))
	for _, arg := range args {
		if len(arg) > 2 && strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") {
			shorts := arg[1:]
			allVerbosity := true
			for _, char := range shorts {
				if char != 'v' {
					allVerbosity = false
					break
				}
			}
			if allVerbosity {
				for range shorts {
					expanded = append(expanded, "-v")
				}
				continue
			}
		}
		expanded = append(expanded, arg)
	}
	return expanded
}

func resolveBase(label, cliValue string, cliSet bool, canonicalEnv, legacyEnv, defaultValue string, lookup LookupEnv, warnings []string) (string, []string) {
	if cliSet {
		return cliValue, warnings
	}
	canonical, canonicalSet := lookup(canonicalEnv)
	legacy, legacySet := lookup(legacyEnv)
	if canonicalSet {
		if legacySet && canonical != legacy {
			warnings = append(warnings, fmt.Sprintf("both %s and %s are set for %s; using %s", canonicalEnv, legacyEnv, label, canonicalEnv))
		}
		return canonical, warnings
	}
	if legacySet {
		return legacy, warnings
	}
	return defaultValue, warnings
}

func resolveNamedString(cliValue string, cliSet bool, env string, lookup LookupEnv) string {
	if cliSet {
		return cliValue
	}
	if value, ok := lookup(env); ok {
		return value
	}
	return ""
}

func resolveInt(label string, cliValue int, cliSet bool, env string, defaultValue int, lookup LookupEnv) (int, error) {
	if cliSet {
		return cliValue, nil
	}
	raw, ok := lookup(env)
	if !ok {
		return defaultValue, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s from %s must be an integer: %w", label, env, err)
	}
	return value, nil
}

func resolveBool(label string, cliValue bool, cliSet bool, env string, defaultValue bool, lookup LookupEnv) (bool, error) {
	if cliSet {
		return cliValue, nil
	}
	raw, ok := lookup(env)
	if !ok {
		return defaultValue, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s from %s must be true or false: %w", label, env, err)
	}
	return value, nil
}
