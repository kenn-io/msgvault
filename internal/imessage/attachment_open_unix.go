//go:build unix

package imessage

import "syscall"

// A replaced source path must not block the importer on a FIFO open.
const attachmentNonblock = syscall.O_NONBLOCK
