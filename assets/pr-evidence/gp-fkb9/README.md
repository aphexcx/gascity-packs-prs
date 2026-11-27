# gp-fkb9: the Google Chat skill says whom an agent tags

- START (base, aphexcx/gascity-packs-prs main): `96dfa21b6f988c934fe434260f031e2e3277ce10`. Main had not moved when the work started. Every line the ticket cites was read again there, and none had moved.
- Last code commit: `ebae8d3c15c7b4f99b60edbf0cbaef1e7e6faa7d`. This evidence is the commit after it.
- Commits, in order:
  - `79a568399951cef30fed277a40fb0ef3899b34e3`: T2, the Go test of Q2. Green on START's code.
  - `d072202274d536df67af2e083ff7088ca921471e`: T1, the skill test. Red at START.
  - `347d61db5efde36cfbcf55e2bde9d05542194484`: the rule in the skill, and the changelog entry.
  - `30ec61f401c3c383c50731a28e6bb04ef55ec259`: review round 1 folds, tests. Red against 347d61db.
  - `3379c136fda895d66de516e8e902bcd55628b6bb`: review round 1 folds, text.
  - `4a4223cd7dc5f9a5af35803d0cd5a24b7df82492`: review round 2 folds, tests. Red against 3379c136.
  - `ebae8d3c15c7b4f99b60edbf0cbaef1e7e6faa7d`: review round 2 folds, text.

The CTO's pick, 2026-11-27: "tag when an answer is needed". The notification itself is UNPROVEN, because no lane calls Google.

## STEP ONE

### Q1. The form

A mention of one person is `<users/{user}>` in the message's text, where `{user}` is the person's numeric Chat id.

- **The source.** Google's page "Format messages | Google Chat | Google for Developers", https://developers.google.com/workspace/chat/format-messages, section "Mention users in a text message", read on the web on 2026-11-27. It says: "To @mention a specific user, add `<users/{user}>` to the text message where `{user}` is the ID of the user." Its example reads: "Assigning ticket #942 to <users/123456789012345678901>."
- **The email alias.** The same section names it: "Use the user's email address as an alias for the {user} value. For example, if the email address is mahan@example.com, you can specify the user as users/mahan@example.com. To use an email alias, your Google Chat app must authenticate as a user." The skill does not give this form, because the send path can rewrite an email inside a tag (Review, round 1, finding 1).
- **A message sent as a user.** The page's mention section speaks of "Chat apps" sending. Its email-alias sentence, however, requires user authentication, so the section covers a message sent as a user.
  - Google's page "Send a message using the Google Chat API", https://developers.google.com/workspace/chat/create-messages, says: "When you authenticate as a user, the Chat app sends the message on behalf of the user." Its user-authentication sample text lists "Mentions of other Chat users", and links back to the format page.
  - The reference "REST Resource: spaces.messages", https://developers.google.com/workspace/chat/api/reference/rest/v1/spaces.messages, says of `text`: "You can also @mention a Google Chat user, or everyone in the space". Of `formattedText` it says: "User mentions using the format <users/{user}>."
- **This adapter.** It sends as the city's Workspace user, through user OAuth or a key holder's domain-wide delegation for one user (gchat/README.md:80-110, gchat/adapter/internal/token/keyholder.go:20-29). Every message it creates states `MARKUP_SYNTAX_CHAT` (gchat/adapter/internal/chat/chat.go, both creates, since round 2). So the page's other form, `<chat-user data-user="...">`, given "If your app sends messages using the Markdown syntax", does not apply. Round 1 drew this from `Message.Text` alone, which held only in a project saved as Chat: the page says a request that names no syntax takes the project's saved one, and "Newly created Chat apps default to Markdown."
- **A tag of everyone.** The page's form that reaches everyone in a space (`<users/all>`) is not given by the skill, and T1 asserts that it is absent.

### Q2. The send path

A numeric-id tag reaches Google byte for byte in any message that Split does not cut by rune, except inside a Markdown link's title or target. ToChat drops a title with the tag in it: `[plan](https://example.com "ask <users/1234567890>")` becomes `<https://example.com|plan>`. It rewrites a target into its own link form: `[x](<users/1234567890>)` becomes `<users/1234567890|x>`.

- **The commands.** gchat/commands/send.sh:66-83 reads the body (`--body` or `--body-file`) raw, with `jq -Rs`, into the JSON `text`. reply-current.sh, publish.sh and upload.sh are each one line, `exec sh send.sh <mode> "$@"` (line 4). common.sh:35-39 posts it with `--data-binary @-`.
- **The server.** gchat/adapter/internal/server/server.go:846 copies `cmd.Text` into `PublishRequest.Text` unchanged. normalize (server.go:420-445) does not touch the text.
- **The conversion,** server.go:581: `markdown.Split(markdown.ToChat(markdown.StripHandle(p.Text, ...)), 32000)`.
  - StripHandle (markdown.go:704-731) drops only a leading `handle:`. Text that begins with `<` comes back unchanged.
  - ToChat (markdown.go:15-85, inline formatting 201-279) rewrites only escapes, code spans, `[...](...)` links and runs of `*`, `_` and `~`. The characters `<`, `>`, `/` and digits pass as they are.
  - Split (markdown.go:576-646) returns a message of 32,000 bytes or less whole. It divides a longer one at a blank line or a space where one fits, and cuts by rune only where none does.
- **The send.** server.go:675 sends each part's text to `CreateMessageWithAttachmentsAndTime` (chat.go:231-270), which puts it in `Message.Text`. The Go client encodes the request as JSON, where `<` may be written `\u003c`. JSON defines that as the same character, so Google decodes the same text.
- **T2,** `TestMentionFormPassesThrough` (gchat/adapter/internal/markdown/markdown_test.go), is green. It runs StripHandle, ToChat and Split as server.go:581 does, on `<users/1234567890>` placed:
  - alone;
  - in the middle of a sentence;
  - first on the first line, and first on a later line;
  - first after the agent's handle;
  - in a list item, and in a heading;
  - after, before and inside Markdown bold;
  - beside Markdown underscores;
  - before and after a Markdown link;
  - straddling the 32,000-byte part limit, where it moves whole into the second part.
- **Mutants.** Each ran on a copy of the converter in a scratch module, never in the tree:
  - one that writes `<` as `&lt;` fails all 14 subtests;
  - one that lets Split cut at any rune fails "across the part limit";
  - one that drops the intraword-underscore guard passes. It is equivalent here, because Chat's italic marker for `_` is `_` itself.
- **Limits.**
  - An email in place of the id is not byte for byte. ToChat rewrites a pair of underscores between punctuation, so `alex-__ops__-team@example.com` becomes `alex-*ops*-team@example.com`, and in a heading the markers are removed. This was found in review, and it is why the skill gives the numeric id alone.
  - Where Split has no blank line or space to cut at within a part (an unbroken run of 32,000 bytes, or a long code block), it cuts by rune and could divide a tag. The skill already asks for short replies.

### Q3. The identifier

An agent can get the person's numeric id from what it is already given.

- **The turn's frame line.**
  - gchat/adapter/internal/provenance/provenance.go:50-72 writes `[from: Name <address> | class | item ...]`.
  - gchat/adapter/internal/principal/principal.go:87-99 sets the address to the sender's Chat id `users/<id>` (`u.Name`), unless the Workspace directory gives an email.
  - gchat/README.md:247-251 says a desktop sign-in has no directory scope, "so the frame shows the Chat id".
  - A recovered turn starts with `[catch-up]` before the frame (gchat/adapter/internal/inbound/inbound.go:1152). That is why the skill names the `[from: ...]` line, not "the first line".
- **`gc gchat read`.** gchat/adapter/internal/server/server.go:1501 sets each message's `actor.id` to `m.Sender.Name`, the sender's `users/<id>`. The JSON field is `actor.id` (gchat/adapter/internal/wire/wire.go:18-19 and :34). The command is gchat/commands/read.sh, which the skill already names.
- **When the frame shows an email,** the id is the `actor.id` of that person's messages in `gc gchat read` on the same space.
- **Sources the skill does not use.** `gws people searchDirectoryPeople` (named in the skill's "Sharing work") gives an email, not the id. It also reaches people outside the conversation, which is a case not ruled.
- **Limits.**
  - A mention inside an incoming message reaches the agent as the person's name only (inbound.go:1574-1600). That person's id comes from `gc gchat read`, if they wrote in the space.
  - A reaction turn names whoever reacted (inbound.go:1057). In a city whose frame shows emails, a person known only by a reaction has no id source. Giving one needs a change to what a turn carries, which is outside this ticket.

All three hold, so the rule was written. ONE PREMISE STAYS NOT RUN: that Chat then notifies the person. It is UNPROVEN.

## The skill's section, before and after

The section at START is in [section-before.md](section-before.md), and at the last code commit in [section-after.md](section-after.md). The new part, in full, at the end of "## How to reply":

```text
**Whom to tag.** Tag the one person whose answer or action your message
needs. Tag nobody when your message asks nothing of anyone.

Write a tag as `<users/ID>`, where ID is the person's numeric Chat id, not
as a typed `@Name` or an email address. Their turn's `[from: ...]` line may
show `<users/ID>`; when it shows an email, `gc gchat read` on the same space
gives the `users/ID` of each person who wrote there as `actor.id`. For
example, to ask Alex Example (`users/1234567890`) to approve a date:

    gc gchat reply-current --to spaces/<id>/messages/<id> --body '<users/1234567890> can you approve Friday?'
```

(In the skill, the command is a fenced `bash` block.) Nothing else in the skill changed. "DM or @mention: it is addressed to you. Always answer." stays as it is.

## Tests

- **T1,** `gchat/tests/test_tag_skill.py` (new).
  - Red at START as first committed (d0722022): [t1-red-at-start.txt](t1-red-at-start.txt).
  - Its final form, run against START's skill in a scratch tree of the same layout: red, in [t1-final-red-at-start.txt](t1-final-red-at-start.txt).
  - Red against 347d61db ([t1-fold-red-at-347d61db.txt](t1-fold-red-at-347d61db.txt)) and against 3379c136 ([t1-r2-red-at-3379c136.txt](t1-r2-red-at-3379c136.txt)).
  - Green at the last code commit: [t1-green.txt](t1-green.txt).
  - Skill-text mutants, each red: an `@Alex` body; the two sentences split by a word; the example tagging by email; the rule removed; a `<users/²>` tag.
- **T2,** `TestMentionFormPassesThrough` in gchat/adapter/internal/markdown/markdown_test.go: green, 14 subtests.
- **T3,** `gchat/tests/test_join_skill.py`, unedited: green.

## THE COMMANDS

Each command ran at the last code commit `ebae8d3c15c7b4f99b60edbf0cbaef1e7e6faa7d`, in a clean detached worktree, through `verify` as defined in `.claude/skills/verify/SKILL.md` at main. The toolchain was Python 3.12.13 (pytest 9.2.0), Node v24.21.0 and Go 1.27.3.

- `verify python3 -m pytest tests contributing-to-gascity/tests gascity/tests discord/tests github/tests oversight-rig/tests slack-full/tests slack-channel/tests pr-pipeline/tests profiler/tests whatsapp/tests gchat/tests scripts/tests follow-through/tests -q -p no:cacheprovider`: 3125 passed, 132 skipped, 2 warnings, 10932 subtests passed in 605.17s. Exit 0.
- `verify -C gchat/adapter go vet ./...`: no output. Exit 0.
- `verify -C gchat/adapter go test -count=1 -json -timeout=30m ./...`: exit 1 ([go-test-summary.txt](go-test-summary.txt)).
  - Packages: 23 pass, 2 fail, 1 has no test files.
  - Top-level tests: 892 pass, 3 fail, 2 skip. Subtests: 1885 pass, 8 fail.
  - T2 passed with all 14 subtests.
  - The three failures are main's own tests, which trip their own time limits under load. While this run went, another lane on the same machine was running the Google Chat e2e suite.
    - `internal/app TestRevokedTokenReplacementResumes` and `internal/app TestCredentialGenerationRefresh/refresh_recovers`: "health did not become ready in 5s".
    - `internal/inbound TestLookupHoldCarriesCredentialKind` (7 subtests): each lookup has a 100 ms context (inbound_test.go:1937), and under load it expires before the fake's answer, so the hold reads "transient".
  - Run alone, each passed 5 times of 5 at this head ([isolated-head.txt](isolated-head.txt)) and at START ([isolated-start.txt](isolated-start.txt)). They are named here and not fixed.
- `gofmt -l .` in gchat/adapter: printed nothing. Exit 0.
- `git diff --check origin/main..HEAD`: exit 0.

A first run of these commands at 347d61db and a second at 3379c136 were stopped when the review folds moved the head. Only the run at the last code commit is reported.

## Review

Both rounds were STANDARD and ran through the city's Codex launcher as `codex exec --sandbox read-only -`. That form was used because `codex review --base` refuses a prompt, and the ticket asks the reviewer to be told the rule, the NOT RULED paragraph and the word UNPROVEN.

### Self-review first

- **NOTE:** the intraword-underscore mutant of the converter survived T2. It is equivalent here (Chat's `_` marker is `_`), so the outcome is DOES NOT MATTER, with no change.
- Round 1 then found what the self-review missed.

### Round 1, at 347d61db

Both answers were No: no new line tags in a case the two sentences do not cover, and no pushed line names a customer, a person or a real id. Findings: 2 MAJOR (both UNKNOWN), 2 MINOR, 1 NOTE.

1. **UNKNOWN (MAJOR):** an email inside a tag is rewritten by ToChat when it holds a pair of underscores between punctuation. Reproduced in a scratch probe. The class is a tag id that can carry a Markdown marker, and its only case is the email alias. FIXED at 30ec61f4 (tests) and 3379c136 (text): the skill gives the numeric id only and says not to tag by email. `test_every_tag_the_skill_writes_uses_a_numeric_id` is the class test.
2. **UNKNOWN (MAJOR):** no fixture took a tag across Split's part limit. FIXED at 30ec61f4 with the subtest "across the part limit", which the Split-by-rune mutant fails.
3. **MINOR:** "the first line" is wrong for a `[catch-up]` turn, and the frame ends with `| item ...` (inbound.go:1152). FIXED at 3379c136: the part now says "Their turn's `[from: ...]` line".
4. **MINOR:** "Sharing work" says the reminder names the sender "without an address" (main line 205). NOT FIXED. It was there before this change, and the ticket says nothing else in the skill changes. It goes to the mayor as a follow-up.
5. **NOTE:** T1 checked independent substrings. FIXED at 30ec61f4: the two sentences are pinned as one run, the example's `--body` must start with the tag, and every tag must be numeric.

### Round 2, at 3379c136

Both answers were No again. Outcomes 1 and 3 held, outcome 4 held as reported, and outcomes 2 and 5 held in part. Findings: 2 MAJOR (both UNKNOWN), 1 NOTE.

1. **UNKNOWN (MAJOR):** a reaction turn names the reactor, while `gc gchat read` lists only message senders (inbound.go:1057, server.go:1501). FIXED at 4a4223cd (test) and ebae8d3c (text): the skill says the read gives the id "of each person who wrote there". A person known only by a reaction is a limit, under Q3.
2. **UNKNOWN (MAJOR):** Split also cuts by rune where a long code block leaves no cut point, so the test comment's guarantee was too wide. FIXED at 4a4223cd: the comment now says where Split can cut by rune. Split itself is not changed (no change under gchat/adapter but the one test). The shape needs a reply of over 32,000 bytes ending in a code block, followed at once by a tag.
3. **NOTE:** `str.isdigit` accepts a superscript digit. FIXED at 4a4223cd: the check is `[0-9]+`, and the `<users/²>` mutant fails it.

Round 2's folds have not had a Codex round of their own.

## README

README: nothing touched. gchat/README.md says nothing on how an agent addresses a person. Its frame paragraph (lines 242-255) is about what an agent is given.
