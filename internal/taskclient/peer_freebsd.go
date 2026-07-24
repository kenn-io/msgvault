//go:build freebsd

package taskclient

import (
	"fmt"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

func peerCredentialsSupported() bool { return true }

func verifyPeerCredentials(conn net.Conn, expectedOwner uint32) error {
	syscallConn, ok := conn.(syscall.Conn)
	if !ok {
		return fmt.Errorf("%w: Unix peer credential handle unavailable", ErrInsecureEndpoint)
	}
	var credential *unix.Xucred
	var socketErr error
	raw, err := syscallConn.SyscallConn()
	if err != nil {
		return fmt.Errorf("%w: Unix peer credential handle unavailable", ErrInsecureEndpoint)
	}
	err = raw.Control(func(fd uintptr) {
		credential, socketErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	})
	if err != nil || socketErr != nil || credential == nil || credential.Uid != expectedOwner {
		return fmt.Errorf("%w: Unix peer credential mismatch", ErrInsecureEndpoint)
	}
	return nil
}
