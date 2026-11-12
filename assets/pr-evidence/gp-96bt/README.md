# Shared WhatsApp env-file resolution

Base: `c546802da5d42fb5d073b7d0bab6716b2e48b6f5`.

Enrollment, startup and the doctor now call `whatsapp/adapter/env-file.sh`.
It preserves startup's existing precedence: the non-empty explicit override,
then the non-empty service secrets directory, then the home-directory default.
`GC_CITY_PATH` continues to locate adapter state; it does not select an env file.
Relative paths are anchored to the adapter directory, where startup already
loaded them. No deployed files or credentials were read, moved or changed.

## Observed path matrix

All values below name temporary fixtures, not deployed paths:

- **override**: the file selected by `GC_WHATSAPP_ADAPTER_ENV`.
- **service**: the env file under `GC_SERVICE_SECRETS_DIR`.
- **city**: the old inferred secrets file under `GC_CITY_PATH`.
- **home**: the default env file under the temporary `HOME`.

The columns headed C, E and S mean `GC_CITY_PATH`,
`GC_WHATSAPP_ADAPTER_ENV` and `GC_SERVICE_SECRETS_DIR`, respectively.
1 means a non-empty value; 0 means unset. Each triple is enroll / run / doctor.

| C | E | S | Base | Head |
| --- | --- | --- | --- | --- |
| 0 | 0 | 0 | home / home / home | home / home / home |
| 0 | 0 | 1 | service / service / service | service / service / service |
| 0 | 1 | 0 | override / override / override | override / override / override |
| 0 | 1 | 1 | override / override / override | override / override / override |
| 1 | 0 | 0 | city / home / city | home / home / home |
| 1 | 0 | 1 | service / service / service | service / service / service |
| 1 | 1 | 0 | override / override / override | override / override / override |
| 1 | 1 | 1 | override / override / override | override / override / override |

The test runs the real enrollment script against an in-process loopback
gateway, identifies the one file it writes, runs the real startup shell with
a fake Node executable to observe the loaded file and credential, and gives
each candidate file the sole complete set of required variables in turn to
observe the doctor's choice. It also verifies mode 0600 and the request body.
All fixture paths contain spaces. The head repeats all eight rows with
relative paths in a temporary copy of the pack, from a different working
directory, and gets the same head table.

## Red and green runs

The Python commands ran from `whatsapp/`, with the supported Node toolchain
first on PATH and `GC_TEMPLATE` unset. Node was **v24.21.0**, measured with
`pnpm exec node --version`. The Python runner was **3.13** with pytest provided
by `uv run --with pytest`.

Initial regression, before changing production code:

```text
env -u GC_TEMPLATE uv run --with pytest python3 -m pytest tests -q -s -k env_file_agrees_across_enroll_run_and_doctor
1 failed, 7 passed, 57 deselected in 4.33s
Failure: C=1, E=0, S=0; ['city'] != ['home'].
```

The initial shared resolver still returned relative strings; extending the
same behavioral matrix exposed caller-dependent working directories:

```text
env -u GC_TEMPLATE uv run --with pytest python3 -m pytest tests -q -s -k env_file_agrees_across_enroll_run_and_doctor
8 failed, 8 passed, 57 deselected in 10.23s
Failure: all eight relative-path rows wrote outside the adapter's selected file.
```

After anchoring relative paths in the resolver:

```text
env -u GC_TEMPLATE uv run --with pytest python3 -m pytest tests -q -s
73 passed in 30.99s
```

The existing doctor test for shell expressions still verifies that an env
file's command substitution is reported without executing it. The doctor
executes only the pack-owned path resolver, never the selected env file.

## Broader adapter suite

From `whatsapp/adapter/test/`, `pnpm exec node --test` completed with
252 tests: 250 passed and 2 failed by timing out while awaiting delivery
acknowledgements (the conversation keeper case and the read-receipt pipeline
case). These tests spawn the adapter JavaScript directly; both their source
and `whatsapp/adapter/src/` are identical to the base revision. They do not
execute any changed production file. The raw local logs are kept outside
the public branch; this document contains only fixture-independent results.

## Compatibility and scope

All eight startup selections remain the same, including the home default
when only the city path is set. Missing selected files still fail closed;
the resolver never chooses a different file based on existence. No credential
migration is needed. The gateway, enrollment protocol, adapter JavaScript and
deployed pack pins are unchanged.
