//go:build !unix

package media

func Remove(root, space, message string, selection map[string]bool) Removal {
	return Removal{Remaining: -1, Cause: "unsupported"}
}
