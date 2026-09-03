package imap

import (
	"context"
	"net"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gmailapi "go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestReconnectRetriesDroppedInitialConnection(t *testing.T) {
	addr, _, accepted := testutil.StartIMAPMemServerWithDroppedConnections(
		t, map[string]int{"INBOX": 1}, 1)
	client := newTestClient(t, addr)
	client.sleep = func(context.Context, time.Duration) error { return nil }

	response, err := client.ListMessages(t.Context(), "", "")
	require.NoError(t, err)
	assert.Len(t, response.Messages, 1)
	assert.GreaterOrEqual(t, accepted(), int64(2))
}

func TestRetryUsesBoundedConnectionBackoff(t *testing.T) {
	addr, _, accepted := testutil.StartIMAPMemServerWithDroppedConnections(
		t, map[string]int{"INBOX": 1}, 3)
	client := newTestClient(t, addr)
	var delays []time.Duration
	client.sleep = func(_ context.Context, delay time.Duration) error {
		delays = append(delays, delay)
		return nil
	}

	response, err := client.ListMessages(t.Context(), "", "")
	require.NoError(t, err)
	assert.Len(t, response.Messages, 1)
	assert.Equal(t, connectRetryDelays[:], delays)
	assert.Equal(t, int64(4), accepted())
}

func TestRetryDoesNotRepeatRejectedLogin(t *testing.T) {
	addr, _, accepted := testutil.StartIMAPMemServerWithDroppedConnections(
		t, map[string]int{"INBOX": 1}, 0)
	host, portText, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)
	client := NewClient(&Config{
		Host: host, Port: port, Username: testutil.IMAPTestUsername,
	}, "wrong-password")
	t.Cleanup(func() { _ = client.Close() })
	sleepCalled := false
	client.sleep = func(context.Context, time.Duration) error {
		sleepCalled = true
		return nil
	}

	_, err = client.ListMessages(t.Context(), "", "")
	assert.Error(t, err)
	assert.False(t, sleepCalled)
	assert.Equal(t, int64(1), accepted())
}

type countedListener struct {
	net.Listener
	accepted atomic.Int64
}

func (l *countedListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil {
		l.accepted.Add(1)
	}
	return conn, err
}

func startProtocolEOFGreetingServer(t *testing.T) (string, func() int64) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	counted := &countedListener{Listener: listener}
	go func() {
		for {
			conn, err := counted.Accept()
			if err != nil {
				return
			}
			_, _ = conn.Write([]byte("* NO synthetic protocol EOF\r\n"))
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = listener.Close() })
	return listener.Addr().String(), func() int64 { return counted.accepted.Load() }
}

func TestRetryDoesNotClassifyProtocolEOF(t *testing.T) {
	addr, accepted := startProtocolEOFGreetingServer(t)
	client := newTestClient(t, addr)
	sleepCalled := false
	client.sleep = func(context.Context, time.Duration) error {
		sleepCalled = true
		return nil
	}

	_, err := client.ListMessages(t.Context(), "", "")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "EOF")
	assert.False(t, sleepCalled)
	assert.Equal(t, int64(1), accepted())
}

func TestReconnectPreservesMailboxCursorState(t *testing.T) {
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 1})
	client := newTestClient(t, addr)

	client.mu.Lock()
	defer client.mu.Unlock()
	require.NoError(t, client.connect(t.Context()))
	require.NoError(t, client.selectMailbox("INBOX"))
	uidValidity := client.selectedUIDValidity
	client.messageListCache = []gmailapi.MessageID{{ID: "INBOX|1"}}
	client.msgIDToLabels = map[string][]string{"message": {"INBOX"}}
	require.NoError(t, client.reconnect(t.Context()))
	assert.Empty(t, client.selectedMailbox)
	assert.Zero(t, client.selectedUIDValidity)
	assert.Equal(t, []gmailapi.MessageID{{ID: "INBOX|1"}}, client.messageListCache)
	assert.Equal(t, map[string][]string{"message": {"INBOX"}}, client.msgIDToLabels)
	require.NoError(t, client.selectMailbox("INBOX"))
	assert.Equal(t, uidValidity, client.selectedUIDValidity)
}
