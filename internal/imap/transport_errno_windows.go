package imap

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// transportErrnos are the socket errors that mean the peer or network is
// unavailable or dropped the connection, so a fresh attempt is worth making.
// Windows reports these through Winsock and system error codes rather than
// POSIX errnos.
var transportErrnos = []syscall.Errno{
	windows.WSAECONNREFUSED,
	windows.WSAENETUNREACH,
	windows.WSAEHOSTUNREACH,
	windows.WSAENETDOWN,

	syscall.WSAECONNRESET,
	syscall.WSAECONNABORTED,
	syscall.ERROR_NETNAME_DELETED,
	syscall.ERROR_BROKEN_PIPE,
}
