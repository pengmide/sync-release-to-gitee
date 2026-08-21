# release2gitee

release2gitee is a Go CLI that mirrors GitHub Releases to Gitee Releases.
It preserves the Rust 1.2.1 synchronization rules: GitHub is the source,
matching Gitee tags are skipped, and only the latest configured tags retain
uploaded attachments.

## Quick start

Use environment variables rather than placing tokens in a shell script:

    export GITHUB_OWNER=example
    export GITHUB_REPO=project
    export GITHUB_TOKEN='<optional GitHub API token>'
    export GITEE_OWNER=example
    export GITEE_REPO=project
    export GITEE_TOKEN='<Gitee token>'

    release2gitee --dry-run
    release2gitee

The lower-case base-variable spellings documented by the legacy Rust README
remain supported as compatibility aliases. Command-line values override
environment variables.

## Safety behavior

- --dry-run performs all read-only planning but sends no create, delete, or
  upload request.
- A Gitee Release that already has the same tag_name is skipped; this tool
  does not update or repair it.
- Attachments are staged under a per-run temporary directory and are removed
  after success or controlled failure.
- Do not put tokens in command lines, checked-in scripts, or logs.

See [the implementation plan](docs/plan.md) for the complete behavior
contract, cleanup boundary, and recovery model.

## Build and verify

    go test ./...
    go vet ./...
    go build ./cmd/release2gitee

The CI workflow builds Linux amd64, Windows amd64, and macOS amd64/arm64
artifacts. A release workflow can inject a tag into the binary through the
main.version linker variable.
