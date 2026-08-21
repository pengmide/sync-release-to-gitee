#!/usr/bin/env sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
project_root=$(CDPATH= cd -- "$script_dir/.." && pwd)
binary="$project_root/dist/sync-release-to-gitee"

# GitHub (lumi-ai-lab/harness-data) -> Gitee (git_pengmd/harness-release).
export GITHUB_OWNER=lumi-ai-lab
export GITHUB_REPO=harness-data
export GITEE_OWNER=git_pengmd
export GITEE_REPO=harness-release

# Keep the legacy retention behavior: only the latest Release retains assets.
export release2gitee__github_latest_release_count=1
export release2gitee__gitee_retain_release_attach_files_count=1

: "${GITEE_TOKEN:?set GITEE_TOKEN through a secret store or CI secret}"
[ -x "$binary" ] || {
  echo "missing executable: $binary (run scripts/build-dist.sh first)" >&2
  exit 1
}

exec "$binary" -v "$@"
