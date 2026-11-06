# gp-ktdn: boot registration measurements

## Baselines

- Packs base: `8cb01a83` (`origin/main`, fetched with the required credential helper).
- Fork source: local `origin/integration` at `f22b3ed4a5b090e6960373ffc9e7a414aa085309` (2026-11-03). Read with `git show`, without modifying the fork checkout.
- Lane boundary verified: top-level is this lane, outside the rig root; common git directory is the rig root's `.git`. Session `ci-wisp-89x4hvh` is the sole live writer here. Branch `gp-ktdn` restamped on the bead.
- Read open papercut `pc_bfe4a5c080b5` in full: registration failed 12 seconds after boot at 15:35:47Z on 2026-11-03; manual service restart at 15:42Z recovered it. This establishes that 12 seconds was too early, not the eventual city-ready latency.

## Supervisor ordering and restart policy

`cmd/gc/cmd_supervisor.go:2353` builds the city runtime before wiring controller state, running pool boot hooks, and calling `publishManagedCity` (around line 2478). `cmd/gc/city_runtime.go:489` creates the workspace service manager and calls `Reload` synchronously. `internal/workspacesvc/manager.go:172` constructs proxy services during that reload. `internal/workspacesvc/proxy_process.go:106` starts the process and waits for its socket and a successful health check before returning.

Consequently, registration cannot be a prerequisite for the health listener: that would hold the city construction waiting on a route it has not published yet. `/healthz` must return HTTP 200 with `X-GC-Health: degraded` and `X-GC-Health-Reason: registering: attempt N` during registration. This is the existing advisory contract (`proxy_process.go:343–370`); non-2xx readiness would retain the boot cycle.

Current fork source retries constructor failures through `nextConstructionRetry` (manager.go:175–179; upstream #1774), whereas the reported box remained at `config_error`. Once an instance exists, process exit sets degraded status and a one-second restart backoff; `Tick` restarts it (`proxy_process.go:150–179,255–275`). This does not establish which fork revision was installed on the box.

`internal/config/service.go` defines no service readiness/dependency field. `ServiceProcessConfig` contains only `command` and `health_path`. No pack readiness key can express the desired ordering at this revision.

## Failure classification and timing

The observed failure is HTTP 404 with `city not found or not running: hou-dynasty` in the detail and code `city-not-found`. A legacy compatibility failure is HTTP 400 naming `reply_instructions`; current gc uses HTTP 422 with a structured validation error. Only `errors[].location=body.reply_instructions` with `message=unexpected property` qualifies. Other validation failures remain fatal. Baseline registration flattens status and body into an error string and retries without the template for every error.

Use the specified ten-minute total deadline with waits of 1, 2, 4, 8, 16, then 30 seconds, clipped to the remaining deadline. Include request time in that deadline. Retry connection refusal and the specific city-not-running 404 only; retain the last failure when the deadline expires.

## Mayor's box measurements (received during implementation)

Requested in `ci-wisp-02wrk91`, supplied in `ci-wisp-kc6jt5x`. Source: `/Users/tailor512/city/assets/ops/pr-evidence/gp-ktdn-boot-measure/README.md`, with seven command/output pairs gathered read-only by the mayor at 04:53–04:58Z on 2026-11-04. This lane performed no exec-plane action.

- Deployed gc: 1.5.1 fork `895b2094d`. The second 11/3 boot was at 20:24:40Z; supervisor started at 20:24:43Z and dolt at 20:24:45Z.
- Adapter started and failed registration at 20:25:17Z, 37 seconds after boot. Supervisor log order confirms service construction precedes `Launching city`. Seven timed phases sum to 39.7 seconds plus an untimed skills phase. City readiness is estimated at +45 to +65 seconds; the supervisor log has no timestamps, so this is an estimate, not a direct ready timestamp.
- No automatic adapter restart occurred before the manual restart at 20:32:23Z. This distinguishes deployed behavior from the constructor-retry code on the newer local integration ref.
- Exact boot response: `{"type":"urn:gascity:error:city-not-found","title":"City Not Found","status":404,"detail":"not_found: city not found or not running: hou-dynasty","code":"city-not-found"}`.
- A harmless unknown-field probe against a nonexistent city returned HTTP 422 with code `validation-failed`, `message=unexpected property`, and `location=body.<field>`. Body validation occurs before city lookup. The test models that measured shape for `body.reply_instructions`; the box itself supports that field, so it was not directly rejected in the probe.
- Ten minutes covers the approximately one-minute clean boot with margin for slow bead-store startup. The original cut's 12-second timing was an estimate; the earlier boot timestamp did not survive.

The mayor clarified the handoff policy: worker self-review, draft PR and READY with head SHA; mayor owns the STANDARD Codex gate, cross-model Fable review, founder merge word and rollout. No readiness key was found to request a fork follow-up.
