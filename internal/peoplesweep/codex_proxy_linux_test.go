//go:build linux

package peoplesweep

import (
	"bufio"
	"context"
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
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
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
		conn, err := net.Dial("unix", session.SocketPath())
		requireChecks.NoError(err)
		requireChecks.NoError(conn.SetDeadline(time.Now().Add(2 * time.Second)))
		_, err = io.WriteString(conn, request)
		requireChecks.NoError(err)
		line, err := bufio.NewReader(conn).ReadString('\n')
		requireChecks.NoError(err)
		assertChecks.True(strings.HasPrefix(line, "HTTP/1.1 403"), line)
		requireChecks.NoError(conn.Close())
	}
	assertChecks.Zero(dials.Load())

	for index, authority := range []string{"chatgpt.com:443", "auth.openai.com:443"} {
		conn, err := net.Dial("unix", session.SocketPath())
		requireChecks.NoError(err)
		requireChecks.NoError(conn.SetDeadline(time.Now().Add(2 * time.Second)))
		_, err = io.WriteString(conn, "CONNECT "+authority+" HTTP/1.1\r\nHost: "+authority+"\r\n\r\n")
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
	assertChecks.NoFileExists(session.SocketPath())
}
