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
  chat, a cleanup that deletes outside its own store), exposes a
  credential, lets a value anyone but the city's own operator controls (a
  chat sender, a webhook, an issue or a pull request the pack reads) run a
  command on a city's machine (an injection), or takes a message from
  someone who is not the owner as the owner's (a sender check removed or
  bypassed).
- MAJOR: a fault in a shape our cities run that loses or duplicates a
  message, a reaction or a work step, or a prompt or formula that tells an
  agent a command or a flag gc does not have, or stops telling it a rule it
  needs.
- MINOR: a real fault the caller sees at once and can retry, a narrow
  race an operator must cause, an injection only the city's own operator
  can reach, or wording that misleads without changing what an agent
  does.
- NOTE: naming, wording, a test's shape, a cheaper way.

A fault this file gives no other grade takes the grade of its effect
under these four. A failing test, check or build with no other effect
shown is MINOR, and a security fault is MAJOR at least.

Two terms this file uses:

- A shape our cities run: a pack of this repository as a city of ours
  imports it, with the chat platform, the model provider, the runtime and
  the settings that city uses.
- No city of ours runs: no city of ours imports that pack, or uses that
  chat platform, provider, runtime or setting, today.

This repository keeps no list of what our cities import or use. When the
diff and the repository do not settle whether a city of ours runs a
shape, the finding says UNKNOWN, names the fact that would settle it, and
is graded as if a city ran the shape.

## What not to report

- What a CI job itself prints when it runs on this pull request: a failing
  test, a compile or vet error, a lint finding, a registry validation
  error or a pack that will not stand up against gc from the Check job
  (.github/workflows/ci.yml), an alert from CodeQL
  (.github/workflows/codeql.yml), a failure from Latest pack releases
  (.github/workflows/pack-release-compatibility.yml). A fault no job
  printed is reported, whatever a job's name suggests it covers: a job
  that did not run, or whose run the reviewer cannot see, printed nothing.
  Among the suites no job runs are the Go tests of the Google Chat adapter
  and of board-watcher, the JavaScript and TypeScript tests of the
  WhatsApp adapter and gateway, the WeCom adapter and reachable, and
  wecom/tests and board-watcher/tests (ci.yml:49-125).
- Always reported: a secret, a key, a token, an injection or any other
  security fault in the diff, whatever CodeQL says; the scanner is a
  floor, not the reviewer's replacement.
- Always reviewed by hand: a change to REVIEW.md itself, whatever CI runs
  on its pull request. A heading, a rule or the Prune line it removes or
  breaks is MAJOR unless a commit message of the pull request names the
  change and says why; then it is a NOTE that quotes that reason.
- A fault that needs a chat platform, a provider, a runtime or a setting
  no city of ours runs is reported as PRECONDITION, with the precondition
  in one sentence, never as MAJOR. UNKNOWN is not PRECONDITION.
- At most five NOTE (or NIT) findings in one review: report the five that
  matter most and count the rest in one line. A MINOR is a real fault,
  never capped and never dropped. A SAFETY review names every finding,
  since it is clean only with none at any severity
  (docs/ops/citadel-codex-gates.md).
- A claim without a file:line is not reported. Every finding names the
  file and line it rests on; one about something the diff removes or
  leaves out names the line it held on main, or the line where it
  belongs; one in a commit message names the commit's sha, and one in the
  pull request's title or body names that field. A finding about
  something private says where it is and never repeats it.

## Triggers (rules a diff can show)

A diff that breaks one of these is reported at MAJOR at least. Each says
what breaks it, with examples from main.

- Nothing private in any file, test fixture, comment or design document. A
  value is private when it is real and ours or a customer's: the name of a
  customer, a customer's company, city or box, or a person at a customer;
  a phone number, an email address, or an account, phone-number, space,
  channel or message id, or a message timestamp, that a real chat or
  account uses; text a person sent; a key's or a token's value; a path on
  a machine of ours (a home directory, a temporary folder); a host that is
  one of our machines or a private service; our plans about a vendor's
  terms, the prices we pay, or a founder's words that are not about how
  the pack works. A value is not private when it is invented and looks
  invented (an example.com address, an id such as users/1, a token such as
  xoxb-1); when it names where every city keeps a thing, not the thing (a
  path under $GC_CITY_PATH, an environment variable's name); when it is a
  public endpoint the pack must call (the gateway at
  whatsapp/pack.toml:6); when it is a noreply address, as a commit's
  author and its Co-Authored-By trailer are; or when it is a founder's
  name with a one-line quote that explains why the pack changed. Private,
  on main: the home-directory path at whatsapp/gateway/README.md:46, and
  the message timestamp that cites a founder's approval at
  docs/ops/citadel-codex-gates.md:4. Not private, on main: the ids and
  addresses at gchat/adapter/internal/principal/principal_test.go:16, the
  token at slack-channel/adapter/config_test.go:11, and the secrets file
  every city keeps under $GC_CITY_PATH at whatsapp/README.md:108. A line
  the diff does not add or change is not a finding of the diff. A line the
  diff adds or changes is judged by what it shows and to whom, even when
  the same value already appears somewhere on the base branch. A private
  value the diff adds is MAJOR, and CRITICAL when it is text a person sent
  or a key's or a token's value. A key or a token in the shape its
  provider issues (a Slack token's full length, a private key's block) is
  graded as real, whatever places it. Any other value that does not look
  invented, and that nothing in the diff, the pull request or the
  repository places, is a MINOR asking for an invented one, until the
  diff, the pull request or a commit says or shows that it is real. This
  repository is public and a pack's files reach every city that imports
  it. Packs PR 90 (opened 2026-11-12) carried a business number's
  phone-number id and account id in a design document.
- The same test holds for commit messages, the pull request's title and
  body, and the squash note a merge writes: a founder's approval is cited
  by its words and its date, never by a channel id, a message timestamp or
  a local path. Private, on main: the squash of PR 92 (e84b685a) carries a
  channel id and message timestamps; as of 2026-11-12, each of the eight
  squashes on main up to PR 92 carried one or the other, and two of the
  last three a home-directory path. Not private: the noreply author and
  the Co-Authored-By trailer of 148e4a0f.
- A move of an instruction an agent needs, out of text the pack delivers
  or loads by itself (an adapter's delivery, a command's output) into a
  prompt fragment or another file that reaches an agent only when a city's
  configuration composes it (append_fragments or inject_fragments_append,
  reachable/README.md:12-27), shows two things in the diff and its
  commits: the pack's README names the new home with the line a city adds
  to compose it, and the pack's CHANGELOG.md (or, for a pack without one,
  a commit message) says a city must add that line before it takes the
  change. A move without both is MAJOR, since an agent in a city that has
  not composed the new home stops being told the rule. A copy that leaves
  the old text in place is not a move. With the line, on main:
  reachable/README.md:12-27. Without it: slack-full/README.md:262 names
  the fragment slack-v0 and no line, and Packs PR 102 (opened 2026-11-12)
  moved the read and react rules there; its gate and its read were clean.
- A path is built from inbound data when some part of it is a value a
  sender or a chat platform chose (an attachment's file name, a message
  id, a conversation or chat name). Code that deletes or overwrites a file
  at such a path keeps it in its own store when each such part is first
  made one path component, with no separator and no leading dot, so it can
  never be "." or ".." (whatsapp/adapter/src/media.js:97-107 and :121-135,
  used at :383-385), or when the joined path is resolved and checked to
  lie inside the store before the call; joining a sender's raw file name
  to the store and deleting there is CRITICAL. A diff that adds or changes
  such a call is reviewed at the SAFETY class, and its tests, run in a
  temporary directory only, show the path cannot leave its store with a
  hostile value ("../", an absolute path). A review of that diff run below
  SAFETY is a class breach: its first finding says so, at MAJOR, with the
  call's file:line, so the review is run again at SAFETY. Packs PR 113
  (opened 2026-11-17) was built under SAFETY because a wrong path there
  deletes a user's files.

## Pruning

Prune by: 2027-02-17

On or before that date each rule above is read against the reviews since
it was written. A rule no review cited and no diff triggered is removed in
a pull request; a shape this file names as one no city of ours runs is
removed the day a city starts running it; a finding that repeats in two
pull requests and is readable from a diff is added here or as a lint rule.
