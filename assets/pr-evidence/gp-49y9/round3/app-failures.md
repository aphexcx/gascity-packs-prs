# App package failures

Every scheduled run is retained in `app-runs.json`; a later passing run does not erase an earlier failure. No wait, timeout or existing fixture was edited, and no extra rerun was used to replace a result.

## TestSubscriptionRefusalRetriesWithoutSignIn/404

Observed in final run 01 at `32228ffd052970c40472cfb080a64399b7dc63d7`. That invocation exited 1 after 532.968 seconds, with 80 top-level tests and 141 subtests passing. Its 403 sibling passed. The failure was:

```text
subscription_test.go:354: health did not become ready in 5s: {State:catching-up Reason:subscription refused: Workspace Events API not enabled (HTTP 404)}
```

The test waits for the refusal notice and two subscription discoveries, then enables the fake endpoint and inserts an ACTIVE subscription before its five-second readiness assertion. Readiness covers subscription recovery, startup/history work and the monitor's readiness update; the recorded health snapshot alone does not establish which scheduling step consumed the deadline.

By reading, this fixture is on the PR's broader startup path. `app_test.go:82-83` sets the coalescing window to `0s`, but `workers.go:133` still launches `RunWindows` for a running generation, and `coalesce.go:362-386` enters the inbound gate once before waiting, even at zero. Subscription initialization precedes that worker launch, and the normal monitor chooses ready at `workers.go:148-165`. The direct subscription-retry logic and the failing assertion were not changed in this round. This is not proof that coalescing caused the timeout, nor proof that this app fixture is outside the PR's execution path.

Final occurrence counts and all other failures, if any, are summarized in the evidence README after the complete 20-run measurement.

## TestFreshTokenIdentityDeliversFirstDM

Observed during final run 03 at the same tested commit. The failing test itself ended after 5.07 seconds:

```text
app_test.go:512: health did not become ready in 5s: {State:catching-up Reason:recovering retained history}
```

This is the initial ready check, before the test publishes its first DM; it is not a failed DM-delivery assertion. The test installs a fresh identity and a fake DM space, starts the app and waits five seconds for ready. By reading, the PR is on this startup path: workers launch the inbound poller and `RunWindows` before catch-up/readiness, and `RunWindows` enters the shared inbound gate even though the fixture sets the window to `0s`. Startup catch-up runs `r.Pass`, and the monitor eventually marks the generation ready. This round's owner-refresh and header cases are not exercised by a published DM at the failing point. The observed snapshot does not establish why startup missed its deadline. No wait, timeout or fixture was changed.

## TestShutdownCheckpointsBeforeDrainingStalledRequestBody/bounded

First observed in final run 04 at the same tested commit. The invocation exited 1 after 533.204 seconds, with 80 top-level tests and 141 subtests passing. It repeated in runs 05 and 12 at the same assertion. The `post-drain-save` sibling passed in all three runs. The failure was:

```text
shutdown_test.go:87: HTTP body prevented bounded drain
```

The test had already reached ready, stalled an HTTP request body after `100 Continue`, canceled the app and observed the initial checkpoint within its 500 ms check. It then missed the return deadline at 1,500 ms after cancellation. This is the final bounded-drain assertion, not the initial-checkpoint assertion at line 63.

By reading, `app.go:161-185` checkpoints before a one-second HTTP shutdown, closes remaining connections and bounds its worker wait with that same shutdown context. Generation cleanup joins its workers at `workers.go:124`, including the unconditionally launched `RunWindows` at line 133. That worker enters the inbound gate even with the fixture's zero-second window. The PR is therefore on the broader generation/shutdown path; the HTTP drain implementation and these test deadlines are unchanged in this round. The retained output does not isolate which scheduling or drain phase missed the deadline, so it does not establish causality. No waits, timeouts or fixtures were changed.

Final run 13 failed the same bounded subtest at an earlier phase, after 4.36 seconds:

```text
shutdown_test.go:63: condition did not become true
```

Here the initial 500 ms checkpoint check did not observe a replacement state file after cancellation. The later bounded-return assertion was not reached; the `post-drain-save` sibling passed. `saveShutdownState` at `app.go:190-202` takes the ledger mutex before saving, and the inbound processor shares that mutex (`workers.go:83`). The fixture had reached ready and launched its normal workers, including the PR's window worker. The output does not show whether scheduling, lock acquisition or persistence delayed the checkpoint. This occurrence is included in the same per-test total but is distinct from the three line-87 drain failures above.

## TestMissingTokenReplacementResumes

Observed during final run 06. The failing test ended after 5.36 seconds:

```text
app_test.go:237: health did not become ready in 5s: {State:initializing Reason:connecting and reconciling subscriptions}
```

The test first observes needs-sign-in and adapter registration, replaces the credential file, then waits five seconds for ready. It publishes no message. `app.go` marks initialization before starting the generation; that generation's normal route constructs the inbound processor, reconciles the subscription, freezes history and then launches `RunWindows` even at the fixture's `0s` setting. Thus the broader successful-startup path includes this PR, while this failure's initializing snapshot does not show that the window worker or the changed owner/withdrawal/header code had executed. The output does not isolate the delayed phase or establish causality. The test and its wait are unchanged.

Final run 10 failed the same test at an earlier assertion, after 1.51 seconds:

```text
app_test.go:225: health did not become needs-sign-in in 1s: {State:initializing Reason:initialization in progress}
```

This assertion precedes adapter-registration polling and credential replacement. The missing-token branch at `app.go:362-365` sets needs-sign-in and returns before starting a generation; the run therefore did not reach this fixture's later window-worker path. The snapshot does not identify what delayed entry into that branch. This is a separate observed phase from run 06's post-replacement readiness timeout, although the final per-test occurrence total groups both under the same test name. Neither occurrence establishes a cause.

## TestRevokedTokenReplacementResumes

Observed during final run 14. The failing test ended after 5.17 seconds:

```text
app_test.go:348: health did not become ready in 5s: {State:catching-up Reason:recovering retained history}
```

The test starts with an expired credential and a fake `invalid_grant`, observes needs-sign-in, clears the fake error and replaces the credential. Its subsequent five-second ready check fails; it publishes no message. The recorded catching-up state is set at `workers.go:130`, immediately before the poller, `RunWindows` and catch-up workers launch; the monitor later requires completed catch-up and the other readiness conditions. Thus the restarted generation includes the broader PR path even with the fixture's `0s` window. The output does not isolate whether catch-up, another readiness condition or scheduling consumed the deadline, and does not prove that coalescing caused it. The test and wait are unchanged.

## TestManagedSourceLateIdentityReachesRunningClient

Observed during final run 15. The failing test ended after 5.07 seconds:

```text
keyholder_test.go:128: health did not become ready in 5s: {State:catching-up Reason:recovering retained history}
```

The initial ready check fails while the fake People endpoint is intentionally unavailable. The test has not yet enabled that endpoint, checked recovered identity, or published its later DM. `managedFixture` uses the ordinary fixture's `0s` window; its running generation still includes `RunWindows` alongside polling and catch-up before the readiness monitor. The PR is on that broader startup path, but this failure does not exercise the later identity-delivery assertion or isolate which readiness phase missed the deadline. A context-canceled proxy message follows during teardown and does not by itself identify the cause. No wait or fixture was changed.
