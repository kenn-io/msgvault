package imap

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gmailapi "go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/testutil"
)

type retryCountingListener struct {
	net.Listener

	accepted  atomic.Int64
	transform func(net.Conn, int64) net.Conn
}

func (l *retryCountingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err //nolint:wrapcheck // the listener exposes the socket's typed close cause
	}
	count := l.accepted.Add(1)
	if l.transform != nil {
		conn = l.transform(conn, count)
	}
	return conn, nil
}

type retryDropConn struct {
	net.Conn
}

func (c *retryDropConn) Read([]byte) (int, error) {
	return 0, io.EOF
}

func (c *retryDropConn) Write([]byte) (int, error) {
	return 0, io.ErrClosedPipe
}

type retryShortWriteConn struct {
	net.Conn

	limit int
	wrote bool
}

func (c *retryShortWriteConn) Write(p []byte) (int, error) {
	if c.wrote {
		return 0, io.ErrClosedPipe
	}
	c.wrote = true
	if len(p) > c.limit {
		p = p[:c.limit]
	}
	n, err := c.Conn.Write(p)
	if err != nil {
		return n, err //nolint:wrapcheck // the fixture preserves the socket error for the client
	}
	return n, io.ErrShortWrite
}

type retryCloseAfterReadConn struct {
	net.Conn

	closeAfter int64
	reset      bool
	reads      atomic.Int64
}

func (c *retryCloseAfterReadConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 && c.reads.Add(1) == c.closeAfter {
		if c.reset {
			if tcpConn, ok := c.Conn.(*net.TCPConn); ok {
				_ = tcpConn.SetLinger(0)
			}
		}
		_ = c.Close()
	}
	return n, err //nolint:wrapcheck // the fixture preserves the socket's typed transport cause
}

type retryPhaseGateConn struct {
	net.Conn

	writeStarted chan struct{}
	allowWrite   chan struct{}
	readReady    chan struct{}
	writeOnce    sync.Once
}

func (c *retryPhaseGateConn) Read(p []byte) (int, error) {
	<-c.readReady
	p[0] = 0
	return 1, nil
}

func (c *retryPhaseGateConn) Write(p []byte) (int, error) {
	c.writeOnce.Do(func() { close(c.writeStarted) })
	<-c.allowWrite
	return len(p), nil
}

func newIMAPServerTLSConfig(t *testing.T) *tls.Config {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	certificate := server.TLS.Certificates[0]
	server.Close()
	return &tls.Config{
		Certificates: []tls.Certificate{certificate},
		NextProtos:   []string{"imap"},
	}
}

func startRetryMemServer(
	t *testing.T,
	mode string,
	transform func(net.Conn, int64) net.Conn,
	startTLSConfig *tls.Config,
) (string, *retryCountingListener) {
	t.Helper()
	base, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = base.Close() })

	listener := base
	if mode == "implicit-tls" {
		listener = tls.NewListener(base, newIMAPServerTLSConfig(t))
	}
	counted := &retryCountingListener{Listener: listener, transform: transform}
	testutil.ServeIMAPMemServer(t, counted, map[string]int{"INBOX": 1}, startTLSConfig)
	return base.Addr().String(), counted
}

func newRetryClient(t *testing.T, addr string, mode string) *Client {
	t.Helper()
	client := newTestClient(t, addr)
	client.config.TLS = mode == "implicit-tls"
	client.config.STARTTLS = mode == "starttls"
	if client.config.TLS || client.config.STARTTLS {
		client.tlsConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // test certificate is intentionally untrusted
	}
	return client
}

func connectRetryClient(ctx context.Context, t *testing.T, client *Client) error {
	t.Helper()
	client.mu.Lock()
	defer client.mu.Unlock()
	return client.connect(ctx)
}

func TestConnectRetry_ImplicitTLSDisconnectBeforeGreeting(t *testing.T) {
	addr, accepted := startRetryMemServer(t, "implicit-tls", func(conn net.Conn, count int64) net.Conn {
		if count != 1 {
			return conn
		}
		tlsConn, ok := conn.(*tls.Conn)
		if ok {
			_ = tlsConn.Handshake()
		}
		return &retryDropConn{Conn: conn}
	}, nil)
	client := newRetryClient(t, addr, "implicit-tls")
	client.sleep = func(context.Context, time.Duration) error { return nil }

	response, err := client.ListMessages(t.Context(), "", "")
	require.NoError(t, err)
	require.Len(t, response.Messages, 1)
	assert.Equal(t, int64(2), accepted.accepted.Load())
}

func TestConnectRetry_TransportModesBeforeGreeting(t *testing.T) {
	tests := []struct {
		name string
		mode string
	}{
		{name: "plaintext", mode: "plaintext"},
		{name: "implicit TLS", mode: "implicit-tls"},
	}
	// The pinned fork sends STARTTLS eagerly from NewStartTLS, so that
	// protocol-write case is covered by TestConnectRetry_STARTTLSResponseTransportFailureIsTerminal.
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transform := func(conn net.Conn, count int64) net.Conn {
				if count == 1 && tt.mode == "implicit-tls" {
					tlsConn, ok := conn.(*tls.Conn)
					if ok {
						_ = tlsConn.Handshake()
					}
				}
				if count == 1 {
					if tcpConn, ok := conn.(*net.TCPConn); ok {
						_ = tcpConn.SetLinger(0)
					}
					_ = conn.Close()
					return &retryDropConn{Conn: conn}
				}
				return conn
			}
			var startTLSConfig *tls.Config
			if tt.mode == "starttls" {
				startTLSConfig = newIMAPServerTLSConfig(t)
			}
			addr, accepted := startRetryMemServer(t, tt.mode, transform, startTLSConfig)
			client := newRetryClient(t, addr, tt.mode)
			client.sleep = func(context.Context, time.Duration) error { return nil }

			response, err := client.ListMessages(t.Context(), "", "")
			require.NoError(t, err)
			require.Len(t, response.Messages, 1)
			assert.Equal(t, int64(2), accepted.accepted.Load())
		})
	}
}

func TestConnectRetry_STARTTLSPhaseRetainsOverlappingResponse(t *testing.T) {
	peer, rawConn := net.Pipe()
	t.Cleanup(func() {
		_ = peer.Close()
		_ = rawConn.Close()
	})
	gate := &retryPhaseGateConn{
		Conn:         rawConn,
		writeStarted: make(chan struct{}),
		allowWrite:   make(chan struct{}),
		readReady:    make(chan struct{}),
	}
	phaseConn := &startTLSPhaseConn{Conn: gate}

	writeDone := make(chan struct{})
	go func() {
		_, _ = phaseConn.Write([]byte("STARTTLS"))
		close(writeDone)
	}()
	select {
	case <-gate.writeStarted:
	case <-time.After(time.Second):
		require.FailNow(t, "STARTTLS command write did not start")
	}

	readDone := make(chan struct{})
	go func() {
		_, _ = phaseConn.Read(make([]byte, 1))
		close(readDone)
	}()
	close(gate.readReady)
	select {
	case <-readDone:
	case <-time.After(time.Second):
		require.FailNow(t, "overlapping STARTTLS response read did not complete")
	}
	close(gate.allowWrite)
	select {
	case <-writeDone:
	case <-time.After(time.Second):
		require.FailNow(t, "STARTTLS command write did not complete")
	}

	_, err := phaseConn.Write([]byte("client hello"))
	require.NoError(t, err)
	assert.True(t, phaseConn.handshakeStarted())
}

func TestConnectRetry_PartialGreetingIsTerminal(t *testing.T) {
	addr, accepted := startRetryMemServer(t, "plaintext", func(conn net.Conn, _ int64) net.Conn {
		return &retryShortWriteConn{Conn: conn, limit: 6}
	}, nil)
	client := newRetryClient(t, addr, "plaintext")
	sleepCalled := false
	client.sleep = func(context.Context, time.Duration) error {
		sleepCalled = true
		return nil
	}

	_, err := client.ListMessages(t.Context(), "", "")
	require.Error(t, err)
	assert.False(t, sleepCalled)
	assert.Equal(t, int64(1), accepted.accepted.Load())
}

func TestConnectRetry_ImplicitTLSHandshakeFailureRetries(t *testing.T) {
	addr, accepted := startRetryMemServer(t, "implicit-tls", func(conn net.Conn, count int64) net.Conn {
		if count == 1 {
			return &retryDropConn{Conn: conn}
		}
		return conn
	}, nil)
	client := newRetryClient(t, addr, "implicit-tls")
	client.sleep = func(context.Context, time.Duration) error { return nil }

	response, err := client.ListMessages(t.Context(), "", "")
	require.NoError(t, err)
	require.Len(t, response.Messages, 1)
	assert.Equal(t, int64(2), accepted.accepted.Load())
}

func TestConnectRetry_CertificateFailureIsTerminal(t *testing.T) {
	for _, mode := range []string{"implicit-tls", "starttls"} {
		t.Run(mode, func(t *testing.T) {
			var serverTLSConfig *tls.Config
			if mode == "starttls" {
				serverTLSConfig = newIMAPServerTLSConfig(t)
			}
			addr, accepted := startRetryMemServer(t, mode, nil, serverTLSConfig)
			client := newTestClient(t, addr)
			client.config.TLS = mode == "implicit-tls"
			client.config.STARTTLS = mode == "starttls"
			sleepCalled := false
			client.sleep = func(context.Context, time.Duration) error {
				sleepCalled = true
				return nil
			}

			_, err := client.ListMessages(t.Context(), "", "")
			require.Error(t, err)
			assert.False(t, sleepCalled)
			assert.Equal(t, int64(1), accepted.accepted.Load())
		})
	}
}

type retryTemporaryError struct{}

func (retryTemporaryError) Error() string   { return "temporary transport failure" }
func (retryTemporaryError) Timeout() bool   { return false }
func (retryTemporaryError) Temporary() bool { return true }

type retryTimeoutError struct{}

func (retryTimeoutError) Error() string   { return "timeout transport failure" }
func (retryTimeoutError) Timeout() bool   { return true }
func (retryTimeoutError) Temporary() bool { return false }

func TestConnectRetry_TransportErrorTypes(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "eof", err: io.EOF, want: true},
		{name: "unexpected eof", err: io.ErrUnexpectedEOF, want: true},
		{name: "temporary network error", err: retryTemporaryError{}, want: true},
		{name: "timeout network error", err: retryTimeoutError{}, want: true},
		{name: "broken pipe", err: fmt.Errorf("write: %w", syscall.EPIPE), want: true},
		{name: "windows connection reset", err: fmt.Errorf("read: %w", syscall.Errno(10054)), want: true},
		{name: "context canceled", err: context.Canceled, want: false},
		{name: "tls record", err: tls.RecordHeaderError{}, want: false},
		{name: "tls alert", err: tls.AlertError(40), want: false},
		{name: "plain error", err: errors.New("connection reset"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isRetryableTransportError(tt.err))
		})
	}
}

func TestConnectRetry_BoundedScheduleAndWarnings(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	counted := &retryCountingListener{Listener: listener, transform: func(conn net.Conn, _ int64) net.Conn {
		if tcpConn, ok := conn.(*net.TCPConn); ok {
			_ = tcpConn.SetLinger(0)
		}
		_ = conn.Close()
		return &retryDropConn{Conn: conn}
	}}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := counted.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	client := newRetryClient(t, listener.Addr().String(), "plaintext")
	var logs bytes.Buffer
	client.logger = slog.New(slog.NewTextHandler(&logs, nil))
	var delays []time.Duration
	client.sleep = func(_ context.Context, delay time.Duration) error {
		delays = append(delays, delay)
		return nil
	}

	err = connectRetryClient(t.Context(), t, client)
	require.Error(t, err)
	assert.Equal(t, connectRetryDelays[:], delays)
	assert.Equal(t, int64(4), counted.accepted.Load())
	assert.Contains(t, logs.String(), "attempt=2")
	assert.Contains(t, logs.String(), "attempt=3")
	assert.Contains(t, logs.String(), "attempt=4")
	assert.Contains(t, logs.String(), "limit=4")
	assert.Contains(t, logs.String(), "delay=5s")
	assert.Contains(t, logs.String(), "delay=15s")
	assert.Contains(t, logs.String(), "delay=45s")
	assert.NotContains(t, logs.String(), testutil.IMAPTestPassword)
	assert.NotContains(t, logs.String(), "access-token")
}

func TestConnectRetry_AuthenticationExactlyOnce(t *testing.T) {
	tests := []struct {
		name       string
		authMethod AuthMethod
		password   string
	}{
		{name: "LOGIN", password: "wrong-password"},
		{name: "XOAUTH2", authMethod: AuthXOAuth2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addr, accepted := startRetryMemServer(t, "plaintext", nil, nil)
			client := newRetryClient(t, addr, "plaintext")
			client.config.AuthMethod = tt.authMethod
			if tt.authMethod == AuthXOAuth2 {
				var tokenCalls atomic.Int64
				client.tokenSource = func(context.Context) (string, error) {
					tokenCalls.Add(1)
					return "access-token", errors.New("token source failed")
				}
				client.password = ""
				defer func() { assert.Equal(t, int64(1), tokenCalls.Load()) }()
			} else {
				client.password = tt.password
			}
			var delays []time.Duration
			client.sleep = func(_ context.Context, delay time.Duration) error {
				delays = append(delays, delay)
				return nil
			}

			_, err := client.ListMessages(t.Context(), "", "")
			require.Error(t, err)
			assert.Empty(t, delays)
			assert.Equal(t, int64(1), accepted.accepted.Load())
		})
	}
}

func TestConnectRetry_STARTTLSHandshakeTransportFailureRetries(t *testing.T) {
	for _, reset := range []bool{false, true} {
		t.Run(map[bool]string{false: "eof", true: "reset"}[reset], func(t *testing.T) {
			addr, accepted := startRetryMemServer(t, "starttls", func(conn net.Conn, count int64) net.Conn {
				if count == 1 {
					return &retryCloseAfterReadConn{Conn: conn, closeAfter: 2, reset: reset}
				}
				return conn
			}, newIMAPServerTLSConfig(t))
			client := newRetryClient(t, addr, "starttls")
			client.sleep = func(context.Context, time.Duration) error { return nil }

			response, err := client.ListMessages(t.Context(), "", "")
			require.NoError(t, err)
			require.Len(t, response.Messages, 1)
			assert.Equal(t, int64(2), accepted.accepted.Load())
		})
	}
}

func TestConnectRetry_STARTTLSResponseTransportFailureIsTerminal(t *testing.T) {
	addr, accepted := startRetryMemServer(t, "starttls", func(conn net.Conn, count int64) net.Conn {
		if count == 1 {
			return &retryCloseAfterReadConn{Conn: conn, closeAfter: 1, reset: true}
		}
		return conn
	}, newIMAPServerTLSConfig(t))
	client := newRetryClient(t, addr, "starttls")
	sleepCalled := false
	client.sleep = func(context.Context, time.Duration) error {
		sleepCalled = true
		return nil
	}

	_, err := client.ListMessages(t.Context(), "", "")
	require.Error(t, err)
	assert.False(t, sleepCalled)
	assert.Equal(t, int64(1), accepted.accepted.Load())
}

func TestConnectRetry_STARTTLSProtocolFailureIsTerminal(t *testing.T) {
	addr, accepted := startRetryMemServer(t, "starttls", nil, nil)
	client := newRetryClient(t, addr, "starttls")
	sleepCalled := false
	client.sleep = func(context.Context, time.Duration) error {
		sleepCalled = true
		return nil
	}

	_, err := client.ListMessages(t.Context(), "", "")
	require.Error(t, err)
	assert.False(t, sleepCalled)
	assert.Equal(t, int64(1), accepted.accepted.Load())
}

func TestConnectRetry_STARTTLSHandshakeProtocolFailureIsTerminal(t *testing.T) {
	serverTLSConfig := newIMAPServerTLSConfig(t)
	serverTLSConfig.MinVersion = tls.VersionTLS13
	addr, accepted := startRetryMemServer(t, "starttls", nil, serverTLSConfig)
	client := newRetryClient(t, addr, "starttls")
	client.tlsConfig.MaxVersion = tls.VersionTLS12
	sleepCalled := false
	client.sleep = func(context.Context, time.Duration) error {
		sleepCalled = true
		return nil
	}

	_, err := client.ListMessages(t.Context(), "", "")
	require.Error(t, err)
	assert.False(t, sleepCalled)
	assert.Equal(t, int64(1), accepted.accepted.Load())
}

func TestConnectRetry_CancellationAtBlockingStages(t *testing.T) {
	tests := []struct {
		name string
		run  func(*testing.T, context.Context) error
	}{
		{
			name: "greeting",
			run: func(t *testing.T, ctx context.Context) error {
				t.Helper()
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				require.NoError(t, err)
				defer func() { _ = listener.Close() }()
				counted := &retryCountingListener{Listener: listener}
				go func() {
					conn, err := counted.Accept()
					if err == nil {
						<-ctx.Done()
						_ = conn.Close()
					}
				}()
				client := newRetryClient(t, listener.Addr().String(), "plaintext")
				return connectRetryClient(ctx, t, client)
			},
		},
		{
			name: "STARTTLS",
			run: func(t *testing.T, ctx context.Context) error {
				t.Helper()
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				require.NoError(t, err)
				defer func() { _ = listener.Close() }()
				counted := &retryCountingListener{Listener: listener}
				go func() {
					conn, err := counted.Accept()
					if err == nil {
						<-ctx.Done()
						_ = conn.Close()
					}
				}()
				client := newRetryClient(t, listener.Addr().String(), "starttls")
				return connectRetryClient(ctx, t, client)
			},
		},
		{
			name: "authentication",
			run: func(t *testing.T, ctx context.Context) error {
				t.Helper()
				addr, _ := startRetryMemServer(t, "plaintext", nil, nil)
				client := newRetryClient(t, addr, "plaintext")
				client.config.AuthMethod = AuthXOAuth2
				client.tokenSource = func(ctx context.Context) (string, error) {
					<-ctx.Done()
					return "", ctx.Err()
				}
				return connectRetryClient(ctx, t, client)
			},
		},
		{
			name: "backoff",
			run: func(t *testing.T, ctx context.Context) error {
				t.Helper()
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				require.NoError(t, err)
				defer func() { _ = listener.Close() }()
				counted := &retryCountingListener{Listener: listener, transform: func(conn net.Conn, _ int64) net.Conn {
					_ = conn.Close()
					return &retryDropConn{Conn: conn}
				}}
				go func() {
					conn, err := counted.Accept()
					if err == nil {
						_ = conn.Close()
					}
				}()
				client := newRetryClient(t, listener.Addr().String(), "plaintext")
				client.sleep = func(ctx context.Context, _ time.Duration) error {
					return sleepContext(ctx, time.Hour)
				}
				return connectRetryClient(ctx, t, client)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
			defer cancel()
			err := tt.run(t, ctx)
			assert.ErrorIs(t, err, context.DeadlineExceeded)
		})
	}
}

func TestConnectRetry_CancelDuringBackoff(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	counted := &retryCountingListener{Listener: listener, transform: func(conn net.Conn, _ int64) net.Conn {
		_ = conn.Close()
		return &retryDropConn{Conn: conn}
	}}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		conn, err := counted.Accept()
		if err == nil {
			_ = conn.Close()
		}
	}()

	client := newRetryClient(t, listener.Addr().String(), "plaintext")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client.sleep = func(ctx context.Context, _ time.Duration) error {
		cancel()
		return sleepContext(ctx, time.Hour)
	}

	err = connectRetryClient(ctx, t, client)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, int64(1), counted.accepted.Load())
}

func TestReconnectPreservesMailboxCursorState(t *testing.T) {
	addr, _ := startRetryMemServer(t, "plaintext", nil, nil)
	client := newRetryClient(t, addr, "plaintext")

	client.mu.Lock()
	require.NoError(t, client.connect(t.Context()))
	require.NoError(t, client.selectMailbox("INBOX"))
	uidValidity := client.selectedUIDValidity
	client.messageListCache = []gmailapi.MessageID{{ID: "INBOX|1"}}
	client.msgIDToLabels = map[string][]string{"message": {"INBOX"}}
	require.NoError(t, client.reconnect(t.Context()))
	client.mu.Unlock()

	assert.Empty(t, client.selectedMailbox)
	assert.Zero(t, client.selectedUIDValidity)
	assert.Equal(t, []gmailapi.MessageID{{ID: "INBOX|1"}}, client.messageListCache)
	assert.Equal(t, map[string][]string{"message": {"INBOX"}}, client.msgIDToLabels)
	client.mu.Lock()
	require.NoError(t, client.selectMailbox("INBOX"))
	client.mu.Unlock()
	assert.Equal(t, uidValidity, client.selectedUIDValidity)
}
