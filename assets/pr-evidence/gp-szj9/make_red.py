"""Build the round-3 RED trees for gp-szj9 (PR 74's Opus read r2 MAJOR 1 and
the bead's ADDENDUM 1, PR 76's 409 ambiguous_recipient).

Run from a clean checkout of the round's head:

    python3 -I assets/pr-evidence/gp-szj9/make_red.py <out-dir>

It writes two copies of whatsapp/adapter under <out-dir> (nothing in the
checkout changes), each the named commit's adapter (git archive) with the
round's test/gate.test.js and test/fakes.js added. The fakes gain PR 76's
user_id on a window entry, a send to a user_id, the 409 ambiguous_recipient
and a failing window read; nothing they answered before changes.

  24d75c41/  the round-2 head. RED: the two "Opus read r2 MAJOR 1" tests (the
             gateway key binds neither the recipient nor the whole message)
             and the four "ADDENDUM 1" tests (every 409 is gateway_refused).
  e5a7fda6/  the round's key fix, before the resend. RED: the four
             "ADDENDUM 1" tests only.

Then run, in each tree's whatsapp/adapter:

    node --test --test-reporter=spec test/gate.test.js
"""

import io
import pathlib
import shutil
import subprocess
import sys
import tarfile

TREES = (
    '24d75c415434dba4d7177fc7faec906e807a8227',
    'e5a7fda6',
)
ROUND_FILES = ('test/gate.test.js', 'test/fakes.js')


def main():
    if len(sys.argv) != 2:
        sys.exit('usage: make_red.py <out-dir>')
    out = pathlib.Path(sys.argv[1]).resolve()
    repo = pathlib.Path(subprocess.check_output(['git', 'rev-parse', '--show-toplevel'], text=True).strip())
    adapter = repo / 'whatsapp' / 'adapter'

    for rev in TREES:
        tree = out / rev[:8]
        archive = subprocess.check_output(['git', '-C', str(repo), 'archive', '--format=tar', rev, 'whatsapp/adapter'])
        with tarfile.open(fileobj=io.BytesIO(archive)) as tar:
            tar.extractall(tree, filter='data')
        for name in ROUND_FILES:
            shutil.copy2(adapter / name, tree / 'whatsapp' / 'adapter' / name)
        print(f'{tree}/whatsapp/adapter: {rev[:8]} plus the round\'s {" and ".join(ROUND_FILES)}')


if __name__ == '__main__':
    main()
