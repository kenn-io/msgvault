package imap

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestReadCommandCancellationClosesCapturedTransport(t *testing.T) {
	local, remote := net.Pipe()
	conn := imapclient.New(local, nil)
	defer func() { _ = conn.Close(); _ = remote.Close() }()
	greeted := make(chan struct{})
	go func() { _, _ = remote.Write([]byte("* PREAUTH fixture ready\r\n")); close(greeted) }()
	require.NoError(t, conn.WaitGreeting())
	<-greeted
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 0})
	client := newTestClient(t, addr)
	client.conn = conn
	client.logger = slog.Default()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		client.mu.Lock()
		defer client.mu.Unlock()
		_, err := runCommand(ctx, client, func() (struct{}, error) { return struct{}{}, conn.Noop().Wait() })
		finished <- err
	}()
	_, err := bufio.NewReader(remote).ReadString('\n')
	require.NoError(t, err, "command is blocked awaiting a server response")
	cancel()
	select {
	case err := <-finished:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		// Bound an observable cancellation transition; release the baseline
		// command before failing so this regression cannot leak a goroutine.
		_ = conn.Close()
		<-finished
		assert.Fail(t, "cancelled read command did not terminate its captured transport")
	}
	require.Nil(t, client.conn, "the cancelled current transport must not be reused")
	require.NoError(t, client.withConn(t.Context(), func(active *imapclient.Client) error {
		return active.Noop().Wait()
	}), "the next read reconnects through the real loopback server")
	assert.NotSame(t, conn, client.conn)
	select {
	case <-conn.Closed():
	default:
		assert.Fail(t, "command returned before the decoder terminated")
	}
}

func commandContextPipe(t *testing.T) (*imapclient.Client, net.Conn) {
	t.Helper()
	local, remote := net.Pipe()
	conn := imapclient.New(local, nil)
	t.Cleanup(func() { _ = conn.Close(); _ = remote.Close() })
	greeted := make(chan struct{})
	go func() { _, _ = remote.Write([]byte("* PREAUTH fixture ready\r\n")); close(greeted) }()
	require.NoError(t, conn.WaitGreeting())
	<-greeted
	return conn, remote
}

func TestReadCommandCancellationPreservesReplacementTransport(t *testing.T) {
	old, peer := commandContextPipe(t)
	replacement, replacementPeer := commandContextPipe(t)
	client := &Client{conn: old, logger: slog.Default()}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		client.mu.Lock()
		_, err := runCommand(ctx, client, func() (struct{}, error) {
			client.conn = replacement
			return struct{}{}, old.Noop().Wait()
		})
		client.mu.Unlock()
		finished <- err
	}()
	_, err := bufio.NewReader(peer).ReadString('\n')
	require.NoError(t, err)
	cancel()
	select {
	case err := <-finished:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		_ = old.Close()
		<-finished
		require.FailNow(t, "captured command did not terminate")
	}
	assert.Same(t, replacement, client.conn)
	select {
	case <-replacement.Closed():
		assert.Fail(t, "replacement transport was closed")
	default:
	}
	answered := make(chan struct{})
	go func() {
		line, err := bufio.NewReader(replacementPeer).ReadString('\n')
		if err == nil {
			_, _ = fmt.Fprintf(replacementPeer, "%s OK completed\r\n", strings.Fields(line)[0])
		}
		close(answered)
	}()
	require.NoError(t, client.withConn(t.Context(), func(conn *imapclient.Client) error { return conn.Noop().Wait() }))
	<-answered
}

func TestReadCommandCancellationJoinsStalledCleanup(t *testing.T) {
	conn, peer := commandContextPipe(t)
	client := &Client{conn: conn, logger: slog.Default()}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- client.CloseContext(ctx) }()
	line, err := bufio.NewReader(peer).ReadString('\n')
	require.NoError(t, err)
	assert.Contains(t, line, "LOGOUT")
	cancel()
	select {
	case err := <-finished:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		_ = conn.Close()
		<-finished
		require.FailNow(t, "cleanup did not terminate after cancellation")
	}
	assert.Nil(t, client.conn)
	select {
	case <-conn.Closed():
	default:
		assert.Fail(t, "cleanup returned before decoder termination")
	}
}
