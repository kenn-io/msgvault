package imap

import (
	"bufio"
	"context"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadCommandCancellationClosesCapturedTransport(t *testing.T) {
	local, remote := net.Pipe()
	conn := imapclient.New(local, nil)
	defer func() { _ = conn.Close(); _ = remote.Close() }()
	greeted := make(chan struct{})
	go func() { _, _ = remote.Write([]byte("* PREAUTH fixture ready\r\n")); close(greeted) }()
	require.NoError(t, conn.WaitGreeting())
	<-greeted
	client := &Client{conn: conn, logger: slog.Default()}
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
	select {
	case <-conn.Closed():
	default:
		assert.Fail(t, "command returned before the decoder terminated")
	}
}
