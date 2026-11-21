//go:build darwin

package peoplesweep

import (
	"bufio"
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func dialCodexProxySession(t *testing.T, session CodexProxySession) net.Conn {
	t.Helper()
	endpoint, err := url.Parse(session.Endpoint())
	require.NoError(t, err)
	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", endpoint.Host)
	require.NoError(t, err)
	return conn
}

func codexProxySessionCredential(t *testing.T, session CodexProxySession) string {
	t.Helper()
	endpoint, err := url.Parse(session.Endpoint())
	require.NoError(t, err)
	password, ok := endpoint.User.Password()
	require.True(t, ok)
	return "Proxy-Authorization: Basic " +
		base64.StdEncoding.EncodeToString([]byte(endpoint.User.Username()+":"+password)) + "\r\n"
}

func assertCodexProxySessionClosed(t *testing.T, session CodexProxySession) {
	t.Helper()
	endpoint, err := url.Parse(session.Endpoint())
	require.NoError(t, err)
	_, err = (&net.Dialer{Timeout: time.Second}).DialContext(t.Context(), "tcp", endpoint.Host)
	assert.Error(t, err)
}

// TestCodexServiceProxyRequiresSessionCredential catches another local user
// relaying through the loopback proxy, which unlike the Linux socket is
// reachable by every process on the host.
func TestCodexServiceProxyRequiresSessionCredential(t *testing.T) {
	requireChecks := require.New(t)
	workRoot := t.TempDir()
	requireChecks.NoError(os.Chmod(workRoot, 0o700))
	var dials atomic.Int64
	proxy := newCodexHostServiceProxy(func(context.Context, string, string) (net.Conn, error) {
		dials.Add(1)
		client, server := net.Pipe()
		_ = server.Close()
		return client, nil
	})
	session, err := proxy.Attach(t.Context(), workRoot)
	requireChecks.NoError(err)
	t.Cleanup(func() { require.NoError(t, session.Close()) })
	endpoint, err := url.Parse(session.Endpoint())
	requireChecks.NoError(err)
	assert.Equal(t, "127.0.0.1", endpoint.Hostname())
	other := "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(codexProxyUser+":"+strings.Repeat("0", 64))) + "\r\n"
	valid := codexProxySessionCredential(t, session)
	for name, headers := range map[string]string{
		"missing":   "",
		"other":     other,
		"duplicate": valid + valid,
		"bearer":    strings.Replace(valid, "Basic", "Bearer", 1),
	} {
		t.Run(name, func(t *testing.T) {
			conn := dialCodexProxySession(t, session)
			defer func() { _ = conn.Close() }()
			require.NoError(t, conn.SetDeadline(time.Now().Add(2*time.Second)))
			_, err := io.WriteString(conn, "CONNECT chatgpt.com:443 HTTP/1.1\r\nHost: chatgpt.com:443\r\n"+headers+"\r\n")
			require.NoError(t, err)
			line, err := bufio.NewReader(conn).ReadString('\n')
			require.NoError(t, err)
			assert.True(t, strings.HasPrefix(line, "HTTP/1.1 403"), line)
		})
	}
	assert.Zero(t, dials.Load())
}
