//go:build !windows

package imap

import "syscall"

var testConnectivityErrors = []error{
	syscall.ECONNREFUSED,
	syscall.ENETUNREACH,
	syscall.EHOSTUNREACH,
	syscall.ENETDOWN,
}
