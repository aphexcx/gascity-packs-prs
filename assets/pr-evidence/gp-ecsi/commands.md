# PR 79 Round 1 verification (gp-ecsi)

Base: `727f921460ed4673321e7ef7f5532ce236e042e6`.
The source changes are committed on top of that head without rewriting history.
Go runtime: `go1.27.3 darwin/arm64` (`go-version.log`).
No Node or pnpm command is needed for this Go/Python change.

Commands below run in `gchat/adapter`, except the Python command, which runs in
`gchat`. Exit codes are recorded in `completion-manifest.tsv`.

| Command | Result | Evidence |
| --- | --- | --- |
| `go test ./internal/inbound ./internal/chat -run 'TestSpaceInventoryForbiddenRetriesMessage\|TestResourceDenied$' -count=1 -timeout=2m` before the classifier fix | Expected failure: all four inventory-error cases were acknowledged instead of held; five classifier cases incorrectly returned resource denial | `regression-before.log` |
| `go test ./... -count=1 -timeout=30m` | Running; final result will be recorded before READY | `go-test.log` |
| `go vet ./...` | Exit 0, zero output lines | `go-vet.log` |
| `gofmt -l .` | Exit 0, zero files | `gofmt.log` |
| `uv run --with pytest --with PyYAML --with jsonschema python -m pytest tests -q -p no:cacheprovider` | Exit 0; 20 passed, 17 subtests passed | `pytest.log` |
| `git diff --check -- gchat/adapter` from the lane root | Exit 0 | `diff-check.log` |

The Python environment's pytest, yaml, and jsonschema imports were verified
before running pytest (`python-environment.log`). The before-fix run added the
new regression tests to the base source and deliberately retained the faulty
classifier. The final full suite exercises the same tests with the fix.

The existing recognized membership/resource-denial tests remain in place.
The added inventory regression verifies Nack plus ErrCredentials, no recorded
change/message/transport acknowledgement or inbound delivery, and exactly one
durably delivered turn when the same event is retried after inventory recovery.
The fake-gc test separately preserves HTTP 422 for a missing attachment provider ID.

The full Go run and the separate Codex review must finish before publication.
