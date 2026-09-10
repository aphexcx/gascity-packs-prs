"""Real-git test of the lane handoff lifecycle the formula text prescribes.

The do-work and build-basic-review steps hand an item between agent lanes by
BRANCH (a directory is per agent). git allows one worktree per branch, so the
text fixes one lifecycle (gp-0mvp round 6, codex gate r5):

1. a WRITER (implement, the review fix lane) holds the item's branch only while
   writing and releases it with `git switch --detach` when it hands off;
2. a READER (the review lanes) never takes the branch: it detaches its own lane
   at the recorded commit, so parallel readers never contend;
3. the FIX lane takes the branch, commits, releases it; when a holder still has
   the branch it fails closed with the holder named and forces nothing;
4. a crashed writer's branch is released from that lane by the operator, never
   by another agent's step;
5. close-source-anchor reads `git log -1 <branch>`.

Every step below runs the exact git commands the formula text names, in real
worktrees under a temporary directory, with no network. The static rows in
test_formula_assets pin the sentences; this file pins that the sentences work.
"""

from __future__ import annotations

import pathlib
import subprocess
import tempfile
import unittest


def git(cwd: pathlib.Path, *args: str, check: bool = True) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        ["git", "-C", str(cwd), *args],
        check=check,
        capture_output=True,
        text=True,
    )


def out(cwd: pathlib.Path, *args: str) -> str:
    return git(cwd, *args).stdout.strip()


def commit(repo: pathlib.Path, name: str, content: str, message: str) -> str:
    (repo / name).write_text(content, encoding="utf-8")
    git(repo, "add", name)
    git(
        repo,
        "-c", "user.name=t", "-c", "user.email=t@example.invalid",
        "commit", "--quiet", "-m", message,
    )
    return out(repo, "rev-parse", "HEAD")


ITEM_BRANCH = "gp-item1"


class Rig:
    """A rig checkout (the human's) plus per-role lanes as worktrees of it,
    laid out the way the city's `work_dir = ".worktrees/{{.Rig}}/lane-..."`
    patch does. Each lane starts on its own step branch, as `pre_start`
    (worker-worktree.sh) leaves it: the step bead's branch, not the item's."""

    def __init__(self, root: pathlib.Path) -> None:
        self.root = root
        self.rig = root / "rig"
        self.lanes_root = root / ".worktrees" / "rig"
        subprocess.run(["git", "init", "--quiet", "-b", "main", str(self.rig)], check=True)
        self.main_sha = commit(self.rig, "README.md", "hello\n", "init")
        self.rig_before = self.snapshot(self.rig)
        self.stderr_seen: list[str] = []

    def snapshot(self, checkout: pathlib.Path) -> tuple[str, str, str]:
        return (
            out(checkout, "branch", "--show-current"),
            out(checkout, "rev-parse", "HEAD"),
            out(checkout, "status", "--porcelain"),
        )

    def lane(self, role: str) -> pathlib.Path:
        path = self.lanes_root / f"lane-gc.{role}"
        path.parent.mkdir(parents=True, exist_ok=True)
        # pre_start puts the lane on the STEP bead's branch, cut from HEAD.
        git(self.rig, "worktree", "add", "--quiet", "-b", f"step-{role}", str(path), "main")
        return path

    def run(self, lane: pathlib.Path, *args: str, check: bool = True) -> subprocess.CompletedProcess[str]:
        proc = git(lane, *args, check=False)
        self.stderr_seen.append(proc.stderr)
        if check:
            assert proc.returncode == 0, f"git {' '.join(args)} in {lane.name}: {proc.stderr}"
        return proc

    def holder_of(self, branch: str) -> str | None:
        """The worktree path that has `branch` checked out, from
        `git worktree list --porcelain` (the command the fix lane names)."""
        current: str | None = None
        for line in out(self.rig, "worktree", "list", "--porcelain").splitlines():
            if line.startswith("worktree "):
                current = line[len("worktree "):]
            elif line == f"branch refs/heads/{branch}":
                return current
        return None

    # --- the lifecycle steps, each the formula text's commands ---------------

    def operator_prepares(self, lane: pathlib.Path) -> None:
        """prepare-worktree step 4, lane case: create the item's branch from HEAD
        (the lane was detached) and detach the lane from any branch."""
        self.run(lane, "switch", "--detach")  # the operator's lane starts detached here
        assert out(lane, "branch", "--show-current") == ""
        self.run(lane, "branch", ITEM_BRANCH, "HEAD")
        self.run(lane, "switch", "--detach")
        assert out(lane, "rev-parse", "--verify", f"refs/heads/{ITEM_BRANCH}")
        assert out(lane, "branch", "--show-current") == ""

    def writer_takes(self, lane: pathlib.Path, check: bool = True) -> subprocess.CompletedProcess[str]:
        """implement / apply-review-findings: switch the OWN lane onto the branch."""
        return self.run(lane, "switch", "--no-overwrite-ignore", ITEM_BRANCH, check=check)

    def writer_releases(self, lane: pathlib.Path) -> None:
        """Rule 1: after the final commit, before the close."""
        self.run(lane, "switch", "--detach")
        assert out(lane, "branch", "--show-current") == ""

    def reader_detaches(self, lane: pathlib.Path, commit_id: str) -> subprocess.Popen[str]:
        """Rule 2: a review lane inspects the recorded commit detached; returned
        as a process so two readers can do it at the same time."""
        return subprocess.Popen(
            ["git", "-C", str(lane), "switch", "--detach", "--no-overwrite-ignore", commit_id],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )


class LaneLifecycleTests(unittest.TestCase):
    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.rig = Rig(pathlib.Path(self._tmp.name).resolve())

    def tearDown(self) -> None:
        self._tmp.cleanup()

    def assert_rig_root_untouched(self) -> None:
        self.assertEqual(self.rig.snapshot(self.rig.rig), self.rig.rig_before)

    def assert_no_contention(self) -> None:
        for err in self.rig.stderr_seen:
            self.assertNotIn("already checked out", err)
            self.assertNotIn("already used by worktree", err)

    def test_hold_while_writing_release_on_handoff_inspect_detached(self) -> None:
        rig = self.rig
        operator = rig.lane("run-operator")
        implement = rig.lane("implementation-worker-1")
        reviewer_a = rig.lane("implementation-reviewer-1")
        reviewer_b = rig.lane("implementation-reviewer-2")
        fix = rig.lane("implementation-worker-2")

        # prepare-worktree (operator lane): the branch exists, the lane is detached.
        rig.operator_prepares(operator)
        self.assertEqual(out(operator, "branch", "--show-current"), "")

        # implement (its own lane): take, commit, RELEASE (rule 1).
        rig.writer_takes(implement)
        self.assertEqual(out(implement, "branch", "--show-current"), ITEM_BRANCH)
        impl_commit = commit(implement, "feature.txt", "implemented\n", "implement")
        rig.writer_releases(implement)
        self.assertEqual(out(implement, "rev-parse", "HEAD"), impl_commit)  # HEAD stays
        self.assertIsNone(rig.holder_of(ITEM_BRANCH))  # the branch is free
        self.assertEqual(out(operator, "branch", "--show-current"), "")

        # close-source-anchor / review setup read the commit by branch (rule 5).
        recorded = out(reviewer_a, "rev-parse", ITEM_BRANCH)
        self.assertEqual(recorded, impl_commit)
        self.assertEqual(out(reviewer_a, "log", "-1", "--format=%H", ITEM_BRANCH), impl_commit)

        # TWO reviewers detach at the recorded commit at the same time (rule 2).
        procs = [rig.reader_detaches(reviewer_a, recorded), rig.reader_detaches(reviewer_b, recorded)]
        for proc in procs:
            _, err = proc.communicate(timeout=60)
            rig.stderr_seen.append(err)
            self.assertEqual(proc.returncode, 0, err)
        for reviewer in (reviewer_a, reviewer_b):
            self.assertEqual(out(reviewer, "branch", "--show-current"), "")
            self.assertEqual(out(reviewer, "rev-parse", "HEAD"), recorded)
            self.assertEqual(out(reviewer, "show", f"{recorded}:feature.txt"), "implemented")
            self.assertEqual((reviewer / "feature.txt").read_text(encoding="utf-8"), "implemented\n")
        self.assertIsNone(rig.holder_of(ITEM_BRANCH))  # readers never took it
        self.assertEqual(out(operator, "branch", "--show-current"), "")

        # apply-review-findings (a third lane): the branch is free; take, commit, release (rule 3).
        rig.writer_takes(fix)
        self.assertEqual(rig.holder_of(ITEM_BRANCH), str(fix))
        fix_commit = commit(fix, "feature.txt", "implemented\nfixed\n", "fix")
        rig.writer_releases(fix)
        self.assertIsNone(rig.holder_of(ITEM_BRANCH))

        # The close reads the branch tip: the fix commit, on top of the implementation.
        self.assertEqual(out(operator, "log", "-1", "--format=%H", ITEM_BRANCH), fix_commit)
        self.assertEqual(out(operator, "rev-parse", f"{fix_commit}^"), impl_commit)
        # Reviewers still sit at the commit they inspected; nothing moved under them.
        for reviewer in (reviewer_a, reviewer_b):
            self.assertEqual(out(reviewer, "rev-parse", "HEAD"), impl_commit)

        self.assert_no_contention()
        self.assertEqual(out(operator, "branch", "--show-current"), "")
        self.assert_rig_root_untouched()

    def test_writer_that_did_not_release_blocks_the_fix_lane_with_the_holder_named(self) -> None:
        rig = self.rig
        operator = rig.lane("run-operator")
        implement = rig.lane("implementation-worker-1")
        reviewer = rig.lane("implementation-reviewer-1")
        fix = rig.lane("implementation-worker-2")

        rig.operator_prepares(operator)
        rig.writer_takes(implement)
        impl_commit = commit(implement, "feature.txt", "implemented\n", "implement")
        # ... and the implementation lane crashes before `switch --detach`.
        self.assertEqual(rig.holder_of(ITEM_BRANCH), str(implement))

        # A reader is unaffected: it never wanted the branch.
        proc = rig.reader_detaches(reviewer, impl_commit)
        _, err = proc.communicate(timeout=60)
        self.assertEqual(proc.returncode, 0, err)
        self.assertEqual(out(reviewer, "show", f"{impl_commit}:feature.txt"), "implemented")

        # The fix lane's switch fails; git and `git worktree list` both name the holder.
        fix_before = rig.snapshot(fix)
        refused = rig.writer_takes(fix, check=False)
        self.assertNotEqual(refused.returncode, 0)
        # git < 2.45 says "already checked out at", newer says "already used by worktree at".
        self.assertRegex(refused.stderr + refused.stdout, r"already (checked out|used by worktree) at")
        self.assertIn(implement.name, refused.stderr + refused.stdout)
        self.assertEqual(rig.holder_of(ITEM_BRANCH), str(implement))

        # Nothing was forced: the fix lane is where it was, the holder still
        # holds, the operator lane is still detached, the rig root untouched.
        self.assertEqual(rig.snapshot(fix), fix_before)
        self.assertEqual(out(implement, "branch", "--show-current"), ITEM_BRANCH)
        self.assertEqual(out(implement, "rev-parse", "HEAD"), impl_commit)
        self.assertEqual(out(operator, "branch", "--show-current"), "")
        self.assert_rig_root_untouched()

        # Rule 4: the operator releases the branch FROM THE HOLDER'S LANE; only
        # then does the fix lane's own switch succeed.
        rig.run(implement, "switch", "--detach")
        self.assertIsNone(rig.holder_of(ITEM_BRANCH))
        rig.writer_takes(fix)
        self.assertEqual(rig.holder_of(ITEM_BRANCH), str(fix))
        fix_commit = commit(fix, "feature.txt", "implemented\nfixed\n", "fix")
        rig.writer_releases(fix)
        self.assertEqual(out(operator, "log", "-1", "--format=%H", ITEM_BRANCH), fix_commit)
        self.assert_rig_root_untouched()

    def test_detach_at_commit_refuses_to_overwrite_an_ignored_file(self) -> None:
        """The reader's `switch --detach --no-overwrite-ignore <commit>` keeps
        the round-5 guarantee: an ignored file in the lane that the commit
        tracks makes git refuse, and the lane is left as it was."""
        rig = self.rig
        operator = rig.lane("run-operator")
        implement = rig.lane("implementation-worker-1")
        reviewer = rig.lane("implementation-reviewer-1")

        rig.operator_prepares(operator)
        rig.writer_takes(implement)
        impl_commit = commit(implement, "build.out", "tracked\n", "implement tracks a build output")
        rig.writer_releases(implement)

        (reviewer / "build.out").write_text("ignored local\n", encoding="utf-8")
        info_exclude = pathlib.Path(out(reviewer, "rev-parse", "--git-path", "info/exclude"))
        info_exclude.parent.mkdir(parents=True, exist_ok=True)
        info_exclude.write_text("build.out\n", encoding="utf-8")
        self.assertEqual(out(reviewer, "status", "--porcelain"), "")  # ignored, not untracked

        before = rig.snapshot(reviewer)
        proc = rig.reader_detaches(reviewer, impl_commit)
        _, err = proc.communicate(timeout=60)
        self.assertNotEqual(proc.returncode, 0, "git overwrote an ignored file")
        self.assertIn("build.out", err)
        self.assertEqual(rig.snapshot(reviewer), before)
        self.assertEqual((reviewer / "build.out").read_text(encoding="utf-8"), "ignored local\n")
        self.assert_rig_root_untouched()


if __name__ == "__main__":
    unittest.main()
