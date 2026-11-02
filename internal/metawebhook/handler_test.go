package metawebhook

import (
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testHandler(t *testing.T, limit int64) (*Handler, *Spool, *int) {
	t.Helper()
	s, _ := testSpool(t)
	wake := 0
	h, err := NewHandler(HandlerOptions{Apps: map[string]AppCredentials{"9": {Secret: []byte("synthetic-app-secret"), VerifyToken: "synthetic-verify"}}, Spool: s, MaxBodyBytes: limit, Wake: func() { wake++ }})
	require.NoError(t, err)
	return h, s, &wake
}
func TestHandshake(t *testing.T) {
	h, _, _ := testHandler(t, 0)
	for _, tt := range []struct {
		app, mode, token, challenge string
		want                        int
	}{
		{"9", "subscribe", "synthetic-verify", "A123", 200}, {"9", "subscribe", "wrong", "A123", 403}, {"9", "wrong", "synthetic-verify", "A123", 403},
		{"9", "subscribe", "synthetic-verify", "<script>", 403}, {"9", "subscribe", "synthetic-verify", strings.Repeat("a", 65), 403}, {"8", "subscribe", "synthetic-verify", "A123", 404},
	} {
		q := url.Values{"hub.mode": {tt.mode}, "hub.verify_token": {tt.token}, "hub.challenge": {tt.challenge}}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("GET", "/meta/webhook/"+tt.app+"?"+q.Encode(), nil))
		assert.Equal(t, tt.want, rr.Code)
		if tt.want == 200 {
			assert.Equal(t, "A123", rr.Body.String())
			assert.Equal(t, "text/plain", rr.Header().Get("Content-Type"))
		} else {
			assert.Empty(t, rr.Body.String())
		}
	}
}
func post(t *testing.T, h *Handler, body []byte, signature string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/meta/webhook/9", bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", signature)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	h.ServeHTTP(rr, req)
	return rr
}
func TestPostSignedSpoolsBeforeOK(t *testing.T) {
	h, s, wake := testHandler(t, 0)
	body := []byte(`{"object":"page","entry":[]}`)
	rr := post(t, h, body, signed(body))
	assert.Equal(t, 200, rr.Code)
	assert.Equal(t, 1, *wake)
	due, err := s.Due(10)
	require.NoError(t, err)
	require.Len(t, due, 1)
	got, err := s.Body("9", due[0].ID)
	require.NoError(t, err)
	assert.Equal(t, body, got)
	rr = post(t, h, body, signed(body))
	assert.Equal(t, 200, rr.Code)
	assert.Equal(t, uint64(1), h.Counters().Accepted)
	assert.Equal(t, uint64(1), h.Counters().Duplicates)
	assert.False(t, h.Counters().LastAccepted.IsZero())
}
func TestPostUnsignedNotSpooled(t *testing.T) {
	h, s, _ := testHandler(t, 0)
	rr := post(t, h, []byte("synthetic"), "")
	assert.Equal(t, 401, rr.Code)
	due, err := s.Due(10)
	require.NoError(t, err)
	assert.Empty(t, due)
	assert.Equal(t, uint64(1), h.Counters().SignatureFailures)
}
func TestPostTooLarge413(t *testing.T) {
	h, _, _ := testHandler(t, 3)
	body := []byte("four")
	rr := post(t, h, body, signed(body))
	assert.Equal(t, 413, rr.Code)
	assert.Equal(t, uint64(1), h.Counters().TooLarge)
}
func TestPostSpoolFull503(t *testing.T) {
	h, s, _ := testHandler(t, 0)
	s.maxBytes = 1
	body := []byte("synthetic")
	rr := post(t, h, body, signed(body))
	assert.Equal(t, 503, rr.Code)
	assert.Equal(t, uint64(1), h.Counters().Unavailable)
}

type waitingBody struct {
	started chan<- struct{}
	release <-chan struct{}
	reader  io.Reader
	once    sync.Once
}

func (b *waitingBody) Read(p []byte) (int, error) {
	b.once.Do(func() { b.started <- struct{}{}; <-b.release })
	return b.reader.Read(p)
}
func TestInFlightCap503(t *testing.T) {
	s, _ := testSpool(t)
	h, err := NewHandler(HandlerOptions{Apps: map[string]AppCredentials{"9": {Secret: []byte("synthetic-app-secret"), VerifyToken: "synthetic-verify"}}, Spool: s})
	require.NoError(t, err)
	started := make(chan struct{}, 16)
	release := make(chan struct{})
	var wg sync.WaitGroup
	defer func() { close(release); wg.Wait() }()
	body := []byte("synthetic")
	for range 16 {
		wg.Go(func() {
			req := httptest.NewRequest("POST", "/meta/webhook/9", &waitingBody{started: started, release: release, reader: bytes.NewReader(body)})
			req.Header.Set("X-Hub-Signature-256", signed(body))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			assert.Equal(t, 200, rr.Code)
		})
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for range 16 {
		select {
		case <-started:
		case <-ctx.Done():
			require.FailNow(t, "requests never reached body read")
		}
	}
	rr := post(t, h, body, signed(body))
	assert.Equal(t, 503, rr.Code)
}
func TestMethodAndPath(t *testing.T) {
	h, _, _ := testHandler(t, 0)
	for _, tt := range []struct {
		method, path string
		code         int
	}{{"PUT", "/meta/webhook/9", 405}, {"GET", "/meta/webhook/9/x", 404}, {"POST", "/meta/webhook/8", 404}} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(tt.method, tt.path, nil))
		assert.Equal(t, tt.code, rr.Code)
	}
}
func TestResponsesNeverReflectInput(t *testing.T) {
	h, _, _ := testHandler(t, 0)
	rr := post(t, h, []byte("synthetic-sensitive-body"), "synthetic-sensitive-signature")
	assert.Empty(t, rr.Body.String())
}

func TestResponsesNeverReflectMalformedPath(t *testing.T) {
	h, _, _ := testHandler(t, 0)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/meta/webhook/9/../synthetic-sensitive-path", nil))
	assert.Equal(t, 404, rr.Code)
	assert.NotContains(t, rr.Body.String(), "synthetic-sensitive-path")
	assert.Empty(t, rr.Header().Get("Location"))
}
