package bland

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/callsync"
)

// The listing pages oldest first by creation date, so calls created during a
// traversal land after the offset instead of shifting it.
func TestListCallsPagesOldestFirstByCreation(t *testing.T) {
	assertions := assert.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertions.Equal("created_at", r.URL.Query().Get("sort_by"))
		assertions.Equal("true", r.URL.Query().Get("ascending"))
		assertions.Equal("0", r.URL.Query().Get("from"))
		assertions.Equal("2", r.URL.Query().Get("to"))
		assertions.Equal("2026-09-24", r.URL.Query().Get("start_date"))
		_, _ = w.Write([]byte(`{"count":1,"total_count":1,"calls":[{"call_id":"call-1"}]}`))
	}))
	defer srv.Close()
	_, err := NewClient(srv.URL+"/v1", "synthetic-key").ListCalls(t.Context(), ListOptions{Limit: 2, CreatedAfter: "2026-09-24"})
	require.NoError(t, err)
}

func TestBYOTAndPayloadFailures(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/calls") {
			assertions.Equal("byot-key", r.Header.Get("Encrypted_key"))
		} else {
			assertions.Empty(r.Header.Get("Encrypted_key"))
		}
		switch r.URL.Path {
		case "/v1/calls":
			_, _ = w.Write([]byte(`{"count":1,"calls":[]}`))
		case "/v1/calls/call-1":
			_, _ = w.Write([]byte(`{"c_id":"call-1","created_at":"2026-10-01T11:00:00Z"}`))
		case "/v1/postcall/webhooks/call-2":
			// An account without postcall webhooks answers 200 with null data and errors.
			_, _ = w.Write([]byte(`{"data":null,"errors":[{"error":"NOT_FOUND","message":"No postcall webhook data"}]}`))
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL+"/v1", "test-key")
	c.EncryptedKey = "byot-key"
	// These reads only show which requests carry the BYOT header.
	_, _ = c.GetCall(t.Context(), "call-1")
	_, _ = c.GetPostCall(t.Context(), "call-2")
	_, err := c.ListCalls(t.Context(), ListOptions{Limit: 1})
	requirements.ErrorIs(err, ErrInvalidPayload, "count must match the calls")
}

// A recording Bland refuses or lacks is that recording's failure, not the
// account's, and Bland's "not ready yet" body means waiting on any status.
func TestHTTPErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		audio  bool
		want   error
		text   string
		// location redirects the request there.
		location string
	}{
		{name: "401", status: http.StatusUnauthorized, want: ErrAuthentication},
		{name: "403", status: http.StatusForbidden, want: ErrAuthentication},
		{name: "404", status: http.StatusNotFound, want: ErrNotFound},
		{name: "429", status: http.StatusTooManyRequests, want: ErrRateLimited},
		{name: "400", status: http.StatusBadRequest},
		{name: "recording 403", status: http.StatusForbidden, audio: true},
		{name: "recording 404", status: http.StatusNotFound, audio: true},
		{name: "recording not ready", status: http.StatusNotFound, body: `{"data":null,"errors":[{"error":"CALL_RECORDING_NOT_FOUND","message":"Call recording not found"}]}`, audio: true, want: ErrNotFound},
		{name: "recording redirect to storage fails", status: http.StatusFound, location: "https://127.0.0.1:1/audio?token=secret-token", audio: true, want: callsync.ErrTransport},
		{name: "recording not audio", status: http.StatusOK, body: "<html>not audio</html>", audio: true, text: "not audio"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)

			requests := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				w.Header().Set("Retry-After", "0")
				if tc.location != "" {
					w.Header().Set("Location", tc.location)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c := NewClient(srv.URL, "test-key")
			var err error
			if tc.audio {
				_, err = c.OpenRecording(t.Context(), "call-1", 1<<20)
			} else {
				_, err = c.GetCall(t.Context(), "call-1")
			}
			switch {
			case tc.text != "":
				require.ErrorContains(t, err, tc.text)
			case tc.want == nil:
				got, isHTTP := errors.AsType[*HTTPError](err)
				require.True(t, isHTTP)
				assertions.Equal(tc.status, got.StatusCode)
			default:
				require.ErrorIs(t, err, tc.want)
			}
			assertions.NotContains(err.Error(), "secret-token", "a signed storage URL stays out of the error")
			if tc.status == http.StatusTooManyRequests {
				assertions.Equal(3, requests)
			} else {
				assertions.Equal(1, requests)
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := NewClient("https://api.bland.ai/v1", "test-key").GetCall(ctx, "call-1")
	require.ErrorIs(t, err, context.Canceled)
}
