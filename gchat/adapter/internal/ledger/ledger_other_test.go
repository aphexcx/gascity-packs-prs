//go:build !unix

package ledger_test

import "testing"

func TestCorruptCopyWriteFailureRemovesPartialFile(t *testing.T) {
	t.Skip("requires Unix file-size resource limits")
}
func TestSaveWriteFailureRemovesPartialFile(t *testing.T) {
	t.Skip("requires Unix file-size resource limits")
}
