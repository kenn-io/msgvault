//go:build unix

package peoplesweep

import (
	"os"
	"syscall"
)

// Opening without following links or blocking keeps a swapped-in symlink or
// FIFO from being read before the regular-file check.
const legacyCredentialOpenFlags = os.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
