# Final verification measurements

The two handoff candidates ran once each on unchanged PR head `ad1f45c3` with Python 3.12.13 and Node 24.21.0. The Reachable sources and existing pack tests match main at `148e4a0f`. The root Python suite also contains the PR’s earlier contract tests. The commands ran serially with an empty temporary home and offline tool settings. Times include verification setup.

| Candidate | One-minute load | Time | Exit | Result |
| --- | ---: | ---: | ---: | --- |
| Reachable Node suite with archive replay filtered by its exact name | 16.03 | 29 s | 0 | 91 passed and one existing opt-in performance case skipped. Node omits the name-filtered replay case from its total of 92. |
| CI’s fourteen Python directories | 15.34 | 591 s | 1 | 3,044 passed, 131 skipped, 10,746 subtests passed and one packaging assertion failed. |
| Same fourteen directories, clean main at `148e4a0f`, cache provider disabled | 10.03 | 540 s | 0 | 2,997 passed, 131 skipped and 10,746 subtests passed. Git status was clean after the run. |

```sh
verify -C reachable/test node --test --test-skip-pattern '^review 4: archive replay advances in bounded chunks without dropping rows$'
verify python3 -m pytest tests contributing-to-gascity/tests gascity/tests discord/tests github/tests oversight-rig/tests slack-full/tests slack-channel/tests pr-pipeline/tests profiler/tests whatsapp/tests gchat/tests scripts/tests follow-through/tests -q
```

`oversight-rig/tests/test_optional_executive_status_skill.py::test_pack_ships_complete_executive_status_skill` compared every packaged file with its expected inventory. It found four ignored files from the previous isolated pytest run under the skill’s `tests/.pytest_cache` directory. The first run and its failure are preserved here.

The control used a clean local clone of main with the pytest cache provider disabled. The original cache entries stayed in the original checkout. The restored Python command covers root, contributing-to-gascity, Gas City, Discord, GitHub, oversight-rig, Slack full, Slack channel, pr-pipeline, profiler, WhatsApp, Google Chat, scripts and follow-through. [Reachable result](round3-reachable.txt) · [First Python result](round3-python-first.txt) · [Clean control](round3-python-control.txt)

## Regression controls

| Check | Before | After |
| --- | --- | --- |
| Required commands and recursive collection | The earlier inventory omitted the handoff checks and rejected three nested fixtures. | The command comparison uses CI’s Python directory list. Python, Node and Vitest recognize the nested fixtures. |
| Scan failure, injected exit 17 | The plain call and pipefail pipeline failed correctly. Conditional and left-hand AND/OR calls dispatched the child. | All five calls returned 17 without dispatch. |
| WeCom doctor child | The fixture cleared the offline switch and its shim attempted the fetch stand-in. | The real runtime doctor used the installed executable on its short environment. Zero fetch calls. |
| Go cache locations | Both explicit and derived location cases failed with HOME unset. | Both preserved the provisioned module and build directories with downloads disabled. |
| Tracked WhatsApp tests | Node received only the tracked test, with no notice for the extra file. | The extra file was named on stderr and omitted from Node’s arguments. |
| Pytest disk state | The real Make invocation left a self-ignored cache in a committed fixture. | All six Make and direct pytest forms executed a passing test, left Git status clean and created no pytest cache. |

The initial focused run against the previous implementation had nine failures and two passes. After the first set of changes, 57 cases passed. The cache-specific red case then failed on the old Make block. The final focused run passed all 58 contract cases. [Focused results](round3-controls.txt)
