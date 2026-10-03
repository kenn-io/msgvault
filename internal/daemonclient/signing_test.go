package daemonclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/requestsign"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestSigningAllNativeTransportPaths(t *testing.T) {
	requirements := require.New(t)

	secret := bytes.Repeat([]byte{0x52}, 64)
	var received atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := requestsign.Verify(r, "https://"+r.Host+r.URL.RequestURI(), secret, "reader-1", time.Now())
		if err != nil {
			http.Error(w, "signature invalid", http.StatusUnauthorized)
			return
		}
		received.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok","api_schema_version":"2.33.0"}`)
	}))
	defer server.Close()
	c, err := New(Config{URL: server.URL + "/msgvault", APIKey: "native-client-key", SigningKeyID: "reader-1", SigningSecret: secret, HTTPClient: server.Client()})
	requirements.NoError(err)
	typed, err := c.GeneratedClient()
	requirements.NoError(err)
	_, err = typed.GetHealthWithResponse(t.Context())
	requirements.NoError(err)
	for _, stream := range []bool{false, true} {
		var response *http.Response
		if stream {
			response, err = c.DoGeneratedStreamingRequestWithContext(t.Context(), http.MethodGet, "/api/v1/health", nil)
		} else {
			response, err = c.DoGeneratedRequestWithContext(t.Context(), http.MethodGet, "/api/v1/health", nil)
		}
		requirements.NoError(err)
		requirements.Equal(http.StatusOK, response.StatusCode)
		requirements.NoError(response.Body.Close())
	}
	assert.Equal(t, int64(3), received.Load())
}

func TestSignedNativeHTTP2RemainsEnabled(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	secret := bytes.Repeat([]byte{0x52}, 64)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, 2, r.ProtoMajor)
		_, err := requestsign.Verify(r, "https://"+r.Host+r.URL.RequestURI(), secret, "reader-1", time.Now())
		if !assert.NoError(t, err) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	c, err := New(Config{URL: server.URL, APIKey: "native-client-key", SigningKeyID: "reader-1", SigningSecret: secret, HTTPClient: server.Client()})
	requirements.NoError(err)
	response, err := c.DoGeneratedStreamingRequestWithContext(t.Context(), http.MethodGet, "/api/v1/health", nil)
	requirements.NoError(err)
	assertions.Equal(2, response.ProtoMajor)
	assertions.Equal(http.StatusOK, response.StatusCode)
	requirements.NoError(response.Body.Close())
}

func TestSignedClientRedirectAndConfiguration(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	secret := bytes.Repeat([]byte{0x52}, 64)
	var leaked atomic.Bool
	destination := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Store(true); w.WriteHeader(http.StatusOK) }))
	defer destination.Close()
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	c, err := New(Config{URL: origin.URL, APIKey: "native-client-key", SigningKeyID: "reader-1", SigningSecret: secret, HTTPClient: origin.Client()})
	require.NoError(t, err)
	response, err := c.DoGeneratedRequestWithContext(context.Background(), http.MethodGet, "/api/v1/health", nil)
	if response != nil {
		_ = response.Body.Close()
	}
	requirements.Error(err)
	assertions.False(leaked.Load())
	for _, cfg := range []Config{
		{URL: "http://archive.example.test", AllowInsecure: true, APIKey: "key", SigningKeyID: "reader-1", SigningSecret: secret},
		{URL: "https://archive.example.test", APIKey: "key", SigningKeyID: "reader-1"},
		{URL: "https://archive.example.test", APIKey: "key", SigningSecret: secret},
		{URL: "https://archive.example.test", AgentToken: "token", SigningKeyID: "reader-1", SigningSecret: secret},
		{URL: "https://archive.example.test?x=y", APIKey: "key", SigningKeyID: "reader-1", SigningSecret: secret},
	} {
		_, err := New(cfg)
		requirements.Error(err)
	}
}

func TestSignedClientRetriesFreshSignature(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	secret := bytes.Repeat([]byte{0x52}, 64)
	var mu sync.Mutex
	var inputs []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		inputs = append(inputs, r.Header.Get("Signature-Input"))
		attempt := len(inputs)
		mu.Unlock()
		if attempt == 1 {
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"error":"operation_in_progress","message":"fixture maintenance"}`)
			return
		}
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	}))
	defer server.Close()
	c, err := New(Config{URL: server.URL, APIKey: "native-client-key", SigningKeyID: "reader-1", SigningSecret: secret, HTTPClient: server.Client()})
	requirements.NoError(err)
	_, err = APIResponse(c, func(client *apiclient.Client) (*generated.GetHealthResp, error) {
		return client.GetHealthWithResponse(t.Context())
	})
	requirements.NoError(err)
	mu.Lock()
	defer mu.Unlock()
	requirements.Len(inputs, 2)
	assertions.NotEmpty(inputs[0])
	assertions.NotEqual(inputs[0], inputs[1])
}

func TestSignedTransportPinsBeforeNetworkDispatch(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	secret := bytes.Repeat([]byte{0x52}, 64)
	var received atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { received.Add(1); w.WriteHeader(http.StatusOK) }))
	defer server.Close()
	c, err := New(Config{URL: server.URL + "/msgvault", APIKey: "native-client-key", SigningKeyID: "reader-1", SigningSecret: secret, HTTPClient: server.Client()})
	requirements.NoError(err)
	for _, target := range []string{server.URL + "/outside", strings.Replace(server.URL, "https://", "http://", 1) + "/msgvault/api/v1/health", server.URL + "/msgvault-other/api/v1/health"} {
		r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
		requirements.NoError(err)
		r.Header.Set("X-Api-Key", "native-client-key")
		response, err := c.httpClient.Do(r)
		if response != nil {
			_ = response.Body.Close()
		}
		requirements.Error(err)
	}
	assertions.Zero(received.Load(), "pinning must reject before contacting any target")
}

func TestSignedGeneratedErrorsBoundedBeforeDecoding(t *testing.T) {
	secret := bytes.Repeat([]byte{0x52}, 64)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, strings.Repeat("x", 80<<10))
	}))
	defer server.Close()
	client, err := New(Config{URL: server.URL, APIKey: "native-client-key", SigningKeyID: "reader-1", SigningSecret: secret, HTTPClient: server.Client()})
	require.NoError(t, err)
	typed, err := client.GeneratedClient()
	require.NoError(t, err)
	_, err = typed.GetHealthWithResponse(t.Context())
	require.ErrorContains(t, err, "64 KiB")
}
func TestSignedClientDoesNotImplicitlyReplayAfterLostResponse(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	secret := bytes.Repeat([]byte{0x52}, 64)
	var received atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := requestsign.Verify(r, "https://"+r.Host+r.URL.RequestURI(), secret, "reader-1", time.Now())
		if !assert.NoError(t, err) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if received.Add(1) == 2 {
			hijacker, ok := w.(http.Hijacker)
			if !assert.True(t, ok) {
				return
			}
			conn, _, err := hijacker.Hijack()
			if !assert.NoError(t, err) {
				return
			}
			assert.NoError(t, conn.Close())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	}))
	defer server.Close()
	client, err := New(Config{URL: server.URL, APIKey: "native-client-key", SigningKeyID: "reader-1", SigningSecret: secret, HTTPClient: server.Client()})
	requirements.NoError(err)
	typed, err := client.GeneratedClient()
	requirements.NoError(err)
	_, err = typed.GetHealthWithResponse(t.Context())
	requirements.NoError(err)
	_, err = typed.GetHealthWithResponse(t.Context())
	requirements.Error(err, "lost signed response cannot cause the stdlib to replay the nonce")
	assertions.Equal(int64(2), received.Load())
	_, err = typed.GetHealthWithResponse(t.Context())
	requirements.NoError(err, "an explicit native request must remain usable after a lost response")
	assertions.Equal(int64(3), received.Load())
}

func TestSignedNativeRateRetriesFreshAndBounded(t *testing.T) {
	for _, path := range []string{"typed", "direct", "stream"} {
		t.Run(path, func(t *testing.T) {
			assertions := assert.New(t)

			requirements := require.New(t)

			secret := bytes.Repeat([]byte{0x52}, 64)
			var mu sync.Mutex
			var inputs []string
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				inputs = append(inputs, r.Header.Get("Signature-Input"))
				attempt := len(inputs)
				mu.Unlock()
				if attempt == 1 {
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = io.WriteString(w, `{"error":"remote_rate_limited","message":"fixture rate limit"}`)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"status":"ok"}`)
			}))
			defer server.Close()
			c, err := New(Config{URL: server.URL, APIKey: "native-client-key", SigningKeyID: "reader-1", SigningSecret: secret, HTTPClient: server.Client()})
			requirements.NoError(err)
			if path == "typed" {
				typed, err := c.GeneratedClient()
				requirements.NoError(err)
				response, err := typed.GetHealthWithResponse(t.Context())
				requirements.NoError(err)
				assertions.Equal(http.StatusOK, response.StatusCode)
			} else {
				call := c.DoGeneratedRequestWithContext
				if path == "stream" {
					call = c.DoGeneratedStreamingRequestWithContext
				}
				response, err := call(t.Context(), http.MethodGet, "/api/v1/health", nil)
				requirements.NoError(err)
				assertions.Equal(http.StatusOK, response.StatusCode)
				requirements.NoError(response.Body.Close())
			}
			mu.Lock()
			defer mu.Unlock()
			requirements.Len(inputs, 2)
			assertions.NotEqual(inputs[0], inputs[1], "a rejected attempt must get a fresh nonce")
		})
	}
}

func TestNativeRateRetryBudgetAndCancellation(t *testing.T) {
	for _, signed := range []bool{false, true} {
		t.Run(fmt.Sprintf("signed=%t", signed), func(t *testing.T) {
			assertions := assert.New(t)

			requirements := require.New(t)

			var attempts atomic.Int64
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				attempts.Add(1)
				w.WriteHeader(http.StatusTooManyRequests)
			}))
			defer server.Close()
			cfg := Config{URL: server.URL, APIKey: "native-client-key", HTTPClient: server.Client()}
			if signed {
				cfg.SigningKeyID = "reader-1"
				cfg.SigningSecret = bytes.Repeat([]byte{0x52}, 64)
			}
			c, err := New(cfg)
			requirements.NoError(err)
			response, err := c.DoGeneratedRequestWithContext(t.Context(), http.MethodGet, "/api/v1/health", nil)
			requirements.NoError(err)
			assertions.Equal(http.StatusTooManyRequests, response.StatusCode)
			requirements.NoError(response.Body.Close())
			want := int64(1)
			if signed {
				want = 3
			}
			assertions.Equal(want, attempts.Load())
		})
	}
	for _, root := range []bool{false, true} {
		t.Run(fmt.Sprintf("root cancellation=%t", root), func(t *testing.T) {
			admitted := make(chan struct{})
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusTooManyRequests)
				close(admitted)
			}))
			defer server.Close()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			cfg := Config{URL: server.URL, APIKey: "native-client-key", SigningKeyID: "reader-1", SigningSecret: bytes.Repeat([]byte{0x52}, 64), HTTPClient: server.Client()}
			requestContext := ctx
			if root {
				cfg.Context = ctx
				requestContext = t.Context()
			}
			c, err := New(cfg)
			require.NoError(t, err)
			done := make(chan error, 1)
			go func() {
				response, err := c.DoGeneratedRequestWithContext(requestContext, http.MethodGet, "/api/v1/health", nil)
				if response != nil {
					_ = response.Body.Close()
				}
				done <- err
			}()
			select {
			case <-admitted:
			case <-t.Context().Done():
				require.NoError(t, t.Context().Err())
			}
			cancel()
			require.ErrorIs(t, <-done, context.Canceled)
		})
	}
}

func TestSignedRateRetryRecreatesNativeMutationBody(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	secret := bytes.Repeat([]byte{0x52}, 64)
	var attempts atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wireBody, err := io.ReadAll(io.LimitReader(r.Body, 1024))
		if !assert.NoError(t, err) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(wireBody))
		_, err = requestsign.Verify(r, "https://"+r.Host+r.URL.RequestURI(), secret, "reader-1", time.Now())
		if !assert.NoError(t, err) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		defer func() { _ = r.Body.Close() }()
		var body generated.CreateCLICollectionBody
		assert.NoError(t, json.Unmarshal(wireBody, &body))
		assert.Equal(t, "fixture-collection", body.Name)
		assert.Equal(t, []string{"source@example.test"}, body.Accounts)
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	c, err := New(Config{URL: server.URL, APIKey: "native-client-key", SigningKeyID: "reader-1", SigningSecret: secret, HTTPClient: server.Client()})
	requirements.NoError(err)
	response, err := c.DoGeneratedStreamingRequestWithContext(t.Context(), http.MethodPost, "/api/v1/cli/collections", &generated.CreateCLICollectionRequestOptions{Body: &generated.CreateCLICollectionBody{Name: "fixture-collection", Accounts: []string{"source@example.test"}}})
	requirements.NoError(err)
	assertions.Equal(http.StatusOK, response.StatusCode)
	requirements.NoError(response.Body.Close())
	assertions.Equal(int64(2), attempts.Load())
}
