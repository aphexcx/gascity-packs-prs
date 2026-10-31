# Runtime measurements

- `uv run --with pytest --with pyyaml --with jsonschema python3 -c ...`
  imported all three modules successfully before the first test run.
  Python: 3.13.13 (Clang 22.1.3).
- Pytest, PyYAML and jsonschema resolved inside the same uv-managed
  Python 3.13 environment, under
  `/Users/tailor512/.cache/uv/archive-v0/gb3FBlmUVI6IRwGS/lib/python3.13/site-packages/`.
- `pnpm exec node --version`: `v24.21.0`, exit 0. This is the city wrapper's
  Node; the verification suites for this change are Python tests.
- No unsupported-engine warning was emitted. The full suite's existing Slack
  outbound test emits two Python fork deprecation warnings.
