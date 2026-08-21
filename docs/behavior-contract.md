# Behavior contract

The Go implementation uses Rust release2gitee 1.2.1 as its normal-sync
baseline.

- GitHub and Gitee Releases are read from their first pages only.
- The attachment retention list is calculated from the stable,
  compatibility-version-sorted, de-duplicated merge of Gitee then GitHub
  Releases.
- tag_name is the only identity key. Any Gitee Release with a matching tag is
  skipped without metadata or asset reconciliation.
- GitHub Release processing follows ascending GitHub Release ID, independently
  of the retention sort.
- A non-retained Gitee Release with more than two assets is deleted and
  recreated with its metadata and the selected target branch.
- A retained new GitHub Release downloads all of its assets before creating
  the Gitee Release. Non-retained new Releases are metadata only.
- Release bodies and a downloaded asset named latest.json use the historic
  exact repository-URL string replacement.

Intentional safety changes are documented in [plan.md](plan.md): per-run
staging, atomic download completion, secret redaction, dry-run, and explicit
failure/rollback handling.
