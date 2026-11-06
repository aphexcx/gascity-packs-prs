"""Build the round-2 RED trees for gp-wf4e (PR 74's codex gate r1 findings).

Run from a clean checkout of the round's head:

    python3 -I assets/pr-evidence/gp-wf4e/make_red.py <out-dir>

It writes two copies of whatsapp/adapter under <out-dir> (nothing in the
checkout changes):

  ba915531/  the gate's head (git archive ba915531) with the round's
             test/gate.test.js added: every gate test is RED there.
  v1/        the round's head with ONE change: the restore gap of a detected
             rollback written AFTER the head read (the round's first cut of
             the MAJOR 1 fix) instead of before it. Only the crash test
             ("a crash while the detection's head read is held") is RED there.

Then run, in each: node --test --test-reporter=spec test/gate.test.js
"""

import io
import pathlib
import shutil
import subprocess
import sys
import tarfile

DURABLE_FIRST = """        const gap = journal.append({
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
"""

AFTER_THE_HEAD_READ = """        let head;
        let headError = '';
        try {
          const headPage = await gc.transcript({ scopeId: city, conversationId, limit: 1, order: 'desc' });
          head = Array.isArray(headPage?.items) ? headPage.items[0] : undefined;
        } catch (err) {
          headError = err.message;
        }
        const headSequence = headError ? null : head ? recordField(head, 'Sequence') ?? 0 : 0;
        journal.append({
          t: 'restore_gap',
          conversation_id: conversationId,
          watermark_sequence: wm.sequence,
          watermark_accepted_at: wm.accepted_at ?? null,
          head_sequence: headSequence,
          head_created_at: head ? recordField(head, 'CreatedAt') ?? null : null,
          found_at: clock(),
          found_by: reason,
        });
        const headNote = headError ? `head unknown: ${headError}` : `head ${headSequence}`;
"""


def main():
    if len(sys.argv) != 2:
        sys.exit('usage: make_red.py <out-dir>')
    out = pathlib.Path(sys.argv[1]).resolve()
    repo = pathlib.Path(subprocess.check_output(['git', 'rev-parse', '--show-toplevel'], text=True).strip())
    adapter = repo / 'whatsapp' / 'adapter'

    gate_head = out / 'ba915531'
    archive = subprocess.check_output(['git', '-C', str(repo), 'archive', '--format=tar', 'ba915531', 'whatsapp/adapter'])
    with tarfile.open(fileobj=io.BytesIO(archive)) as tar:
        tar.extractall(gate_head, filter='data')
    shutil.copy2(adapter / 'test' / 'gate.test.js', gate_head / 'whatsapp' / 'adapter' / 'test' / 'gate.test.js')
    print(f'{gate_head}/whatsapp/adapter: ba915531 plus the round\'s test/gate.test.js')

    v1 = out / 'v1'
    shutil.copytree(adapter, v1 / 'whatsapp' / 'adapter', ignore=shutil.ignore_patterns('node_modules'))
    inbound = v1 / 'whatsapp' / 'adapter' / 'src' / 'inbound.js'
    text = inbound.read_text()
    if text.count(DURABLE_FIRST) != 1:
        sys.exit(f'the durable-first block occurs {text.count(DURABLE_FIRST)} time(s) in {inbound}; expected exactly once (run at the round head)')
    inbound.write_text(text.replace(DURABLE_FIRST, AFTER_THE_HEAD_READ))
    print(f'{v1}/whatsapp/adapter: the round head, the gap written after the head read')


if __name__ == '__main__':
    main()
