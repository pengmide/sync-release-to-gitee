#!/usr/bin/env sh
set -eu

: "${GITHUB_OWNER:?set GITHUB_OWNER}"
: "${GITHUB_REPO:?set GITHUB_REPO}"
: "${GITEE_OWNER:?set GITEE_OWNER}"
: "${GITEE_REPO:?set GITEE_REPO}"
: "${GITEE_TOKEN:?set GITEE_TOKEN through a secret store or CI secret}"

release2gitee --dry-run
release2gitee
