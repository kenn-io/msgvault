package imap

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
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

func TestRetryDoesNotBackoffPermanentDialFailure(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := listener.Addr().String()
	require.NoError(t, listener.Close())

	client := newTestClient(t, addr)
	sleepCalled := false
	client.sleep = func(context.Context, time.Duration) error {
		sleepCalled = true
		return nil
	}

	_, err = client.ListMessages(t.Context(), "", "")
	assert.Error(t, err)
	assert.False(t, sleepCalled)
}

func TestIsTransientSocketError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "broken pipe", err: fmt.Errorf("write: %w", syscall.EPIPE), want: true},
		{name: "connection reset", err: fmt.Errorf("read: %w", syscall.ECONNRESET), want: true},
		{name: "connection aborted", err: fmt.Errorf("read: %w", syscall.ECONNABORTED), want: true},
		{name: "windows broken pipe", err: fmt.Errorf("write: %w", syscall.Errno(109)), want: true},
		{name: "windows connection abort", err: fmt.Errorf("write: %w", syscall.Errno(10053)), want: true},
		{name: "windows connection reset", err: fmt.Errorf("write: %w", syscall.Errno(10054)), want: true},
		{name: "windows network name deleted", err: fmt.Errorf("write: %w", syscall.Errno(64)), want: true},
		{name: "connection refused", err: fmt.Errorf("dial: %w", syscall.ECONNREFUSED), want: false},
		{name: "canceled", err: context.Canceled, want: false},
		{name: "non-transport", err: errors.New("broken pipe"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isTransientSocketError(tt.err))
		})
	}
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

func TestConnectRetriesDroppedLoginResponse(t *testing.T) {
	addr, accepted := startDroppedAuthServer(t)
	host, portText, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)
	client := NewClient(&Config{
		Host: host, Port: port, Username: testutil.IMAPTestUsername,
	}, testutil.IMAPTestPassword)
	t.Cleanup(func() { _ = client.Close() })
	var delays []time.Duration
	client.sleep = func(_ context.Context, delay time.Duration) error {
		delays = append(delays, delay)
		return nil
	}

	client.mu.Lock()
	err = client.connect(t.Context())
	client.mu.Unlock()
	require.NoError(t, err)
	assert.Equal(t, connectRetryDelays[:1], delays)
	assert.Equal(t, int64(2), accepted())
}

func TestConnectRetriesDroppedXOAUTH2Response(t *testing.T) {
	addr, accepted := startDroppedAuthServer(t)
	host, portText, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)
	var tokenCalls atomic.Int64
	client := NewClient(&Config{
		Host: host, Port: port, Username: testutil.IMAPTestUsername,
		AuthMethod: AuthXOAuth2,
	}, "", WithTokenSource(func(context.Context) (string, error) {
		tokenCalls.Add(1)
		return "test-token", nil
	}))
	t.Cleanup(func() { _ = client.Close() })
	var delays []time.Duration
	client.sleep = func(_ context.Context, delay time.Duration) error {
		delays = append(delays, delay)
		return nil
	}

	client.mu.Lock()
	err = client.connect(t.Context())
	client.mu.Unlock()
	require.NoError(t, err)
	assert.Equal(t, connectRetryDelays[:1], delays)
	assert.Equal(t, int64(2), accepted())
	assert.Equal(t, int64(2), tokenCalls.Load())
}

func TestConnectDoesNotRetryPermanentAuthResponses(t *testing.T) {
	tests := []struct {
		name       string
		authMethod AuthMethod
		response   string
	}{
		{name: "login NO", response: "NO invalid credentials"},
		{name: "login AUTHENTICATIONFAILED", response: "NO [AUTHENTICATIONFAILED] invalid credentials"},
		{name: "login BAD", response: "BAD authentication rejected"},
		{name: "XOAUTH2 NO", authMethod: AuthXOAuth2, response: "NO authentication rejected"},
		{name: "XOAUTH2 AUTHENTICATIONFAILED", authMethod: AuthXOAuth2, response: "NO [AUTHENTICATIONFAILED] authentication rejected"},
		{name: "XOAUTH2 BAD", authMethod: AuthXOAuth2, response: "BAD authentication rejected"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addr, accepted := startAuthResponseServer(t, tt.response)
			host, portText, err := net.SplitHostPort(addr)
			require.NoError(t, err)
			port, err := strconv.Atoi(portText)
			require.NoError(t, err)
			config := &Config{
				Host: host, Port: port, Username: testutil.IMAPTestUsername,
				AuthMethod: tt.authMethod,
			}
			var options []Option
			if tt.authMethod == AuthXOAuth2 {
				options = append(options, WithTokenSource(func(context.Context) (string, error) {
					return "test-token", nil
				}))
			}
			client := NewClient(config, testutil.IMAPTestPassword, options...)
			t.Cleanup(func() { _ = client.Close() })
			sleepCalled := false
			client.sleep = func(context.Context, time.Duration) error {
				sleepCalled = true
				return nil
			}

			client.mu.Lock()
			err = client.connect(t.Context())
			client.mu.Unlock()
			require.Error(t, err)
			assert.False(t, sleepCalled)
			assert.Equal(t, int64(1), accepted())
			var statusErr *imaplib.Error
			require.ErrorAs(t, err, &statusErr)
		})
	}
}

func TestConnectDoesNotRetryPartialAuthResponse(t *testing.T) {
	for _, authMethod := range []AuthMethod{"", AuthXOAuth2} {
		name := "LOGIN"
		var options []Option
		if authMethod == AuthXOAuth2 {
			name = "XOAUTH2"
			options = append(options, WithTokenSource(func(context.Context) (string, error) {
				return "test-token", nil
			}))
		}
		t.Run(name, func(t *testing.T) {
			addr, accepted := startAuthResponseServer(t, "partial")
			host, portText, err := net.SplitHostPort(addr)
			require.NoError(t, err)
			port, err := strconv.Atoi(portText)
			require.NoError(t, err)
			client := NewClient(&Config{
				Host: host, Port: port, Username: testutil.IMAPTestUsername,
				AuthMethod: authMethod,
			}, testutil.IMAPTestPassword, options...)
			t.Cleanup(func() { _ = client.Close() })
			sleepCalled := false
			client.sleep = func(context.Context, time.Duration) error {
				sleepCalled = true
				return nil
			}

			client.mu.Lock()
			err = client.connect(t.Context())
			client.mu.Unlock()
			require.Error(t, err)
			assert.False(t, sleepCalled)
			assert.Equal(t, int64(1), accepted())
		})
	}
}

func TestTransportLost(t *testing.T) {
	reset := syscall.ECONNRESET
	matchingReset := fmt.Errorf("read: %w", reset)
	matchingTimeout := &net.DNSError{Name: "imap.test", Err: "timeout", IsTimeout: true}
	tests := []struct {
		name  string
		stage connectStage
		err   error
		probe *progressConn
		want  bool
	}{
		{
			name:  "dial timeout",
			stage: stageDial,
			err:   matchingTimeout,
			want:  true,
		},
		{
			name:  "observed reset",
			stage: stageAuth,
			err:   matchingReset,
			probe: &progressConn{readErr: reset, requestWritten: true},
			want:  true,
		},
		{
			name:  "observed timeout",
			stage: stageAuth,
			err:   matchingTimeout,
			probe: &progressConn{readErr: matchingTimeout, requestWritten: true},
			want:  true,
		},
		{
			name:  "bare EOF after request",
			stage: stageAuth,
			err:   io.ErrUnexpectedEOF,
			probe: &progressConn{readErr: io.EOF, requestWritten: true},
			want:  true,
		},
		{
			name:  "partial response EOF",
			stage: stageAuth,
			err:   fmt.Errorf("decode: %w", io.ErrUnexpectedEOF),
			probe: &progressConn{readErr: io.EOF, bytesSinceRequestWrite: 4, requestWritten: true},
			want:  false,
		},
		{
			name:  "unobserved local socket error",
			stage: stageAuth,
			err:   fmt.Errorf("write: %w", syscall.EPIPE),
			probe: &progressConn{requestWritten: true},
			want:  false,
		},
		{
			name:  "text only",
			stage: stageAuth,
			err:   errors.New("broken pipe"),
			probe: &progressConn{requestWritten: true},
			want:  false,
		},
		{
			name:  "refused",
			stage: stageDial,
			err:   fmt.Errorf("dial: %w", syscall.ECONNREFUSED),
			want:  false,
		},
		{
			name:  "status response",
			stage: stageAuth,
			err:   &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "bad credentials"},
			probe: &progressConn{readErr: io.EOF, requestWritten: true},
			want:  false,
		},
		{
			name:  "certificate",
			stage: stageSetup,
			err:   x509.UnknownAuthorityError{},
			probe: &progressConn{readErr: reset, requestWritten: true},
			want:  false,
		},
		{
			name:  "canceled",
			stage: stageAuth,
			err:   context.Canceled,
			probe: &progressConn{readErr: reset, requestWritten: true},
			want:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, transportLost(tt.stage, tt.err, tt.probe))
		})
	}
}

func TestTransportLostAfterSuccessfulSTARTTLSResponse(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	probe := &progressConn{Conn: client}
	go func() {
		buffer := make([]byte, 128)
		_, _ = server.Read(buffer)
		_, _ = server.Write([]byte("A001 OK Begin TLS\r\n"))
	}()

	_, err := probe.Write([]byte("A001 STARTTLS\r\n"))
	require.NoError(t, err)
	buffer := make([]byte, 128)
	_, err = probe.Read(buffer)
	require.NoError(t, err)

	probe.mu.Lock()
	probe.readErr = io.EOF
	probe.mu.Unlock()
	assert.True(t, transportLost(stageSetup, io.ErrUnexpectedEOF, probe))
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

func startDroppedAuthServer(t *testing.T) (string, func() int64) {
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
			connectionNumber := counted.accepted.Load()
			go func(conn net.Conn, connectionNumber int64) {
				defer conn.Close()
				if _, err := io.WriteString(conn, "* OK ready\r\n"); err != nil {
					return
				}
				reader := bufio.NewReader(conn)
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						return
					}
					fields := strings.Fields(line)
					if len(fields) < 2 {
						return
					}
					switch strings.ToUpper(fields[1]) {
					case "CAPABILITY":
						_, _ = fmt.Fprintf(conn, "* CAPABILITY IMAP4rev1 AUTH=XOAUTH2\r\n%s OK capabilities\r\n", fields[0])
					case "LOGIN", "AUTHENTICATE":
						if connectionNumber == 1 {
							if tcpConn, ok := conn.(*net.TCPConn); ok {
								_ = tcpConn.SetLinger(0)
							}
							return
						}
						_, _ = fmt.Fprintf(conn, "%s OK authenticated\r\n", fields[0])
						return
					default:
						_, _ = fmt.Fprintf(conn, "%s OK accepted\r\n", fields[0])
					}
				}
			}(conn, connectionNumber)
		}
	}()
	t.Cleanup(func() { _ = listener.Close() })
	return listener.Addr().String(), func() int64 { return counted.accepted.Load() }
}

func startAuthResponseServer(t *testing.T, response string) (string, func() int64) {
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
			go func(conn net.Conn) {
				defer conn.Close()
				if _, err := io.WriteString(conn, "* OK ready\r\n"); err != nil {
					return
				}
				reader := bufio.NewReader(conn)
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						return
					}
					fields := strings.Fields(line)
					if len(fields) < 2 {
						return
					}
					switch strings.ToUpper(fields[1]) {
					case "CAPABILITY":
						_, _ = fmt.Fprintf(conn, "* CAPABILITY IMAP4rev1 AUTH=XOAUTH2\r\n%s OK capabilities\r\n", fields[0])
					case "LOGIN", "AUTHENTICATE":
						if response == "partial" {
							_, _ = fmt.Fprintf(conn, "%s", fields[0])
							return
						}
						_, _ = fmt.Fprintf(conn, "%s %s\r\n", fields[0], response)
						return
					default:
						_, _ = fmt.Fprintf(conn, "%s OK accepted\r\n", fields[0])
					}
				}
			}(conn)
		}
	}()
	t.Cleanup(func() { _ = listener.Close() })
	return listener.Addr().String(), func() int64 { return counted.accepted.Load() }
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

func TestRetryDoesNotClassifyMalformedGreeting(t *testing.T) {
	addr, accepted := startMalformedGreetingServer(t)
	client := newTestClient(t, addr)
	sleepCalled := false
	client.sleep = func(context.Context, time.Duration) error {
		sleepCalled = true
		return nil
	}

	_, err := client.ListMessages(t.Context(), "", "")
	assert.Error(t, err)
	assert.False(t, sleepCalled)
	assert.Equal(t, int64(1), accepted())
}

func TestRetryDoesNotClassifyTruncatedGreeting(t *testing.T) {
	addr, accepted := startTruncatedGreetingServer(t)
	client := newTestClient(t, addr)
	sleepCalled := false
	client.sleep = func(context.Context, time.Duration) error {
		sleepCalled = true
		return nil
	}

	_, err := client.ListMessages(t.Context(), "", "")
	assert.Error(t, err)
	assert.False(t, sleepCalled)
	assert.Equal(t, int64(1), accepted())
}

func TestRetryDoesNotClassifySTARTTLSTruncatedGreeting(t *testing.T) {
	addr, accepted := startTruncatedSTARTTLSGreetingServer(t)
	host, portText, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)
	client := NewClient(&Config{
		Host: host, Port: port, STARTTLS: true, Username: testutil.IMAPTestUsername,
	}, testutil.IMAPTestPassword)
	sleepCalled := false
	client.sleep = func(context.Context, time.Duration) error {
		sleepCalled = true
		return nil
	}

	err = client.connect(t.Context())
	assert.Error(t, err)
	assert.False(t, sleepCalled)
	assert.Equal(t, int64(1), accepted())
}

func TestRetryDoesNotClassifySTARTTLSProtocolFailure(t *testing.T) {
	addr, accepted := startTruncatedSTARTTLSServer(t)
	host, portText, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)
	client := NewClient(&Config{
		Host: host, Port: port, STARTTLS: true, Username: testutil.IMAPTestUsername,
	}, testutil.IMAPTestPassword)
	sleepCalled := false
	client.sleep = func(context.Context, time.Duration) error {
		sleepCalled = true
		return nil
	}

	err = client.connect(t.Context())
	assert.Error(t, err)
	assert.False(t, sleepCalled)
	assert.Equal(t, int64(1), accepted())
}

func TestRetryDoesNotClassifySTARTTLSUntaggedRejection(t *testing.T) {
	addr, accepted := startRejectedSTARTTLSServer(t)
	host, portText, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)
	client := NewClient(&Config{
		Host: host, Port: port, STARTTLS: true, Username: testutil.IMAPTestUsername,
	}, testutil.IMAPTestPassword)
	sleepCalled := false
	client.sleep = func(context.Context, time.Duration) error {
		sleepCalled = true
		return nil
	}

	err = client.connect(t.Context())
	assert.Error(t, err)
	assert.False(t, sleepCalled)
	assert.Equal(t, int64(1), accepted())
}

func TestRetryRetriesSTARTTLSConnectionFailure(t *testing.T) {
	addr, accepted := startDroppedSTARTTLSServer(t, len(connectRetryDelays)+1)
	host, portText, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)
	client := NewClient(&Config{
		Host: host, Port: port, STARTTLS: true, Username: testutil.IMAPTestUsername,
	}, testutil.IMAPTestPassword)
	var delays []time.Duration
	client.sleep = func(_ context.Context, delay time.Duration) error {
		delays = append(delays, delay)
		return nil
	}

	err = client.connect(t.Context())
	assert.Error(t, err)
	assert.Equal(t, connectRetryDelays[:], delays)
	assert.Equal(t, int64(len(connectRetryDelays)+1), accepted())
}

func TestRetryRetriesDroppedSTARTTLSGreeting(t *testing.T) {
	addr, accepted := startDroppedSTARTTLSGreetingServer(t)
	host, portText, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)
	client := NewClient(&Config{
		Host: host, Port: port, STARTTLS: true, Username: testutil.IMAPTestUsername,
	}, testutil.IMAPTestPassword)
	t.Cleanup(func() { _ = client.Close() })
	var delays []time.Duration
	client.sleep = func(_ context.Context, delay time.Duration) error {
		delays = append(delays, delay)
		return nil
	}

	err = client.connect(t.Context())
	assert.Error(t, err)
	assert.Equal(t, connectRetryDelays[:1], delays)
	assert.Equal(t, int64(2), accepted())
}

func TestRetryRetriesResetAfterSTARTTLSCommand(t *testing.T) {
	addr, accepted := startResetSTARTTLSResponseServer(t)
	host, portText, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)
	client := NewClient(&Config{
		Host: host, Port: port, STARTTLS: true, Username: testutil.IMAPTestUsername,
	}, testutil.IMAPTestPassword)
	var delays []time.Duration
	client.sleep = func(_ context.Context, delay time.Duration) error {
		delays = append(delays, delay)
		return nil
	}

	err = client.connect(t.Context())
	assert.Error(t, err)
	assert.Equal(t, connectRetryDelays[:1], delays)
	assert.Equal(t, int64(2), accepted())
}

func TestDialIMAPRetriesSTARTTLSHandshakeLossAfterOK(t *testing.T) {
	addr, accepted := startDroppedSTARTTLSHandshakeResponseServer(t)
	client := newTestClient(t, addr)
	client.config.STARTTLS = true
	conn, probe, err := client.dialIMAP(t.Context(), addr, &imapclient.Options{
		TLSConfig: &tls.Config{InsecureSkipVerify: true}, // test server closes before presenting a certificate
	})
	if conn != nil {
		_ = conn.Close()
	}
	require.Error(t, err)
	var retryErr *retryableConnectError
	require.ErrorAs(t, classifyConnect(stageSetup, addr, err, probe), &retryErr)
	assert.Equal(t, int64(1), accepted())
}

func TestDialIMAPImplicitTLSSuccess(t *testing.T) {
	addr, _ := startImplicitTLSServer(t, func(conn net.Conn) {
		defer conn.Close()
		_, _ = conn.Write([]byte("* OK ready\r\n"))
	})
	host, portText, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)
	client := NewClient(&Config{Host: host, Port: port, TLS: true}, "")
	conn, _, err := client.dialIMAP(t.Context(), addr, &imapclient.Options{
		TLSConfig: &tls.Config{InsecureSkipVerify: true}, // test certificate is intentionally not trusted
	})
	require.NoError(t, err)
	require.NoError(t, waitGreeting(t.Context(), conn))
	require.NoError(t, conn.Close())
}

func TestDialIMAPSTARTTLSSuccess(t *testing.T) {
	addr, _ := startSTARTTLSServer(t, func(conn net.Conn) {
		defer conn.Close()
		for {
			buffer := make([]byte, 128)
			n, err := conn.Read(buffer)
			if err != nil {
				return
			}
			fields := strings.Fields(string(buffer[:n]))
			if len(fields) < 2 {
				return
			}
			switch fields[1] {
			case "CAPABILITY":
				_, _ = fmt.Fprintf(conn, "* CAPABILITY IMAP4rev1\r\n%s OK capabilities\r\n", fields[0])
			case "LOGIN":
				_, _ = fmt.Fprintf(conn, "%s OK logged in\r\n", fields[0])
				return
			default:
				_, _ = fmt.Fprintf(conn, "%s BAD unexpected command\r\n", fields[0])
				return
			}
		}
	})
	host, portText, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)
	client := NewClient(&Config{Host: host, Port: port, STARTTLS: true}, "")
	conn, _, err := client.dialIMAP(t.Context(), addr, &imapclient.Options{
		TLSConfig: &tls.Config{InsecureSkipVerify: true}, // test certificate is intentionally not trusted
	})
	require.NoError(t, err)
	require.NoError(t, waitGreeting(t.Context(), conn))
	require.NoError(t, conn.Login("alice@example.test", "secret").Wait())
	require.NoError(t, conn.Close())
}

func TestRetryRetriesImplicitTLSHandshakeFailure(t *testing.T) {
	addr, accepted := startDroppedTLSHandshakeServer(t, len(connectRetryDelays)+1)
	host, portText, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)
	client := NewClient(&Config{Host: host, Port: port, TLS: true}, "")
	var delays []time.Duration
	client.sleep = func(_ context.Context, delay time.Duration) error {
		delays = append(delays, delay)
		return nil
	}

	err = client.connect(t.Context())
	assert.Error(t, err)
	assert.Equal(t, connectRetryDelays[:], delays)
	assert.Equal(t, int64(len(connectRetryDelays)+1), accepted())
}

func TestRetryDoesNotRepeatImplicitTLSCertificateFailure(t *testing.T) {
	addr, accepted := startImplicitTLSServer(t, func(conn net.Conn) {
		defer conn.Close()
		buffer := make([]byte, 1)
		_, _ = conn.Read(buffer)
	})
	host, portText, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)
	client := NewClient(&Config{Host: host, Port: port, TLS: true}, "")
	sleepCalled := false
	client.sleep = func(context.Context, time.Duration) error {
		sleepCalled = true
		return nil
	}

	err = client.connect(t.Context())
	assert.Error(t, err)
	assert.False(t, sleepCalled)
	assert.Equal(t, int64(1), accepted())
}

func TestRetryDoesNotRepeatCanceledImplicitTLSHandshake(t *testing.T) {
	addr, accepted := startSilentTCPServer(t)
	host, portText, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)
	client := NewClient(&Config{Host: host, Port: port, TLS: true}, "")
	sleepCalled := false
	client.sleep = func(context.Context, time.Duration) error {
		sleepCalled = true
		return nil
	}
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()

	err = client.connect(ctx)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.False(t, sleepCalled)
	assert.Equal(t, int64(1), accepted())
}

func TestRetryHonorsCancellationDuringGreeting(t *testing.T) {
	addr, accepted := startSilentGreetingServer(t)
	client := newTestClient(t, addr)
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()

	err := client.connect(ctx)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Equal(t, int64(1), accepted())
}

func TestRetryHonorsCancellationDuringCapabilityLookup(t *testing.T) {
	addr, accepted := startSilentCapabilityServer(t)
	client := newTestClient(t, addr)
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()

	err := client.connect(ctx)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Equal(t, int64(1), accepted())
}

func TestRetryHonorsCancellationDuringAuthentication(t *testing.T) {
	addr, accepted := startSilentAuthenticationServer(t)
	client := newTestClient(t, addr)
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()

	err := client.connect(ctx)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Equal(t, int64(1), accepted())
}

func TestRetryPropagatesCancellationDuringBackoff(t *testing.T) {
	addr, _, accepted := testutil.StartIMAPMemServerWithDroppedConnections(
		t, map[string]int{"INBOX": 1}, 1)
	client := newTestClient(t, addr)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client.sleep = func(ctx context.Context, _ time.Duration) error {
		cancel()
		return sleepContext(ctx, time.Hour)
	}

	err := client.connect(ctx)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, int64(1), accepted())
}

func TestRetryExhaustionReturnsUnderlyingConnectionError(t *testing.T) {
	addr, _, accepted := testutil.StartIMAPMemServerWithDroppedConnections(
		t, map[string]int{"INBOX": 1}, len(connectRetryDelays)+1)
	client := newTestClient(t, addr)
	client.sleep = func(context.Context, time.Duration) error { return nil }

	err := client.connect(t.Context())
	assert.Error(t, err)
	var retryErr *retryableConnectError
	assert.False(t, errors.As(err, &retryErr))
	assert.Equal(t, int64(len(connectRetryDelays)+1), accepted())
}

func startMalformedGreetingServer(t *testing.T) (string, func() int64) {
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
			_, _ = conn.Write([]byte("not an IMAP greeting\\r\\n"))
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = listener.Close() })
	return listener.Addr().String(), func() int64 { return counted.accepted.Load() }
}

func startTruncatedGreetingServer(t *testing.T) (string, func() int64) {
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
			_, _ = conn.Write([]byte("* OK truncated"))
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = listener.Close() })
	return listener.Addr().String(), func() int64 { return counted.accepted.Load() }
}

func startTruncatedSTARTTLSServer(t *testing.T) (string, func() int64) {
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
			_, _ = conn.Write([]byte("* OK ready for STARTTLS\r\n"))
			buffer := make([]byte, 128)
			_, _ = conn.Read(buffer)
			_, _ = conn.Write([]byte("A001 OK"))
			if tcpConn, ok := conn.(*net.TCPConn); ok {
				_ = tcpConn.CloseWrite()
			}
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = listener.Close() })
	return listener.Addr().String(), func() int64 { return counted.accepted.Load() }
}

func startTruncatedSTARTTLSGreetingServer(t *testing.T) (string, func() int64) {
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
			_, _ = conn.Write([]byte("* OK truncated"))
			time.Sleep(50 * time.Millisecond)
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = listener.Close() })
	return listener.Addr().String(), func() int64 { return counted.accepted.Load() }
}

func startRejectedSTARTTLSServer(t *testing.T) (string, func() int64) {
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
			_, _ = conn.Write([]byte("* OK ready for STARTTLS\r\n"))
			buffer := make([]byte, 128)
			_, _ = conn.Read(buffer)
			_, _ = conn.Write([]byte("* NO STARTTLS rejected\r\n"))
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = listener.Close() })
	return listener.Addr().String(), func() int64 { return counted.accepted.Load() }
}

func startDroppedSTARTTLSServer(t *testing.T, connections int) (string, func() int64) {
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
			_, _ = conn.Write([]byte("* OK ready for STARTTLS\r\n"))
			buffer := make([]byte, 128)
			_, _ = conn.Read(buffer)
			_ = conn.Close()
			if int(counted.accepted.Load()) >= connections {
				return
			}
		}
	}()
	t.Cleanup(func() { _ = listener.Close() })
	return listener.Addr().String(), func() int64 { return counted.accepted.Load() }
}

func startDroppedSTARTTLSGreetingServer(t *testing.T) (string, func() int64) {
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
			if counted.accepted.Load() == 1 {
				_ = conn.Close()
				continue
			}
			_, _ = conn.Write([]byte("* OK ready for STARTTLS\r\n"))
			buffer := make([]byte, 128)
			_, _ = conn.Read(buffer)
			_, _ = conn.Write([]byte("* NO STARTTLS rejected\r\n"))
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = listener.Close() })
	return listener.Addr().String(), func() int64 { return counted.accepted.Load() }
}

func startResetSTARTTLSResponseServer(t *testing.T) (string, func() int64) {
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
			_, _ = conn.Write([]byte("* OK ready for STARTTLS\r\n"))
			time.Sleep(20 * time.Millisecond)
			buffer := make([]byte, 128)
			_, _ = conn.Read(buffer)
			if counted.accepted.Load() == 1 {
				if tcpConn, ok := conn.(*net.TCPConn); ok {
					_ = tcpConn.SetLinger(0)
				}
				_ = conn.Close()
				continue
			}
			_, _ = conn.Write([]byte("* NO STARTTLS rejected\r\n"))
			time.Sleep(50 * time.Millisecond)
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = listener.Close() })
	return listener.Addr().String(), func() int64 { return counted.accepted.Load() }
}

func startDroppedSTARTTLSHandshakeResponseServer(t *testing.T) (string, func() int64) {
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
			_, _ = conn.Write([]byte("* OK ready for STARTTLS\r\n"))
			buffer := make([]byte, 128)
			n, err := conn.Read(buffer)
			if err != nil {
				_ = conn.Close()
				continue
			}
			fields := strings.Fields(string(buffer[:n]))
			if len(fields) == 0 {
				_ = conn.Close()
				continue
			}
			_, _ = fmt.Fprintf(conn, "%s OK Begin TLS\r\n", fields[0])
			if tcpConn, ok := conn.(*net.TCPConn); ok {
				_ = tcpConn.SetLinger(0)
			}
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = listener.Close() })
	return listener.Addr().String(), func() int64 { return counted.accepted.Load() }
}

func startImplicitTLSServer(t *testing.T, handler func(net.Conn)) (string, func() int64) {
	t.Helper()
	certServer := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	certificate := certServer.TLS.Certificates[0]
	certServer.Close()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	tlsListener := tls.NewListener(listener, &tls.Config{
		Certificates: []tls.Certificate{certificate},
	})
	counted := &countedListener{Listener: tlsListener}
	go func() {
		for {
			conn, err := counted.Accept()
			if err != nil {
				return
			}
			go handler(conn)
		}
	}()
	t.Cleanup(func() {
		_ = tlsListener.Close()
	})
	return listener.Addr().String(), func() int64 { return counted.accepted.Load() }
}

func startSTARTTLSServer(t *testing.T, handler func(net.Conn)) (string, func() int64) {
	t.Helper()
	certServer := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	certificate := certServer.TLS.Certificates[0]
	certServer.Close()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	counted := &countedListener{Listener: listener}
	go func() {
		for {
			conn, err := counted.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				_, _ = conn.Write([]byte("* OK ready for STARTTLS\r\n"))
				buffer := make([]byte, 128)
				n, err := conn.Read(buffer)
				if err != nil {
					return
				}
				fields := strings.Fields(string(buffer[:n]))
				if len(fields) == 0 {
					return
				}
				if _, err := fmt.Fprintf(conn, "%s OK Begin TLS\r\n", fields[0]); err != nil {
					return
				}
				tlsConn := tls.Server(conn, &tls.Config{
					Certificates: []tls.Certificate{certificate},
				})
				if err := tlsConn.Handshake(); err != nil {
					return
				}
				handler(tlsConn)
			}(conn)
		}
	}()
	t.Cleanup(func() { _ = listener.Close() })
	return listener.Addr().String(), func() int64 { return counted.accepted.Load() }
}

func startDroppedTLSHandshakeServer(t *testing.T, connections int) (string, func() int64) {
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
			if tcpConn, ok := conn.(*net.TCPConn); ok {
				_ = tcpConn.SetLinger(0)
			}
			_ = conn.Close()
			if int(counted.accepted.Load()) >= connections {
				return
			}
		}
	}()
	t.Cleanup(func() { _ = listener.Close() })
	return listener.Addr().String(), func() int64 { return counted.accepted.Load() }
}

func startSilentTCPServer(t *testing.T) (string, func() int64) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	counted := &countedListener{Listener: listener}
	release := make(chan struct{})
	go func() {
		conn, err := counted.Accept()
		if err != nil {
			return
		}
		<-release
		_ = conn.Close()
	}()
	t.Cleanup(func() {
		close(release)
		_ = listener.Close()
	})
	return listener.Addr().String(), func() int64 { return counted.accepted.Load() }
}

func startSilentGreetingServer(t *testing.T) (string, func() int64) {
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
			buffer := make([]byte, 1)
			_, _ = conn.Read(buffer)
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = listener.Close() })
	return listener.Addr().String(), func() int64 { return counted.accepted.Load() }
}

func startSilentCapabilityServer(t *testing.T) (string, func() int64) {
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
			_, _ = io.WriteString(conn, "* OK ready\r\n")
			buffer := make([]byte, 128)
			_, _ = conn.Read(buffer)
			_, _ = conn.Read(buffer)
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = listener.Close() })
	return listener.Addr().String(), func() int64 { return counted.accepted.Load() }
}

func startSilentAuthenticationServer(t *testing.T) (string, func() int64) {
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
			_, _ = io.WriteString(conn, "* OK ready\r\n")
			buffer := make([]byte, 128)
			n, err := conn.Read(buffer)
			if err != nil {
				_ = conn.Close()
				continue
			}
			fields := strings.Fields(string(buffer[:n]))
			if len(fields) == 0 {
				_ = conn.Close()
				continue
			}
			_, _ = fmt.Fprintf(conn, "* CAPABILITY IMAP4rev1\r\n%s OK capabilities\r\n", fields[0])
			_, _ = conn.Read(buffer)
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = listener.Close() })
	return listener.Addr().String(), func() int64 { return counted.accepted.Load() }
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
