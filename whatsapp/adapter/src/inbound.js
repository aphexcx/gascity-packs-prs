// inbound.js — the gateway inbox to gc extmsg, through the write-ahead
// journal (design line 114; the pack-adapter spec's SCOPE item 3).
//
// The engine is a function of the journal and the clock. tick() is called
// often; every decision (poll, post, read-back, in-doubt resolution, pending
// deadline, watermark read, ack, compaction) is taken from the fold and
// clock(), so a restart recomputes the whole schedule and a SIGKILL anywhere
// loses nothing the gateway does not still hold.
//
// The order per delivery: received (full envelope) -> posted (lookup
// boundary) -> POST /extmsg/inbound -> accepted (verdict, terminal,
// sequence) -> on a delivered verdict the ack by id -> acknowledged. Pending
// is read back on a schedule fixed by the accepted line; a terminal hold
// waits for the operator; no answer leaves the entry posted for the in-doubt
// rule; every dead entry owes its ack until the gateway confirms it.

import { createHash } from 'node:crypto';

import { killPoint } from './clock.js';
import { readWholeInbox } from './gateway.js';
import { recordField } from './gc.js';
import { isRetryableAnswer, isTransient, ladderDelay } from './http.js';
import { ackOwed, entryState } from './journal.js';
import { toMs } from './ledger.js';
import { pasteFound, probeShape } from './evidence.js';
import {
  inboundAtMs, lastFour, pmidOfRid, providerMessageId, renderMessage, ridFor, statusOf,
} from './render.js';

export const READBACK_FAST_MS = 5000;
export const READBACK_FAST_FOR_MS = 10 * 60 * 1000;
export const READBACK_SLOW_MS = 60000;
export const READBACK_SLOW_UNTIL_MS = 60 * 60 * 1000;
export const READBACK_TAIL_MS = 5 * 60 * 1000;
export const PENDING_DEADLINE_MS = 24 * 60 * 60 * 1000;
export const REPOST_BOUND = 3;
export const GRACE_MARGIN_MS = 10000;
export const ACK_BATCH = 100;
// A delivery the gateway took this long before the journal's genesis may have
// been posted by a lost journal (the margin absorbs clock skew between the
// gateway and the box).
export const LOST_JOURNAL_MARGIN_MS = 60000;

// THE CLASSIFICATION OF A NEW RID (the codex gate r1's second finding on PR
// 74). One function decides what a delivery the journal has never seen IS,
// and the decision is written IN that delivery's received line (journal.js
// applies it in the fold), so no crash and no failed append can leave the
// delivery looking fresh to advance():
//   - a dead-letter file line names it: a journal that is gone dead-lettered
//     it; dead again, its ack owed, never posted;
//   - fresh only on positive proof: both instants finite and the gateway
//     took it at or after this journal's genesis less the margin (the
//     margin, kept from the first cut, absorbs a box clock ahead of the
//     gateway's; it also admits a delivery taken up to a minute before the
//     genesis as fresh: a design question for the mayor, since moving it
//     the other way delays every delivery of a new journal's first minute);
//   - otherwise in doubt (a lost journal may have posted it): the in-doubt
//     rule from a lookup at zero, never a plain first post; the grace and
//     the pending deadline run from the gateway's receipt, or from now when
//     the delivery carries none.
export function classifyNew({ receivedAtMs, genesisAt, deadLetter, now }) {
  if (deadLetter) return { recovery: 'dead', recovery_reason: deadLetter.reason };
  if (Number.isFinite(receivedAtMs) && Number.isFinite(genesisAt) && receivedAtMs >= genesisAt - LOST_JOURNAL_MARGIN_MS) return {};
  return { recovery: 'in_doubt', recovery_at: Number.isFinite(receivedAtMs) ? receivedAtMs : now };
}

// Logs and operator surfaces name an entry by a digest, never by its rid: a
// wamid base64-encodes the sender's phone number.
export function ref(rid) {
  return `m-${createHash('sha256').update(String(rid)).digest('hex').slice(0, 10)}`;
}

// The gateway's reading 7 (its README and window.ts CUSTOMER_MESSAGE_TYPES at 392f9c09), order included.
const WINDOW_TYPES = new Set(['text', 'reaction', 'image', 'audio', 'video', 'document', 'sticker', 'location', 'contacts', 'interactive', 'button', 'order']);
const CHAIN_STATES = new Set(['received', 'posted', 'redeliver', 'accepted:failed', 'accepted:partial', 'accepted:no_route']);

export const NO_ROUTE_ON_MARKED_NOTE = 'gc took this message; either it was already delivered (gc suppressed the marked copy as a repeat) '
  + 'or nobody is routed to the conversation; do not re-send it from here';

// --- receipt verdicts (the WeCom adapter's receiptVerdict, inbound.js:138-155
// at 8cb01a83, with the terminal flag read FIRST and partial kept apart) ----

const key = (value) => (typeof value === 'string' ? value.toLowerCase().replace(/[_\s-]/g, '') : '');

function shortfall(item) {
  const delivered = item?.delivered_bytes;
  const expected = item?.expected_bytes;
  return Number.isSafeInteger(delivered) && delivered >= 0 && Number.isSafeInteger(expected) && expected > 0 && delivered < expected;
}

export function verdictOf(delivery) {
  if (!delivery || typeof delivery !== 'object' || Array.isArray(delivery)) return { verdict: 'unsupported', terminal: false };
  const status = key(delivery.status);
  const members = Array.isArray(delivery.members) ? delivery.members : [];
  // The terminal hold is read before any other mapping: wecom's verdict code
  // never reads it, and its followReceipt holds a concluded pending until
  // expiry (inbound.js:935); this design holds it apart for the operator.
  if (status === 'pending' && delivery.terminal === true) return { verdict: 'pending', terminal: true };
  if (status === 'pending') return { verdict: 'pending', terminal: false };
  if (members.some((m) => key(m?.status) === 'partial')) return { verdict: 'partial', terminal: false };
  if (members.some((m) => key(m?.status) === 'failed' || (key(m?.status) === 'delivered' && shortfall(m)))) {
    return { verdict: 'failed', terminal: false };
  }
  if (status === 'partial') return { verdict: 'partial', terminal: false };
  if (status === 'failed') return { verdict: 'failed', terminal: false };
  if (status === 'delivered') {
    if (members.some((m) => key(m?.status) === 'pending')) return { verdict: 'pending', terminal: false };
    return { verdict: shortfall(delivery) ? 'failed' : 'delivered', terminal: false };
  }
  if (status === 'noroute') return { verdict: 'no_route', terminal: false };
  return { verdict: 'unsupported', terminal: false };
}

function membersOf(delivery) {
  const members = Array.isArray(delivery?.members) ? delivery.members : [];
  return members.map((m) => m?.session_id).filter((id) => typeof id === 'string' && id);
}

export function readbackInterval(sinceAcceptedMs) {
  if (sinceAcceptedMs < READBACK_FAST_FOR_MS) return READBACK_FAST_MS;
  if (sinceAcceptedMs < READBACK_SLOW_UNTIL_MS) return READBACK_SLOW_MS;
  return READBACK_TAIL_MS;
}

export function createInbound({
  journal,
  deadLetters,
  gateway,
  gc,
  city,
  clock,
  log = () => {},
  media,
  ledger,
  keeper,
  pollMs = 2000,
  gcPostTimeoutMs = 30000,
  watermarkCheckMs = 60000,
  compactMs = 10 * 60 * 1000,
  routeAgent = 'mayor',
  onAuthFailure = () => {},
}) {
  const graceMs = gcPostTimeoutMs + GRACE_MARGIN_MS;
  const ackOwedIds = new Map(); // gateway delivery id -> rid (null for a status delivery that is only acked)
  const ridBusy = new Set();
  const senderBusy = new Set();
  const nextAt = new Map();
  const attempts = new Map();
  const httpRefused = new Set();
  const inFlightPost = new Set();
  const readCounts = new Map();
  const stats = { polls: 0, lastPollAt: null, lastPollError: '', acks: 0, lastWatermarkReadAt: null, lastCompactAt: null };

  let started = false;
  let authFailed = false;
  let polling = false;
  let pollFailures = 0;
  let nextPollAt = 0;
  let acking = false;
  let ackAttempt = 0;
  let ackNextAt = 0;
  let watermarkWanted = true;
  let watermarkFailures = 0;
  let watermarkNextTryAt = 0;
  let watermarkChain = Promise.resolve();
  let watermarkRunning = false;
  let nextCompactAt = 0;
  let stopped = false;
  let keeperBusy = false;
  // Polls are single-flight; an acknowledged rid is dropped by compaction
  // only once a poll that BEGAN after its ack has completed, since a page the
  // gateway computed before deleting the delivery may still bring it back.
  let pollSeq = 0;
  let lastCompletedPollSeq = 0;
  const ackedAtPoll = new Map();
  const malformedSeen = new Set();

  const entries = () => journal.entries;

  // --- startup ---------------------------------------------------------------

  function startup() {
    // A kill between the dead-letter file line and the dead journal line:
    // the file holds the rid, the fold does not show it dead. Repeat the
    // transition from the file line, with no second file line.
    for (const line of deadLetters.entries()) {
      const e = entries().get(line.rid);
      if (e && !e.dead && !e.acknowledged) {
        journal.append({ t: 'dead', rid: line.rid, reason: line.reason, replayed: true });
        log(`${ref(line.rid)}: dead-letter line found without its journal line; dead transition repeated (${line.reason})`);
      }
    }
    for (const e of entries().values()) {
      if (ackOwed(e) && e.deliveryId !== undefined) ackOwedIds.set(e.deliveryId, e.rid);
      // Every pending entry is read back at once; the schedule is the fold's.
      if (entryState(e) === 'accepted:pending') nextAt.set(e.rid, 0);
      // A kill between a join's received line and its bind loses nothing.
      if (e.kind === 'joined' && e.received?.envelope) bindJoined(e.rid, e.received.envelope);
    }
    watermarkWanted = true;
    compact('start');
    started = true; // only once all of the above held: a throw is retried next tick
    void driveAcks(true);
  }

  // The invite's keeper fields (consent, hours, zone) move onto the phone
  // that joined with its code. Called at the join's first sight, at every
  // later sight, at startup for every joined entry in the journal, and at
  // every poll for each bind still owed: keeper.bindInvite is idempotent.
  // A failed keeper write is never swallowed (the codex gate r1's MINOR on
  // PR 74): the rid is a bind OWED, and driveAcks holds its ack until the
  // bind is saved, so the gateway keeps the delivery and the journal keeps
  // its envelope (compaction drops only an acknowledged rid) for the retry,
  // across a restart too.
  const emptyCodeSeen = new Set();
  const bindOwed = new Set();
  function bindJoined(rid, delivery) {
    if (delivery?.kind !== 'joined' || !keeper?.bindInvite) return;
    const who = `${ref(rid)} (…${lastFour(delivery.wa_id)})`;
    const code = String(delivery.message?.text?.body ?? delivery.message?.code ?? '').trim();
    if (!code) {
      if (!emptyCodeSeen.has(rid)) log(`${who}: joined with no invite code; the keeper stays OFF for that phone until gc whatsapp consent --on`);
      emptyCodeSeen.add(rid);
      return;
    }
    try {
      keeper.bindInvite(code, String(delivery.wa_id));
    } catch (err) {
      if (!bindOwed.has(rid)) log(`${who}: the invite's keeper fields were not bound (${err?.message ?? err}); the join stays unacknowledged until the bind is saved (retried at every poll)`);
      bindOwed.add(rid);
      return;
    }
    if (bindOwed.delete(rid)) {
      log(`${who}: the invite's keeper fields are bound; the join's ack goes now`);
      void driveAcks(true);
    }
  }

  function retryOwedBinds() {
    for (const rid of [...bindOwed]) {
      const envelope = entries().get(rid)?.received?.envelope;
      if (envelope) bindJoined(rid, envelope);
      else bindOwed.delete(rid); // no entry left to bind from
    }
  }

  // --- the gateway poll ------------------------------------------------------

  async function poll() {
    polling = true;
    const seq = ++pollSeq;
    try {
      retryOwedBinds();
      const deliveries = await readWholeInbox(gateway);
      pollFailures = 0;
      lastCompletedPollSeq = seq;
      stats.polls += 1;
      stats.lastPollAt = clock();
      stats.lastPollError = '';
      let gotDelivery = false;
      for (const delivery of deliveries) {
        if (await takeDelivery(delivery)) gotDelivery = true;
      }
      if (gotDelivery) {
        // Rule 1: the all-watermark read runs after every poll that returned a delivery.
        watermarkWanted = true;
        keeper?.noteDelivery?.();
      }
      nextPollAt = clock() + pollMs;
    } catch (err) {
      stats.lastPollError = err.message;
      if (err.auth) {
        authFailed = true;
        log('gateway answered 401: the credential is unknown or revoked; polling stopped (gc whatsapp enroll)');
        onAuthFailure(err);
        return;
      }
      const delay = isTransient(err) ? ladderDelay(pollFailures++) : pollMs;
      log(`gateway inbox poll failed (${err.message}); next poll in ${delay / 1000}s`);
      nextPollAt = clock() + delay;
    } finally {
      polling = false;
    }
    // Rule 2: an owed ack is re-driven at every poll until the gateway confirms it.
    await driveAcks(false);
  }

  async function takeDelivery(delivery) {
    const pmid = providerMessageId(delivery);
    if (!pmid || delivery?.id === undefined || !delivery.wa_id) {
      if (!malformedSeen.has(delivery?.id)) {
        malformedSeen.add(delivery?.id);
        log(`gateway delivery ${JSON.stringify(delivery?.id)} has no message id or wa_id; left unacknowledged`);
      }
      return false;
    }
    if (delivery.kind === 'status' && statusOf(delivery) !== 'failed') {
      // A sent/delivered/read status updates nothing gc needs; ack it.
      ackOwedIds.set(delivery.id, null);
      return false;
    }
    const rid = ridFor(pmid);
    const existing = entries().get(rid);
    if (!existing) {
      // Classified first, then ONE received line carrying the decision.
      const verdict = classifyNew({
        receivedAtMs: toMs(delivery.received_at), // integer epoch seconds on the wire (the gateway's reading 1)
        genesisAt: journal.genesisAt,
        deadLetter: deadLetters.get(rid),
        now: clock(),
      });
      killPoint('before-received', rid);
      journal.append({
        t: 'received',
        rid,
        delivery_id: delivery.id,
        kind: delivery.kind,
        wa_id: String(delivery.wa_id),
        profile_name: delivery.profile_name ?? '',
        envelope: delivery,
        received_at: delivery.received_at ?? null,
        ...verdict,
      });
      killPoint('after-received', rid);
      if (verdict.recovery === 'dead') {
        // Dead-lettered by a journal that is gone (its ack never confirmed):
        // the file line is the record. Dead again, ack owed, never posted.
        log(`${ref(rid)}: already dead-lettered (${verdict.recovery_reason}) by a lost journal; ack owed, never posted`);
        bindJoined(rid, delivery);
        oweAck(rid);
        return true;
      }
      if (verdict.recovery === 'in_doubt') {
        log(`${ref(rid)}: received by the gateway before this journal began; resolved by the in-doubt rule`);
      }
      // The window moves on the owner's message of every known type, the
      // reaction included, never on a system notice or an unsupported type
      // (the gateway's reading 7, update 2); the gateway stays the authority.
      if (delivery.kind === 'joined' || (delivery.kind === 'message' && WINDOW_TYPES.has(String(delivery.message?.type ?? '')))) {
        ledger?.noteInbound?.(String(delivery.wa_id), inboundAtMs(delivery));
      }
      bindJoined(rid, delivery);
      return true;
    }
    bindJoined(rid, delivery); // every sight: a no-op once bound
    // The gateway re-reads everything unacknowledged: the fold decides.
    // A dead entry whose ack failed meets its dead line here, never the
    // no-entry branch (the gate r7's second rule).
    if (ackOwed(existing) && existing.deliveryId !== undefined) ackOwedIds.set(existing.deliveryId, rid);
    if (existing.deliveryId !== undefined && existing.deliveryId !== delivery.id && entryState(existing) === 'acknowledged') {
      ackOwedIds.set(delivery.id, null);
    }
    // A held delivery the gateway returns again is not a new delivery.
    return false;
  }

  // --- acks ------------------------------------------------------------------

  function confirmAcked(batch) {
    for (const [id, rid] of batch) {
      if (rid) {
        const e = entries().get(rid);
        if (e && !e.acknowledged) {
          // The acknowledged line first: if it cannot be written, the ack
          // stays owed and is sent again (the gateway's ack is idempotent).
          journal.append({ t: 'acknowledged', rid });
          ackedAtPoll.set(rid, pollSeq);
          stats.acks += 1;
          nextAt.delete(rid);
          attempts.delete(rid);
        }
      }
      if (ackOwedIds.get(id) === rid) ackOwedIds.delete(id);
    }
  }

  // Acks go in batches of at most ACK_BATCH ids; a batch the gateway refuses
  // with a 4xx (a parameter cap, a bad id) is split to single ids so one bad
  // id never wedges every ack.
  async function driveAcks(force) {
    if (acking || ackOwedIds.size === 0 || authFailed) return;
    if (!force && clock() < ackNextAt) return;
    // A join whose keeper bind is owed is not retired yet (bindJoined).
    const owed = [...ackOwedIds.entries()].filter(([, rid]) => !rid || !bindOwed.has(rid));
    if (!owed.length) return;
    acking = true;
    const batches = [];
    for (let i = 0; i < owed.length; i += ACK_BATCH) batches.push(owed.slice(i, i + ACK_BATCH));
    let failed = 0;
    let lastError = '';
    try {
      killPoint('before-ack', owed.find(([, rid]) => rid)?.[1] ?? '');
      for (const batch of batches) {
        try {
          await gateway.ack(batch.map(([id]) => id));
          confirmAcked(batch);
        } catch (err) {
          if (err.auth) {
            authFailed = true;
            onAuthFailure(err);
            throw err;
          }
          const refused = Number.isInteger(err.status) && err.status >= 400 && err.status < 500 && !isRetryableAnswer(err);
          if (refused && batch.length > 1) {
            for (const one of batch) {
              try {
                await gateway.ack([one[0]]);
                confirmAcked([one]);
              } catch (inner) {
                failed += 1;
                lastError = inner.message;
              }
            }
          } else {
            failed += batch.length;
            lastError = err.message;
          }
        }
      }
    } catch (err) {
      failed = ackOwedIds.size;
      lastError = err.message;
    } finally {
      acking = false;
    }
    if (failed) {
      const delay = ladderDelay(ackAttempt++);
      ackNextAt = clock() + delay;
      log(`gateway ack of ${failed} id(s) failed (${lastError}); the dead or delivered lines stay owed, retry in ${delay / 1000}s`);
    } else {
      ackAttempt = 0;
      ackNextAt = 0;
    }
  }

  function oweAck(rid) {
    const e = entries().get(rid);
    if (e?.deliveryId !== undefined) ackOwedIds.set(e.deliveryId, rid);
    void driveAcks(true);
  }

  // --- the all-watermark read (THE GATE r7's FIRST RULE) ------------------------
  //
  // Reads the store at EVERY retained watermark, never the entry's
  // conversation alone: a gc-only restore can take conversation A's newer
  // records while B's older watermark stays intact. BEHIND anywhere writes
  // that conversation's restore_gap line, and while any gap stands every
  // in-doubt entry of every conversation takes the marked path.

  async function readWatermarksOnce(reason) {
    watermarkRunning = true;
    try {
      for (const [conversationId, wm] of [...journal.watermarks]) {
        if (!Number.isSafeInteger(wm.sequence) || wm.sequence <= 0) continue;
        const page = await gc.transcript({ scopeId: city, conversationId, after: wm.sequence - 1, limit: 1, order: 'asc' });
        const record = Array.isArray(page?.items) ? page.items[0] : undefined;
        const intact = record !== undefined
          && recordField(record, 'Sequence') === wm.sequence
          && recordField(record, 'ProviderMessageID') === wm.provider_message_id;
        if (intact || journal.gaps.has(conversationId)) continue;
        // DETECTED: the store's record at this watermark's sequence is not
        // the watermark's message. The gap line is written NOW, before any
        // further await (the codex gate r1's first finding on PR 74), so an
        // acceptance that raises the watermark meanwhile, a failed head read
        // or a crash cannot un-make the detection: the record this read
        // found missing stays missing whatever the watermark does next, and
        // the gap stands until the operator's clear-gap, which runs on
        // watermarkChain with every read, never inside one.
        const gap = journal.append({
          t: 'restore_gap',
          conversation_id: conversationId,
          watermark_sequence: wm.sequence,
          watermark_accepted_at: wm.accepted_at ?? null,
          head_sequence: null, // unknown until the head read below
          head_created_at: null,
          found_at: clock(),
          found_by: reason,
        });
        // The store's head is the operator's context only (status, the
        // doctor row, clear-gap's print): read after, recorded on the same
        // gap; a failed read leaves it unknown.
        let headNote;
        try {
          const headPage = await gc.transcript({ scopeId: city, conversationId, limit: 1, order: 'desc' });
          const head = Array.isArray(headPage?.items) ? headPage.items[0] : undefined;
          const headSequence = head ? recordField(head, 'Sequence') ?? 0 : 0;
          journal.append({ ...gap, head_sequence: headSequence, head_created_at: head ? recordField(head, 'CreatedAt') ?? null : null });
          headNote = `head ${headSequence}`;
        } catch (err) {
          headNote = `head unknown: ${err.message}`;
        }
        log(`restore gap: conversation ...${lastFour(conversationId)} watermark ${wm.sequence} not in the gc store (${headNote}); in-doubt entries everywhere take the marked path until gc whatsapp inbound clear-gap`);
      }
      stats.lastWatermarkReadAt = clock();
      watermarkFailures = 0;
      log(`watermark read (${reason}): ${journal.watermarks.size} watermark(s), ${journal.gaps.size} gap(s) standing`);
    } finally {
      watermarkRunning = false;
    }
  }

  // Serialized, and a caller always gets a read that STARTED after its call:
  // the precondition of a plain post is a read taken after the probe.
  function readAllWatermarks(reason) {
    const run = watermarkChain.catch(() => {}).then(() => readWatermarksOnce(reason));
    watermarkChain = run.catch(() => {});
    return run;
  }

  async function scheduledWatermarkRead() {
    watermarkWanted = false;
    try {
      await readAllWatermarks('schedule');
    } catch (err) {
      watermarkWanted = true;
      watermarkNextTryAt = clock() + ladderDelay(watermarkFailures++);
      log(`watermark read failed (${err.message}); retrying on the ladder`);
    }
  }

  // --- the post --------------------------------------------------------------

  function boundaryFor(conversationId) {
    let boundary = journal.watermarks.get(conversationId)?.sequence ?? 0;
    for (const e of entries().values()) {
      if (e.waId === conversationId && Number.isSafeInteger(e.accepted?.sequence) && e.accepted.sequence > boundary) {
        boundary = e.accepted.sequence;
      }
    }
    return boundary;
  }

  async function postEntry(rid, { marked, reposts }) {
    const e = entries().get(rid);
    const message = await renderMessage(e.received.envelope, { city, marked, media });
    killPoint('before-posted', rid);
    const posted = journal.append({
      t: 'posted',
      rid,
      at: clock(),
      reposts,
      boundary: boundaryFor(e.waId),
      marked: Boolean(marked),
    });
    killPoint('after-posted', rid);
    inFlightPost.add(rid);
    httpRefused.delete(rid);
    let answer;
    try {
      answer = await gc.postInbound(message);
    } catch (err) {
      if (err.notSent || isRetryableAnswer(err)) {
        // The connection was refused (the request never left), or gc answered
        // 5xx/404/408/429 before any fan-out: a repeat is clean.
        httpRefused.add(rid);
        const attempt = attempts.get(rid) ?? 0;
        attempts.set(rid, attempt + 1);
        nextAt.set(rid, clock() + ladderDelay(attempt));
        log(`${ref(rid)}: gc ${err.notSent ? 'unreachable (not sent)' : `answered ${err.status}`}; retry ${attempt + 1} in ${ladderDelay(attempt) / 1000}s`);
        return;
      }
      if (err.noAnswer || err.malformed) {
        // No HTTP answer: gc may have taken it. Never re-posted on the ladder;
        // the in-doubt rule resolves it after the grace.
        log(`${ref(rid)}: gc post got no answer (${err.message}); in doubt after the grace`);
        nextAt.set(rid, posted.at + graceMs);
        return;
      }
      if (err.status >= 400 && err.status < 500) {
        await deadLetter(rid, 'rejected', { detail: `gc answered ${err.status}: ${String(err.body ?? '').slice(0, 300)}` });
        return;
      }
      nextAt.set(rid, posted.at + graceMs);
      return;
    } finally {
      inFlightPost.delete(rid);
    }
    attempts.delete(rid);
    await onAnswer(rid, answer, 'post');
  }

  async function onAnswer(rid, answer, via) {
    const e = entries().get(rid);
    const delivery = answer?.delivery;
    const { verdict, terminal } = verdictOf(delivery);
    const transcriptEntry = answer?.TranscriptEntry;
    const sequence = recordField(transcriptEntry, 'Sequence');
    if (verdict === 'unsupported') {
      // A 2xx without a usable receipt is no verdict: never an ack on it. The
      // entry stays posted and the in-doubt rule decides after the grace.
      log(`${ref(rid)}: gc answered without a usable receipt; in doubt after the grace`);
      nextAt.set(rid, (e.lastPosted?.at ?? clock()) + graceMs);
      return;
    }
    const firstAcceptance = !e.everAccepted;
    const watermarkBefore = journal.watermarks.get(e.waId)?.sequence;
    journal.append({
      t: 'accepted',
      rid,
      receipt_id: typeof delivery?.receipt_id === 'string' ? delivery.receipt_id : '',
      verdict,
      terminal: terminal || undefined,
      sequence: Number.isSafeInteger(sequence) ? sequence : undefined,
      provider_message_id: recordField(transcriptEntry, 'ProviderMessageID') ?? pmidOfRid(rid),
      members: membersOf(delivery),
      via,
      at: clock(),
    });
    killPoint('after-accepted', rid);
    // The accepted line raised the watermark in the fold; the explicit
    // watermark line is what survives the compaction of that rid.
    const wm = journal.watermarks.get(e.waId);
    if (wm && wm.sequence !== watermarkBefore) {
      journal.append({ t: 'watermark', conversation_id: e.waId, ...wm });
    }
    // On gc's first acceptance the owner sees the read receipt (the blue
    // ticks and the typing indicator). Best effort: it changes no state.
    if (firstAcceptance && e.kind === 'message') {
      gateway.read(pmidOfRid(rid)).catch((err) => log(`${ref(rid)}: read receipt failed (${err.message})`));
    }
    await afterVerdict(rid, verdict, terminal, delivery);
  }

  async function afterVerdict(rid, verdict, terminal, delivery) {
    const e = entries().get(rid);
    if (terminal) {
      journal.append({ t: 'held_terminal', rid, receipt_id: delivery?.receipt_id ?? e.accepted?.receipt_id ?? '', members: membersOf(delivery) });
      nextAt.delete(rid);
      log(`${ref(rid)}: TERMINAL HOLD (gc: stop polling, do not redeliver); waiting for gc whatsapp inbound resolve`);
      return;
    }
    switch (verdict) {
      case 'delivered':
        oweAck(rid);
        return;
      case 'pending':
        nextAt.set(rid, clock() + READBACK_FAST_MS);
        return;
      case 'failed':
      case 'partial': {
        // At the bound the dead letter is due at once; below it, the next rung.
        const reposts = e.lastPosted?.reposts ?? 0;
        nextAt.set(rid, reposts >= REPOST_BOUND ? 0 : clock() + ladderDelay(reposts));
        return;
      }
      case 'no_route':
        nextAt.set(rid, 0);
        return;
      default:
        return;
    }
  }

  // --- the chain step --------------------------------------------------------

  async function advance(rid) {
    const e = entries().get(rid);
    const state = entryState(e);
    const now = clock();
    switch (state) {
      case 'received':
        return postEntry(rid, { marked: false, reposts: 0 });
      case 'posted': {
        if (inFlightPost.has(rid)) return undefined;
        if (httpRefused.has(rid)) return postEntry(rid, { marked: e.lastPosted?.marked, reposts: e.lastPosted?.reposts ?? 0 });
        const due = (e.lastPosted?.at ?? 0) + graceMs;
        if (now < due) { nextAt.set(rid, due); return undefined; }
        return resolveInDoubt(rid);
      }
      case 'redeliver':
        return postEntry(rid, { marked: true, reposts: e.lastPosted?.reposts ?? 0 });
      case 'accepted:failed':
      case 'accepted:partial': {
        const reposts = e.lastPosted?.reposts ?? 0;
        if (reposts >= REPOST_BOUND) return deadLetter(rid, 'repost_bound', { detail: `gc answered ${e.accepted.verdict} after ${reposts} reposts` });
        const due = (e.accepted?.at ?? 0) + ladderDelay(reposts);
        if (now < due) { nextAt.set(rid, due); return undefined; }
        return postEntry(rid, { marked: e.redelivered, reposts: reposts + 1 });
      }
      case 'accepted:no_route':
        if (e.redelivered) return noRouteOnMarked(rid);
        return deadLetter(rid, 'no_route', { detail: 'gc answered no_route: nobody is routed to this conversation (the default route to the mayor is missing?)' });
      default:
        return undefined;
    }
  }

  // --- THE IN-DOUBT RULE -------------------------------------------------------

  async function resolveInDoubt(rid) {
    const e = entries().get(rid);
    if (e.redelivered && e.markedPosted) {
      // In doubt again after its one marked copy: never posted a third time.
      return deadLetter(rid, 'in_doubt', { detail: 'in doubt again after the marked copy' });
    }
    // Step 1, before the lookup, the probe and any post: EVERY watermark.
    await readAllWatermarks('in-doubt');
    const present = await lookup(e);
    // An entry first posted before the operator's last clear-gap went into a
    // store that has since been rebased: its absence proves nothing either.
    const postedBeforeClear = (journal.lastGapClearedAt ?? -Infinity) > (e.firstPosted?.at ?? Infinity);
    if (present || e.everAccepted || journal.gaps.size > 0 || postedBeforeClear) {
      // PRESENT, or the journal's own acceptance outranks the store's silence
      // (the restore rule), or a gap stands anywhere (or stood when it was
      // posted): the probe, then the marked copy; never a plain post.
      if (await probe(rid)) return markDelivered(rid, 'probe');
      return redeliver(rid);
    }
    // Never accepted, ABSENT, no gap: the probe once.
    if (await probe(rid)) return markDelivered(rid, 'probe');
    // The precondition of ANY plain post: a fresh read of every watermark
    // taken after the probe and immediately before the post.
    await readAllWatermarks('before-plain-post');
    if (journal.gaps.size > 0) return redeliver(rid);
    log(`${ref(rid)}: absent from the gc store with every watermark intact and the probe NOT FOUND; one plain post`);
    return postEntry(rid, { marked: false, reposts: e.lastPosted?.reposts ?? 0 });
  }

  async function lookup(e) {
    const pmid = pmidOfRid(e.rid);
    const boundary = e.firstPosted?.boundary ?? 0;
    if (await scanTranscript(e.waId, boundary, pmid)) return true;
    // The pass from zero decides: a store rebuilt or restored restarts the
    // conversation at next_sequence 1, below any boundary.
    if (boundary > 0 && await scanTranscript(e.waId, 0, pmid)) return true;
    return false;
  }

  async function scanTranscript(conversationId, after, pmid) {
    let cursor = after;
    for (let page = 0; page < 10000; page++) {
      const answer = await gc.transcript({ scopeId: city, conversationId, after: cursor, limit: 500, order: 'asc' });
      const items = Array.isArray(answer?.items) ? answer.items : [];
      for (const record of items) {
        const kind = recordField(record, 'Kind');
        if (recordField(record, 'ProviderMessageID') === pmid && (kind === undefined || kind === 'inbound')) return true;
      }
      if (items.length < 500) return false;
      const last = recordField(items[items.length - 1], 'Sequence');
      if (!Number.isSafeInteger(last) || last <= cursor) return false;
      cursor = last;
    }
    return false;
  }

  // The receipt's members, else the default route's agent's live sessions:
  // GET /sessions?state=active, every page (next_cursor), newest first.
  async function targetSessions(e) {
    if (e.members.length) return e.members;
    const ids = [];
    let cursor;
    for (let page = 0; page < 50; page++) {
      const list = await gc.sessions({ state: 'active', cursor });
      const items = Array.isArray(list?.items) ? list.items : [];
      for (const s of items) {
        const isAgent = s.template === routeAgent || String(s.template ?? '').endsWith(`/${routeAgent}`) || s.alias === routeAgent;
        if (isAgent && (s.running === true || s.state === 'active') && s.id) ids.push(s.id);
      }
      cursor = list?.next_cursor;
      if (!cursor) break;
    }
    return ids;
  }

  // The paste probe under THE EVIDENCE RULE. A 404 or 409 is an answer (not
  // found); a 5xx or no answer throws so the caller retries on the ladder.
  async function probe(rid) {
    const e = entries().get(rid);
    const pmid = pmidOfRid(rid);
    const sessions = await targetSessions(e);
    if (!sessions.length) {
      journal.append({ t: 'probe', rid, session_id: '', shape: 'none', found: false });
      return false;
    }
    for (const sessionId of sessions) {
      let answer;
      try {
        answer = await gc.sessionTranscript(sessionId);
      } catch (err) {
        if (err.status === 404 || err.status === 409) {
          journal.append({ t: 'probe', rid, session_id: sessionId, shape: 'none', found: false });
          continue;
        }
        throw err;
      }
      const shape = probeShape(answer);
      const found = pasteFound(answer, e.waId, pmid);
      journal.append({ t: 'probe', rid, session_id: sessionId, shape, found });
      if (shape === 'pane') log(`${ref(rid)}: session ${sessionId} answered a terminal snapshot (format text): no evidence`);
      if (found) return true;
    }
    return false;
  }

  async function markDelivered(rid, via) {
    journal.append({ t: 'accepted', rid, receipt_id: '', verdict: 'delivered', via, at: clock() });
    log(`${ref(rid)}: delivered (${via})`);
    oweAck(rid);
  }

  async function redeliver(rid) {
    const e = entries().get(rid);
    journal.append({ t: 'redelivered', rid });
    killPoint('after-redelivered', rid);
    return postEntry(rid, { marked: true, reposts: e.lastPosted?.reposts ?? 0 });
  }

  async function noRouteOnMarked(rid) {
    // ONE PATH (design line 114): the suppression answer and the empty
    // membership answer are the same bytes; one more probe, else the dead letter.
    if (await probe(rid)) return markDelivered(rid, 'probe');
    return deadLetter(rid, 'redelivery_no_route', { detail: NO_ROUTE_ON_MARKED_NOTE });
  }

  // --- the dead letter: AN ACKNOWLEDGEMENT OWED (THE GATE r7's SECOND RULE) ------

  async function deadLetter(rid, reason, { detail = '' } = {}) {
    const e = entries().get(rid);
    const reads = readCounts.get(rid);
    deadLetters.write(e.waId, {
      rid,
      reason,
      detail,
      wa_id: e.waId,
      delivery_id: e.deliveryId,
      envelope: e.received?.envelope,
      receipt_id: e.accepted?.receipt_id ?? '',
      read_count: reads?.count ?? 0,
      last_read_at: reads?.last ?? null,
      at: clock(),
    });
    killPoint('between-deadletter-and-dead', rid);
    journal.append({ t: 'dead', rid, reason });
    nextAt.delete(rid);
    log(`${ref(rid)}: dead-lettered (${reason}); ack owed until the gateway confirms it`);
    oweAck(rid);
  }

  // --- the read-back ---------------------------------------------------------

  // The receipt is read FIRST, every time: a conclusion that lands just
  // before the deadline is acted on; only a receipt that still reads
  // pending at the deadline is dead-lettered.
  async function readBack(rid) {
    const e = entries().get(rid);
    const pastDeadline = () => clock() - (e.firstPosted?.at ?? clock()) >= PENDING_DEADLINE_MS;
    const expire = () => deadLetter(rid, 'pending_deadline', { detail: 'gc receipt still pending 24 hours after the first post' });
    const receiptId = e.accepted?.receipt_id;
    if (!receiptId) return resolveInDoubt(rid);
    const answer = await gc.receipt(receiptId);
    const reads = readCounts.get(rid) ?? { count: 0, last: null };
    readCounts.set(rid, { count: reads.count + 1, last: clock() });
    const state = key(answer?.state);
    const schedule = () => (pastDeadline() ? expire() : nextAt.set(rid, clock() + readbackInterval(clock() - (e.accepted?.at ?? clock()))));
    if (answer?.receipt_id !== receiptId && answer?.receipt_id !== undefined) {
      log(`${ref(rid)}: receipt answer names another id; read again later`);
      return schedule();
    }
    if (state === 'pending') return schedule();
    if (state === 'unknown') {
      log(`${ref(rid)}: receipt reads unknown; the in-doubt rule`);
      return resolveInDoubt(rid);
    }
    if (state !== 'concluded') return schedule();
    const { verdict, terminal } = verdictOf(answer.delivery);
    if (terminal) return afterVerdict(rid, verdict, terminal, answer.delivery);
    if (verdict === 'pending') return schedule(); // gc's hold: stays on the schedule until gc forgets it
    if (verdict === 'unsupported') {
      // A concluded receipt without a usable delivery is no verdict.
      log(`${ref(rid)}: concluded receipt without a usable delivery; the in-doubt rule`);
      return resolveInDoubt(rid);
    }
    journal.append({ t: 'accepted', rid, receipt_id: receiptId, verdict, members: membersOf(answer.delivery), via: 'readback', at: clock() });
    return afterVerdict(rid, verdict, false, answer.delivery);
  }

  // --- the tick --------------------------------------------------------------

  function runRid(rid, task, onDone = () => {}) {
    ridBusy.add(rid);
    task()
      .catch((err) => {
        // A lookup, watermark, probe or read-back call that failed is retried
        // on the ladder; the entry stays where it is.
        const attempt = attempts.get(rid) ?? 0;
        attempts.set(rid, attempt + 1);
        nextAt.set(rid, clock() + ladderDelay(attempt));
        log(`${ref(rid)}: step failed (${err.message}); retry in ${ladderDelay(attempt) / 1000}s`);
      })
      .finally(() => {
        ridBusy.delete(rid);
        onDone();
      });
  }

  function chains() {
    const bySender = new Map();
    for (const e of entries().values()) {
      if (!e.received) continue;
      if (!bySender.has(e.waId)) bySender.set(e.waId, []);
      bySender.get(e.waId).push(e);
    }
    for (const list of bySender.values()) list.sort((a, b) => Number(a.deliveryId) - Number(b.deliveryId));
    return bySender;
  }

  function compact(reason) {
    try {
      // An acknowledged rid goes only once a poll that began after its ack
      // has completed (a page computed before the gateway's delete would
      // otherwise bring it back to the no-entry branch).
      const result = journal.compact((e) => lastCompletedPollSeq > (ackedAtPoll.get(e.rid) ?? 0));
      for (const rid of ackedAtPoll.keys()) if (!entries().has(rid)) ackedAtPoll.delete(rid);
      stats.lastCompactAt = clock();
      log(`journal compacted (${reason}): ${result.droppedEntries} acknowledged entr(ies) dropped, ${result.lines} line(s) kept`);
    } catch (err) {
      log(`journal compaction failed (${err.message}); the journal is unchanged`);
    }
    nextCompactAt = clock() + compactMs;
  }

  async function tick() {
    if (stopped) return;
    if (!started) startup();
    const now = clock();

    if (!authFailed && !polling && now >= nextPollAt) void poll();
    if (!acking && ackOwedIds.size && now >= ackNextAt) void driveAcks(false);

    if (journal.watermarks.size && !watermarkRunning
      && now >= watermarkNextTryAt
      && (watermarkWanted || now >= (stats.lastWatermarkReadAt ?? 0) + watermarkCheckMs)) {
      void scheduledWatermarkRead();
    }

    for (const [waId, list] of chains()) {
      if (senderBusy.has(waId)) continue;
      const head = list.find((e) => CHAIN_STATES.has(entryState(e)));
      if (!head || ridBusy.has(head.rid)) continue;
      if ((nextAt.get(head.rid) ?? 0) > now) continue;
      senderBusy.add(waId);
      runRid(head.rid, () => advance(head.rid), () => senderBusy.delete(waId));
    }

    for (const e of entries().values()) {
      if (entryState(e) !== 'accepted:pending' || ridBusy.has(e.rid)) continue;
      if ((nextAt.get(e.rid) ?? 0) > now) continue;
      runRid(e.rid, () => readBack(e.rid));
    }

    if (now >= nextCompactAt) compact('cadence');
    if (keeper?.tick && !keeperBusy) {
      keeperBusy = true;
      Promise.resolve()
        .then(() => keeper.tick())
        .catch((err) => log(`keeper tick failed (${err.message})`))
        .finally(() => { keeperBusy = false; });
    }
  }

  // --- the operator's verbs --------------------------------------------------

  // The operator names a hold by its ref (what status and the doctor show)
  // or by its rid (the journal's received line).
  async function resolve(name, as) {
    let e = entries().get(name);
    if (!e) e = [...entries().values()].find((x) => ref(x.rid) === name);
    if (!e) return { status: 404, json: { error: `no journal entry for ${name}` } };
    const rid = e.rid;
    if (entryState(e) !== 'held') return { status: 409, json: { error: `${ref(rid)} is not a terminal hold (state ${entryState(e)})` } };
    const info = {
      ref: ref(rid),
      wa_id_last4: lastFour(e.waId),
      receipt_id: e.held?.receipt_id ?? e.accepted?.receipt_id ?? '',
      held_at: e.held?.at ?? null,
    };
    if (as === 'delivered') {
      journal.append({ t: 'accepted', rid, receipt_id: info.receipt_id, verdict: 'delivered', via: 'operator', at: clock() });
      oweAck(rid);
    } else if (as === 'dead') {
      await deadLetter(rid, 'held_terminal', { detail: 'the operator resolved the terminal hold with --dead' });
    } else {
      return { status: 400, json: { error: 'as must be delivered or dead' } };
    }
    return { status: 200, json: { ok: true, as, ...info } };
  }

  // Serialized with the watermark reads, so a read in flight can never write
  // a gap from the watermark the clear just rebased.
  function clearGap(waId) {
    const run = watermarkChain.catch(() => {}).then(() => clearGapNow(waId));
    watermarkChain = run.catch(() => {});
    return run;
  }

  async function clearGapNow(waId) {
    const conversationId = String(waId);
    const gap = journal.gaps.get(conversationId);
    if (!gap) return { status: 404, json: { error: `no restore gap stands for ...${lastFour(conversationId)}` } };
    const headPage = await gc.transcript({ scopeId: city, conversationId, limit: 1, order: 'desc' });
    const head = Array.isArray(headPage?.items) ? headPage.items[0] : undefined;
    journal.append({ t: 'gap_cleared', conversation_id: conversationId });
    if (head) {
      // REBASE: the operator's word is the one hand that may lower a watermark.
      journal.append({
        t: 'watermark',
        conversation_id: conversationId,
        sequence: recordField(head, 'Sequence'),
        provider_message_id: recordField(head, 'ProviderMessageID'),
        accepted_at: recordField(head, 'CreatedAt'),
        rebased_at: clock(),
      });
    } else {
      journal.append({ t: 'watermark_cleared', conversation_id: conversationId });
    }
    return {
      status: 200,
      json: {
        ok: true,
        gap,
        head: head ? { sequence: recordField(head, 'Sequence'), created_at: recordField(head, 'CreatedAt') } : null,
      },
    };
  }

  function status() {
    const counts = {};
    const held = [];
    for (const e of entries().values()) {
      const state = entryState(e);
      counts[state] = (counts[state] ?? 0) + 1;
      if (state === 'held') held.push({ ref: ref(e.rid), wa_id_last4: lastFour(e.waId), receipt_id: e.held?.receipt_id ?? e.accepted?.receipt_id ?? '', held_at: e.held?.at ?? null });
    }
    return {
      auth_failed: authFailed,
      last_poll_at: stats.lastPollAt,
      last_poll_error: stats.lastPollError,
      polls: stats.polls,
      entries: counts,
      acks_owed: ackOwedIds.size,
      binds_owed: bindOwed.size,
      held,
      gaps: [...journal.gaps.values()].map((g) => ({ ...g, conversation_id: `...${lastFour(g.conversation_id)}` })),
      watermarks: journal.watermarks.size,
      dead_letters: deadLetters.count(),
      last_watermark_read_at: stats.lastWatermarkReadAt,
    };
  }

  return {
    tick,
    resolve,
    clearGap,
    status,
    stop() { stopped = true; },
    get authFailed() { return authFailed; },
    // exposed for tests and the doctor's runtime view
    _readAllWatermarks: readAllWatermarks,
  };
}
