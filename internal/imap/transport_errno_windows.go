package imap

import "syscall"

// transportErrnos are the socket errors that mean the peer or network dropped
// the connection, so a fresh connection attempt is worth making. Windows
// reports these through Winsock and system error codes rather than POSIX
// errnos.
var transportErrnos = []syscall.Errno{
	syscall.WSAECONNRESET,
	syscall.WSAECONNABORTED,
	syscall.ERROR_NETNAME_DELETED,
	syscall.ERROR_BROKEN_PIPE,
}
