# Mods backlog

Accepted but deliberately deferred. Do not pick these up without the owner's
go-ahead.

- [x] Split the farm into 5 aliases (`/mf/auto` + `auto2`–`auto5`, ~87 accounts
  each, round-robin by account id, alive-only) with S3 buckets `mf`–`mf5`.
  Done 2026-10-12 via `D:\mediafire\tools\mf_split_aliases.py` (re-runnable —
  it re-reads DB liveness and re-balances deterministically).
- [ ] `/mf/fast` alias + `mff` S3 bucket — small alias over the ~20 healthiest
  MediaFire accounts. The owner will request this when needed.
- [ ] 16 accounts still locked after 4 relogin rounds on a clean IP — likely
  need password resets on MediaFire; owner decision. (7 remain as of
  2026-10-15: ids 22–26, 28, 101.)
- [ ] S3 LIST does not include pending (spooled, not-yet-finalized) objects:
  during the async finalize window HEAD/GET are served from the spool
  (mod.8), but directory listings only show the object after it lands on
  MediaFire. rclone `cat`/`ls` immediately after a PUT therefore come back
  empty (exit 0). Pre-existing since mod.8, not a mod.10 regression. Fix
  would merge pending spool entries into `server/s3` list results.
