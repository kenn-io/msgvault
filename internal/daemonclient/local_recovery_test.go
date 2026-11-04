package daemonclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	client.EnableLocalDaemonRecovery(func(context.Context) (LocalDaemonEndpoint, error) {
		resolutions++
		return LocalDaemonEndpoint{URL: target.URL}, nil
	})
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

func TestLocalDaemonRecoveryMessageURLsUseCurrentEndpoint(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	replacement := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(replacement.Close)
	client, err := New(Config{URL: closedLocalDaemonURL(t), AllowInsecure: true})
	requirements.NoError(err)
	client.EnableLocalDaemonRecovery(func(context.Context) (LocalDaemonEndpoint, error) {
		return LocalDaemonEndpoint{URL: replacement.URL}, nil
	})
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, client.BaseURL()+"/api/v1/health", nil)
	requirements.NoError(err)
	response, err := client.httpClient.Do(request)
	requirements.NoError(err)
	assertions.Equal(http.StatusNoContent, response.StatusCode)
	requirements.NoError(response.Body.Close())

	assertions.Equal(replacement.URL+"/messages/42", client.messageWebURL(context.Background(), 42))
}

func TestLocalDaemonRecoveryReplaysBodyAndRefreshesCredentials(t *testing.T) {
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
		assert.Equal(t, "synthetic-trace", r.Header.Get("X-Trace"))
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		assert.JSONEq(t, `{"value":"synthetic"}`, string(body))
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(target.Close)
	client, err := New(Config{URL: closedLocalDaemonURL(t), AllowInsecure: true})
	requirements.NoError(err)
	client.EnableLocalDaemonRecovery(func(context.Context) (LocalDaemonEndpoint, error) {
		return LocalDaemonEndpoint{URL: target.URL, APIKey: "replacement-key"}, nil
	})
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, client.BaseURL()+"/api/v1/example?cursor=next", strings.NewReader(`{"value":"synthetic"}`))
	requirements.NoError(err)
	request.Header.Set("X-Api-Key", "old-key")
	request.Header.Set("X-Trace", "synthetic-trace")
	response, err := client.httpClient.Do(request)
	requirements.NoError(err)
	assertions.Equal(strings.TrimPrefix(target.URL, "http://"), response.Request.Host)
	requirements.NoError(response.Body.Close())
	assertions.Equal(int32(1), calls.Load())
	assertions.Equal(strings.TrimPrefix(client.BaseURL(), "http://"), request.Host, "the caller's request is immutable")
}

func TestLocalDaemonRecoveryFailureIsBoundedAndLaterCallsCanRecover(t *testing.T) {
	for _, resolverFailure := range []bool{false, true} {
		t.Run(strconv.FormatBool(resolverFailure), func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			dead := closedLocalDaemonURL(t)
			target := dead
			var resolutions int
			client, err := New(Config{URL: dead, AllowInsecure: true})
			requirements.NoError(err)
			client.EnableLocalDaemonRecovery(func(context.Context) (LocalDaemonEndpoint, error) {
				resolutions++
				if resolverFailure && resolutions == 1 {
					return LocalDaemonEndpoint{}, errors.New("synthetic discovery failure")
				}
				return LocalDaemonEndpoint{URL: target}, nil
			})
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
		t.Run(strconv.FormatBool(afterRecovery), func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, err := io.ReadAll(r.Body)
				assert.NoError(t, err)
				calls.Add(1)
				hijacker, ok := w.(http.Hijacker)
				if !ok {
					assert.Fail(t, "response writer does not support hijacking")
					return
				}
				conn, _, err := hijacker.Hijack()
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
			client.EnableLocalDaemonRecovery(func(context.Context) (LocalDaemonEndpoint, error) {
				resolutions++
				return LocalDaemonEndpoint{URL: server.URL}, nil
			})
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, initial+"/write", strings.NewReader("write once"))
			requirements.NoError(err)
			response, err := client.httpClient.Do(request)
			if response != nil {
				requirements.NoError(response.Body.Close())
			}
			requirements.Error(err)
			var unavailable *LocalDaemonUnavailableError
			assertions.NotErrorAs(err, &unavailable, "an accepted write must not receive retry advice")
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
	assertions := assert.New(t)
	requirements := require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	t.Cleanup(server.Close)
	client, err := New(Config{URL: server.URL, AllowInsecure: true})
	requirements.NoError(err)
	client.EnableLocalDaemonRecovery(func(context.Context) (LocalDaemonEndpoint, error) {
		require.Fail(t, "HTTP errors must not trigger discovery")
		return LocalDaemonEndpoint{}, errors.New("unexpected discovery")
	})
	_, err = client.GetHealth(t.Context())
	requirements.Error(err)
	var unavailable *LocalDaemonUnavailableError
	assertions.NotErrorAs(err, &unavailable)
}

func TestLocalDaemonRecoveryConcurrentCallsShareResolution(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	const callers = 8
	dead := closedLocalDaemonURL(t)
	dialing := make(chan struct{}, callers)
	release := make(chan struct{})
	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	requirements.True(ok)
	transport := defaultTransport.Clone()
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
	client.EnableLocalDaemonRecovery(func(context.Context) (LocalDaemonEndpoint, error) {
		resolutions.Add(1)
		return LocalDaemonEndpoint{URL: server.URL}, nil
	})
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
	requirements := require.New(t)
	client, err := New(Config{URL: closedLocalDaemonURL(t), AllowInsecure: true})
	requirements.NoError(err)
	entered, release := make(chan struct{}), make(chan struct{})
	client.EnableLocalDaemonRecovery(func(ctx context.Context) (LocalDaemonEndpoint, error) {
		close(entered)
		select {
		case <-release:
			return LocalDaemonEndpoint{}, errors.New("synthetic failure")
		case <-ctx.Done():
			return LocalDaemonEndpoint{}, ctx.Err()
		}
	})
	done := make(chan error, 1)
	go func() { _, err := client.GetHealth(t.Context()); done <- err }()
	<-entered
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	// Exercise the wait behind the active resolver directly: the HTTP client
	// itself would reject an already-canceled request before it reaches it.
	transport, ok := client.httpClient.Transport.(*localDaemonRecovery)
	requirements.True(ok)
	_, err = transport.reconnect(ctx, 0)
	requirements.ErrorIs(err, context.Canceled)
	close(release)
	<-done
}

func TestLocalDaemonRecoveryLeavesOtherOriginsAlone(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	client, err := New(Config{URL: closedLocalDaemonURL(t), APIKey: "private-key", AllowInsecure: true})
	requirements.NoError(err)
	client.EnableLocalDaemonRecovery(func(context.Context) (LocalDaemonEndpoint, error) {
		require.Fail(t, "outside origins must not trigger local discovery")
		return LocalDaemonEndpoint{}, errors.New("unexpected discovery")
	})
	response, err := client.httpClient.Get(server.URL + "/outside")
	requirements.NoError(err)
	assertions.Equal(http.StatusNoContent, response.StatusCode)
	requirements.NoError(response.Body.Close())
}

func TestLocalDaemonRecoveryRootCancellationStopsDiscovery(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	root, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	client, err := New(Config{URL: closedLocalDaemonURL(t), AllowInsecure: true, Context: root})
	requirements.NoError(err)
	entered := make(chan struct{})
	client.EnableLocalDaemonRecovery(func(ctx context.Context) (LocalDaemonEndpoint, error) {
		close(entered)
		<-ctx.Done()
		return LocalDaemonEndpoint{}, ctx.Err()
	})
	done := make(chan error, 1)
	go func() { _, err := client.GetHealth(context.Background()); done <- err }()
	<-entered
	cancel()
	assertions.ErrorIs(<-done, context.Canceled)
}

func TestSafeMCPErrorPreservesOnlyLocalRecoveryAdvice(t *testing.T) {
	assertions := assert.New(t)
	local := fmt.Errorf("operation: %w", localDaemonUnavailable(errors.New("private-path /synthetic/archive private-key-value")))
	assertions.Equal("daemon_unavailable: "+LocalDaemonUnavailableMessage, SafeMCPError(local).Error())
	remote := &APIError{Status: 503, Code: "daemon_unavailable", Message: "remote diagnostic"}
	assertions.Equal("daemon request failed (503)", SafeMCPError(remote).Error())
}
