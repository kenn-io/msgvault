package daemonclient

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/clirun"
)

func threadsOAuthRunRequest() CLIRunRequest {
	return CLIRunRequest{Args: []string{"add-threads"}, Env: map[string]string{clirun.EnvThreadsOAuth: "synthetic-oauth-payload"}}
}

func TestThreadsOAuthRejectsRemotePlaintextBeforeTransmission(t *testing.T) {
	require := require.New(t)
	var received atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		received.Add(1)
		_, _ = fmt.Fprintln(w, `{"type":"complete"}`)
	}))
	defer server.Close()
	u, err := url.Parse(server.URL)
	require.NoError(err)
	// Dial the real test endpoint while retaining a remote host in the URL.
	// This proves client admission precedes transmission, without requiring DNS.
	transport := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, u.Host)
	}}
	defer transport.CloseIdleConnections()
	u.Host = "daemon.example:" + u.Port()
	c, err := New(Config{URL: u.String(), APIKey: "synthetic-owner-key", AllowInsecure: true, HTTPClient: &http.Client{Transport: transport}})
	require.NoError(err)
	require.Error(c.RunCLICommand(t.Context(), threadsOAuthRunRequest(), nil))
	assert.Zero(t, received.Load())
}

func TestThreadsOAuthRedirectsNeverReplayPayload(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			var received atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				received.Add(1)
				_, _ = fmt.Fprintln(w, `{"type":"complete"}`)
			}))
			defer target.Close()
			redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target.URL, status)
			}))
			defer redirect.Close()
			c, err := New(Config{URL: redirect.URL, AllowInsecure: true})
			require.NoError(err)
			require.Error(c.RunCLICommand(t.Context(), threadsOAuthRunRequest(), nil))
			assert.Zero(received.Load())
			// Guarding this one request must not mutate shared client policy.
			require.NoError(c.RunCLICommand(t.Context(), CLIRunRequest{Args: []string{"sync"}}, nil))
			assert.Equal(int32(1), received.Load())
		})
	}
}

func TestThreadsOAuthAcceptsLoopbackAndAuthenticatedTLS(t *testing.T) {
	for _, tls := range []bool{false, true} {
		t.Run(strconv.FormatBool(tls), func(t *testing.T) {
			var received atomic.Int32
			handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				received.Add(1)
				_, _ = fmt.Fprintln(w, `{"type":"complete"}`)
			})
			server := httptest.NewUnstartedServer(handler)
			if tls {
				server.StartTLS()
			} else {
				server.Start()
			}
			defer server.Close()
			c, err := New(Config{URL: server.URL, APIKey: "synthetic-owner-key", AllowInsecure: true, HTTPClient: server.Client()})
			require.NoError(t, err)
			require.NoError(t, c.RunCLICommand(t.Context(), threadsOAuthRunRequest(), nil))
			assert.Equal(t, int32(1), received.Load())
		})
	}
}
