//go:build !windows

package imap

import "syscall"

// transportErrnos are the socket errors that mean the peer or network is
// unavailable or dropped the connection, so a fresh attempt is worth making.
var transportErrnos = []syscall.Errno{
	syscall.ECONNREFUSED,
	syscall.ENETUNREACH,
	syscall.EHOSTUNREACH,
	syscall.ENETDOWN,

	syscall.ECONNRESET,
	syscall.ECONNABORTED,
	syscall.EPIPE,
}
