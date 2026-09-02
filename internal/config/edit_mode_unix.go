//go:build !windows

package config

import "io/fs"

// sameConfigModePerm reports whether two observed config modes agree on the
// permission bits this platform can actually observe and enforce. Unix-family
// platforms report real permission bits, so verification stays exact: a
// published file that drifted from the requested mode must never verify.
func sameConfigModePerm(left, right fs.FileMode) bool {
	return left.Perm() == right.Perm()
}
