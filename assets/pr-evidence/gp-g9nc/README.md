# gp-g9nc Linux evidence

See the [dated proof](../../../docs/plans/linux-proof-slack-wecom-20261027.md)
for the scope, measured host, before/after results, and live-credential limits.

- `completion.jsonl`: each recorded command, working directory, exit code,
  and elapsed seconds. A null duration means the tool supplied the exit code
  but no separate measurement was captured.
- `summary.json`: machine-readable test totals and the reviewed source revision.
- `*.log.gz`: original command output, compressed without alteration; read
  with `gzip -dc FILE.log.gz`. Failed baseline/harness runs are intentionally
  retained alongside corrected runs.
- `codex-gate-r1.out`: first STANDARD gate, CLEAN with three minor findings;
  all three are resolved in the final change. Later gate output lives beside
  it and is cited in the draft PR.
- `Dockerfile`: Debian ARM64 proof toolchain. Native gc uses the fork's
  `make build` recipe and system ICU. This image is a test fixture, not a
  deployment image.
- `city.toml`, `pack-local.toml`, `pack-remote.toml`: scratch configuration;
  the remote file pins the reviewed pack source commit.
- `*-fixture.env`: explicitly fake credentials. WeCom targets loopback, and
  Slack has Socket Mode and self-registration disabled. No live round trip
  is claimed.
- `gc-linux-city.service`: concrete systemd unit used for static validation.
- `record.py` and `measure.py`: command/result recorder and supervised
  doctor/socket probe. They run inside the Linux container; `measure.py`
  expects its unprivileged test account to be named `node`.

Pack source changed only in the Slack env doctor and WeCom diagnostics/tests.
Go race results apply unchanged to the final adapter/CLI source. The full
WeCom Node suite passed after the fixture-handle repair. The Python suites
and all doctors were then rerun from gc's materialized reviewed commit.

Subsequent verification of the final PR head is stored under `final/` in the
worker's evidence directory and summarized in the PR body; it is kept outside
the committed snapshot so recording a head does not itself change that head.
