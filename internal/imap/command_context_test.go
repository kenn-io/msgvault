package imap

import (
	"bufio"
	"context"
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
	assert := assert.New(t)
	require := require.New(t)
	local, remote := net.Pipe()
	conn := imapclient.New(local, nil)
	defer func() { _ = conn.Close(); _ = remote.Close() }()
	greeted := make(chan struct{})
	go func() {
		_, _ = remote.Write([]byte("* PREAUTH [CAPABILITY IMAP4rev1] fixture ready\r\n"))
		close(greeted)
	}()
	require.NoError(conn.WaitGreeting())
	<-greeted
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 0})
	client := newTestClient(t, addr)
	client.conn = conn
	client.logger = slog.Default()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		client.mu.Lock()
		defer client.mu.Unlock()
		_, err := runCommand(ctx, client, func() (struct{}, error) { return struct{}{}, conn.Noop().Wait() })
		finished <- err
	}()
	line, err := bufio.NewReader(remote).ReadString('\n')
	require.NoError(err, "command is blocked awaiting a server response")
	require.Contains(line, " NOOP\r\n", "cancel the target read, not an initial capability request")
	cancel()
	select {
	case err := <-finished:
		require.ErrorIs(err, context.Canceled)
	case <-time.After(2 * time.Second):
		// Bound an observable cancellation transition; release the baseline
		// command before failing so this regression cannot leak a goroutine.
		_ = conn.Close()
		<-finished
		assert.Fail("cancelled read command did not terminate its captured transport")
	}
	require.Nil(client.conn, "the cancelled current transport must not be reused")
	require.NoError(client.withConn(t.Context(), func(active *imapclient.Client) error {
		return active.Noop().Wait()
	}), "the next read reconnects through the real loopback server")
	assert.NotSame(conn, client.conn)
	select {
	case <-conn.Closed():
	default:
		assert.Fail("command returned before the decoder terminated")
	}
}

// Advertise capabilities in PREAUTH so the library has no automatic CAPABILITY
// command that could be mistaken for the blocked NOOP or LOGOUT under test.
func commandContextPipe(t *testing.T) (*imapclient.Client, net.Conn) {
	t.Helper()
	local, remote := net.Pipe()
	conn := imapclient.New(local, nil)
	t.Cleanup(func() { _ = conn.Close(); _ = remote.Close() })
	greeted := make(chan struct{})
	go func() {
		_, _ = remote.Write([]byte("* PREAUTH [CAPABILITY IMAP4rev1] fixture ready\r\n"))
		close(greeted)
	}()
	require.NoError(t, conn.WaitGreeting())
	<-greeted
	return conn, remote
}

func TestObserveFolderStatesStopsAfterCanceledStatus(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	conn, peer := commandContextPipe(t)
	client := &Client{conn: conn, logger: slog.Default()}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type result struct {
		states     map[string]FolderState
		panicValue any
	}
	finished := make(chan result, 1)
	go func() {
		out := result{}
		defer func() {
			out.panicValue = recover()
			finished <- out
		}()
		client.mu.Lock()
		defer client.mu.Unlock()
		out.states = client.observeFolderStates(ctx, []string{"INBOX", "Archive"})
	}()
	reader := bufio.NewReader(peer)
	line, err := reader.ReadString('\n')
	require.NoError(err)
	require.Contains(strings.ToUpper(line), "STATUS")
	require.Contains(strings.ToUpper(line), "INBOX")
	cancel()
	var got result
	select {
	case got = <-finished:
	case <-time.After(2 * time.Second):
		_ = conn.Close()
		<-finished
		require.FailNow("cancelled STATUS did not terminate")
	}
	assert.Nil(got.panicValue, "cancellation must stop mailbox enumeration before the invalidated connection is reused")
	assert.Empty(got.states)
	assert.Nil(client.conn)
	_ = peer.SetReadDeadline(time.Now().Add(time.Second))
	line, err = reader.ReadString('\n')
	require.Error(err, "cancellation closes the captured transport before another STATUS")
	assert.NotContains(strings.ToUpper(line), "ARCHIVE")
}

func TestReadCommandCancellationJoinsStalledCleanup(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	conn, peer := commandContextPipe(t)
	client := &Client{conn: conn, logger: slog.Default()}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- client.CloseContext(ctx) }()
	line, err := bufio.NewReader(peer).ReadString('\n')
	require.NoError(err)
	require.Contains(line, " LOGOUT\r\n")
	cancel()
	select {
	case err := <-finished:
		require.ErrorIs(err, context.Canceled)
	case <-time.After(2 * time.Second):
		_ = conn.Close()
		<-finished
		require.FailNow("cleanup did not terminate after cancellation")
	}
	assert.Nil(client.conn)
	select {
	case <-conn.Closed():
	default:
		assert.Fail("cleanup returned before decoder termination")
	}
}
