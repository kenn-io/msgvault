//go:build linux || darwin

package peoplesweep

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCodexServiceProxyAllowsOnlyPinnedConnectAuthorities(t *testing.T) {
	assertChecks := assert.New(t)
	requireChecks := require.New(t)
	workRoot := t.TempDir()
	requireChecks.NoError(os.Chmod(workRoot, 0o700))
	upstream, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	requireChecks.NoError(err)
	t.Cleanup(func() { require.NoError(t, upstream.Close()) })
	var dials atomic.Int64
	proxy := newCodexHostServiceProxy(func(ctx context.Context, network, address string) (net.Conn, error) {
		dials.Add(1)
		assert.Equal(t, "tcp", network)
		assert.Contains(t, codexProxyAuthorities, address)
		return (&net.Dialer{}).DialContext(ctx, "tcp", upstream.Addr().String())
	})
	session, err := proxy.Attach(t.Context(), workRoot)
	requireChecks.NoError(err)
	t.Cleanup(func() { require.NoError(t, session.Close()) })

	for _, request := range []string{
		"CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n",
		"CONNECT 127.0.0.1:443 HTTP/1.1\r\nHost: 127.0.0.1:443\r\n\r\n",
		"CONNECT chatgpt.com:444 HTTP/1.1\r\nHost: chatgpt.com:444\r\n\r\n",
		"CONNECT chatgpt.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n",
		"GET http://chatgpt.com/ HTTP/1.1\r\nHost: chatgpt.com\r\n\r\n",
	} {
		conn := dialCodexProxySession(t, session)
		requireChecks.NoError(conn.SetDeadline(time.Now().Add(2 * time.Second)))
		_, err = io.WriteString(conn, strings.Replace(request, "\r\n\r\n", "\r\n"+codexProxySessionCredential(t, session)+"\r\n", 1))
		requireChecks.NoError(err)
		line, err := bufio.NewReader(conn).ReadString('\n')
		requireChecks.NoError(err)
		assertChecks.True(strings.HasPrefix(line, "HTTP/1.1 403"), line)
		requireChecks.NoError(conn.Close())
	}
	assertChecks.Zero(dials.Load())

	for index, authority := range []string{"chatgpt.com:443", "auth.openai.com:443"} {
		conn := dialCodexProxySession(t, session)
		requireChecks.NoError(conn.SetDeadline(time.Now().Add(2 * time.Second)))
		_, err = io.WriteString(conn, "CONNECT "+authority+" HTTP/1.1\r\nHost: "+authority+"\r\n"+codexProxySessionCredential(t, session)+"\r\n")
		requireChecks.NoError(err)
		tcpUpstream, ok := upstream.(*net.TCPListener)
		requireChecks.True(ok)
		requireChecks.NoError(tcpUpstream.SetDeadline(time.Now().Add(2 * time.Second)))
		upstreamConn, err := upstream.Accept()
		requireChecks.NoError(err)
		requireChecks.NoError(upstreamConn.SetDeadline(time.Now().Add(2 * time.Second)))
		reader := bufio.NewReader(conn)
		line, err := reader.ReadString('\n')
		requireChecks.NoError(err)
		assertChecks.Equal("HTTP/1.1 200 Connection Established\r\n", line)
		line, err = reader.ReadString('\n')
		requireChecks.NoError(err)
		assertChecks.Equal("\r\n", line)
		_, err = io.WriteString(conn, "synthetic request")
		requireChecks.NoError(err)
		request := make([]byte, len("synthetic request"))
		_, err = io.ReadFull(upstreamConn, request)
		requireChecks.NoError(err)
		assertChecks.Equal("synthetic request", string(request))
		_, err = io.WriteString(upstreamConn, "synthetic response")
		requireChecks.NoError(err)
		response := make([]byte, len("synthetic response"))
		_, err = io.ReadFull(reader, response)
		requireChecks.NoError(err)
		assertChecks.Equal("synthetic response", string(response))
		assertChecks.Equal(int64(index+1), dials.Load())
		requireChecks.NoError(conn.Close())
		requireChecks.NoError(upstreamConn.Close())
	}
	requireChecks.NoError(session.Close())
	assertCodexProxySessionClosed(t, session)
}

// TestCodexServiceProxyHoldsListenerUntilClose catches cancellation freeing
// the child's only permitted destination while the child may still run. On
// macOS another process could then bind that port and receive the child's
// connections; the launcher calls Close only after the child exits.
func TestCodexServiceProxyHoldsListenerUntilClose(t *testing.T) {
	requireChecks := require.New(t)
	workRoot := t.TempDir()
	requireChecks.NoError(os.Chmod(workRoot, 0o700))
	upstream, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	requireChecks.NoError(err)
	t.Cleanup(func() { require.NoError(t, upstream.Close()) })
	proxy := newCodexHostServiceProxy(func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", upstream.Addr().String())
	})
	ctx, cancel := context.WithCancel(t.Context())
	session, err := proxy.Attach(ctx, workRoot)
	requireChecks.NoError(err)

	tunnel := dialCodexProxySession(t, session)
	defer func() { _ = tunnel.Close() }()
	requireChecks.NoError(tunnel.SetDeadline(time.Now().Add(2 * time.Second)))
	_, err = io.WriteString(tunnel, "CONNECT chatgpt.com:443 HTTP/1.1\r\nHost: chatgpt.com:443\r\n"+codexProxySessionCredential(t, session)+"\r\n")
	requireChecks.NoError(err)
	tunnelReader := bufio.NewReader(tunnel)
	line, err := tunnelReader.ReadString('\n')
	requireChecks.NoError(err)
	requireChecks.Equal("HTTP/1.1 200 Connection Established\r\n", line)

	cancel()
	// The open tunnel ends, and the listener answers no new CONNECT.
	_, err = io.ReadAll(tunnelReader)
	requireChecks.NoError(err)
	late := dialCodexProxySession(t, session)
	defer func() { _ = late.Close() }()
	requireChecks.NoError(late.SetDeadline(time.Now().Add(2 * time.Second)))
	_, _ = io.WriteString(late, "CONNECT chatgpt.com:443 HTTP/1.1\r\nHost: chatgpt.com:443\r\n"+codexProxySessionCredential(t, session)+"\r\n")
	// The proxy closes it unanswered; a reset is as good as an EOF.
	response, err := io.ReadAll(late)
	var netErr net.Error
	requireChecks.False(errors.As(err, &netErr) && netErr.Timeout(), "the late connection was left open")
	assert.Empty(t, string(response))
	assertCodexProxySessionHeld(t, session)

	requireChecks.NoError(session.Close())
	assertCodexProxySessionClosed(t, session)
}

// TestCodexServiceProxyBoundsPendingHandshakes catches unauthenticated local
// connections each holding a goroutine and descriptor without limit.
func TestCodexServiceProxyBoundsPendingHandshakes(t *testing.T) {
	requireChecks := require.New(t)
	workRoot := t.TempDir()
	requireChecks.NoError(os.Chmod(workRoot, 0o700))
	proxy := newCodexHostServiceProxy(func(context.Context, string, string) (net.Conn, error) {
		client, server := net.Pipe()
		_ = server.Close()
		return client, nil
	})
	session, err := proxy.Attach(t.Context(), workRoot)
	requireChecks.NoError(err)
	t.Cleanup(func() { require.NoError(t, session.Close()) })
	idle := make([]net.Conn, 0, maxCodexProxyHandshakes)
	for range maxCodexProxyHandshakes {
		idle = append(idle, dialCodexProxySession(t, session))
	}
	// Once every slot holds an idle connection, the next one is closed unread.
	requireChecks.Eventually(func() bool {
		extra := dialCodexProxySession(t, session)
		defer func() { _ = extra.Close() }()
		_ = extra.SetReadDeadline(time.Now().Add(200 * time.Millisecond)) //nolint:kennlint // deadlinetest: bounds one read of a connection the proxy should already have closed
		_, err := extra.Read(make([]byte, 1))
		var netErr net.Error
		return err != nil && (!errors.As(err, &netErr) || !netErr.Timeout())
	}, 2*time.Second, 20*time.Millisecond)
	for _, conn := range idle {
		requireChecks.NoError(conn.Close())
	}
	// Freed slots admit a CONNECT again.
	requireChecks.Eventually(func() bool {
		conn := dialCodexProxySession(t, session)
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		if _, err := io.WriteString(conn, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n"+codexProxySessionCredential(t, session)+"\r\n"); err != nil {
			return false
		}
		line, err := bufio.NewReader(conn).ReadString('\n')
		return err == nil && strings.HasPrefix(line, "HTTP/1.1 403")
	}, 2*time.Second, 20*time.Millisecond)
}
