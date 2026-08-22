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

	"sync-release-to-gitee/internal/config"
	"sync-release-to-gitee/internal/gitee"
	"sync-release-to-gitee/internal/github"
	"sync-release-to-gitee/internal/httpx"
	"sync-release-to-gitee/internal/syncer"
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
		_, _ = fmt.Fprintf(stderr, "sync-release-to-gitee: %v\n\n%s", err, config.Usage())
		return 2
	}

	logger := newLogger(cfg, stderr)
	for _, warning := range warnings {
		logger.Warn(warning)
	}
	logger.Info(
		"开始同步 Release：GitHub → Gitee",
		"GitHub 源仓库", repositoryName(cfg.GitHubOwner, cfg.GitHubRepo),
		"Gitee 目标仓库", repositoryName(cfg.GiteeOwner, cfg.GiteeRepo),
		"拉取 GitHub Release 上限", cfg.GitHubLatestReleaseCount,
		"保留附件的最新 Release 数", cfg.GiteeRetainReleaseAttachFilesCount,
		"Gitee 目标分支", configuredBranch(cfg.GiteeBranch),
		"执行模式", executionMode(cfg.DryRun),
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	httpClient := httpx.New(httpx.Options{UserAgent: "sync-release-to-gitee/" + version})
	githubClient := github.New(cfg.GitHubAPIBaseURL, cfg.GitHubOwner, cfg.GitHubRepo, cfg.GitHubToken, httpClient)
	giteeClient := gitee.New(cfg.GiteeAPIBaseURL, cfg.GiteeOwner, cfg.GiteeRepo, cfg.GiteeToken, httpClient)
	runner := syncer.New(cfg, githubClient, giteeClient, logger, syncer.RatePolicy{})
	summary, err := runner.Run(ctx)
	if cfg.DryRun {
		for _, line := range syncer.DryRunText(summary.Plan, cfg.GitHubLatestReleaseCount) {
			if _, writeErr := fmt.Fprintln(stdout, line); writeErr != nil {
				_, _ = fmt.Fprintf(stderr, "sync-release-to-gitee: 输出 dry-run 计划失败：%v\n", writeErr)
				return 1
			}
		}
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "sync-release-to-gitee: %v\n", err)
		return 1
	}
	logger.Info(
		"Release 同步完成",
		"仅预览", summary.DryRun,
		"已重建 Release 数", len(summary.CleanedTags),
		"已创建 Release 数", len(summary.CreatedTags),
		"已跳过 Release 数", len(summary.SkippedTags),
		"已上传附件数", uploadedAssetCount(summary.Uploaded),
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

func repositoryName(owner, repo string) string {
	return owner + "/" + repo
}

func configuredBranch(branch string) string {
	if branch == "" {
		return "自动检测"
	}
	return branch
}

func executionMode(dryRun bool) string {
	if dryRun {
		return "仅预览，不写入 Gitee"
	}
	return "实际同步"
}

func uploadedAssetCount(uploaded map[string][]string) int {
	count := 0
	for _, assets := range uploaded {
		count += len(assets)
	}
	return count
}
