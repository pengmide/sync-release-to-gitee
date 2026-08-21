# Recovery

If a run reports that remote state may be partial:

1. Stop other synchronization runs for that Gitee repository.
2. Locate the reported tag and Release ID in Gitee.
3. Confirm which assets are present and whether the Release was created by the
   failed run.
4. Either complete the Release manually or delete only the reported Release ID
   and re-run after reviewing --dry-run.

Do not delete by tag alone, and do not use a bulk-delete script as recovery.
The program only compensates by deleting the exact ID returned by its own
create request.
