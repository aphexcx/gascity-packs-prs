# Operator-facing changes

This record shows the old (`-`) and new (`+`) lines for every changed command, reply instruction, receipt field, hint, skill paragraph and README/changelog entry. New files contain only `+` lines. Code around strings is retained to show when each message appears. The fixed reaction tables and four shared rules are unchanged.

## whatsapp/CHANGELOG.md

```diff
+## Unreleased (gp-bod9)
+
+- Add `publish --reply-to` and quote the inbound message in the default reply
+  line. Only the first chunk carries context; retries keep the first quote.
+  Missing quotes are reported on stderr without failing a delivered reply.
+  Adapter and gateway can ship in either order; older gateways send plain.
+  Quotes reach phones after both update. Gateway deployment is separate and
+  requires migration 0005 first; this change adds no migration. Quotes and
+  reactions remain free-form sends inside the 24-hour window and do not reopen it.
+- The updated gateway accepts only known message targets belonging to the
+  calling city. Unknown quotes go plain; unknown reactions are refused.
+  An inbound target older than the gateway's eight-day retention is unknown.
+
+- Add `gc whatsapp react --to --message --emoji` for hand reactions and
+  removal with an empty emoji. It uses the existing gateway route, records
+  a transcript and preserves the 24-hour window. After a pack update and
+  adapter restart the mayor can react; nothing automatic is switched on.
+
```

## whatsapp/README.md

```diff
+## Quoted replies
+
+The reply line under every inbound message now includes
+`gc whatsapp publish --to <conversation_id> --reply-to <message_id> --text-file <path>`.
+It quotes that message on the first part of a text, or on the file's message.
+Leave the flag off subsequent messages of the same answer. The first call's
+quote is retained when its idempotency key is retried, without changing the
+send keys. An invalid id goes plain, including an unexpanded placeholder
+from an older gc. A keeper or delivery-failure id goes plain when the gateway
+does not recognize it. Templates accept the flag but cannot quote.
+
+Only a receipt that requested a quote adds the strings `metadata.quoted`
+and `metadata.quote_reason`. Quoted is `true` when the gateway sent context
+(with an empty reason), `false` for `invalid_id`, `template`,
+`gateway_without_quotes`, `unknown_message` or `meta_refused`, and `unknown`
+with reason `replayed` when only a stored gateway answer is available.
+The command still exits 0 after a successful send and explains a missing
+or uncertain quote on stderr. Meta may omit a quote bubble for a deleted or
+archived target even after accepting the context.
+
+The adapter and gateway can ship in either order. An older gateway ignores
+context and sends plain; the command reports that. Quotes reach a phone only
+when both are updated. Nothing changes on a box until its pack is updated
+and its adapter restarts. Gateway deployment is separate from this change,
+and migration 0005 must precede deployment of the current gateway. There is
+no new migration. Quoted replies use the same 24-hour window and never reopen
+it. Nothing automatic is enabled.
+
+## Hand reactions
+
+`gc whatsapp react --to <conversation_id> --message <message_id> --emoji 👀`
+places a reaction; `--emoji ""` removes it. Use the inbound "Message id:"
+line. The emoji is a character, not a catalog name. All three flags are
+required, and the command needs session attribution just like a publish.
+It prints a resume key before sending and records the delivered reaction in
+the transcript. Reuse that key after a transport error; changing its message
+or emoji is a conflict. A reaction is recorded as the mayor's act when
+`NEVER_SILENT` is on.
+
+A reaction needs the open 24-hour window and never reopens it. It cannot
+use the update template. A refusal exits 1 with a reason and a hint; say the
+same thing in words by the window rule. Nothing reacts automatically: this
+command acts only when the mayor runs it. A box gains it after updating its
+pack and restarting the adapter. No gateway update is needed for this command.
+
+| `gc whatsapp publish --to <conversation_id> --reply-to <message_id> --text-file <path>` | quote the inbound message on the first part of the reply |
+| `gc whatsapp react --to <conversation_id> --message <message_id> --emoji <emoji>` | place a hand reaction inside the window; an empty emoji removes it |
```

## whatsapp/adapter/src/gateway.js

```diff
-      let replayed, acceptedAttempt;
+      let replayed, acceptedAttempt, quoted, quoteReason;
+        const quote = response.headers.get('x-gc-quoted');
+        if (['true', 'false', 'unknown'].includes(quote)) quoted = quote;
+        const reason = response.headers.get('x-gc-quote-reason');
+        if (['unknown_message', 'meta_refused'].includes(reason)) quoteReason = reason;
-      return { ...answer, ...(replayed === undefined ? {} : { replayed }), ...(acceptedAttempt ? { activity_attempt_id: acceptedAttempt } : {}) };
+      return { ...answer, ...(replayed === undefined ? {} : { replayed }), ...(acceptedAttempt ? { activity_attempt_id: acceptedAttempt } : {}),
+        ...(quoted === undefined ? {} : { quoted }), ...(quoteReason ? { quote_reason: quoteReason } : {}) };
```

## whatsapp/adapter/src/index.js

```diff
+    'POST /react': async (body) => publisher.handleReact(body),
-        reply_instructions: 'Reply: write the reply to a file, then run: gc whatsapp publish --to {conversation_id} --text-file <path>',
+        reply_instructions: 'Reply: write the reply to a file, then run: gc whatsapp publish --to {conversation_id} --reply-to {message_ts} --text-file <path>',
```

## whatsapp/adapter/src/outbound.js

```diff
+export const reactionKey = (key, to, messageId, emoji) => `reaction:${sha256Hex(JSON.stringify([key, to, messageId, emoji]))}`;
+const validMessageId = (id) => typeof id === 'string' && id.length >= 1 && id.length <= 256 && !/[\s\x00-\x1f\x7f{}]/u.test(id);
+const requestedQuote = (id) => id === undefined || id === '' ? null
+  : typeof id === 'string' && id.length >= 1 && id.length <= 256 && !/[\s\x00-\x1f\x7f{}]/u.test(id)
+    ? { message_id: id } : { reason: 'invalid_id' };
+const quoteContext = (quote) => quote?.message_id ? { context: { message_id: quote.message_id } } : {};
+function quoteMetadata(quote, answer) {
+  if (!quote) return {};
+  if (quote.reason) return { quoted: 'false', quote_reason: quote.reason };
+  if (answer.quoted === 'true') return { quoted: 'true', quote_reason: '' };
+  if (answer.quoted === 'unknown') return { quoted: 'unknown', quote_reason: 'replayed' };
+  return { quoted: 'false', quote_reason: answer.quote_reason || 'gateway_without_quotes' };
+}
-  function failure(err, { conversationId, conversation, key, chunks, delivered }) {
+  function failure(err, { conversationId, conversation, key, chunks, delivered, reaction = false }) {
+      if (reaction) hint = 'The 24-hour WhatsApp window for this conversation is closed, and a reaction is a free-form message: it was not placed. '
+        + 'Say it in words by the window rule, or wait for the owner\'s next message.';
+    } else if (reaction && err?.status === 422 && err.json?.error === 'unknown_message') {
+      reason = 'unknown_message';
+      hint = 'The reaction was not placed: the message is unknown to this gateway or older than its record. Say it in words by the window rule.';
+    } else if (reaction && err?.json?.error === 'meta_error' && err.json.meta_status >= 400 && err.json.meta_status < 500 && err.json.meta_status !== 429) {
+      gatewayError = 'meta_error';
+      hint = 'The reaction was not placed: Meta refused it. The message may be gone or too old. Say it in words by the window rule.';
+    if (reaction && gatewayError === 'meta_error' && err.json.meta_code != null) metadata.meta_code = String(err.json.meta_code);
-      entry = { endpoint: 'publish', conversation_id: conversationId, text_sha256: textSha, chunks: chunks.length, delivered: 0, message_ids: [], session_id: sessionId };
+      entry = { endpoint: 'publish', conversation_id: conversationId, text_sha256: textSha, chunks: chunks.length, delivered: 0, message_ids: [] };
-        ({ answer, activity } = await sendWithActivity({ to, idempotency_key: textChunkKey(key, conversationId, text, i), type: 'text', text: chunks[i] },
-          conversationId, entry.session_id ?? sessionId));
+        ({ answer, activity } = await sendWithActivity({ to, idempotency_key: textChunkKey(key, conversationId, text, i), type: 'text', text: chunks[i],
+          ...(i === 0 ? quoteContext(entry.quote) : {}) },
+          conversationId, entry.delivered > 0 ? (entry.session_id ?? sessionId) : sessionId));
+      if (i === 0) entry.session_id = activity?.session ?? sessionId;
+      if (i === 0) entry.quote_result = quoteMetadata(entry.quote, answer);
-      metadata: { chunks: String(chunks.length), message_ids: entry.message_ids.join(',') },
+      metadata: { chunks: String(chunks.length), message_ids: entry.message_ids.join(','), ...entry.quote_result },
-      () => chain(conversationId, () => toOwner(context, async (to, closes) => {
-        let entry = progress.get(key);
-        if (!entry) {
-          entry = { endpoint: 'file', conversation_id: conversationId, text_sha256: textSha,
-            transcript_sha256: sha256Hex(text), file: descriptor, caption, chunks: 1, delivered: 0, message_ids: [], session_id: sessionId };
+      () => {
+        // Only the in-flight key owner may reserve progress or choose a quote.
+        if (!existing) {
+          const reserved = { endpoint: 'file', conversation_id: conversationId, text_sha256: textSha,
+            transcript_sha256: sha256Hex(text), file: descriptor, caption, chunks: 1, delivered: 0, message_ids: [],
+            quote: requestedQuote(pub.reply_to_message_id) };
+          if (!progress.save(key, reserved)) return failure({ progressSave: true }, context);
-        const operation = sha256Hex(JSON.stringify([key, conversationId, textSha]));
-        let uploading = false;
-        try {
-          const recoverOnly = !entry.media_id && (!Number.isFinite(closes) || closes * 1000 <= clock());
-          const saveBeforeSend = () => {
-            if (!progress.save(key, entry)) throw Object.assign(new Error('file journal write failed'), { progressSave: true });
-          };
-          if (!entry.media_id && !recoverOnly) {
-            saveBeforeSend();
-            killPoint('before-media-upload', key);
-            uploading = true;
-            const uploaded = await mediaGateway.upload(file, `file-upload:${operation}`);
-            uploading = false;
-            killPoint('after-media-upload', key);
-            if (!str(uploaded?.media_id) || !Number.isFinite(uploaded.expires_at)) throw Object.assign(new Error('invalid upload answer'), { malformed: true });
-            entry.media_id = uploaded.media_id;
-            entry.media_expires_at = uploaded.expires_at;
-          }
-          // A delayed resume starts the send's retention horizon now, not at
-          // upload time. Save before sending so a lost reply cannot age it out.
-          if (!recoverOnly) saveBeforeSend();
-          killPoint('after-media-journal', key);
-          // With no journal handle, probe the existing send key before refusing.
-          // The gateway checks that key before the window and grant. This sentinel
-          // is not a gateway UUID handle, so even a window reopening during the
-          // request cannot cause a new Meta send. No upload is made for the probe.
-          const request = { to, idempotency_key: `file-send:${operation}`, type: file.kind,
-            media: { id: entry.media_id ?? `unuploaded:${operation}`, ...(caption ? { caption } : {}), ...(file.kind === 'document' ? { filename: file.name } : {}) } };
-          const { answer: sent, activity } = caption.trim()
-            ? await sendWithActivity(request, conversationId, entry.session_id ?? sessionId)
-            : { answer: await gateway.send(request) };
-          killPoint('after-media-send', key);
-          if (!str(sent?.message_id)) throw Object.assign(new Error('missing message id'), { noMessageId: true });
-          entry.delivered = 1;
-          entry.message_ids = [sent.message_id];
-          entry.receipt = { conversation, delivered: true, message_id: sent.message_id, idempotency_key: key,
-            metadata: { chunks: '1', message_ids: sent.message_id, file_name: file.name, file_bytes: String(file.size), file_sha256: file.sha256 } };
-          if (activity) entry.activity = activity;
-          progress.save(key, entry);
-          killPoint('after-media-receipt', key);
-          ledger?.noteSend?.(conversationId, { type: file.kind, message_id: sent.message_id, at: clock() });
-          restoreActivity(entry.activity);
-          return { status: 200, json: entry.receipt };
-        } catch (err) {
-          if (err?.status === 422 && err.json?.error === 'window_closed') ledger?.noteWindowClosed?.(conversationId, clock());
-          const refused = failure(err, context);
-          if (!entry.media_id && err?.json?.error === 'media_not_found') {
-            refused.json.failure_kind = 'transient';
-            refused.json.metadata.reason = 'window_changed';
-            refused.json.metadata.hint = 'No completed send was found and the window changed during recovery. Retry with the same idempotency key.';
-            refused.json.error = refused.json.metadata.hint;
-          } else if (err?.json?.error === 'media_not_found' || err?.json?.error === 'media_expired') {
-            refused.json.failure_kind = 'permanent';
-            refused.json.metadata.reason = 'media_expired';
-            refused.json.metadata.hint = 'The upload handle is unavailable or expired. The gateway found no completed send under this key. '
-              + 'Check the chat, then start a new publish with a fresh key if the owner still needs the file; repeating this key cannot restore the handle.';
-            refused.json.error = refused.json.metadata.hint;
-          }
-          if (uploading && err?.json?.error === 'outcome_unknown') {
-            refused.json.metadata.hint = 'Meta may have kept the upload, but the gateway has no confirmed handle. Nothing was sent to the owner. '
-              + 'This key will not upload again; an operator must decide whether to start a new publish with a fresh key.';
-            refused.json.error = refused.json.metadata.hint;
+        return chain(conversationId, () => toOwner(context, async (to, closes) => {
+          let entry = progress.get(key);
+          if (!entry) {
+            entry = { endpoint: 'file', conversation_id: conversationId, text_sha256: textSha,
+              transcript_sha256: sha256Hex(text), file: descriptor, caption, chunks: 1, delivered: 0, message_ids: [] };
-          if (refused.json.metadata.reason === 'window_closed') {
-            refused.json.metadata.hint = 'Files can be sent only after the owner writes again. A file cannot use the update template. '
-              + `Retry with the same --idempotency-key ${key} to recover any completed send without sending twice.`;
-            refused.json.error = refused.json.metadata.hint;
+          const operation = sha256Hex(JSON.stringify([key, conversationId, textSha]));
+          let uploading = false;
+          try {
+            const recoverOnly = !entry.media_id && (!Number.isFinite(closes) || closes * 1000 <= clock());
+            const saveBeforeSend = () => {
+              if (!progress.save(key, entry)) throw Object.assign(new Error('file journal write failed'), { progressSave: true });
+            };
+            if (!entry.media_id && !recoverOnly) {
+              saveBeforeSend();
+              killPoint('before-media-upload', key);
+              uploading = true;
+              const uploaded = await mediaGateway.upload(file, `file-upload:${operation}`);
+              uploading = false;
+              killPoint('after-media-upload', key);
+              if (!str(uploaded?.media_id) || !Number.isFinite(uploaded.expires_at)) throw Object.assign(new Error('invalid upload answer'), { malformed: true });
+              entry.media_id = uploaded.media_id;
+              entry.media_expires_at = uploaded.expires_at;
+            }
+            // A delayed resume starts the send's retention horizon now, not at
+            // upload time. Save before sending so a lost reply cannot age it out.
+            if (!recoverOnly) saveBeforeSend();
+            killPoint('after-media-journal', key);
+            // With no journal handle, probe the existing send key before refusing.
+            // The gateway checks that key before the window and grant. This sentinel
+            // is not a gateway UUID handle, so even a window reopening during the
+            // request cannot cause a new Meta send. No upload is made for the probe.
+            const request = { to, idempotency_key: `file-send:${operation}`, type: file.kind, ...quoteContext(entry.quote),
+              media: { id: entry.media_id ?? `unuploaded:${operation}`, ...(caption ? { caption } : {}), ...(file.kind === 'document' ? { filename: file.name } : {}) } };
+            const { answer: sent, activity } = caption.trim()
+              ? await sendWithActivity(request, conversationId, sessionId)
+              : { answer: await gateway.send(request) };
+            killPoint('after-media-send', key);
+            if (!str(sent?.message_id)) throw Object.assign(new Error('missing message id'), { noMessageId: true });
+            entry.delivered = 1;
+            entry.message_ids = [sent.message_id];
+            entry.receipt = { conversation, delivered: true, message_id: sent.message_id, idempotency_key: key,
+              metadata: { chunks: '1', message_ids: sent.message_id, file_name: file.name, file_bytes: String(file.size), file_sha256: file.sha256,
+                ...quoteMetadata(entry.quote, sent) } };
+            if (activity) entry.activity = activity;
+            progress.save(key, entry);
+            killPoint('after-media-receipt', key);
+            ledger?.noteSend?.(conversationId, { type: file.kind, message_id: sent.message_id, at: clock() });
+            restoreActivity(entry.activity);
+            return { status: 200, json: entry.receipt };
+          } catch (err) {
+            if (err?.status === 422 && err.json?.error === 'window_closed') ledger?.noteWindowClosed?.(conversationId, clock());
+            const refused = failure(err, context);
+            if (!entry.media_id && err?.json?.error === 'media_not_found') {
+              refused.json.failure_kind = 'transient';
+              refused.json.metadata.reason = 'window_changed';
+              refused.json.metadata.hint = 'No completed send was found and the window changed during recovery. Retry with the same idempotency key.';
+              refused.json.error = refused.json.metadata.hint;
+            } else if (err?.json?.error === 'media_not_found' || err?.json?.error === 'media_expired') {
+              refused.json.failure_kind = 'permanent';
+              refused.json.metadata.reason = 'media_expired';
+              refused.json.metadata.hint = 'The upload handle is unavailable or expired. The gateway found no completed send under this key. '
+                + 'Check the chat, then start a new publish with a fresh key if the owner still needs the file; repeating this key cannot restore the handle.';
+              refused.json.error = refused.json.metadata.hint;
+            }
+            if (uploading && err?.json?.error === 'outcome_unknown') {
+              refused.json.metadata.hint = 'Meta may have kept the upload, but the gateway has no confirmed handle. Nothing was sent to the owner. '
+                + 'This key will not upload again; an operator must decide whether to start a new publish with a fresh key.';
+              refused.json.error = refused.json.metadata.hint;
+            }
+            if (refused.json.metadata.reason === 'window_closed') {
+              refused.json.metadata.hint = 'Files can be sent only after the owner writes again. A file cannot use the update template. '
+                + `Retry with the same --idempotency-key ${key} to recover any completed send without sending twice.`;
+              refused.json.error = refused.json.metadata.hint;
+            }
+            return refused;
-          return refused;
-        }
-      })));
+        }));
+      });
-        () => chain(conversationId, () => toOwner(
-          { conversationId, conversation, key, chunks: chunkText(pub.text).length, delivered: progress.get(key)?.delivered ?? 0 },
-          (to) => sendChunks({ key, conversationId, to, text: pub.text, textSha, conversation, sessionId }),
-        )));
+        () => {
+          if (!entry) {
+            const reserved = { endpoint: 'publish', conversation_id: conversationId, text_sha256: textSha,
+              chunks: chunkText(pub.text).length, delivered: 0, message_ids: [],
+              quote: requestedQuote(pub.reply_to_message_id) };
+            if (!progress.save(key, reserved)) return failure({ progressSave: true }, { conversationId, conversation, key, chunks: reserved.chunks, delivered: 0 });
+          }
+          return chain(conversationId, () => toOwner(
+            { conversationId, conversation, key, chunks: chunkText(pub.text).length, delivered: progress.get(key)?.delivered ?? 0 },
+            (to) => sendChunks({ key, conversationId, to, text: pub.text, textSha, conversation, sessionId }),
+          ));
+        });
+  async function handleReact(raw) {
+    try {
+      let body;
+      try { body = JSON.parse(raw); } catch { return bad('invalid JSON'); }
+      if (!isObject(body)) return bad('the body must be a JSON object');
+      const metadata = isObject(body.metadata) ? body.metadata : {};
+      const sessionId = str(body.session_id) || str(metadata.source_session_id);
+      if (!sessionId) return bad(ATTRIBUTION_ERROR);
+      const conversationId = str(body.conversation?.conversation_id);
+      if (!conversationId) return bad('conversation.conversation_id is required');
+      if (!validMessageId(body.message_id)) return bad('message_id must be 1 to 256 characters without whitespace, controls or braces');
+      if (typeof body.emoji !== 'string' || body.emoji.length > 32 || /[a-z0-9\s\x00-\x1f\x7f]/i.test(body.emoji)) {
+        return bad('emoji takes the emoji character itself (or an empty string to remove it), at most 32 UTF-16 units, without ASCII letters, digits or whitespace');
+      }
+      if (body.idempotency_key !== undefined && (typeof body.idempotency_key !== 'string' || !body.idempotency_key)) return bad('idempotency_key must be a non-empty string when given');
+      const key = body.idempotency_key || `whatsapp-reaction-${crypto.randomUUID()}`;
+      const conversation = { ...conversationRef(city, conversationId), ...body.conversation, conversation_id: conversationId };
+      const textSha = sha256Hex(JSON.stringify([body.message_id, body.emoji]));
+      const text = body.emoji ? `[reaction ${body.emoji} to ${body.message_id}]` : `[reaction removed from ${body.message_id}]`;
+      const existing = progress.get(key);
+      if (existing && (existing.endpoint !== 'reaction' || entryConversation(existing) !== conversationId || existing.text_sha256 !== textSha)) return conflict(conversation, key);
+      let answer;
+      if (existing?.receipt) answer = { status: 200, json: clone(existing.receipt) };
+      else {
+        const context = { conversationId, conversation, key, chunks: 1, delivered: 0, reaction: true };
+        answer = await once(key, { conversationId, textSha, endpoint: 'reaction' }, conversation,
+          () => chain(conversationId, () => toOwner(context, async (to) => {
+            const entry = progress.get(key) ?? { endpoint: 'reaction', conversation_id: conversationId, text_sha256: textSha,
+              transcript_sha256: sha256Hex(text) };
+            progress.save(key, entry);
+            try {
+              const { answer: sent, activity } = await sendWithActivity({ to, idempotency_key: reactionKey(key, conversationId, body.message_id, body.emoji),
+                type: 'reaction', reaction: { message_id: body.message_id, emoji: body.emoji } }, conversationId, sessionId);
+              entry.activity = activity;
+              entry.receipt = { conversation, delivered: true, message_id: sent.message_id, idempotency_key: key,
+                metadata: { reaction: body.emoji ? 'placed' : 'removed', reacted_to: body.message_id } };
+              progress.save(key, entry);
+              ledger?.noteSend?.(conversationId, { type: 'reaction', message_id: sent.message_id, at: activity?.at ?? clock() });
+              restoreActivity(activity);
+              log(`reaction to …${lastFour(conversationId)} ${body.emoji ? 'placed' : 'removed'}`);
+              return { status: 200, json: entry.receipt };
+            } catch (err) {
+              if (err?.status === 422 && err.json?.error === 'window_closed') {
+                ledger?.noteWindowClosed?.(conversationId, clock());
+                try { onWindowClosed(conversationId); } catch { log('reaction window-closed hook failed'); }
+              }
+              return failure(err, context);
+            }
+          })));
+      }
+      return metadata.origin === 'cli' ? withTranscript(answer, { key, sessionId, conversationId, text }) : answer;
+    } catch {
+      log('reaction failed inside the adapter');
+      return { status: 500, json: { delivered: false, failure_kind: 'transient', error: 'internal adapter error; retry with the same idempotency key' } };
+    }
+  }
+
-        metadata: { template: UPDATE_TEMPLATE, language: updateLanguage, ...(journaled ? {} : { journaled: 'false' }) },
+        metadata: { template: UPDATE_TEMPLATE, language: updateLanguage, ...(journaled ? {} : { journaled: 'false' }),
+          ...quoteMetadata(existing?.quote, answer) },
-        () => chain(to, () => toOwner(
-          { conversationId: to, conversation, key, chunks: 1, delivered: 0 },
-          (recipient) => (ledger?.windowOpen?.(to) ? windowOpen() : sendUpdate({ key, to, recipient, text, textSha, conversation, sessionId })),
-        )));
+        () => {
+          if (!entry) {
+            const reserved = { endpoint: 'update', conversation_id: to, text_sha256: textSha,
+              quote: requestedQuote(body.reply_to_message_id) ? { reason: 'template' } : null };
+            if (!progress.save(key, reserved)) return failure({ progressSave: true }, { conversationId: to, conversation, key, chunks: 1, delivered: 0 });
+          }
+          return chain(to, () => toOwner(
+            { conversationId: to, conversation, key, chunks: 1, delivered: 0 },
+            (recipient) => (ledger?.windowOpen?.(to) ? windowOpen() : sendUpdate({ key, to, recipient, text, textSha, conversation, sessionId })),
+          ));
+        });
-  return { handlePublish, handlePublishUpdate, notice };
+  return { handlePublish, handlePublishUpdate, handleReact, notice };
```

## whatsapp/commands/publish.sh

```diff
+reply_to=""
+    --reply-to) [ "$#" -ge 2 ] || wa_usage_error "--reply-to needs a value"; reply_to="$2"; shift 2 ;;
+    --reply-to=*) reply_to="${1#*=}"; shift ;;
-  body=$(jq -n --arg to "$to" --arg text "$text" --arg key "$idempotency_key" --arg session "$session" \
-    '{to: $to, text: $text, idempotency_key: $key} + (if $session != "" then {session_id: $session} else {} end)')
+  body=$(jq -n --arg to "$to" --arg text "$text" --arg key "$idempotency_key" --arg session "$session" --arg reply_to "$reply_to" \
+    '{to: $to, text: $text, idempotency_key: $key} + (if $session != "" then {session_id: $session} else {} end)
+     + (if $reply_to != "" then {reply_to_message_id: $reply_to} else {} end)')
-    --arg file "$file" --arg caption "$caption" --arg caption_file "$caption_file" \
+    --arg file "$file" --arg caption "$caption" --arg caption_file "$caption_file" --arg reply_to "$reply_to" \
-     + (if $session != "" then {session_id: $session} else {} end)')
+     + (if $session != "" then {session_id: $session} else {} end)
+     + (if $reply_to != "" then {reply_to_message_id: $reply_to} else {} end)')
+quoted=$(printf '%s' "$WA_BODY" | jq -r '.metadata.quoted // empty')
+case "$quoted" in
+  false)
+    reason=$(printf '%s' "$WA_BODY" | jq -r '.metadata.quote_reason // "unknown"')
+    echo "gc whatsapp publish: sent without the quote ($reason)" >&2 ;;
+  unknown)
+    echo 'gc whatsapp publish: sent; the gateway did not say whether the quote was applied' >&2 ;;
+esac
```

## whatsapp/commands/publish/help.md

```diff
-gc whatsapp publish --to <conversation_id> --text-file <path>
+gc whatsapp publish --to <conversation_id> --reply-to <message_id> --text-file <path>
+- `--reply-to` (or `--reply-to=ID`): quote the id from the inbound "Message id:"
+  line on the first chunk, or on a file's message. An empty value means no
+  quote. With `--update` it is accepted but ignored: templates cannot quote.
+  A retry keeps the first call's quote even when this flag changes. A quote
+  that could not be applied never fails a send; stderr names the reason.
+  An older gateway sends plain; a replay may leave the quote outcome unknown.
```

## whatsapp/commands/react.sh

```diff
+#!/bin/sh
+# A hand reaction, through the adapter and the city's gateway credential.
+set -eu
+WA_VERB=react
+. "$(dirname "$0")/common.sh"
+wa_require curl
+wa_require jq
+
+to=""
+message=""
+emoji=""
+emoji_set=false
+idempotency_key=""
+session="${GC_SESSION_ID:-}"
+while [ "$#" -gt 0 ]; do
+  case "$1" in
+    --to) [ "$#" -ge 2 ] || wa_usage_error "--to needs a value"; to="$2"; shift 2 ;;
+    --to=*) to="${1#*=}"; shift ;;
+    --message) [ "$#" -ge 2 ] || wa_usage_error "--message needs a value"; message="$2"; shift 2 ;;
+    --message=*) message="${1#*=}"; shift ;;
+    --emoji) [ "$#" -ge 2 ] || wa_usage_error "--emoji needs a value"; emoji="$2"; emoji_set=true; shift 2 ;;
+    --emoji=*) emoji="${1#*=}"; emoji_set=true; shift ;;
+    --idempotency-key) [ "$#" -ge 2 ] || wa_usage_error "--idempotency-key needs a value"; idempotency_key="$2"; shift 2 ;;
+    --idempotency-key=*) idempotency_key="${1#*=}"; shift ;;
+    --session) [ "$#" -ge 2 ] || wa_usage_error "--session needs a value"; session="$2"; shift 2 ;;
+    -h|--help) cat "$(dirname "$0")/react/help.md"; exit 0 ;;
+    *) wa_usage_error "unknown argument $1" ;;
+  esac
+done
+[ -n "$to" ] || wa_usage_error "--to <conversation_id> is required"
+[ -n "$message" ] || wa_usage_error "--message <message_id> is required"
+[ "$emoji_set" = true ] || wa_usage_error '--emoji is required (an empty value removes the reaction)'
+if [ -z "$idempotency_key" ]; then
+  if command -v uuidgen >/dev/null 2>&1; then
+    idempotency_key="whatsapp-cli-$(uuidgen | tr '[:upper:]' '[:lower:]')"
+  else
+    idempotency_key="whatsapp-cli-$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')"
+  fi
+fi
+echo "gc whatsapp react: idempotency key $idempotency_key (rerun with --idempotency-key $idempotency_key to resume without duplicating)" >&2
+body=$(jq -n --arg city "${GC_CITY_NAME:-}" --arg to "$to" --arg message "$message" --arg emoji "$emoji" \
+  --arg key "$idempotency_key" --arg session "$session" \
+  '{conversation: {scope_id: $city, provider: "whatsapp", account_id: "dynasty", conversation_id: $to, kind: "dm"},
+    message_id: $message, emoji: $emoji, idempotency_key: $key, metadata: {origin: "cli"}}
+   + (if $session != "" then {session_id: $session} else {} end)')
+wa_call POST /react "$body"
+if [ "$WA_STATUS" -ge 400 ] 2>/dev/null; then
+  err=$(wa_error_of "$WA_BODY")
+  echo "gc whatsapp react: adapter answered HTTP $WA_STATUS${err:+: $err}" >&2
+  exit 1
+fi
+delivered=$(printf '%s' "$WA_BODY" | jq -r '.delivered // false')
+if [ "$delivered" != true ]; then
+  reason=$(printf '%s' "$WA_BODY" | jq -r '.metadata.reason // .failure_kind // "not delivered"')
+  hint=$(printf '%s' "$WA_BODY" | jq -r '.metadata.hint // empty')
+  echo "gc whatsapp react: not delivered ($reason)" >&2
+  [ -n "$hint" ] && echo "gc whatsapp react: $hint" >&2
+  exit 1
+fi
+printf '%s\n' "$WA_BODY"
```

## whatsapp/commands/react/help.md

```diff
+# gc whatsapp react
+
+Place a reaction on one message:
+
+```sh
+gc whatsapp react --to <conversation_id> --message <message_id> --emoji 👀
+```
+
+All three flags are required. Use the id on the inbound message's "Message id:"
+line. `--emoji` takes the emoji character itself, not a name such as `eyes`.
+An empty value (`--emoji ""`) removes the reaction. A reaction needs the open
+24-hour window; it cannot use an update template or reopen the window.
+
+`--session <session_id>` overrides `GC_SESSION_ID`. A session is required.
+The command prints its resume key on stderr before sending. Reuse it with
+`--idempotency-key <key>` after a transport failure; do not change the target
+or emoji under that key. Exit 0 prints the receipt on stdout; exit 1 explains
+a refusal on stderr; exit 2 means a usage error. When a reaction fails, say
+the same thing in words by the window rule.
```

## whatsapp/gateway/README.md

```diff
+## Quoted replies
+
+`POST /v1/send` accepts optional `context: {message_id: string}` on text,
+image, audio, video and document sends. The id has 1 to 256 UTF-16 units;
+other context shapes return `400 bad_request`. Templates and reactions
+ignore context. The frozen v1 contract above remains unchanged.
+
+The gateway quotes only an id in this city's message/joined deliveries or
+its own sends. An unknown id, including another city's id or an inbound
+whose eight-day retention has ended, goes as plain text or media. A reaction
+whose target is unknown is refused `422 unknown_message`, without spending
+its key. Reaction targets older than the inbound retention are refused.
+
+A known quote is sent to Meta as `context: {message_id}`. A definite Meta
+4xx refusal, except HTTP 429 or window code 131047, retries once without
+context under the same claimed key. That attempt's result stands. No
+answer, a 5xx or success without an id remains `outcome_unknown` with no
+second call. Window refusals and rate limits never trigger quote fallback.
+
+The response body remains `{message_id}`. A successful request with context
+has `x-gc-quoted: true|false|unknown`. False adds `x-gc-quote-reason:
+unknown_message|meta_refused`. A stored-answer replay says unknown; the
+gateway stores no quote result. Requests without context have neither
+header. The adapter reports absent headers as `gateway_without_quotes`,
+replay as `replayed`, invalid ids as `invalid_id`, and templates as `template`.
+Only receipts for a requested quote gain the string fields `quoted` and
+`quote_reason`; a sent quote has `quoted: "true"` and an empty reason.
+
+The adapter and gateway may ship in either order: an older gateway ignores
+context and sends plain, which the adapter reports. A quote reaches the
+phone only when both are updated. The default reply line quotes its inbound
+message, only on the first chunk. Nothing enables automatic reactions.
+The 24-hour window still applies and neither a quote nor a reaction opens
+it. Gateway deployment is separate; migration 0005 must precede deployment
+of the current gateway. This change adds no migration or database column.
+
```

## whatsapp/skills/whatsapp/SKILL.md

```diff
-gc whatsapp publish --to <conversation_id> --text-file <path>
+gc whatsapp publish --to <conversation_id> --reply-to <message_id> --text-file <path>
+The reply line quotes the message it sits under. Keep the quote on the first
+message of an answer and leave it off the ones that follow; a long reply
+automatically quotes only its first part. A file can carry the quote too.
+If the quote could not be applied, the command says so on stderr and the
+message still went. A template cannot carry a quote.
+
-You cannot place a reaction on WhatsApp yet. The adapter may place one
-from the first table when a message arrives. Until you can, a message that
-only thanks, confirms, agrees or closes a topic still gets an answer from
-you in words (a reaction from them still rarely needs one).
+You can place a reaction with:
+
+```
+gc whatsapp react --to <conversation_id> --message <message_id> --emoji 👀
+```
+
+Use the id on the "Message id:" line under the message. In an owner's
+`[reaction EMOJI to ID]` line, ID is the message the owner reacted to.
+The flag takes the emoji character itself. An empty value (`--emoji ""`)
+takes your reaction back. The adapter may place one from the first table
+when a message arrives.
+
+A reaction needs the open 24-hour window and does not reopen it. If it is
+refused, use words by the window rule below; a reaction cannot use a template.
+When `gc whatsapp react` fails, the reaction was not placed, so say the same thing in words.
+
+A reaction to an old, deleted, missing or reaction message may be accepted
+and then fail; that failure arrives as a delivery-failed line.
```

## whatsapp/template-fragments/whatsapp-v0.template.md

```diff
-  message: `gc whatsapp publish --to <conversation_id> --text-file <path>`.
+  message: `gc whatsapp publish --to <conversation_id> --reply-to <message_id> --text-file <path>`.
+  Quote only the first message of an answer; stderr says when a quote was not applied.
+- **React**: `gc whatsapp react --to <conversation_id> --message <message_id> --emoji <emoji>`;
+  use the Message id under the message. An empty emoji removes it; the window must be open.
```
