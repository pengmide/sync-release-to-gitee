// release2gitee mirrors GitHub Releases to Gitee Releases.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"

	"github.com/pengmd/sync-release-to-gitee/internal/config"
	"github.com/pengmd/sync-release-to-gitee/internal/gitee"
	"github.com/pengmd/sync-release-to-gitee/internal/github"
	"github.com/pengmd/sync-release-to-gitee/internal/httpx"
	"github.com/pengmd/sync-release-to-gitee/internal/syncer"
)

// version is intentionally mutable so release builds can inject a tag with:
// -ldflags "-X main.version=vX.Y.Z".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, os.LookupEnv))
}

func run(args []string, stdout, stderr io.Writer, lookup config.LookupEnv) int {
	cfg, warnings, err := config.Load(args, lookup)
	switch {
	case errors.Is(err, config.ErrHelpRequested):
		_, _ = fmt.Fprint(stdout, config.Usage())
		return 0
	case errors.Is(err, config.ErrVersionRequested):
		_, _ = fmt.Fprintln(stdout, version)
		return 0
	case err != nil:
		_, _ = fmt.Fprintf(stderr, "release2gitee: %v\n\n%s", err, config.Usage())
		return 2
	}

	logger := newLogger(cfg, stderr)
	for _, warning := range warnings {
		logger.Warn(warning)
	}
	logger.Info("starting release synchronization", "config", config.RedactedSummary(cfg))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	httpClient := httpx.New(httpx.Options{UserAgent: "release2gitee/" + version})
	githubClient := github.New(cfg.GitHubAPIBaseURL, cfg.GitHubOwner, cfg.GitHubRepo, cfg.GitHubToken, httpClient)
	giteeClient := gitee.New(cfg.GiteeAPIBaseURL, cfg.GiteeOwner, cfg.GiteeRepo, cfg.GiteeToken, httpClient)
	runner := syncer.New(cfg, githubClient, giteeClient, logger, syncer.RatePolicy{})
	summary, err := runner.Run(ctx)
	if cfg.DryRun {
		for _, line := range syncer.DryRunText(summary.Plan, cfg.GitHubLatestReleaseCount) {
			if _, writeErr := fmt.Fprintln(stdout, line); writeErr != nil {
				_, _ = fmt.Fprintf(stderr, "release2gitee: print dry-run plan: %v\n", writeErr)
				return 1
			}
		}
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "release2gitee: %v\n", err)
		return 1
	}
	logger.Info(
		"release synchronization completed",
		"dry_run", summary.DryRun,
		"cleaned", len(summary.CleanedTags),
		"created", len(summary.CreatedTags),
		"skipped", len(summary.SkippedTags),
	)
	return 0
}

func newLogger(cfg config.Config, output io.Writer) *slog.Logger {
	level := slog.LevelInfo
	if cfg.Verbosity > 0 {
		level = slog.LevelDebug
	}
	if cfg.Quiet {
		level = slog.LevelError + 4
	}
	return slog.New(slog.NewTextHandler(output, &slog.HandlerOptions{Level: level}))
}
