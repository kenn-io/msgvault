//go:build !windows

package credentialprovider_test

import "syscall"

const errConnectionReset = syscall.ECONNRESET
