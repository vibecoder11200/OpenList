# Agent Instructions

## Issues

Before creating an issue, review the available issue templates in the `.github` directory.

When drafting the issue:

- Use the most appropriate template.
- Follow the template structure.
- Fill in all required sections.
- Remove sections that the template explicitly marks as optional or not applicable.
- Do not invent reproduction steps, logs, screenshots, or expected behavior.

## Pull Requests

Before creating a pull request, read `.github/PULL_REQUEST_TEMPLATE.md`.

When drafting the pull request:

- Follow the template structure.
- Use the title format required by the template.
- Fill in or remove each section according to the template guidance.
- Include testing details, or explain why testing was not run.
- Do not invent testing results.
- Do not claim validation, verification, or review steps that were not actually performed.

## Automated Contributions

Fully automated contributions are not considered equivalent to normal community participation.

A contribution may be considered fully automated if it is submitted through an automated agent, or if the submitting account participates in project discussions through an automated agent, without meaningful human review or intervention.

When making this determination, maintainers may consider the overall behavior of the account, including but not limited to disclosed agent usage, interaction patterns, response characteristics, and other available evidence. No single factor is determinative.

Maintainers reserve the right to accept, reject, modify, or reimplement any contribution independently of any action taken against the submitting account. Acceptance of a contribution does not imply acceptance of the submitting account or its contribution method. If an account is determined to be primarily operated through automated processes, we may need to restrict its future participation in contributions until that determination is rescinded.

## Git Commits

When creating commits, follow the repository `git-commit` skill rules:

- Use Conventional Commits title format: `type(scope): subject`.
- Allowed types: `feat`, `fix`, `refactor`, `perf`, `docs`, `style`, `test`, `build`, `ci`, `chore`, `revert`.
- Use a meaningful scope based on the main module, package, or feature.
- Write the subject in imperative mood and describe the actual change.
- Use a concise Markdown list in the commit body, with each item describing one key change.
- Do not invent changes that are not present in the diff.
- Do not describe behavior, refactors, fixes, or tests that are not reflected in the commit.

Include at most one `Co-authored-by` trailer that matches the AI assistant actually used to produce the change.

Examples:

- `Co-authored-by: Codex <267193182+codex@users.noreply.github.com>`
- `Co-authored-by: GitHub Copilot <copilot@github.com>`
- `Co-authored-by: Claude <81847+claude@users.noreply.github.com>`

If you are not one of the listed assistants, do not add a `Co-authored-by` trailer.

Instead, ask the human collaborator to provide the exact `Co-authored-by` trailer to use. Do not invent, infer, or generate one yourself.

## Mod Releases

The `mods` branch (fork `vibecoder11200/OpenList`) is the maintained mod line that runs in
production ("Drive Hub", OpenList container on the AnVPS host, deploy dir `/opt/openlist`).
Frontend patches live in the fork `vibecoder11200/OpenList-Frontend` (branch `mods`) and are
vendored as a build into `public/dist`; `build.sh` uses the committed dist when
`public/dist/index.html` exists instead of fetching the official release asset.

Version format: `v<upstream-base>-mod.<n>` (for example `v4.2.6-mod.1`).

- Derive `<n>` from the tag that is actually deployed: read the current tag first
  (`git -C /root/openlist-src-new describe --abbrev=0 --tags`, e.g. `v4.2.6-mod.2`) and release
  the next one (`v4.2.6-mod.3`). Never assume a fixed number.
- Bump `<n>` by one for every production release of the mod. Do this proactively as part of
  the release, without waiting to be asked.
- Reset `<n>` to `1` when the mod is rebased onto a new upstream base version.
- Mod tags live in the VPS repo `/root/openlist-src-new` (the local repo usually carries only
  upstream tags), so always read the tag there, not locally.

The production build happens on the VPS in `/root/openlist-src-new`, a standalone git repo
refreshed from a deploy tarball. `build.sh` derives the reported version from
`git describe --abbrev=0 --tags`, so the tag must exist in that repo before building, or the
site reports `v0.0.0`.

Standard release process:

1. Commit the changes on the `mods` branch and push to `vibecoder11200/OpenList`
   (and to `vibecoder11200/OpenList-Frontend` when frontend sources changed).
2. Create the deploy archive with LF endings preserved:
   `git -c core.autocrlf=false -c core.eol=lf archive --format=tar.gz -o openlist-src-deploy.tar.gz HEAD`
3. Copy it to the VPS, extract into `/root/openlist-src-new`, then `git add -A` and commit.
4. Bump the version tag in that repo: `git tag v<base>-mod.<n+1>` (delete and re-create the
   tag only when the release itself is being redone).
5. Build: `docker build -t openlist-patched:latest .`, then deploy:
   `cd /opt/openlist && docker compose up -d`.
6. Verify: `docker exec openlist /opt/openlist/openlist version` reports the new tag, then
   smoke-test the changed features. Keep the previous image tag
   (`openlist-patched:backup-<date>`) for rollback.

## MediaFire Farm Layout (as of 2026-10-12)

Prod = Drive Hub site above (admin API `localhost:5244` on the VPS; local tools
tunnel it as `localhost:15244`; S3 gateway is `localhost:5246` / tunneled `15246`,
publicly `https://s3.helloworld.io.vn`). rclone remote name: `drivehub-s3`.

- 442 MediaFire storages mounted (`/mf/mf18` … `/mf/mf529`), DB of accounts and
  cookies is `D:\mediafire\mediafire_ref.db` (column `liveness` written by
  `mf_check.py`). Only `liveness='alive'` accounts join aliases.
- 5 working alias groups, split in ID-order blocks, buckets on the S3 gateway:
  `mf1→/mf/auto1` (100 acc, mf18–mf129), `mf2→/mf/auto2` (100, mf130–mf233),
  `mf3→/mf/auto3` (100, mf234–mf338), `mf4→/mf/auto4` (100, mf339–mf496),
  `mf5→/mf/auto5` (39, mf497–mf529 + revived extras; ~61 free slots — append
  new/revived accounts here, don't re-run a full block split, to keep the
  memorable ID ranges stable).
- `/mf/auto` (bucket `mf`, legacy `mfauto`) is a SPARE: points at one account
  only, not used for uploads. Do not add accounts to it without asking.
- Alias policies: read `first`, write `first`, put `quota`. Consequence: a NEW
  folder must be pre-created (seeded) on many accounts before uploading or all
  files concentrate on the first healthy account. `Videos/Moi` is already seeded
  on every alive account; use `mf_seed_folder.py` for other folders (it accepts
  `--alias /mf/auto2..5` and `all`).
- Tools (in `D:\mediafire\tools`, run with SSH tunnel up):
  `mf_check.py` (liveness + camoufox relogin), `mf_update_cookies.py` (push
  refreshed cookies), `mf_add_to_openlist.py` (mount new accounts, idempotent),
  `mf_split_aliases.py` (re-balance groups; re-runnable, re-reads DB liveness),
  `mf_seed_folder.py` (seed a folder across accounts/groups).
- Never mass-list or mass-scan all 400+ accounts for fun: each alias op fans out
  N MediaFire calls from one IP; scanning the whole farm from a bad IP gets
  logins flagged (a changed client IP once "revived" 60+ accounts that were
  marked dead — verdicts can be IP-poisoned, retry before declaring death).
- 10 accounts remain hard-locked (ids 22–26, 28, 89, 101, 133, 142): browser
  login succeeds but the API keeps rejecting sessions (error 2007) across
  multiple clean-IP rounds — account-level MediaFire restriction on this oldest
  temp-mail/referral generation. 6 siblings revived after a later retry (27,
  76, 87, 114, 134, 139 → now in auto5), so re-check these 10 occasionally
  before writing them off. Fixing them needs manual web-login checks /
  password resets; they stay outside the aliases.
- After running `mf_add_to_openlist.py`, pass `--alias ''` — without it the
  tool re-syncs `/mf/auto` with ALL mounts and undoes the spare.
