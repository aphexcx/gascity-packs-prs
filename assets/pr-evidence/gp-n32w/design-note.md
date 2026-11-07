# Reaction paths for the adoption decision

| Path | When it lands | Emoji selection | Per-message cost | Needs |
| --- | --- | --- | --- | --- |
| A. Existing adapter busy reaction | Dispatch of a targeted inbound; removed when the reply publishes | Configured `BUSY_REACTION`, default `hourglass` | Slack add/remove calls; no model | Slack token, targeted dispatch, pending-mark registry |
| B. Agent-side reaction | After the main agent turn starts and reaches the reaction tool | The agent follows its reaction skill | Main-model inference and a reaction tool call; can share an already-running turn | Agent instruction and `gc slack react` (or the gchat equivalent) |
| C. This adapter acknowledgement | Legacy arrival before routing/coalescing; company first delivery attempt; stays as a receipt | Fixed emoji, local keyword table (`auto`), or small compatible model (`model`) | One Slack add attempt; keywords use no inference, model uses one request capped at 16 completion tokens with a default 6 s timeout | `ACK_REACTION`; model mode also needs endpoint, model name and readable key file |

The gchat skill at PR #79 says “Acknowledge with a reaction, not a message”
and asks the agent to choose an emoji fitting the feeling or topic. That tree
does not establish a separate Bonanza-side reaction model. This PR's model
uses OpenAI-compatible chat completions and falls back to keywords on every
failure. The original installation uses the Teale gateway; a local
OpenAI-compatible server can be selected with `ACK_MODEL_BASE_URL` without a
code change, provided it serves that API and the operator supplies its
model/key configuration.
The timeout bounds the model request; channel lookup and Slack delivery are
additional network work. Neither asynchronous mark has guaranteed arrival
order; use distinct busy and acknowledgement emoji names.

Round 2 uses the company's posting token and first-delivery timing for company
rooms and registered agent DMs. When configured, the persistent receipt replaces
the eyes/check/warning lifecycle; when unset, that lifecycle is unchanged.
The shared four-slot limit queues excess acknowledgements, and paired Slack
events share one selection and post by channel/message timestamp. Fixed ack
and busy emoji collisions are rejected at startup.

I recommend keeping the shipped default off and trying `auto` on Hou first,
with the busy hourglass retained for progress. Enable a local `model` only
after comparing its emoji quality and latency with the keyword baseline.

Afik: adopt (C) on every city with which mode, or keep it Hou-only?
