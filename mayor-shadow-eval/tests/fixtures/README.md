# Watchdog patch fixtures

`mayor-model-watchdog-pristine.txt` is an unchanged copy of jadegate's
`~/city/bin/mayor-model-watchdog.bak-20261028`. SHA-256:
`7349d03117467b4fdec269783688dfbacf3e85c0a983c9ddeadb415784a3677a`.

`mayor-model-watchdog-v1.patch` preserves the delivered v1 patch from `ffab1021`.
Applied to the pristine fixture, it reproduces the installed v1 watchdog byte
for byte. SHA-256 of that result:
`0661aec06a8597e060d1487b0fcf78a363465662f170950bfaa7c225ee56a230`.

The tests apply the delivery patches to temporary copies, then run classification
and `--status` against synthetic transcripts. Host-facing paths and actions are
replaced before imported classification; CLI checks use an empty fixture HOME
and an explicit synthetic source. The preserved executable is data, is not
marked executable, and must never be launched against the real host by tests.
