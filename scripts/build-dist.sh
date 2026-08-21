#!/usr/bin/env sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
project_root=$(CDPATH= cd -- "$script_dir/.." && pwd)
dist_dir="$project_root/dist"
binary_name="sync-release-to-gitee"
build_version=${VERSION:-dev}

mkdir -p "$dist_dir"

printf 'Building %s for %s/%s...\n' "$binary_name" "$(go env GOOS)" "$(go env GOARCH)"
go build -trimpath -ldflags "-s -w -X main.version=$build_version" -o "$dist_dir/$binary_name" "$project_root/cmd/release2gitee"

printf 'Built %s\n' "$dist_dir/$binary_name"
