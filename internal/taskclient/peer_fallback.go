//go:build !darwin && !freebsd && !linux && !windows

package taskclient

import "net"

func peerCredentialsSupported() bool { return false }

func verifyPeerCredentials(net.Conn, uint32) error { return nil }
