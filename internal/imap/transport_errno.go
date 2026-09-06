//go:build !windows

package imap

import "syscall"

// transportErrnos are the socket errors that mean the peer or network dropped
// the connection, so a fresh connection attempt is worth making.
var transportErrnos = []syscall.Errno{
	syscall.ECONNRESET,
	syscall.ECONNABORTED,
	syscall.EPIPE,
}
