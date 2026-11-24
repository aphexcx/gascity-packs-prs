#!/bin/sh
# Usage: git-revert-facts.sh <empty-dir>  -- scratch repos for the revert-line wording.
# Each case builds its own repository, so nothing is reset or deleted.
set -u
cd "$1" || exit 2
g() { git -c user.name=t -c user.email=t@example.invalid -c commit.gpgsign=false "$@"; }

linear() {  # a rebase-shaped landing: A then B touch the same line
  mkdir "$1" && cd "$1" && g init -q -b main && echo base > f && g add f && g commit -qm base
  BASE=$(git rev-parse HEAD)
  echo a > f && g commit -qam A && A=$(git rev-parse HEAD)
  echo b > f && g commit -qam B && B=$(git rev-parse HEAD)
}
fastforward() {  # a fast-forward of a branch that merged its base
  mkdir "$1" && cd "$1" && g init -q -b main && echo base > f && echo x > other && g add f other && g commit -qm base
  g checkout -qb pr && echo f1 > f1 && g add f1 && g commit -qm F1
  g checkout -q main && echo x2 > other && g commit -qam X && LANDED_ON=$(git rev-parse HEAD)
  g checkout -q pr && g merge -q --no-edit main && echo f2 > f2 && g add f2 && g commit -qm F2
  g checkout -q main && g merge -q --ff-only pr && NEWEST=$(git rev-parse HEAD)
}

echo "-- linear: git revert A B (listed oldest first)"
(linear l1; g revert --no-edit "$A" "$B" >/dev/null 2>&1; echo "exit=$? (nonzero = stopped)")
echo "-- linear: git revert A..B"
(linear l2; g revert --no-edit "$A..$B" >/dev/null 2>&1; echo "exit=$?"; git diff --quiet "$BASE" HEAD && echo "tree == base" || echo "tree != base: f is $(cat f)")
echo "-- linear: git revert BASE..B (from the commit it landed on)"
(linear l3; g revert --no-edit "$BASE..$B" >/dev/null 2>&1; echo "exit=$?"; git diff --quiet "$BASE" HEAD && echo "tree == base" || echo "tree != base"; git log --format=%s -2 | tr '\n' '|'; echo)
echo "-- fast-forward: git revert LANDED_ON..NEWEST"
(fastforward f1; g revert --no-edit "$LANDED_ON..$NEWEST" >/dev/null 2>&1; echo "exit=$? (nonzero = stopped)")
echo "-- fast-forward: git revert --no-merges LANDED_ON..NEWEST"
(fastforward f2; g revert --no-edit --no-merges "$LANDED_ON..$NEWEST" >/dev/null 2>&1; echo "exit=$?"; git diff --quiet "$LANDED_ON" HEAD && echo "tree == landed-on" || echo "tree != landed-on")
echo "-- linear: git revert --no-merges BASE..B (the same form on a rebase landing)"
(linear l4; g revert --no-edit --no-merges "$BASE..$B" >/dev/null 2>&1; echo "exit=$?"; git diff --quiet "$BASE" HEAD && echo "tree == base" || echo "tree != base")
