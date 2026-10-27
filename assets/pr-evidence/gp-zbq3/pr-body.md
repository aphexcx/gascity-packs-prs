Shared x.com status links currently lose their paths to the generic base64
redactor. This change preserves HTTPS x.com paths and queries in both Python
and Go, with exact-host and URL-boundary checks. JWTs and recognized token
shapes inside those URLs still redact.

The base64 rule now requires five distinct characters, excluding padding,
and skips digit-only candidates. Five preserves all existing secret fixtures;
the independent hex rule still applies to long numeric strings. All 51 original
fixtures remain unchanged, with 23 additions covering the incident URL shapes,
secret overlap, spoofed/nested URLs, whitespace parity and diversity boundaries.

Measurement: 50 newest founder posts in the documented sweep scope contained
8 URL occurrences; 4 redacted before, 2 after. Both reported live posts now
pass through unchanged. The remaining two gist links also match the out-of-scope
hex rule. Only the observed x.com host was added; the existing Slack permalink
exception and secret-query protection remain. Evidence records IDs and counts,
never founder message bodies, in assets/pr-evidence/gp-zbq3/README.md.

Validation: `uv run --with pytest python -m pytest tests -q` from slack-full:
878 passed, 2 skipped; `go test ./...` from slack-full/adapter: 2 packages passed;
final `go test -race -v ./redact`: 74 shared fixtures plus clipping passed.
Live replay and diff checks pass. The mayor owns the city pin roll and the
separate slack-tail helper update after review and merge.

Bead: gp-zbq3. Papercuts addressed: pc_192a75a6463d, pc_4fe82b567b32.
