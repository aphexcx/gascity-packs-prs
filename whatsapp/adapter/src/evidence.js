// evidence.js — THE EVIDENCE RULE (design line 114, round 7).
//
// GET /v0/city/{city}/session/{id}/transcript answers one of two shapes, told
// apart by its format field (huma_handlers_sessions_query.go:255-286 and
// 306-325 at gc's fork head f22b3ed4a):
//   format "conversation": the provider's own session log, one turn per
//     entry, the entry's record type as role (entryToTurn,
//     handler_agent_output_turns.go:11-14);
//   format "text": a PEEK of the terminal pane (one role "output" turn),
//     which holds unsubmitted drafts and is NO evidence (nudge_delivery.go:22-31:
//     a payload in the pane with the submit unconfirmed is HOLD, never
//     redeliver).
//
// FOUND is exactly: format "conversation" and a turn with role "user" whose
// text does not begin "[result]", whose FIRST line beginning "New message in
// shared conversation " names whatsapp/<wa_id>, and whose LAST line beginning
// "Message id: " is "Message id: <provider_message_id>" followed by nothing
// or by " (in thread " (formatExtmsgNotifyReminder, handler_extmsg.go:656-685
// at the head: the header at 672 precedes the body, the id line at 680
// follows it, so no body text and no later paste can supply either).
// Everything else is NOT FOUND.

const HEADER = 'New message in shared conversation ';
const ID_LABEL = 'Message id: ';

export function probeShape(answer) {
  if (!answer || typeof answer !== 'object') return 'error';
  if (answer.format === 'text') return 'pane';
  if (answer.format === 'conversation') return 'log';
  return 'error';
}

export function turnHoldsPaste(turn, waId, providerMessageId) {
  if (!turn || turn.role !== 'user') return false;
  const text = typeof turn.text === 'string' ? turn.text : '';
  if (text.startsWith('[result]')) return false;
  const lines = text.split('\n');
  const header = lines.find((line) => line.startsWith(HEADER));
  if (header === undefined) return false;
  if (header !== `${HEADER}whatsapp/${waId}:`) return false;
  let idLine;
  for (const line of lines) if (line.startsWith(ID_LABEL)) idLine = line;
  if (idLine === undefined) return false;
  const rest = idLine.slice(ID_LABEL.length);
  return rest === providerMessageId || rest.startsWith(`${providerMessageId} (in thread `);
}

export function pasteFound(answer, waId, providerMessageId) {
  if (probeShape(answer) !== 'log') return false;
  const turns = Array.isArray(answer.turns) ? answer.turns : [];
  return turns.some((turn) => turnHoldsPaste(turn, String(waId), String(providerMessageId)));
}
