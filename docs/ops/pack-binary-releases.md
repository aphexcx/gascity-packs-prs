# Pack binary releases

From the fork checkout, build both Linux architectures without publishing:

```bash
scripts/release-pack-binaries.sh gchat
scripts/release-pack-binaries.sh slack-full --commit <full-commit-sha>
```

The default is HEAD. Builds use a temporary archive of that commit, including
its `pack.toml`; uncommitted edits are excluded. Go must be installed on the
build host. All binaries use `CGO_ENABLED=0`, `-trimpath`, and stripped symbols.
The script prints release, asset, SHA-256 and size columns and writes:

| Release directory/tag | Assets (each also has `<binary>.sha256`) |
| --- | --- |
| `gchat-linux-amd64-<short-commit>` | `gc-gchat-adapter` |
| `gchat-linux-arm64-<short-commit>` | `gc-gchat-adapter` |
| `slack-full-linux-amd64-<short-commit>` | `gc-slack-adapter`, `gc-slack-cli` |
| `slack-full-linux-arm64-<short-commit>` | `gc-slack-adapter`, `gc-slack-cli` |

Directories live under `dist/`. Short commits use `git rev-parse --short=8`
(longer only if necessary for uniqueness). Sidecars use `sha256sum` format
with the bare asset name; run `sha256sum -c <binary>.sha256` beside the asset.
These are the bare binary names expected by boxboxbox's image installer.
Its `PACKS_PIN`, `GCHAT_COMMIT` and `SLACK_FULL_COMMIT` must identify the source
commit used here, with the release tag's short commit for asset lookup.

After the approved PR is merged, a maintainer may add `--publish --commit
<merge-sha>` to create both architecture releases on
`aphexcx/gascity-packs-prs`. They are prereleases; existing local or remote
tags are refused. `gh` must be authenticated for that repository. Build-only
runs need no GitHub credentials. Publishing is separate from opening a PR.

Install the adapter on PATH to start either pack without Go. Each `adapter/run.sh`
loads its existing secrets configuration, then chooses the corresponding
`GC_GCHAT_ADAPTER_BIN` or `GC_SLACK_ADAPTER_BIN` exact executable path, a binary
on PATH, or the local build. An invalid explicit override fails. Relative paths
are resolved from the launch directory. Shell functions cannot shadow lookup.
Local missing/stale gchat binaries and missing Slack binaries retain their
source-build fallback.

Gchat logs the selected prebuilt path and its `version` output; local and release
builds stamp `main.Version` from `gchat/pack.toml` (0.5.0). Slack's pack version is
0.0.1, but neither its adapter nor CLI has `main.Version` or a version command.
The linker flag has no effect on those binaries; Slack logs the selected path
and that version reporting is unavailable, without starting a second service.
