package msmail

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/msgraph"
)

func TestMicrosoftInboxTransportRejectsRedirects(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPatch} {
		for _, status := range []int{301, 302, 303, 307, 308} {
			t.Run(method+http.StatusText(status), func(t *testing.T) {
				var redirects, attempts atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/redirected" {
						redirects.Add(1)
						_, _ = w.Write([]byte(`{}`))
						return
					}
					attempts.Add(1)
					w.Header().Set("Location", "/redirected")
					w.WriteHeader(status)
				}))
				defer srv.Close()
				c := NewClient(srv.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 10000)
				var err error
				if method == http.MethodGet {
					var out map[string]any
					err = c.GetJSONOnce(t.Context(), "/metadata", &out)
				} else {
					_, err = c.SendNoRetry(t.Context(), method, "/metadata", map[string]any{"categories": []string{"Next"}}, `W/"v1"`)
				}
				require.Error(t, err)
				assert.Equal(t, int32(1), attempts.Load())
				assert.Zero(t, redirects.Load())
			})
		}
	}
}

func TestMicrosoftInboxTransportBoundsSuccessAndErrorBodies(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPatch} {
		for _, status := range []int{200, 400, 500} {
			for _, chunked := range []bool{false, true} {
				t.Run(method+http.StatusText(status)+map[bool]string{false: " length", true: " stream"}[chunked], func(t *testing.T) {
					assertions := assert.New(t)
					var attempts atomic.Int32
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						attempts.Add(1)
						if !chunked {
							w.Header().Set("Content-Length", "2097152")
						}
						w.WriteHeader(status)
						if chunked {
							flusher, ok := w.(http.Flusher)
							require.True(t, ok)
							flusher.Flush()
						}
						_, _ = w.Write([]byte(strings.Repeat("x", 2<<20)))
					}))
					defer srv.Close()
					c := NewClient(srv.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 10000)
					var err error
					if method == http.MethodGet {
						var out map[string]any
						err = c.GetJSONOnce(t.Context(), "/metadata", &out)
					} else {
						_, err = c.SendNoRetry(t.Context(), method, "/metadata", nil, `W/"v1"`)
					}
					require.Error(t, err)
					assertions.Less(len(err.Error()), (1<<20)+1024)
					assertions.Equal(int32(1), attempts.Load())
					assertions.True(errors.Is(err, msgraph.ErrTooLarge), "oversized response must be classified before parsing its status body")
				})
			}
		}
	}
}

func TestMicrosoftInboxTransportCrossOriginAndImporterPolicy(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	var destination, origin atomic.Int32
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		destination.Add(1)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer dest.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		origin.Add(1)
		w.Header().Set("Location", dest.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 10000)
	var out map[string]any
	requirements.Error(c.GetJSONOnce(t.Context(), "/metadata", &out))
	_, err := c.SendNoRetry(t.Context(), http.MethodPatch, "/metadata", map[string]any{"categories": []string{}}, "version")
	requirements.Error(err)
	assertions.Zero(destination.Load())
	assertions.Equal(int32(2), origin.Load())
	requirements.NoError(c.GetJSON(t.Context(), "/metadata", &out), "exact-target helpers must not alter importer HTTP policy")
	assertions.Equal(int32(1), destination.Load())
}

func TestMicrosoftInboxTransportDoesNotRetryFailures(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPatch} {
		for _, status := range []int{429, 503, -1} {
			t.Run(method+http.StatusText(status), func(t *testing.T) {
				var attempts atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					attempts.Add(1)
					if status == -1 {
						hijacker, ok := w.(http.Hijacker)
						require.True(t, ok)
						connection, _, err := hijacker.Hijack()
						if assert.NoError(t, err) {
							assert.NoError(t, connection.Close())
						}
						return
					}
					w.Header().Set("Retry-After", "0")
					w.WriteHeader(status)
				}))
				defer srv.Close()
				c := NewClient(srv.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 10000)
				var err error
				if method == http.MethodGet {
					var out map[string]any
					err = c.GetJSONOnce(t.Context(), "/metadata", &out)
				} else {
					_, err = c.SendNoRetry(t.Context(), method, "/metadata", nil, "version")
				}
				require.Error(t, err)
				assert.Equal(t, int32(1), attempts.Load())
			})
		}
	}
}

func FuzzMicrosoftInboxMetadataCompleteness(f *testing.F) {
	f.Add([]byte(`{"id":"immutable-1","categories":[],"isRead":false,"parentFolderId":"inbox","@odata.etag":"version"}`))
	f.Add([]byte(`{"id":"immutable-1","categories":null,"isRead":false,"parentFolderId":"inbox","@odata.etag":"version"}`))
	f.Add([]byte(`{"id":"immutable-1","categories":[],"parentFolderId":"inbox","@odata.etag":"version"}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var snapshot inboxSnapshot
		if json.Unmarshal(data, &snapshot) != nil {
			return
		}
		var raw map[string]jsontext.Value
		require.NoError(t, json.Unmarshal(data, &raw))
		want := true
		for _, field := range []string{"id", "categories", "isRead", "parentFolderId", "@odata.etag"} {
			if len(raw[field]) == 0 || string(raw[field]) == "null" {
				want = false
			}
		}
		var id, parent, revision string
		_ = json.Unmarshal(raw["id"], &id)
		_ = json.Unmarshal(raw["parentFolderId"], &parent)
		_ = json.Unmarshal(raw["@odata.etag"], &revision)
		want = want && id == "immutable-1" && strings.TrimSpace(parent) != "" && strings.TrimSpace(revision) != ""
		assert.Equal(t, want, snapshot.complete("immutable-1"))
	})
}
