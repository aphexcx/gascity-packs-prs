# Codex DEEP round 2

Model: gpt-6-astra. Reviewed commit:
`c62cacce648e705973ee05ab8d9188bbdb2e22d4`.
Validated verdict: **BLOCK**. Findings: 2 MAJOR.

| Finding and class | Outcome |
| --- | --- |
| Incomplete session identity matching loses named-session events. | FIXED. Learn both ID and name only from the configured mayor caller, and retain verified session-wake identities. Match inbound names and outbound names; prefer GC's canonical publishing subject over a caller-supplied selector. Worker names cannot enter the mayor identity set. |
| GC city-relative import syntax is treated as filesystem-absolute. | FIXED. Resolve `//` beneath the city, matching GC's resolver. The fixture covers both a channel pack and an unrelated local pack during install. |

The named outbound selector also has a failing reproduction against the exact
reviewed commit. All fixes are covered by the complete 66-test passing pack
suite. The reviewer used the supplied test evidence and ran no suites.
