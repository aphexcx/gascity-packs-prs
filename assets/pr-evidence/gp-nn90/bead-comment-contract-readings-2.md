CONTRACT v1 READINGS, UPDATE 2 (gp-nn90, the gateway lane, after its self-review; posted on gp-nn90 AND gp-9hf1). Readings 4 and 7 of the first comment changed; the others stand. No route or field changed. The gateway head is 11285f36 on branch feat/gp-nn90-whatsapp-gateway, and whatsapp/gateway/README.md carries all of this.

4. SEND, revised: the contract says "the SAME answer again for a repeated idempotency_key without a second Meta call", and this is now read strictly. Once a call may have reached Meta, that key never calls Meta again. The send errors are:
   - 409 in_flight: the same key was claimed under 60 s ago. Retry.
   - 502 meta_error {meta_status, meta_code}: Meta ANSWERED and refused (a 4xx). Nothing was sent, so a retry under the same key sends.
   - 502 outcome_unknown {meta_status, meta_code}: no answer, a 5xx, or a 2xx without a message id. Meta may have sent it, and the key is spent.
   - 409 outcome_unknown: a repeat of such a key, or of a claim abandoned over 60 s ago (the Worker died between the Meta call and the record). No second call is made.

   A box that wants to resend after outcome_unknown uses a NEW key, at the risk of the owner reading the text twice. That choice is the box's (gp-9hf1).
7. WINDOW, revised: a REACTION now moves last_inbound_at. A thumbs-up is how an owner most often acknowledges the window-keeper's nudge; if Meta disagrees, its 131047 refusal reads 422 window_closed, so the cost is one refused send. A system notice, an unsupported type and an unknown type are delivered but do not move it. This is still flagged as a judgment call for the founders.

NEW, behaviours beyond the contract (no route or field changed):
- A status that beats its send's own record is held apart and delivered to the city when the send records its wamid. The box may see a status row with an id later than rows that arrived after it.
- A message Meta stamped more than 8 days ago is dropped and counted.
- A stranger's sixth invite-code text in a day is not bound even by a live code.
