# Self-review outcomes

Round 1: DEEP review using `gpt-6-astra`, base `205d0aecedf944792f1e3b3fbc420b9e5ad910d8`, reviewed head `b645f4feeaf0565042818dbc06c44e824039c94d`. Verdict: **BLOCK**. The installed gate helper validated the verdict. This summary excludes local paths from the raw record.

| Finding | Class and outcome |
| --- | --- |
| MAJOR: a rejected publish can acquire a reaction receipt | Progress changed before in-flight key ownership was checked. **FIXED** in `b981c9803b990cc505da73ebaae89b2e87366a3b`: text, file and update reserve progress only after `once()` checks the fingerprint. Three parameterized tests first reproduced false delivery, then passed. |
| MAJOR: HTTP 400 provider throttles lose quote context or fail reactions permanently | **INTENDED**, under the task owner's explicit ruling. Reactions retain A3: every Meta 4xx except HTTP 429 is permanent. The base gateway send path at `whatsapp/gateway/src/gateway.ts:1119–1126` has no classifier for HTTP 400 body throttle codes: it frees the key for any 4xx and singles out only window code 131047. B7 therefore retains its explicit exceptions, HTTP 429 and 131047, without introducing a new code list. Follow-up gap: a known target receiving HTTP 400/code 131056 can trigger one unquoted attempt; a reaction gets a permanent failure. |
| MINOR: a definitively unsent attempt freezes the wrong session | **FIXED** in `b981c9803b990cc505da73ebaae89b2e87366a3b`: quote choice is retained independently of attribution. A successful first text chunk fixes its session; file and reaction attempts use the current session, while the activity journal continues preserving ambiguous attempts. Three parameterized retries from another session failed first, then passed. |
| MINOR: existing gateway tests use unknown reaction targets | **FIXED** in `f77f58e383d09553609119294a9b854153b31fa0`: the BSUID send and window-invariance fixtures now target their own retained joined message. Existing routing and clock assertions remain intact. Full gateway run: 333 passed. |

The six review regressions were committed separately in `c83027ff9234422e88425732f2798df32d2f3cdc`. Command, from `whatsapp/adapter/test`:

```sh
pnpm exec node --test --test-name-pattern 'in-flight key ownership|unsent attempt' react.test.js
```

Before the fix: six failed, zero passed. After the fix: six passed, zero failed. Each exact case name is listed in [tests.md](tests.md).

The evidence runner now records the Git SHA and tracked-dirty state both before and after each verification command. Earlier logs lacked that stamp; their results are historical rather than assertions of a later head's state.
