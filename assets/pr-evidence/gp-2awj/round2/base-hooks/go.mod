// The off-parity hooks at the PR base, kept for re-running the base. This
// go.mod keeps them out of the repository root module: each file is copied
// over its path in a tree of c9969436 and runs there, never here.
module example.invalid/offparity-base-hooks

go 1.22
