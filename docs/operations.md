# Operations

## Credentials

Use CI secrets or environment variables. The standard variables are
GITHUB_OWNER, GITHUB_REPO, GITHUB_TOKEN, GITEE_OWNER, GITEE_REPO, and
GITEE_TOKEN. Never commit a token, pass one in a command line, or paste one
into a log.

GITHUB_TOKEN is optional for public API reads. GITEE_TOKEN is required for
Gitee reads and writes.

## Safe rollout

1. Run release2gitee --dry-run against a test mirror and review the planned
   retained tags and destructive release recreations.
2. Run a real sync on the test mirror.
3. Repeat on a non-critical repository before production.
4. Do not run the Rust and Go binaries against the same target repository at
   the same time.

If a run reports partial remote state, follow [recovery.md](recovery.md)
before retrying.

## Observability

The CLI logs tags, action counts, and Release IDs where relevant. It redacts
tokens and does not log Authorization headers, request payloads, or full
response bodies.

## CI

The CI workflow runs formatting, vet, unit tests, native help/version smoke
tests on Linux/macOS/Windows, the Linux race detector, and release-target
cross-builds. Real Gitee write tests are intentionally excluded from automatic
CI and must run in a protected manual workflow with a dedicated test mirror.
