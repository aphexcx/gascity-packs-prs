# stopwatch

`gc gchat stopwatch [--state-dir DIRECTORY] [--since TIME] [--until TIME] [--json]`

Read monthly adapter journals without a running adapter. Prints message counts,
nearest-rank median and p90 seconds to the first reaction and first words, and
messages older than one hour without words, grouped by release (oldest first).

Use `--state-dir DIRECTORY` for a custom state location. `--since TIME` and
`--until TIME` filter inclusively by the inbound message's channel timestamp;
TIME must include a timezone. `--json` prints the same numbers as JSON.
Reply matching stays within the conversation and thread; one reply answers all
waiting messages there. Malformed lines, including a torn last append, are skipped.
The command never contacts a channel and never deletes journals.
