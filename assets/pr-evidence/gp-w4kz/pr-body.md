## Round 2 — 2026-11-07 (gp-w4kz)

External calls now have deadlines, late identity discovery reaches the running client, and concurrent media readers share the storage quota. This round also prevents legacy user text from becoming trusted provenance, clears stale organization identity, enforces the join boundary, and lets live-space send recovery proceed after another space is deleted.

All changes extend round-one head `788acecf695b79b4b23c1c5cba996fd86fd6d305`. The eleven requested findings (m1–m9, N1, and the added R1) have separate primary commits in the specified order. Five correction commits retain the m1 test-fixture/evidence repair, m9 JSON compatibility repair, retirement-race fix, authentication-mode alignment, and launcher-fixture helper without rewriting earlier commits. Existing Syndi and round-one commits retain their IDs. No registry or other pack changes; no new dependencies.

| Finding | Result | Commit(s) |
| --- | --- | --- |
| m4 | Directory lookup expires after 30 seconds and falls back to external classification. | `3aa49767` |
| m5 | Unknown managed identity is retried and a later discovery rebuilds the running client. | `01029d07` |
| m6 | Only explicitly marked adapter frames are reused or stripped; legacy user text is preserved. | `aca6bae6` |
| m7 | Identity changes clear the learned domain atomically, including persistence rollback. | `dd78ca30` |
| m8 | Accounting and downloads are serialized across normalizers; usage errors refuse downloads. | `80ba69c1` |
| m9 | A persisted join lower bound clamps message and event overlap. | `ac7a10b4`, `1d273d27` |
| m1 | History 404 retires the space, retains fetched records, and releases live-space recovery; concurrent HTTP requests handle disappearance with a structured 404. | `26c838f7`, `a5ef6bf4`, `a5c2b7d2` |
| m2 | Uploads receive a size-based deadline, with matching shell timeout and recovery budget. | `65e1c833` |
| m3 | Subscription get/list/create/patch/reactivate requests have the two-minute operation deadline. | `56a3b3a5` |
| N1 | Shell launchers preserve supported nonempty endpoint/path overrides; authentication mode stays file-based, including the default when omitted. | `d6d986cc`, `4ad81ce4`, `4cefd2e7` |
| R1 | Unknown per-space history 403s hold that space; 401 and scope errors still trigger credential repair. | `8703590d` |

**Compatibility choices:** the optional `provenance_framed` flag defaults false for old records; no text-based migration grants trust. The optional `joined_at` field is omitted when unknown and does not infer a join from a moving cursor. Shell precedence now follows Go's existing supported override list; credential mode remains file-owned. Upload budgets allow 256 KiB/s plus 25 seconds on the server (825 seconds at the existing 200 MiB maximum); the CLI uses actual file size plus 45 seconds (845 seconds at the maximum).

**Validation on Go 1.27.3:** at source `4cefd2e756a537a998845427b292b428adb08d26`, `go test ./... -count=1 -timeout=30m -json -p=1 -parallel=1` from `gchat/adapter` passed **21 packages, 588 top-level tests, and 1,088 subtests**, with no failures or skips. `go vet ./...`: zero lines; `gofmt -l .`: zero files. From `gchat/`, `uv run --with pytest --with PyYAML --with jsonschema python -m pytest tests -q -p no:cacheprovider`: **23 tests and 22 subtests passed**.

Every earlier failed run is retained under `assets/pr-evidence/gp-w4kz`: r1 exposed the ledger JSON compatibility issue; r2 hit the existing one-second rate-limit and 500ms shutdown assertions, each subsequently passing five isolated repetitions; r3 exposed the launcher fixture's missing helper. The final serialized run passes all of them. No existing test deadline or assertion was loosened.

**Codex self-review:** r1 BLOCK at `8703590d` found the retirement race, fixed in `a5c2b7d2`; r2 CLEAN at `a5c2b7d2` reported an N1 minor, fixed in `4ad81ce4`; r3 BLOCK at `4ad81ce4` found the launcher fixture gap, fixed in `4cefd2e7`; r4 CLEAN at `4cefd2e7` found no actionable issues in that correction. Reports, exact reviewed SHAs, commands, test logs, and history audit are in `assets/pr-evidence/gp-w4kz`. The final commit adds evidence only.

The PR stays **DRAFT** for the mayor's gate and read. Final integration requires a **merge commit**, preserving the imported history, after Afik's merge word.

---

## Round 1 — 2026-11-07 (gp-ecsi)

Head: `788acecf695b79b4b23c1c5cba996fd86fd6d305`. Source fix: `85af4f7c5efa2557f8f8c61f5770946fc13fb6db`, directly on the cleaned head `727f921460ed4673321e7ef7f5532ce236e042e6`. Both new commits are by Citadel Mayor with the required bead, Fable co-author, and Codex review trailers. All 351 Syndi commits retain their IDs; the removed snapshot remains outside this branch's ancestry.

- **M1:** resource denial is recognized positively. API-disabled, access-policy, project-permission, and unknown 403s from global space inventory keep the message uncommitted for credential handling and retry. The regression test verifies recovery delivers the same event exactly once; both recognized space-denial cases still pass.
- **M2:** the revision-order test requires and strips the provenance frame with `provenance.FrameOf` before checking the exact message body.
- **N2:** the oversize/forbidden attachment fixtures now supply `Attachment.Name`. A separate fake-gc test still requires HTTP 422 for a missing provider ID.

**Validation on Go 1.27.3:** `go test ./... -count=1 -timeout=30m -json -p=1 -parallel=1` in `gchat/adapter`: **21 packages, 576 top-level tests, 1,075 subtests passed**, zero failures or skips. `go vet ./...`: zero lines; `gofmt -l .`: zero files. From `gchat`, `uv run --with pytest --with PyYAML --with jsonschema python -m pytest tests -q -p no:cacheprovider`: **20 passed, 17 subtests passed**.

Three earlier full runs each passed 20/21 packages and all e2e cases, but hit existing short-deadline tests: quarantine recovery, token replacement readiness, and identity replacement publishing. The first two passed five isolated repetitions each. The identity timeout also reproduced with the original classifier from `727f9214`. The final run serialized packages and independent tests; no assertion or deadline was changed. All runs, including the failures, are retained in [the evidence directory](https://github.com/aphexcx/gascity-packs-prs/tree/788acecf695b79b4b23c1c5cba996fd86fd6d305/assets/pr-evidence/gp-ecsi).

**Codex self-review r2: CLEAN** at `85af4f7c`, with no actionable findings. The final commit adds evidence only, and the whole `gchat/` tree is unchanged. The PR stays **DRAFT** for the mayor's gate and read; later-round findings remain outside this round.

---

> **Head note (citadel packs lane, bead gp-gr2b, 2026-11-06).** Opened for Afik's order of 18:44Z ("make a pr and run it through the normal gates ... make sure all the history is there") from Robert's archive `gchat-adapter-syndi-2026-11-06.zip` (1,483,814 bytes, sha256 `de9c3faf937ba6cae93f3f320f3faeeddf293ce5434c322d1cf139f030732090`).
>
> - **Fetch recipe.** In a fresh clone of aphexcx/gascity-packs-prs that has main `8cb01a83`: `git bundle verify gchat-adapter-history.bundle` (okay; it requires `8cb01a83`), then `git fetch gchat-adapter-history.bundle gchat-pack:gchat-pack gchat-history:gchat-history` (`gchat-pack` = `ea216e9b`, `gchat-history` = `8a2c8a84`). Pushed as fetched: nothing squashed, rebased or re-authored.
> - **History.** `git log --oneline gchat-history | wc -l` = 350, every one authored `Syndi (Bonanza Mayor) <syndi@apmhelp.com>`. `main..gchat-pack` = 352: the 350, the subtree-add merge `83b8e4b6` (its second parent is `gchat-history`'s tip), and the registration commit `ea216e9b`. `git merge-base main gchat-pack` = `8cb01a83`.
> - **Tree.** `gchat/` at `ea216e9b` equals the archive's `pr-branch/gchat/` byte for byte (`diff -r` clean, 118 files).
> - **Brought current.** `0960807b` merges main `2372b4f2` (7 commits past the bundle's base) as its own commit, with no rebase. The one conflict was the pytest line in `.github/workflows/ci.yml` (main added `whatsapp/tests`, this branch `gchat/tests`), resolved by keeping both. Nothing under `gchat/` changed: its tree is `bd495c55` at both heads.
> - The Bonanza box's deployed 0.4.1 copy is included at `9a27040c`, with the attachment MIME fix re-applied at `41ad3336`.
> - The registry entry is not in this PR: the fork carries no registry entry for its packs (whatsapp and wecom have none here); gchat is registered upstream when it lands there and its release is pinned.
> - Round 3 carries the Bonanza box's deployed 0.4.1 copy as one commit (the complete archive of 11/6, 128 source files, sha256 e386f209...), then re-applies the 10/28 attachment mime_type fix (8a2c8a84) on top, because the box copy predates it. The 350 commits of history are untouched.
> - **Checks at `41ad3336`** (`9a27040c` is the box's copy, `41ad3336` the fix re-applied).
>   - `pytest gchat/tests`: 20 passed, 17 subtests.
>   - The pack's Go suite, `go test ./... -count=1` per `gchat/README.md` (CI does not run it), two full runs: in the second, 20 of 21 packages pass; `test/e2e` fails two cases.
>     - `TestEndToEnd/an_oversize_and_a_forbidden_attachment_deliver_the_message_with_an_unavailable_marker`, both kinds (`e2e_test.go:593`), as at `0960807b`. It passes at `9a27040c` and fails from the re-applied fix: that fix's fake gc refuses an empty `provider_id`, and the test's attachment has no `Name`.
>     - `TestEndToEnd/an_edit_then_a_delete,_and_an_update_arriving_before_its_creation,_apply_in_the_right_order` (`e2e_test.go:524`, "revision order lost"), new with the box's copy: it fails at `9a27040c` too. The order is right. Since 0.4.0 the turn text starts with the provenance frame line, and this test still compares the text exactly (`server_test.go` strips the frame first; this test does not).
>     - The first run also failed `internal/app` `TestShutdownCheckpointsBeforeDrainingStalledRequestBody/bounded` and `internal/server` `TestUndeliveredCommandNeverRetriesAutomatically/gc-refused`. Both pass 5 of 5 run alone and in the second full run (load timing).
>   - `go vet ./...` clean; `gofmt -l` lists nothing.
>   - CI: Check, CodeQL (go, python) and Latest pack releases green.
>
> Below the line: the archive's pack summary, with registry and deployment status corrected against the current tree.

---

## gchat: Google Chat provider pack

A Google Chat provider for Gas City, added as `gchat/` beside `slack-full` and `wecom`, with its development history (350 commits from the scaffold of 2026-10-22 to 2026-10-28, authored on Bonanza, APM Help's city).

**What it is.** One Workspace user identity speaks for the city's agents. Inbound: DMs, messages in joined spaces, edits, deletions, reactions and attachments through the Workspace Events API and Pub/Sub streaming pull. Outbound: text, files and reactions through the Chat API. No public listener or webhook; gc supervises the Go adapter and proxies its private Unix socket. DMs always wake the bound agent; spaces default to every message with a per-space `mentions` option; unbound conversations route to a default agent and `gc extmsg bind` can route individual conversations. Commands: auth, publish, reply-current, upload, react, read, spaces, status, resolve, state; a doctor check; idempotency keys and delivery/recording results; retained history and catch-up after restarts. Cards, dialogs, slash commands and multiple identities are out of scope for 0.1.0.

**In this PR**
- `gchat/` with history (subtree merge).
- No `registry.toml` entry: this fork does not register its packs; upstream registration and release pinning remain a maintainer action.
- `README.md`: a Google Chat pack section beside the Slack packs.
- `Makefile` and `.github/workflows/ci.yml`: `gchat/tests` added to the pytest runs.

**Checks run**
- `python3 validate_registry.py --require-git`: ok.
- `python3 -m pytest gchat/tests -q`: 20 passed, 17 subtests.
- `python3 -m pytest tests -q`: 300 passed; the one failure (`test_make_test_names_the_runner...`) is the absence of `make` on the authoring box, not this change.

**Known follow-ups**
- The deployed Bonanza 0.4.1 delta, including the key-holder token source, directory identity, and space-joined history sweep, is already included at `9a27040c`.
- The live deployment on Bonanza has run this adapter since 2026-10-22 against APM Help's Workspace; the Google-side setup checklist is in `gchat/README.md`.

Supersedes the draft PR 65 (snapshot without history) opened from the citadel lane.

🤖 Generated with [Claude Code](https://claude.com/claude-code)



---

**History clean-up (2026-11-07, citadel mayor, on Afik's approval of 01:50Z).** Afik's pick (c): this pack runs on Bonanza, will run on other cities and will probably be upstreamed, so the history was cleaned to remove the snapshot of the city repo and anything else that should not be public. The branch was rewritten from the old head `41ad33362051fe67580f6833c8c5a477e777bdfc` to `727f921460ed4673321e7ef7f5532ce236e042e6` (a forced update under `--force-with-lease` on the old head, 02:21Z).

- **Removed.** One commit, `c229e14e` ("Plan: three Task 1 corrections from the first lane's papercuts"), a 27-file snapshot of the Bonanza city repo (`.agents/`, `.claude/`, `.codex/`, `.cursor/`, `AGENTS.md`, `CLAUDE.md`, `agents/mayor/prompt.template.md`, `audit/*.md`, `city.toml`, `docs/superpowers/*`, `packs.lock`, `tools/jev-gate/*`) that was a second root of the adapter history. Its audit journal carried a tailnet hostname, a Tailscale 100.x address, Slack channel and DM ids with message timestamps, bead ids and the founders' words. None of it ever reached a later tree, so the pack tree is unchanged. The merge `bf3ca830` that joined that root is kept as a single-parent commit (`f95ec7b7`; its tree and message unchanged).
- **Preserved.** Every other commit's tree, author, author date, committer, committer date and message, byte for byte: 355 commits on `main..gchat-pack` (351 Syndi, 4 Citadel Mayor; before: 356 and 352, the difference the removed snapshot). Six commits below the join keep their shas; 349 above it have new shas only because their ancestry changed. The sorted list of (author, email, both dates, committer, tree, subject, message hash) differs from the old branch by exactly the removed commit's line.
- **Proofs.** `git diff --stat 41ad3336 727f9214` empty (tree `e4f1e8c7` at both heads); roots now exactly two (`c45afe63`, main's own, and `9ea456f3`, the adapter's scaffold); `gchat/` at `4c3c0621` (the 0.4.1 copy) equals the complete archive's extraction (`diff -rq` clean, 128 files) and at the head differs by the mime_type fix's six files, the fix's patch-id `d52d129d` equal before and after; a pattern sweep over every blob and message in the new range (tailnet names, 100.x and 10.90.x addresses, Agent37 ids, API-key and token shapes, Slack ids and timestamps, bead ids) has zero hits; gitleaks 8.30.1 over the new range reports only the synthetic test id_token `e30.<{"sub":"123456"}>.signature` that an httptest fake serves (a false positive, left as authored). The identifiers that remain are test fixtures (`spaces/scripted`, example.com addresses, `U000E0041`).
- **Shas named above now map to:** `8a2c8a84` to `fbd7ab7e`, `83b8e4b6` to `765f9af2`, `ea216e9b` to `8d469f7e`, `0960807b` to `924de256`, `d6eb8432` to `2142df68`, `9a27040c` to `4c3c0621`, `41ad3336` to `727f9214`; `8cb01a83` and `2372b4f2` are main's and unchanged. The method and every measurement are in citadel's record `assets/records/gchat-adapter-20261106/history-rewrite-20261107.md`.
