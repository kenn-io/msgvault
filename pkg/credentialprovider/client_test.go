package credentialprovider_test

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/pkg/credentialprovider"
)

func TestExchangeHelper(t *testing.T) {
	requirements := require.New(t)
	executable, err := os.Executable()
	requirements.NoError(err)
	r, err := credentialprovider.DecodeRequest(strings.NewReader(requestJSON))
	requirements.NoError(err)
	for _, tc := range []struct {
		mode string
		want error
	}{
		{"ready", nil},
		{"denied", nil},
		{"wrong-scope", credentialprovider.ErrInvalidResponse},
		{"nonzero", credentialprovider.ErrProviderFailed},
		{"invalid-request", credentialprovider.ErrInvalidRequest},
		{"oversize", credentialprovider.ErrInvalidResponse},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			response, err := credentialprovider.Exchange(t.Context(), []string{executable, "-test.run=^TestProviderProcess$", "--", tc.mode}, r)
			if tc.want != nil {
				requirements.ErrorIs(err, tc.want)
				assertions.Nil(response.Credential)
				assertions.NotContains(err.Error(), "synthetic-token")
				return
			}
			requirements.NoError(err)
			if tc.mode == "ready" {
				requirements.NotNil(response.Credential)
				assertions.Equal("synthetic-token", response.Credential.Token)
			} else {
				assertions.Equal(credentialprovider.StatusDenied, response.Status)
			}
		})
	}
}

func TestExchangeCancellationStopsDescendants(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	executable, err := os.Executable()
	requirements.NoError(err)
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	requirements.NoError(err)
	t.Cleanup(func() { _ = listener.Close() })
	requirements.NoError(listener.SetDeadline(time.Now().Add(30 * time.Second)))
	r, err := credentialprovider.DecodeRequest(strings.NewReader(requestJSON))
	requirements.NoError(err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := credentialprovider.Exchange(ctx, []string{executable, "-test.run=^TestProviderProcess$", "--", "parent", listener.Addr().String()}, r)
		done <- err
	}()
	// The grandchild, not its parent, owns this connection. Its closure proves the
	// helper's descendant was stopped, even if it inherited stdout's pipe.
	conn, err := listener.Accept()
	requirements.NoError(err)
	t.Cleanup(func() { _ = conn.Close() })
	requirements.NoError(conn.SetDeadline(time.Now().Add(30 * time.Second)))
	var ready [1]byte
	_, err = io.ReadFull(conn, ready[:])
	requirements.NoError(err)
	cancel()
	select {
	case err := <-done:
		requirements.ErrorIs(err, context.Canceled)
	case <-time.After(30 * time.Second):
		requirements.FailNow("helper did not stop")
	}
	_, err = conn.Read(ready[:])
	assertions.True(errors.Is(err, io.EOF) || errors.Is(err, errConnectionReset),
		"expected the descendant's connection to close, got %v", err)
}

// This is the executable peer, not a substitute for Exchange. It reads the real
// stdin contract and can fail after writing a credential or retain a descendant.
func TestProviderProcess(t *testing.T) {
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) < 2 {
		return
	}
	mode := args[1]
	if mode == "child" {
		conn, err := net.Dial("tcp", args[2])
		if err != nil {
			os.Exit(1)
		}
		_, _ = conn.Write([]byte("1"))
		var b [1]byte
		_, _ = conn.Read(b[:])
		os.Exit(0)
	}
	r, err := credentialprovider.DecodeRequest(os.Stdin)
	if err != nil {
		os.Exit(2)
	}
	if mode == "parent" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestProviderProcess$", "--", "child", args[2]) // #nosec G702 -- this test executable starts itself, with a local test listener address as one argument.
		cmd.Stdout = os.Stdout
		if cmd.Run() != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	if mode == "oversize" {
		_, _ = io.WriteString(os.Stdout, readyJSON+strings.Repeat(" ", credentialprovider.MaxDocumentBytes+1-len(readyJSON)))
		os.Exit(0)
	}
	if mode == "denied" {
		err := credentialprovider.WriteResponse(os.Stdout, r, credentialprovider.Response{Version: r.Version, Operation: r.Operation, RequestID: r.RequestID, Status: credentialprovider.StatusDenied, Reason: "delegation_expired"})
		if err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	data := readyJSON
	if mode == "wrong-scope" {
		data = strings.Replace(data, `archive-a`, `archive-b`, 1)
	}
	_, _ = io.WriteString(os.Stdout, data)
	_, _ = io.WriteString(os.Stderr, "synthetic-token must never reach diagnostics")
	if mode == "nonzero" {
		os.Exit(1)
	}
	if mode == "invalid-request" {
		os.Exit(2)
	}
	os.Exit(0)
}
