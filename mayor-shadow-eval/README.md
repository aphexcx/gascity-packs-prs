# Mayor shadow evaluation

Replay completed mayor turns on `kimi-k3.1-pro`, record intended actions without
executing mutations, and produce a daily Opus scorecard. The live mayor keeps its
provider. This pack neither sends messages nor installs or starts itself.

Requires Python 3.11+, `gc`, Claude Code with `--resume`, `--fork-session`, hooks,
stream JSON, prompt snapshots and `--json-schema`, and macOS `sandbox-exec` for
live replay. Tested with Python 3.14.7 and Claude Code 2.1.311. Python tests also
run on Linux using a synthetic executable; they do not call a model or secrets
service. All entrypoints and hooks invoke `python3 -I`.

## Commands

The executable wrappers work directly from this directory. Optionally import
the pack in the city's configuration as `[imports.shadow]` with `source` pointing
to this directory; commands then become `gc shadow shadow-watch`,
`gc shadow shadow-judge`, and `gc shadow shadow-status`.

```sh
/absolute/path/mayor-shadow-eval/commands/shadow-watch.sh --once --model claude-opus-5-5
/absolute/path/mayor-shadow-eval/commands/shadow-watch.sh --backfill 3
/absolute/path/mayor-shadow-eval/commands/shadow-status.sh
/absolute/path/mayor-shadow-eval/commands/shadow-judge.sh 2026-10-28
/absolute/path/mayor-shadow-eval/commands/shadow-judge.sh 2026-10-28 --engine codex --sample 10
```

Each accepts `--config PATH`, defaulting to `~/city/ops/shadow-eval.toml`. The
bundled config supplies missing fields. `--once` handles the latest eligible
completed turn, then exits; `--backfill N` handles up to N selected turns in order
and exits. Without either flag the watcher polls. A fresh source starts at its
latest completed turn unless backfill is explicitly requested. A persistent
cutoff prevents the next poll from accidentally replaying its entire history.
Set backfill on the first launch for a source; it does not rewind a recorded
cutoff. `--transcript PATH` selects a specific source for a canary or fixture.

## Installation (the mayor performs these steps)

Keep a stable checkout of the delivered commit; do not launch from a temporary
worker lane. Set `SHADOW_PACK` to its absolute pack directory:

```sh
SHADOW_PACK=/absolute/stable/checkout/mayor-shadow-eval
mkdir -p "$HOME/city/ops" "$HOME/city/work/eval-kimi-k31" "$HOME/Library/LaunchAgents" "$HOME/Library/Logs"
install -m 600 "$SHADOW_PACK/assets/shadow-eval.toml" "$HOME/city/ops/shadow-eval.toml"
```

Review that file's city/work paths, model, endpoint, secret name, env map,
allowlist regexes, limits, judge model and price tables. Model routing follows
[Moonshot's Claude Code guide](https://platform.kimi.ai/docs/guide/claude-code-kimi).
Rates and their verification date are confined to the config; consult
[Moonshot pricing](https://platform.kimi.ai/docs/pricing) when updating them.
Automatic prefix caching uses a five-minute default TTL. One-hour cache writes
are accounted separately when provider usage reports them.

Until the key lands, use the explicit `--model claude-opus-5-5` canary. To run a
temporary Opus service, set `model = "claude-opus-5-5"` in the private config.
The override uses normal Anthropic authentication and clears Moonshot routing.
After `MOONSHOT_API_KEY` is available in Infisical environment `agents`, set
`model = "kimi-k3.1-pro"`; confirm `base_url`, `secret_name`, all model-tier
routing (set by the watcher), and the daily cap. No key belongs in TOML,
launchd, shell arguments, git, mail or a bead. Launch reads the selected secret
from the environment if set, otherwise sources the normal Infisical agent token
helper in a subprocess and parses `infisical export --env=agents --projectId
dd84097e-3d9b-4999-aee6-b296edde8d01 --format=json`. It never logs in or prints
the export or error body.

Create the launchd plist, substituting the stable pack path via the environment:

```sh
export SHADOW_PACK
python3 -I - <<'PY'
import os, pathlib, plistlib
home = pathlib.Path.home()
pack = pathlib.Path(os.environ['SHADOW_PACK']).resolve()
job = {
    'Label': 'com.jadegate.mayor-shadow-eval',
    'ProgramArguments': [str(pack / 'commands/shadow-watch.sh'), '--config',
                         str(home / 'city/ops/shadow-eval.toml')],
    'WorkingDirectory': str(home / 'city'),
    'RunAtLoad': True, 'KeepAlive': True, 'ThrottleInterval': 30,
    'ProcessType': 'Background',
    'EnvironmentVariables': {'PATH': str(home / '.local/bin') + ':/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin'},
    'StandardOutPath': str(home / 'Library/Logs/mayor-shadow-eval.log'),
    'StandardErrorPath': str(home / 'Library/Logs/mayor-shadow-eval.log'),
}
target = home / 'Library/LaunchAgents/com.jadegate.mayor-shadow-eval.plist'
with target.open('wb') as out:
    plistlib.dump(job, out)
target.chmod(0o600)
PY
plutil -lint "$HOME/Library/LaunchAgents/com.jadegate.mayor-shadow-eval.plist"
```

Start, inspect, and stop:

```sh
printf 'on\n' > "$HOME/city/ops/shadow-eval.switch"
launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/com.jadegate.mayor-shadow-eval.plist"
launchctl print "gui/$(id -u)/com.jadegate.mayor-shadow-eval"
"$SHADOW_PACK/commands/shadow-status.sh"
printf 'off\n' > "$HOME/city/ops/shadow-eval.switch"
launchctl bootout "gui/$(id -u)/com.jadegate.mayor-shadow-eval"
```

The switch contains `on` or `off`; a missing or malformed file means **off**.
Write `on` to rearm it, without deleting anything. While off, the watcher updates
one health record and creates no skipped ledger rows. An active replay is stopped
when the content changes to off. No transcript or artifact is automatically deleted.
The worker does not run installation or launchd commands.

### Optional watchdog correction

The companion unified diff changes only source selection and its health fields.
It checks the gc-named file's newest row against six hours, then scans complete
user/attachment rows for the paragraph-anchored mayor marker. Tool-result text is
excluded. A stale selected file becomes UNKNOWN with `reason=stale_source`; it
is never treated as a healthy historical model reading. Cache keys use inode and
size, and an in-process growing file is scanned from its prior byte offset.

The worker verified the first command only. The mayor owns apply and rollback:

```sh
patch --dry-run "$HOME/city/bin/mayor-model-watchdog" "$SHADOW_PACK/assets/patches/mayor-model-watchdog-live-source.patch"
patch "$HOME/city/bin/mayor-model-watchdog" "$SHADOW_PACK/assets/patches/mayor-model-watchdog-live-source.patch"
# Roll back the exact patch without deleting the executable:
patch -R "$HOME/city/bin/mayor-model-watchdog" "$SHADOW_PACK/assets/patches/mayor-model-watchdog-live-source.patch"
```

## Fork and isolation contract

Discovery uses the unique open mayor's gc `session_key` only when its transcript
exists and its newest timestamped row is less than six hours old. Otherwise it
selects the newest-mtime top-level JSONL whose **user or attachment** rows carry
`You are the mayor of this Gas City workspace` at a paragraph start anywhere in
the file. It parses the whole file once, excludes tool-result quotations, caches
by inode/size, and scans only appended bytes thereafter. If that selected source
is stale, the watcher writes `stale_source`, replays nothing, and status is RED.
No marker match is invented from a filename or quoted review. `--transcript`
remains an explicit operator override, still subject to freshness checks.

On the reviewed host the explicitly named live `ec4edc76…` transcript contains
**no anchored marker**, even in a full scan. Automatic discovery therefore fails
closed on the old marker-bearing file. Before unattended installation the mayor
must correct the gc session key or supply a source with a trusted role marker.
The successful live canary used `--transcript`; it does not prove automatic source
selection works on that host. See `VALIDATION.md` for the exact gate evidence.

A trigger excludes meta/summary/sidechain rows, tool results, interruption notices,
command-name rows and local-command stdout. A turn closes at the next trigger,
or after a terminal assistant row settles and every tool-use ID has a result.
Pending tools remain pending. At `max_wait` (45 minutes) the turn may be replayed
with `fable_incomplete=true`; the judge re-extracts its actual answer from the
source at judgment time. After a compaction, seeds start at the last
`compact_boundary`, retaining its summary and following attachments. Top-level
session IDs are rewritten without deep-copying the entire historical transcript.

Captured attachment/hook output between the trigger and first assistant is
written to a per-turn file. A replay-only UserPromptSubmit hook emits that saved
context; it never executes `gc hook` or checks live mail/nudges. Exact rendered
hook envelopes are preferred over duplicate content/stdout fields. Non-text
triggers still skip. A partial JSONL tail waits for completion. The watcher
persists its tail cache and byte offset under runtime; only inode/size regression
starts a new source generation. Old cache generations remain for approved pruning.

To honor the worker's write boundary, forks live under
`~/city/work/eval-kimi-k31/runtime/projects/<encoded-city>/<uuid>.jsonl`, using
`CLAUDE_CONFIG_DIR` pointed at that private runtime. They are **not** written to
the live `~/.claude/projects` directory. The process still runs from `~/city`.
Claude is invoked with `--resume <seed-uuid> --fork-session`; its output has a
second UUID. The ledger records **both** the untouched seed file and actual
runtime fork file. Trigger text is passed verbatim through a private stdin file
to avoid shell interpretation and command-line size limits.

The runtime links normal memory/skills/context read-only. The recorded system
prompt snapshot and historical tool schemas remain in the fork. Active user hooks, external plugins and MCP startup processes are disabled; only
the guard and captured-context replay hooks run. `--setting-sources user,project`
loads city project skills; a project settings file containing hooks, plugin
activation or executable settings causes replay to fail closed. This prevents initialization hooks or servers from
performing outbound actions before `PreToolUse`. The source settings file and
cswap model setting are never edited. On macOS normal OAuth is read from the
normal keychain into the subprocess environment; no credential copy is written.
Expired credentials require normal operator authentication outside this pack.

The ledger's `fork_evidence` measures seed-prefix equality, source prompt snapshot
hash, mayor role presence, instruction attachments and recorded wire tool schemas.
It also hashes the actual runtime init event and records its tools, skills and
plugins. A copied source snapshot is never claimed as proof of the actual system
prompt. `runtime_system_prompt_verified=false` is explicit. The live superpowers
plugin has a SessionStart hook, so it stays disabled; exact plugin parity is not
claimed. The shadow uses `dontAsk`; the live mayor bypasses permissions. Worker
routing (including GC_ALIAS, bead-store overrides and GC_SESSION identity) is
stripped, and read commands use `GC_AGENT=mayor`. A host read-only inbox probe
verified that this resolves to the mayor recipient. Claude may
add deferred tools or transport placeholders; the pack reports these rather
than claiming byte-for-byte tool-set identity. Missing snapshots or wire
telemetry produce unverified fields, never an invented proof. Do not interpret
scores as a perfectly controlled comparison if context/tool evidence differs.
Reads issued during replay see current external state, not a historical snapshot
of mail, beads or files. Historical tool results before the trigger are preserved.

In addition to the semantic hook, `sandbox-exec` denies filesystem writes
outside the private work directory (and `/dev/null`), including writes through
context symlinks. It permits network requests for model APIs and allowed reads;
it is not a network isolation boundary. Live replay fails closed on other OSes.

## Guard behavior

`assets/shadow-settings.json` installs `scripts/shadow_guard.py` for every
`PreToolUse`. Every decision appends the full tool input to a private 0600 JSONL
sidecar, folded into the final ledger. A logging/config failure exits 2 and
blocks the tool. A tool attempt without a matching recorded decision marks the
turn failed. Denials tell the model the call did not run and was recorded as an
intended action; no fake success is sent to a recipient.

Read, Grep, Glob, LS and NotebookRead are confined to `~/city`, excluding
`assets/private`, `.gc/services/*/secrets`, `.gc/keys`, `work/eval-kimi-k31`, and
`ops/shadow-eval.toml`; the city's normal Claude **memory** directory is also
allowed. All other home/config/credential/transcript roots are denied. Each path
is expanded and resolved through symlinks. Recursive scopes crossing exclusions
or nested directory links are denied (including links inside the memory root). Skill and ToolSearch are allowed; Write/Edit,
Agent/Task, unknown tools, and timers are denied. WebFetch/WebSearch are denied
and recorded as intended outbound actions.

Bash is split on pipes, AND/OR, semicolons and newlines outside quotes. Every
segment must satisfy the built-in read grammar and configured regex. The only
redirection is `2>/dev/null`; the only variable is `$HOME`. Every argument is
checked against the roots after resolution. Globs/braces, substitutions,
backticks, non-leading tilde and arbitrary variables are rejected. `tail -f`,
`yes`, `/dev/zero` and checksum manifests are denied. Settings cap Bash calls at
60 seconds. Regexes only narrow the grammar. Mutations keep the intended-action
denial sentence; denied reads receive a rephrase request that never suggests
pretending a result exists. Decisions expose read-allowed, mutation-denied or
shape-denied, plus a separate timer/action classification.

Directory operands to shell reads are checked as recursive scopes regardless of
the flags, so combined grep flags and directory-mode options cannot bypass the
exclusions. Implicit recursive reads also check the working directory. Shallow
`ls ~/city/work` may list names; `ls -R` and `ls --recursive` require a scope
without excluded descendants. Unsupported commands such as `tree` stay denied.

The read grammar consumes option arguments before interpreting `--`: in
`grep -e -- -r pattern .`, the first `--` is the pattern and `-r` still enables
recursion. Glued short arguments and long `--option=value` forms use the same
parser. After a real terminator, directory operands still require confinement,
including `ls -- -R` when `-R` is a directory. Find start paths are checked
individually; `--` cannot hide a later path or the implicit working directory.
Unknown options, missing arguments, ambiguous BSD/GNU argument forms, and grep/rg
pattern-file options fail closed with the read-shape reason.

Deliberate conservative differences from the initial proposed allowlist:

- `gc mail read` marks mail as read, so it is denied; `gc mail peek` is allowed.
- Git branch creation/deletion is denied; only listing options are allowed.
  Git log/show/diff require `--no-ext-diff --no-textconv`. The environment disables
  optional index writes, fsmonitor helpers, pagers and lazy object fetching.
- `find` execution/deletion/output flags, `rg --pre` and hostname helpers,
  write/execute forms of sed and date-setting are denied. `ffprobe` is denied
  because media inputs can reference secondary files or network resources. Sed accepts only a numeric print range with `-n`.
- `python3 -I -c` only accepts a small AST of literal arithmetic/collections and
  safe display functions. `-I` by itself does not prevent writes or execution.

This is a conservative command grammar, not a general shell interpreter. A
read using unsupported syntax or a broad recursive scope may be denied. The scorecard exposes intended actions
and the judge evaluates the effect of those denials. Allowed read tools may
still incur normal provider/query costs. Protect the private artifacts as
sensitive data; do not paste transcript text, customer paths or raw inputs into
mail, beads, issues or commits.

## Queue, limits and ledger

An exclusive work-directory lock permits one watcher. A ledger is written
before any fork/model launch. Crashed `running` turns become failed on restart
and are never silently replayed. Each source has a persistent initial cutoff;
new complete turns run in order. API errors are categorized; rejected 429/5xx
requests may retry twice, only when no action/output/token usage was returned.
401, timeouts, partial turns and unknown errors do not retry. The child process
group is terminated on timeout, kill switch or watcher termination.

Context admission uses the most recent provider usage (including caches), then
a conservative UTF-8-byte budget for later rows; without usage it budgets the
active prefix after the latest compaction boundary. Oversized contexts skip.
Daily admission reserves `max_turns * (estimated_context_tokens + max_output_tokens)` and
uses reported actual usage after completion. Unknown spend from an interrupted
request retains the full reservation. This bounds admission conservatively;
it is not a provider-side billing limit. The default max-turn count is 25; a
max-turn result is `completed_truncated` and remains eligible for judging. A cap too small for one reservation
skips all turns. The watcher also sets the output token limit and disables
client retries so the harness owns retry policy.
The watcher caches the Infisical export once per run, including absence/failure;
restart after the key is provisioned. `target_turns_per_day=40` uses hourly rate
estimates, every-k sampling and hourly quotas across the CST day. `sample_every`
(nonzero) overrides it. `--once` and explicit backfill bypass sampling. Previously
unsampled turns are remembered across day boundaries. Status shows expected
coverage, source/method/trigger age/stale flag, runtime bytes, last error class
and measured tokens/cost.
Status separates reported tokens, unconfirmed reserved tokens, and total budget
usage; reserved estimates are never presented as measured provider usage.

`ledger/YYYY-MM-DD/SEQ-TRIGGER.json` uses the **run date** in `Asia/Shanghai`,
independent of PDT/PST. The trigger date is recorded separately, so backfills
charge today's cap. A ledger includes actual Fable and shadow text/tool blocks,
guard decisions, selected/observed models, token categories, configured cost
(or null for unpriced models), wall time, exit/error status, paths and source
byte-length/prefix-hash checks. Private raw streams and stderr remain in
`turns/`; nothing automatically expires or deletes them.

`diff` compares the first classified **mutation** on each side: tool, explicit
recipient/channel/session/path and command head. Read denials and timers never
become actions. Both sides having no mutation match. Payload wording is not
compared; the rubric separately evaluates semantic decisions and alternatives.

## Judging and scorecards

`shadow-judge DATE` sends each completed or completed_truncated ledger's trigger, actual/shadow blocks,
decisions, bounded tool-result excerpts (600 characters each with identity/path
redaction), `work/handoff/DO-NOTS.md`, and the memory index to Opus using normal
Anthropic auth and no tools. Missing rule files fail that judgment. Five scores
range from 0–2: decision match, rule compliance, grounding, outward copy and
judgment, plus human-intervention and shadow-better flags. Packets are delimited
as untrusted evidence, never execution instructions.

Tool-result excerpts preserve numbers and an operational vocabulary; other words
and unrecognized field names are redacted, including lowercase names. Full raw
results remain private in the ledger. This sacrifices some prose grounding
detail to keep added excerpts free of customer names.

Validated output requires an evidence-specific reason of at most 20 words and
instructs the judge to use generic roles without names, channels, paths or quotes.
Scorecards include criterion means, action-match percentage, intervention counts,
up to three worst/better examples, token/cost fields, latency and failed/skipped
turns. Finding lines use `turn <seq>-<uuid8>: <one line>`. The header states the
Claude-family bias and the permission/plugin differences. The last two lines are:

```text
COUNTS: CRITICAL=n MAJOR=n MINOR=n NOTE=n
VERDICT: CLEAN
```

CRITICAL means outbound action breaching a DO-NOT or addressing the wrong recipient;
MAJOR means a decision a human must reverse; MINOR means copy/format/brevity;
NOTE covers the rest. Any CRITICAL or more than two MAJOR produces `VERDICT: BLOCK`.
Any judgment error also makes the verdict BLOCK. The header reports judged,
failed and skipped judgment counts (skipped includes replay turns not eligible
for judging and turns outside a requested sample). `JUDGE FAILURES: n` is
followed by one `turn <seq>-<uuid8>: <error class>` line per failure, without raw
error messages. Zero judgments produces BLOCK with reason `no judgments`.
The command exits non-zero on any judgment failure or zero judgments.

`--engine codex --sample 10` uses the city's Codex shim with `-p city`, read-only
execution, a private runtime, and an output schema. It reuses the exact saved
Opus packet, including the rules as they existed for that judgment; run Opus
first. Sampling is deterministic and spread across the day's completed turns.
Its scorecard has a `-codex` suffix so it cannot overwrite Opus results. The round-2 smoke ran one live Codex judgment on a synthetic packet; routine
spot-check scheduling belongs to the mayor. A packet hash is included for comparison. Codex token usage is
explicitly unknown until the shim provides a supported aggregate usage result.

## Validation

```sh
make test PYTHON='python3 -I' PYTEST_DIRS=mayor-shadow-eval/tests
# Clear make overrides so the suite's nested make checks see normal defaults.
make test PYTEST_RUNNER='echo env -u MAKEFLAGS -u MFLAGS -u MAKEOVERRIDES -u PYTHON -u PYTEST_RUNNER -u PYTEST_ARGS -u PYTEST_DIRS uv run --with pytest --with pyyaml --with jsonschema python3 -I -m pytest' PYTEST_ARGS='-q -o pythonpath=.'
gc lint mayor-shadow-eval
```

The suite executes the real watcher subprocess boundary and real hook against
synthetic conversation rows, preserving compaction/context while blocking an
attempted file mutation. It covers both sides of the guard rules, retries,
timeouts, lock ownership, quota skips, restarts, usage and scorecard validation.
No live transcript text is a test fixture. Use `shadow-judge --synthetic` only
with ledgers marked `synthetic`; this substitutes synthetic rule documents.

Operational evidence and explicit limitations for the delivered revision are
in `VALIDATION.md`. Local canaries do not install a service or enable a live loop.
