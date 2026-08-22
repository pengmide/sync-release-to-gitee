#!/usr/bin/env sh
set -eu

: "${GITHUB_OWNER:?set GITHUB_OWNER}"
: "${GITHUB_REPO:?set GITHUB_REPO}"
: "${GITEE_OWNER:?set GITEE_OWNER}"
: "${GITEE_REPO:?set GITEE_REPO}"
: "${GITEE_TOKEN:?set GITEE_TOKEN through a secret store or CI secret}"

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
binary="$script_dir/../dist/sync-release-to-gitee"

if [ ! -x "$binary" ]; then
  printf '%s\n' "未找到可执行文件：$binary；请先运行 ./scripts/build-dist.sh" >&2
  exit 1
fi

"$binary" --dry-run
"$binary"
