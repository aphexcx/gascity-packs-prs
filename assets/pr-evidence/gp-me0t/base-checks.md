# Base controls on macOS arm64

The stock Gastown shell loop exited 1 in 25.89 seconds on Python 3.12.13. The first failing script reported `FAIL: valid per-bead legacy worktree was rejected` at `gastown/tests/test_polecat_artifact_dir.sh:585`. An independent control at commit `148e4a0f9d39a9c83b7d9dcaea151b6d884a8ab0` reproduced the failure in a clean detached clone. That control used Bash 5.3.20, Python 3.15.0 and BSD mktemp.

```text
$ env -u GC_TEMPLATE -u GC_BIN -u NEVER_SILENT -u GC_TEST_BIN bash gastown/tests/test_polecat_artifact_dir.sh
FAIL: valid per-bead legacy worktree was rejected
exit=1
```

The shell trace shows BSD mktemp returning a directory under a symbolic link. The validator compares the fixture's literal namespace to its physical path and rejects the mismatch. Setting a physical TMPDIR alone still failed. GNU mktemp with a physical TMPDIR passed that comparison and exposed a second assumption in the same fixture at line 238, where in-place editing uses GNU sed syntax. With both provisioned GNU tools, all 11 unmodified scripts passed. [Shell transcript](gastown-mac.txt)

Recursive root vet also visits an archived Go probe outside the root package. On the unchanged root sources, `go vet ./...` exited 1 in 0.25 seconds because the import at `assets/pr-evidence/gp-zbq3/round-2/probe.go:6` has no provider in the root module. Root package vet and the root embed tests passed. [Root vet](root-package-vet.txt) · [Root tests](root-test.txt)
