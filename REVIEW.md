# Review rules

Every reviewer of a pull request here (a person, the codex gate, a second
model's read) reads this file from the base branch, main, never from the
pull request's own head. It says what failure looks like in this
repository, what not to report, and the few rules a diff can show.

## What MAJOR means here

A pack's files ship to every city that imports it, and its prompts are
what that city's agents are told.

- CRITICAL: a change that removes, exposes or sends a user's files or
  messages where they should not go (an adapter that posts to the wrong
  chat, a cleanup that deletes outside its own store), or exposes a
  credential.
- MAJOR: a fault in a shape our cities run that loses or duplicates a
  message, a reaction or a work step, or a prompt or formula that tells an
  agent a command or a flag gc does not have, or stops telling it a rule it
  needs.
- MINOR: a real fault the caller sees at once and can retry, a narrow
  race an operator must cause, or wording that misleads without changing
  what an agent does.
- NOTE: naming, wording, a test's shape, a cheaper way.

## What not to report

- A fault that needs a runtime, a chat platform, a provider or a setting
  no city of ours runs is reported as PRECONDITION, with the precondition
  in one sentence, never as MAJOR.
- Anything CI already enforces: the Check job of
  .github/workflows/ci.yml, over the packs and test directories it names
  (the registry validation, the Python pack tests with the repository's
  own rule tests, the follow-through Node tests, the role prompt
  composition lint, each maintained pack stood up against gc, the pinned
  gastown lint findings, the gastown shell tests, the root module embed
  tests, and the Go tests of the Slack full adapter and CLI, the Slack
  channel and Slack mini adapters and runtime-cloudflare with its go vet),
  CodeQL's Python and Go analysis, and the Latest pack releases job on a
  pull request that touches the paths it watches.
- At most five MINOR or NOTE findings in one review: report the five that
  matter most and count the rest in one line. A SAFETY review names every
  finding, since it is clean only with none at any severity
  (docs/ops/citadel-codex-gates.md).
- A claim without a file:line (for a commit message, its sha) is not
  reported. Every finding names the file and line, or the commit, it rests
  on.

## Triggers (rules a diff can show)

A diff that breaks one of these is reported at MAJOR at least.

- Nothing private in any file, test fixture, comment or design document:
  no customer's name, company, city or box, or person at a customer; no
  real phone number, email address, account, phone-number, space, channel
  or message id, message timestamp or host name; no real message text; no
  key, token or the path of one; no home-directory path; nothing of our
  plans about a vendor's terms, the prices we pay, or a founder's words
  that are not about how the pack works. A founder's name with a one-line
  quote that explains why the pack changed is fine. This repository is
  public and a pack's files reach every city that imports it. Packs PR 90
  (opened 2026-11-12) carried a business number's phone-number id and
  account id in a design document.
- The same for commit messages, the pull request's title and body, and the
  squash note a merge writes: a founder's approval is cited by its words
  and its date, never by a channel id, a message timestamp or a local
  path. On 2026-11-12 the eight squashes on main through PR 92 each
  carried a channel id or a message timestamp, and two of the last three
  a home-directory path.
- A change of WHERE an instruction lives (out of a delivery, into a prompt
  fragment or another file) says what composes the new home in every city
  that imports the pack, and the review checks it. Packs PR 102 (opened
  2026-11-12) moved the read and react rules out of a Slack delivery into
  a fragment that a city's configuration did not compose; its gate and its
  read were clean.
- Code that deletes or overwrites a file whose path is built from inbound
  data (an attachment, a message id, a chat name) is reviewed at the
  SAFETY class, and its tests, run in a temporary directory only, show the
  path cannot leave its own store. Packs PR 113 (opened 2026-11-17) was
  built under SAFETY because a wrong path there deletes a user's files.

## Pruning

Prune by: 2027-02-17

On or before that date each rule above is read against the reviews since
it was written. A rule no review cited and no diff triggered is removed in
a pull request; a shape in the skip list that a city of ours starts
running is removed the day it starts; a finding that repeats in two pull
requests and is readable from a diff is added here or as a lint rule.
