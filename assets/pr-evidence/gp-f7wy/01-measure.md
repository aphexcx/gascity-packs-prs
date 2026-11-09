# gp-f7wy: measure before changing the native stand-in

Measured at base `2372b4f262aad8e847c416267ba3092d31a083fd` on 2026-11-09.

## Failed runner

Retrieved with `gh run view 42558928310 -R aphexcx/gascity-packs-prs --job 128970811350 --log`.
The complete local capture is `failed-job.log`. Relevant verbatim job payloads:

```text
##[group]Runner Image Provisioner
Hosted Compute Agent
Version: 20261028.624
##[group]Operating System
Ubuntu
26.04.1
LTS
##[group]Runner Image
Image: ubuntu-26.04
Version: 20261101.186.1
E       AssertionError: Security violation: Requested utility `gc-slack-cli` does not match executable name:
E           /usr/lib/cargo/bin/coreutils/echo
slack-full/tests/test_cli_sh.py:116: AssertionError
```

This is PR 81's failing Check job, before this fixture change. The provisioning
version and the runner image version above are separate values.

## Stand-in sweep

Inventoried the existing `slack-full`, `wecom`, `discord`, `cass`, `gascity`, and
`gastown` packs, then searched their test files for symlinks, copies, executable
lookups, and direct coreutils paths. Captures: `test-inventory.txt`,
`stand-in-sweep.txt`, and `native-stand-in-sweep.txt`. The requested `gchat` pack
does not exist in this fork; recorded as papercut `pc_e1e5910bec33`.

| Base location | Stand-in | Different executable name? |
| --- | --- | --- |
| `slack-full/tests/test_cli_sh.py:113` | `gc-slack-cli` symlink to `shutil.which("echo")` | Yes; fails on uutils. |
| `slack-full/tests/test_cli_sh.py:250` | `gc-slack-adapter` symlink to `shutil.which("echo")` | Yes; doctor currently only checks the file. |
| `slack-full/tests/test_cli_sh.py:58` | `head`, `chmod`, `mktemp`, `mv`, `rm` symlinked by the same name | No. |
| `gascity/tests/test_github_app_token.py:34,52` | `openssl`, `curl`, `git` symlinked by the same name | No; also not coreutils. |
| `gascity/tests/test_toolchain_shims.py:209,210,1947` | `npm`, `npx`, `corepack` symlinked to the test's Node shim | Yes, but the target is a shim, not coreutils. |

Other symlinks concern directories, config files, or test-created scripts.
No other renamed coreutils stand-in was found; no sibling fix is needed.

## Native fast path and arguments

`slack-full/commands/_cli.sh:17-18` requires an executable whose successful
`head -c 2` read is not `#!`, then runs `exec "$cli_bin" "$@"`. A shell script
would exercise the legacy rebuild path. A compiled C fixture preserves the
native path and ignores `argv[0]`.

The six verbs are `import-app`, `map-channel`, `map-rig`, `sync-commands`,
`enable-room-launch`, and `post-message`. At line 21,
`ARGS = ("--marker", "two words", "", "*.json")`: `--marker` follows the verb,
so it is not an initial echo option. There are no backslash escapes. Writing
each argument separated by one space and ending with a newline preserves the
unchanged assertion, including the extra separator around the empty argument.

## Local baseline

`local-runtime.txt` records macOS 27.0 arm64, `/bin/echo` as Mach-O, and
`/usr/bin/cc` plus `/usr/bin/gcc`. This native echo tolerates the changed name:
the six existing-binary cases and the doctor case pass before the fix.

From `slack-full/`, using the Makefile's uv dependency recipe:

```sh
env -u GC_TEMPLATE uv run --with pytest --with pyyaml --with jsonschema python3 -m pytest tests -q -k 'test_cli_existing_binary_execs_without_go or test_cli_build_heals_doctor_binaries_row'
```

Result: **7 passed, 902 deselected in 1.59s** (`baseline.log`, exit 0).
The interpreter is uv-managed Python 3.13.13; pytest, PyYAML, and jsonschema
imports were proven from the uv environment before running the tests.
