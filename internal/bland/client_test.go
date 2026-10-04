package bland

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClientReadsOnlyRetainedArtifacts(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertions.Equal("GET", r.Method)
		assertions.Equal("synthetic-key", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/calls":
			assertions.Equal("updated_at", r.URL.Query().Get("sort_by"))
			assertions.Equal("true", r.URL.Query().Get("ascending"))
			assertions.Equal("2", r.URL.Query().Get("limit"))
			_, _ = w.Write([]byte(`{"count":1,"total_count":1,"calls":[{"call_id":"call-1"}]}`))
		case "/v1/calls/call-1":
			_, _ = w.Write([]byte(`{"call_id":"call-1","completed":true}`))
		case "/v1/postcall/webhooks/call-1":
			_, _ = w.Write([]byte(`{"data":{"call_id":"call-1","payload":{"corrected_transcript":[{"text":"Retained words","speaker_label":"user","start":0.5,"end":2}]}}}`))
		default:
			assertions.Fail("unexpected retrieval", r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL+"/v1", "synthetic-key")
	page, err := c.ListCalls(context.Background(), ListOptions{Limit: 2})
	requirements.NoError(err)
	requirements.Len(page.Calls, 1)
	call, err := c.GetCall(t.Context(), "call-1")
	requirements.NoError(err)
	assertions.True(call.Completed)
	raw, err := c.GetPostCall(t.Context(), "call-1")
	requirements.NoError(err)
	assertions.Contains(string(raw), "Retained words")
}

func TestRecordingRejectsErrorsAndRedirects(t *testing.T) {
	assertions := assert.New(t)

	for _, body := range []string{`{"error":"CALL_RECORDING_NOT_FOUND"}`, `<html>error</html>`} {
		t.Run(body, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "audio/mpeg")
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()
			c := NewClient(srv.URL, "synthetic-key")
			_, err := c.OpenRecording(t.Context(), "call-1")
			require.Error(t, err)
		})
	}
	dst := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertions.Empty(r.Header.Get("Authorization"))
		assertions.Fail("redirect destination must not be fetched")
	}))
	defer dst.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, dst.URL, http.StatusFound) }))
	defer srv.Close()
	_, err := NewClient(srv.URL, "synthetic-key").OpenRecording(t.Context(), "call-1")
	require.Error(t, err)
}

func TestRecordingBodyCanOutlastJSONClientTimeout(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseBody := func() { releaseOnce.Do(func() { close(release) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := w.Write([]byte("ID3"))
		assertions.NoError(err)
		flusher, ok := w.(http.Flusher)
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			close(started)
			return
		}
		flusher.Flush()
		close(started)
		<-release
		_, err = w.Write([]byte(strings.Repeat("a", 600)))
		assertions.NoError(err)
	}))
	defer func() {
		releaseBody()
		srv.Close()
	}()
	c := NewClient(srv.URL, "synthetic-key")
	apiTimeout := 40 * time.Millisecond
	c.http.Timeout = apiTimeout
	result := make(chan struct {
		recording *Recording
		err       error
	}, 1)
	go func() {
		recording, err := c.OpenRecording(context.Background(), "call-1")
		result <- struct {
			recording *Recording
			err       error
		}{recording, err}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		requirements.FailNow("media request did not reach its response body")
	}
	select {
	case outcome := <-result:
		releaseBody()
		if outcome.recording != nil {
			requirements.NoError(outcome.recording.Body.Close())
		}
		requirements.NoError(outcome.err, "media body should outlast the JSON request timeout")
	case <-time.After(4 * apiTimeout):
	}
	releaseBody()
	var outcome struct {
		recording *Recording
		err       error
	}
	select {
	case outcome = <-result:
	case <-time.After(time.Second):
		requirements.FailNow("media request did not finish after the body was released")
	}
	requirements.NoError(outcome.err)
	requirements.NotNil(outcome.recording)
	data, err := io.ReadAll(outcome.recording.Body)
	requirements.NoError(err)
	requirements.NoError(outcome.recording.Body.Close())
	assertions.Equal("ID3"+strings.Repeat("a", 600), string(data))
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
			_, _ = w.Write([]byte(`{"c_id":"call-1"}`))
		case "/v1/postcall/webhooks/call-1":
			_, _ = w.Write([]byte(`{"data":{"call_id":"another-call","payload":{}}}`))
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL+"/v1", "test-key")
	c.EncryptedKey = "byot-key"
	_, err := c.GetCall(t.Context(), "call-1")
	requirements.NoError(err)
	_, err = c.ListCalls(t.Context(), ListOptions{Limit: 1})
	requirements.ErrorIs(err, ErrInvalidPayload)
	_, err = c.GetPostCall(t.Context(), "call-1")
	requirements.ErrorIs(err, ErrInvalidPayload)
}
func TestHTTPErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   error
	}{{http.StatusUnauthorized, ErrAuthentication}, {http.StatusForbidden, ErrAuthentication}, {http.StatusNotFound, ErrNotFound}, {http.StatusTooManyRequests, ErrRateLimited}} {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			assertions := assert.New(t)

			requests := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()
			_, err := NewClient(srv.URL, "test-key").GetCall(t.Context(), "call-1")
			require.ErrorIs(t, err, tc.want)
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

func TestCorrectedTranscriptClientContract(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		valid      bool
	}{
		{"documented", `{"status":"success","corrected":[{"start":0.069,"end":2.551,"text":"synthetic speech","speaker":2,"speaker_label":"assistant","confidence":0.762}],"aligned":[{"text":"deprecated"}]}`, true},
		{"empty", `{"corrected":[]}`, true},
		{"malformed", `{"corrected":{}}`, false},
		{"failure", `{"status":"error","corrected":[]}`, false},
		{"invalid offset", `{"corrected":[{"text":"speech","start":-1}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assertions.Equal(http.MethodGet, r.Method)
				assertions.Equal("/v1/calls/call-1/correct", r.URL.Path)
				assertions.Equal("test-key", r.Header.Get("Authorization"))
				assertions.Empty(r.Header.Get("Encrypted_key"))
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c := NewClient(srv.URL+"/v1", "test-key")
			c.EncryptedKey = "byot-secret"
			raw, err := c.GetCorrectedTranscript(t.Context(), "call-1")
			if tc.valid {
				require.NoError(t, err)
				assertions.JSONEq(tc.body, string(raw))
			} else {
				require.ErrorIs(t, err, ErrInvalidPayload)
			}
		})
	}
}
