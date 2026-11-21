//go:build darwin

package peoplesweep

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net"
	"net/url"
	"strconv"
	"syscall"
)

const (
	codexProxyUser         = "msgvault"
	codexProxyListenTrials = 8
)

// listenCodexProxy serves the policy proxy on an ephemeral loopback port, the
// one address the Seatbelt profile lets the child connect to. Loopback ports
// are visible to every local user, so each session requires its own random
// Proxy-Authorization credential, which only the child's environment holds.
func listenCodexProxy(ctx context.Context, _ string) (net.Listener, string, string, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, "", "", errors.New("create codex service proxy credential")
	}
	password := hex.EncodeToString(secret)
	listener, address, err := listenCodexLoopback(ctx)
	if err != nil {
		return nil, "", "", err
	}
	endpoint := (&url.URL{
		Scheme: "http", User: url.UserPassword(codexProxyUser, password),
		Host: net.JoinHostPort("127.0.0.1", strconv.Itoa(address.Port)),
	}).String()
	authorization := "Basic " + base64.StdEncoding.EncodeToString([]byte(codexProxyUser+":"+password))
	return listener, endpoint, authorization, nil
}

// codexLoopbackListener serves 127.0.0.1:port and holds [::1]:port. The
// Seatbelt rule "localhost:port" admits both addresses, so holding the IPv6
// one keeps any other process from listening where the child may connect.
type codexLoopbackListener struct {
	net.Listener

	reserved net.Listener
}

func (l codexLoopbackListener) Close() error {
	err := l.Listener.Close()
	if l.reserved != nil {
		err = errors.Join(err, l.reserved.Close())
	}
	return err
}

func listenCodexLoopback(ctx context.Context) (net.Listener, *net.TCPAddr, error) {
	config := &net.ListenConfig{}
	for range codexProxyListenTrials {
		listener, err := config.Listen(ctx, "tcp4", "127.0.0.1:0")
		if err != nil {
			return nil, nil, errors.New("create codex service proxy listener")
		}
		address, ok := listener.Addr().(*net.TCPAddr)
		if !ok || !address.IP.Equal(net.IPv4(127, 0, 0, 1)) || address.Port <= 0 {
			_ = listener.Close()
			return nil, nil, errors.New("codex service proxy is not on loopback")
		}
		reserved, err := config.Listen(ctx, "tcp6", net.JoinHostPort("::1", strconv.Itoa(address.Port)))
		switch {
		case err == nil:
			return codexLoopbackListener{Listener: listener, reserved: reserved}, address, nil
		case errors.Is(err, syscall.EADDRNOTAVAIL):
			// Without IPv6 loopback, no process can listen on [::1] either.
			return codexLoopbackListener{Listener: listener}, address, nil
		}
		_ = listener.Close()
	}
	return nil, nil, errors.New("reserve codex service proxy loopback port")
}
