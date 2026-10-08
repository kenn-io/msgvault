//go:build unix

package peoplesweep

import "syscall"

// Opening without following links or blocking keeps a swapped-in symlink or
// FIFO from being opened before the regular-file check.
const legacyCredentialNoFollow = syscall.O_NOFOLLOW | syscall.O_NONBLOCK
