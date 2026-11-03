package daemonclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func closedLocalDaemonURL(t *testing.T) string {
	t.Helper()
	requirements := require.New(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	requirements.NoError(err)
	address := "http://" + listener.Addr().String()
	requirements.NoError(listener.Close())
	return address
}

func TestLocalDaemonRecoveryFollowsSuccessiveRestarts(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	var resolutions int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"recovered"}`)
	}))
	t.Cleanup(target.Close)
	client, err := New(Config{URL: closedLocalDaemonURL(t), AllowInsecure: true})
	requirements.NoError(err)
	requirements.NoError(client.EnableLocalDaemonRecovery(func(context.Context) (LocalDaemonEndpoint, error) {
		resolutions++
		return LocalDaemonEndpoint{URL: target.URL}, nil
	}))
	// Exercise the generated client, then the raw generated-request path. Neither
	// may retain its original URL after the transport has found the replacement.
	health, err := client.GetHealth(t.Context())
	requirements.NoError(err)
	assertions.Equal("recovered", health.Status)
	resp, err := client.DoGeneratedRequestWithContext(t.Context(), http.MethodGet, "/health", &generated.RunCLIRequestOptions{})
	requirements.NoError(err)
	requirements.NoError(resp.Body.Close())
	assertions.Equal(1, resolutions)
	target.Close()
	target = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"restarted"}`)
	}))
	t.Cleanup(target.Close)
	health, err = client.GetHealth(t.Context())
	requirements.NoError(err)
	assertions.Equal("restarted", health.Status)
	assertions.Equal(2, resolutions)
}

func TestLocalDaemonRecoveryReplaysBodyAndRefreshesCredentials(t *testing.T) {
	for _, decision := range []bool{false, true} {
		t.Run(fmt.Sprint(decision), func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			var calls atomic.Int32
			var target *httptest.Server
			target = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				assert.Equal(t, strings.TrimPrefix(target.URL, "http://"), r.Host)
				assert.Equal(t, http.MethodPost, r.Method)
				assert.Equal(t, "/api/v1/example?cursor=next", r.URL.RequestURI())
				assert.Equal(t, "replacement-key", r.Header.Get("X-Api-Key"))
				if decision {
					assert.Equal(t, "replacement-token", r.Header.Get(apiprotocol.DaemonRuntimeTokenHeader))
				} else {
					assert.Empty(t, r.Header.Get(apiprotocol.DaemonRuntimeTokenHeader))
				}
				assert.Equal(t, "synthetic-trace", r.Header.Get("X-Trace"))
				body, err := io.ReadAll(r.Body)
				assert.NoError(t, err)
				assert.Equal(t, `{"value":"synthetic"}`, string(body))
				w.WriteHeader(http.StatusNoContent)
			}))
			t.Cleanup(target.Close)
			client, err := New(Config{URL: closedLocalDaemonURL(t), AllowInsecure: true})
			requirements.NoError(err)
			requirements.NoError(client.EnableLocalDaemonRecovery(func(context.Context) (LocalDaemonEndpoint, error) {
				return LocalDaemonEndpoint{URL: target.URL, APIKey: "replacement-key", LocalDaemonToken: "replacement-token"}, nil
			}))
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, client.BaseURL()+"/api/v1/example?cursor=next", strings.NewReader(`{"value":"synthetic"}`))
			requirements.NoError(err)
			request.Header.Set("X-Api-Key", "old-key")
			request.Header.Set("X-Trace", "synthetic-trace")
			if decision {
				request.Header.Set(apiprotocol.DaemonRuntimeTokenHeader, "old-token")
			}
			response, err := client.httpClient.Do(request)
			requirements.NoError(err)
			assertions.Equal(strings.TrimPrefix(target.URL, "http://"), response.Request.Host)
			requirements.NoError(response.Body.Close())
			assertions.Equal(int32(1), calls.Load())
			assertions.Equal(strings.TrimPrefix(client.BaseURL(), "http://"), request.Host, "the caller's request is immutable")
		})
	}
}

func TestLocalDaemonRecoveryFailureIsBoundedAndLaterCallsCanRecover(t *testing.T) {
	for _, resolverFailure := range []bool{false, true} {
		t.Run(fmt.Sprint(resolverFailure), func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			dead := closedLocalDaemonURL(t)
			target := dead
			var resolutions int
			client, err := New(Config{URL: dead, AllowInsecure: true})
			requirements.NoError(err)
			requirements.NoError(client.EnableLocalDaemonRecovery(func(context.Context) (LocalDaemonEndpoint, error) {
				resolutions++
				if resolverFailure && resolutions == 1 {
					return LocalDaemonEndpoint{}, errors.New("synthetic discovery failure")
				}
				return LocalDaemonEndpoint{URL: target}, nil
			}))
			_, err = client.GetHealth(t.Context())
			var unavailable *LocalDaemonUnavailableError
			requirements.ErrorAs(err, &unavailable)
			assertions.Equal(1, resolutions, "no retry loop when recovery fails")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"status":"ok"}`) }))
			t.Cleanup(server.Close)
			target = server.URL
			health, err := client.GetHealth(t.Context())
			requirements.NoError(err)
			assertions.Equal("ok", health.Status)
			assertions.Equal(2, resolutions)
		})
	}
}

func TestLocalDaemonRecoveryDoesNotReplayAmbiguousFailures(t *testing.T) {
	for _, afterRecovery := range []bool{false, true} {
		t.Run(fmt.Sprint(afterRecovery), func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, err := io.ReadAll(r.Body)
				assert.NoError(t, err)
				calls.Add(1)
				conn, _, err := w.(http.Hijacker).Hijack()
				assert.NoError(t, err)
				if conn != nil {
					_ = conn.Close()
				}
			}))
			t.Cleanup(server.Close)
			initial := server.URL
			if afterRecovery {
				initial = closedLocalDaemonURL(t)
			}
			client, err := New(Config{URL: initial, AllowInsecure: true})
			requirements.NoError(err)
			var resolutions int
			requirements.NoError(client.EnableLocalDaemonRecovery(func(context.Context) (LocalDaemonEndpoint, error) {
				resolutions++
				return LocalDaemonEndpoint{URL: server.URL}, nil
			}))
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, initial+"/write", strings.NewReader("write once"))
			requirements.NoError(err)
			_, err = client.httpClient.Do(request)
			requirements.Error(err)
			var unavailable *LocalDaemonUnavailableError
			assertions.False(errors.As(err, &unavailable), "an accepted write must not receive retry advice")
			assertions.Equal(int32(1), calls.Load())
			if afterRecovery {
				assertions.Equal(1, resolutions)
			} else {
				assertions.Zero(resolutions)
			}
		})
	}
}

func TestLocalDaemonRecoveryDoesNotRetryHTTPFailures(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusInternalServerError} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
			t.Cleanup(server.Close)
			client, err := New(Config{URL: server.URL, AllowInsecure: true})
			requirements.NoError(err)
			requirements.NoError(client.EnableLocalDaemonRecovery(func(context.Context) (LocalDaemonEndpoint, error) {
				require.Fail(t, "HTTP errors must not trigger discovery")
				return LocalDaemonEndpoint{}, errors.New("unexpected discovery")
			}))
			_, err = client.GetHealth(t.Context())
			requirements.Error(err)
			var unavailable *LocalDaemonUnavailableError
			assertions.False(errors.As(err, &unavailable))
		})
	}
}

func TestLocalDaemonRecoveryCanceledRequestDoesNotResolve(t *testing.T) {
	requirements := require.New(t)
	client, err := New(Config{URL: closedLocalDaemonURL(t), AllowInsecure: true})
	requirements.NoError(err)
	requirements.NoError(client.EnableLocalDaemonRecovery(func(context.Context) (LocalDaemonEndpoint, error) {
		require.Fail(t, "canceled calls must not start a daemon")
		return LocalDaemonEndpoint{}, errors.New("unexpected discovery")
	}))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = client.GetHealth(ctx)
	requirements.ErrorIs(err, context.Canceled)
}

func TestLocalDaemonRecoveryConcurrentCallsShareResolution(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	const callers = 8
	dead := closedLocalDaemonURL(t)
	dialing := make(chan struct{}, callers)
	release := make(chan struct{})
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if "http://"+address == dead {
			dialing <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	t.Cleanup(transport.CloseIdleConnections)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"status":"ok"}`) }))
	t.Cleanup(server.Close)
	client, err := New(Config{URL: dead, AllowInsecure: true, HTTPClient: &http.Client{Transport: transport}})
	requirements.NoError(err)
	var resolutions atomic.Int32
	requirements.NoError(client.EnableLocalDaemonRecovery(func(context.Context) (LocalDaemonEndpoint, error) {
		resolutions.Add(1)
		return LocalDaemonEndpoint{URL: server.URL}, nil
	}))
	var workers sync.WaitGroup
	for range callers {
		workers.Go(func() {
			health, err := client.GetHealth(t.Context())
			if assert.NoError(t, err) {
				assert.Equal(t, "ok", health.Status)
			}
		})
	}
	for range callers {
		select {
		case <-dialing:
		case <-t.Context().Done():
			requirements.FailNow("calls did not dial")
		}
	}
	close(release)
	workers.Wait()
	assertions.Equal(int32(1), resolutions.Load())
}

func TestLocalDaemonRecoveryWaiterCanCancel(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	client, err := New(Config{URL: closedLocalDaemonURL(t), AllowInsecure: true})
	requirements.NoError(err)
	entered, release := make(chan struct{}), make(chan struct{})
	requirements.NoError(client.EnableLocalDaemonRecovery(func(ctx context.Context) (LocalDaemonEndpoint, error) {
		close(entered)
		select {
		case <-release:
			return LocalDaemonEndpoint{}, errors.New("synthetic failure")
		case <-ctx.Done():
			return LocalDaemonEndpoint{}, ctx.Err()
		}
	}))
	done := make(chan error, 1)
	go func() { _, err := client.GetHealth(t.Context()); done <- err }()
	<-entered
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	// Exercise the wait behind the active resolver directly: the HTTP client
	// itself would reject an already-canceled request before it reaches it.
	_, err = client.httpClient.Transport.(*localDaemonRecovery).reconnect(ctx, 0)
	assertions.ErrorIs(err, context.Canceled)
	close(release)
	<-done
}

func TestLocalDaemonRecoveryLeavesOtherOriginsAlone(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.Header.Get("X-Api-Key"))
		assert.Empty(t, r.Header.Get(apiprotocol.DaemonRuntimeTokenHeader))
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	client, err := New(Config{URL: closedLocalDaemonURL(t), APIKey: "private-key", AllowInsecure: true})
	requirements.NoError(err)
	requirements.NoError(client.EnableLocalDaemonRecovery(func(context.Context) (LocalDaemonEndpoint, error) {
		require.Fail(t, "outside origins must not trigger local discovery")
		return LocalDaemonEndpoint{}, errors.New("unexpected discovery")
	}))
	response, err := client.httpClient.Get(server.URL + "/outside")
	requirements.NoError(err)
	assertions.Equal(http.StatusNoContent, response.StatusCode)
	requirements.NoError(response.Body.Close())
}

func TestLocalDaemonRecoveryDoesNotReplayStreamingBodies(t *testing.T) {
	requirements := require.New(t)
	client, err := New(Config{URL: closedLocalDaemonURL(t), AllowInsecure: true})
	requirements.NoError(err)
	requirements.NoError(client.EnableLocalDaemonRecovery(func(context.Context) (LocalDaemonEndpoint, error) {
		assert.Fail(t, "a streaming request cannot be replayed")
		return LocalDaemonEndpoint{}, errors.New("unexpected discovery")
	}))
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, client.BaseURL()+"/stream", io.NopCloser(strings.NewReader("stream")))
	requirements.NoError(err)
	requirements.Nil(request.GetBody)
	_, err = client.httpClient.Do(request)
	var unavailable *LocalDaemonUnavailableError
	requirements.ErrorAs(err, &unavailable)
}

func TestLocalDaemonRecoveryValidatesDiscoveredOrigin(t *testing.T) {
	requirements := require.New(t)
	for _, endpoint := range []string{"https://127.0.0.1:8080", "http://user@127.0.0.1:8080", "http://127.0.0.1:8080/path", "http://127.0.0.1:8080?token=secret", "http://127.0.0.1:8080#fragment", "http://127.0.0.1:8080?"} {
		t.Run(endpoint, func(t *testing.T) {
			requirements := require.New(t)
			client, err := New(Config{URL: closedLocalDaemonURL(t), AllowInsecure: true})
			requirements.NoError(err)
			requirements.NoError(client.EnableLocalDaemonRecovery(func(context.Context) (LocalDaemonEndpoint, error) { return LocalDaemonEndpoint{URL: endpoint}, nil }))
			_, err = client.GetHealth(t.Context())
			var unavailable *LocalDaemonUnavailableError
			requirements.ErrorAs(err, &unavailable)
		})
	}
	// Runtime discovery supports non-loopback bind addresses. Enabling recovery
	// must not turn that existing local configuration into a startup error.
	client, err := New(Config{URL: "http://192.0.2.1:8080", AllowInsecure: true})
	requirements.NoError(err)
	requirements.NoError(client.EnableLocalDaemonRecovery(func(context.Context) (LocalDaemonEndpoint, error) { return LocalDaemonEndpoint{}, errors.New("unused") }))
}

func TestLocalDaemonRecoveryRootCancellationStopsDiscovery(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	root, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	client, err := New(Config{URL: closedLocalDaemonURL(t), AllowInsecure: true, Context: root})
	requirements.NoError(err)
	entered := make(chan struct{})
	requirements.NoError(client.EnableLocalDaemonRecovery(func(ctx context.Context) (LocalDaemonEndpoint, error) {
		close(entered)
		<-ctx.Done()
		return LocalDaemonEndpoint{}, ctx.Err()
	}))
	done := make(chan error, 1)
	go func() { _, err := client.GetHealth(context.Background()); done <- err }()
	<-entered
	cancel()
	assertions.ErrorIs(<-done, context.Canceled)
}

func TestSafeMCPErrorPreservesOnlyLocalRecoveryAdvice(t *testing.T) {
	assertions := assert.New(t)
	local := fmt.Errorf("operation: %w", &LocalDaemonUnavailableError{Err: errors.New("private-path /synthetic/archive private-key-value")})
	assertions.Equal("daemon_unavailable: "+LocalDaemonUnavailableMessage, SafeMCPError(local).Error())
	remote := &APIError{Status: 503, Code: "daemon_unavailable", Message: "remote diagnostic"}
	assertions.Equal("daemon request failed (503)", SafeMCPError(remote).Error())
}

func FuzzSafeMCPErrorLocalRecovery(f *testing.F) {
	f.Add("synthetic private diagnostics")
	f.Add("http://127.0.0.1:1234 /synthetic/archive")
	f.Fuzz(func(t *testing.T, diagnostic string) {
		assertions := assert.New(t)
		wrapped := fmt.Errorf("backend: %w", &LocalDaemonUnavailableError{Err: errors.New(diagnostic)})
		assertions.Equal("daemon_unavailable: "+LocalDaemonUnavailableMessage, SafeMCPError(wrapped).Error())
	})
}
