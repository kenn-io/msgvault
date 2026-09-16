//go:build windows

package taskclient

import (
	"fmt"
	"net"
	"os"
)

func currentUserID() uint32 { return 0 }

func descriptorFileSecurityCheck() error {
	return ErrDescriptorFileSecurityLimit
}

func validateSecureFileOwner(os.FileInfo, uint32) error {
	return fmt.Errorf("%w: file owner does not match daemon user", ErrInsecureDescriptor)
}

func openSecureRegularFile(string) (*os.File, error) {
	return nil, ErrDescriptorFileSecurityLimit
}

func validateSecureSocket(string, uint32) error {
	return ErrUnixSocketSecurityLimit
}

func peerCredentialsSupported() bool { return false }

func verifyPeerCredentials(net.Conn, uint32) error { return nil }
