//go:build windows

package taskclient

import (
	"errors"
	"net"
	"os"
)

func currentUserID() uint32 { return 0 }

func descriptorFileSecurityCheck() error {
	return ErrDescriptorFileSecurityLimit
}

func fileOwnerID(string) (uint32, error) {
	return 0, ErrDescriptorFileSecurityLimit
}

func fileInfoOwnerID(os.FileInfo) (uint32, error) {
	return 0, errors.New("windows descriptor ownership is unavailable")
}

func openSecureRegularFile(string) (*os.File, error) {
	return nil, ErrDescriptorFileSecurityLimit
}

func validateSecureSocket(string, uint32) error {
	return ErrUnixSocketSecurityLimit
}

func peerCredentialsSupported() bool { return false }

func verifyPeerCredentials(net.Conn, uint32) error { return nil }
