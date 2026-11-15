package imap

import "golang.org/x/sys/windows"

var testConnectivityErrors = []error{
	windows.WSAECONNREFUSED,
	windows.WSAENETUNREACH,
	windows.WSAEHOSTUNREACH,
	windows.WSAENETDOWN,
}
