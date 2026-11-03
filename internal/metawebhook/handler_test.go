package metawebhook

import (
	"bytes"
	"context"
	"io"
	"net/http"
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
	require := require.New(t)
	s, _ := testSpool(t)
	wake := 0
	h, err := NewHandler(HandlerOptions{Apps: map[string]AppCredentials{"9": {Secret: []byte("synthetic-app-secret"), VerifyToken: "synthetic-verify"}}, Spool: s, MaxBodyBytes: limit, Wake: func() { wake++ }})
	require.NoError(err)
	return h, s, &wake
}
func TestHandshake(t *testing.T) {
	assert := assert.New(t)
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
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/meta/webhook/"+tt.app+"?"+q.Encode(), nil))
		assert.Equal(tt.want, rr.Code)
		if tt.want == 200 {
			assert.Equal("A123", rr.Body.String())
			assert.Equal("text/plain", rr.Header().Get("Content-Type"))
		} else {
			assert.Empty(rr.Body.String())
		}
	}
}
func post(t *testing.T, h *Handler, body []byte, signature string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/meta/webhook/9", bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", signature)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	h.ServeHTTP(rr, req)
	return rr
}
func TestPostSignedSpoolsBeforeOK(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	h, s, wake := testHandler(t, 0)
	body := []byte(`{"object":"page","entry":[]}`)
	rr := post(t, h, body, signed(body))
	assert.Equal(200, rr.Code)
	assert.Equal(1, *wake)
	due, err := s.Due(10)
	require.NoError(err)
	require.Len(due, 1)
	got, err := s.Body("9", due[0].ID)
	require.NoError(err)
	assert.Equal(body, got)
	rr = post(t, h, body, signed(body))
	assert.Equal(200, rr.Code)
	assert.Equal(uint64(1), h.Counters().Accepted)
	assert.Equal(uint64(1), h.Counters().Duplicates)
	assert.False(h.Counters().LastAccepted.IsZero())
}
func TestPostUnsignedNotSpooled(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	h, s, _ := testHandler(t, 0)
	rr := post(t, h, []byte("synthetic"), "")
	assert.Equal(401, rr.Code)
	due, err := s.Due(10)
	require.NoError(err)
	assert.Empty(due)
	assert.Equal(uint64(1), h.Counters().SignatureFailures)
}
func TestPostTooLarge413(t *testing.T) {
	assert := assert.New(t)
	h, _, _ := testHandler(t, 3)
	body := []byte("four")
	rr := post(t, h, body, signed(body))
	assert.Equal(413, rr.Code)
	assert.Equal(uint64(1), h.Counters().TooLarge)
}
func TestPostSpoolFull503(t *testing.T) {
	assert := assert.New(t)
	h, s, _ := testHandler(t, 0)
	s.maxBytes = 1
	body := []byte("synthetic")
	rr := post(t, h, body, signed(body))
	assert.Equal(503, rr.Code)
	assert.Equal(uint64(1), h.Counters().Unavailable)
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
	assert := assert.New(t)
	require := require.New(t)
	s, _ := testSpool(t)
	h, err := NewHandler(HandlerOptions{Apps: map[string]AppCredentials{"9": {Secret: []byte("synthetic-app-secret"), VerifyToken: "synthetic-verify"}}, Spool: s})
	require.NoError(err)
	started := make(chan struct{}, 16)
	release := make(chan struct{})
	var wg sync.WaitGroup
	defer func() { close(release); wg.Wait() }()
	body := []byte("synthetic")
	for range 16 {
		wg.Go(func() {
			req := httptest.NewRequest(http.MethodPost, "/meta/webhook/9", &waitingBody{started: started, release: release, reader: bytes.NewReader(body)})
			req.Header.Set("X-Hub-Signature-256", signed(body))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			assert.Equal(200, rr.Code)
		})
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for range 16 {
		select {
		case <-started:
		case <-ctx.Done():
			require.FailNow("requests never reached body read")
		}
	}
	rr := post(t, h, body, signed(body))
	assert.Equal(503, rr.Code)
}
func TestMethodAndPath(t *testing.T) {
	assert := assert.New(t)
	h, _, _ := testHandler(t, 0)
	for _, tt := range []struct {
		method, path string
		code         int
	}{{"PUT", "/meta/webhook/9", 405}, {"GET", "/meta/webhook/9/x", 404}, {"POST", "/meta/webhook/8", 404}} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(tt.method, tt.path, nil))
		assert.Equal(tt.code, rr.Code)
	}
}
func TestResponsesNeverReflectInput(t *testing.T) {
	assert := assert.New(t)
	h, _, _ := testHandler(t, 0)
	rr := post(t, h, []byte("synthetic-sensitive-body"), "synthetic-sensitive-signature")
	assert.Empty(rr.Body.String())
}

func TestResponsesNeverReflectMalformedPath(t *testing.T) {
	assert := assert.New(t)
	h, _, _ := testHandler(t, 0)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/meta/webhook/9/../synthetic-sensitive-path", nil))
	assert.Equal(404, rr.Code)
	assert.NotContains(rr.Body.String(), "synthetic-sensitive-path")
	assert.Empty(rr.Header().Get("Location"))
}
