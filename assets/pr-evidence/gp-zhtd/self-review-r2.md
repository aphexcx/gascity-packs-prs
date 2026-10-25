CLEAN — focused read-only re-review found all four findings resolved.

- Raw and prepared alias/mention-only payloads retain coordinates and one context pointer.
- Ownership scans preserve early and late evidence independently of context-window retention.
- Mention-only forwards now journal rendered bytes, target sessions, and confirmation status.
- Deleted and legacy batch members receive accurate window metadata, including spool handling.

No additional actionable regressions found in the reviewed paths. Reviewed the added tests; did not run tests/builds or modify files.
