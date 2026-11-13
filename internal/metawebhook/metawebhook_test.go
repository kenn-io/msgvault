package metawebhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testSecret = "synthetic-app-secret"

func sign(body string) string {
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func newTestHandler(t *testing.T) (*Handler, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "pending")
	h, err := NewHandler(map[string]App{"9": {Secret: []byte(testSecret), VerifyToken: "synthetic-verify"}}, dir, nil)
	require.NoError(t, err)
	return h, dir
}

func pendingFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// readRecorder records whether the handler read the request body.
type readRecorder struct {
	io.Reader

	read bool
}

func (r *readRecorder) Read(p []byte) (int, error) {
	r.read = true
	return r.Reader.Read(p)
}

func TestHandlerRefusesEarlyWithoutReadingTheBody(t *testing.T) {
	body := `{"object":"page","entry":[]}`
	for _, tc := range []struct {
		name, signature string
		slotsTaken      bool
		status          int
	}{
		{"missing signature", "", false, http.StatusUnauthorized},
		{"malformed signature", "sha256=00", false, http.StatusUnauthorized},
		{"no free body slot", sign(body), true, http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newTestHandler(t)
			if tc.slotsTaken {
				for range maxConcurrentBodies {
					h.bodies <- struct{}{}
				}
			}
			reader := &readRecorder{Reader: strings.NewReader(body)}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/meta/webhook/9", reader)
			req.Header.Set("X-Hub-Signature-256", tc.signature)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			assert.Equal(t, tc.status, rec.Code)
			assert.Equal(t, "close", rec.Header().Get("Connection"))
			assert.False(t, reader.read, "the body is not read")
		})
	}
}

func TestHandlerChallenge(t *testing.T) {
	h, _ := newTestHandler(t)
	for _, tc := range []struct {
		name, query string
		status      int
		body        string
	}{
		{"matching token", "hub.mode=subscribe&hub.verify_token=synthetic-verify&hub.challenge=1158201444", http.StatusOK, "1158201444"},
		{"wrong token", "hub.mode=subscribe&hub.verify_token=other&hub.challenge=1158201444", http.StatusForbidden, ""},
		{"wrong mode", "hub.mode=unsubscribe&hub.verify_token=synthetic-verify&hub.challenge=1158201444", http.StatusForbidden, ""},
		{"markup challenge", "hub.mode=subscribe&hub.verify_token=synthetic-verify&hub.challenge=%3Cscript%3E", http.StatusForbidden, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/meta/webhook/9?"+tc.query, nil))
			assert.Equal(t, tc.status, rec.Code)
			assert.Equal(t, tc.body, rec.Body.String())
		})
	}
}

func TestHandlerStoresOnlySignedDeliveries(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	h, dir := newTestHandler(t)
	body := `{"object":"page","entry":[]}`
	post := func(path, signature, body string) int {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("X-Hub-Signature-256", signature)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	assert.Equal(http.StatusUnauthorized, post("/meta/webhook/9", "", body))
	assert.Equal(http.StatusUnauthorized, post("/meta/webhook/9", sign(body+" "), body))
	assert.Equal(http.StatusNotFound, post("/meta/webhook/10", sign(body), body), "unknown app")
	assert.Equal(http.StatusRequestEntityTooLarge, post("/meta/webhook/9", sign(body), strings.Repeat(" ", maxBodyBytes+1)))
	assert.Empty(pendingFiles(t, dir))

	assert.Equal(http.StatusOK, post("/meta/webhook/9", sign(body), body))
	files := pendingFiles(t, dir)
	require.Len(files, 1)
	assert.Equal("9", strings.Split(files[0], "-")[1])
	stored, err := os.ReadFile(filepath.Join(dir, files[0]))
	require.NoError(err)
	assert.Equal(body, string(stored))
}

// recorder holds what the worker test's field handler saw. The handler runs
// on the worker's goroutine and the test reads on its own, so every access
// takes the lock.
type recorder struct {
	mu       sync.Mutex
	applied  []string
	attempts map[string]int
}

// attempt records an attempt at value and returns how many there were.
func (r *recorder) attempt(value string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.attempts == nil {
		r.attempts = map[string]int{}
	}
	r.attempts[value]++
	return r.attempts[value]
}

func (r *recorder) apply(value string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.applied = append(r.applied, value)
}

func (r *recorder) appliedValues() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.applied)
}

func (r *recorder) attemptCount(value string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.attempts[value]
}

func TestWorkerRetriesEachChangeInOrderAndAppliesTheRest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert, require := assert.New(t), require.New(t)
		h, dir := newTestHandler(t)
		var rec recorder
		feed := func(ctx context.Context, change Change) error {
			value := string(change.Values[0])
			attempts := rec.attempt(value)
			switch value {
			case `"flaky"`:
				if attempts < 3 {
					return errors.New("temporary failure")
				}
			case `"invalid"`:
				return fmt.Errorf("%w: not archivable", ErrUnrecoverable)
			case `"down"`, `"also down"`:
				return errors.New("graph unavailable")
			}
			rec.apply(change.App + ":" + change.EntryID + ":" + value)
			return nil
		}
		w := NewWorker(dir, map[Field]FieldHandler{{"page", "feed"}: feed})
		store := func(values ...string) {
			changes := []string{}
			for _, value := range values {
				changes = append(changes, `{"field":"feed","value":`+value+`}`)
			}
			changes = append(changes, `{"field":"ratings","value":{}}`)
			body := `{"object":"page","entry":[{"id":"100","changes":[` + strings.Join(changes, ",") + `]}]}`
			require.NoError(h.store("9", []byte(body)))
		}
		require.NoError(h.store("9", []byte(`{"object":`)))
		store(`"first"`, `"invalid"`, `"flaky"`)
		store(`"down"`, `"also down"`, `"later"`)
		store(`"after"`)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error)
		go func() { done <- w.Run(ctx) }()

		time.Sleep(30 * time.Minute) // lets the fake clock run through the retry waits
		synctest.Wait()
		assert.Equal([]string{`9:100:"first"`, `9:100:"flaky"`}, rec.appliedValues(),
			"a retried change does not apply the changes before it again; a failing change blocks the rest")
		assert.Equal(1, rec.attemptCount(`"invalid"`), "an unrecoverable change is skipped at once, without retries")
		assert.Equal(3, rec.attemptCount(`"flaky"`))
		assert.Len(pendingFiles(t, dir), 2, "the failing delivery and the one after it wait")

		time.Sleep(time.Hour)
		synctest.Wait()
		assert.Equal([]string{`9:100:"first"`, `9:100:"flaky"`, `9:100:"later"`, `9:100:"after"`}, rec.appliedValues(),
			"after an hour the failing change is skipped and the rest of its delivery and later deliveries apply")
		assert.Equal(1, rec.attemptCount(`"also down"`), "with the retry budget spent, a later failing change gets a single attempt")
		assert.Equal(1, rec.attemptCount(`"invalid"`))
		assert.Empty(pendingFiles(t, dir), "applied and dropped deliveries are deleted")
		attempts := rec.attemptCount(`"down"`)

		store(`"down"`)
		w.Wake()
		time.Sleep(10 * time.Minute)
		synctest.Wait()
		assert.Greater(rec.attemptCount(`"down"`), attempts+1, "the new delivery is being retried")
		cancel()
		require.ErrorIs(<-done, context.Canceled)
		assert.Len(pendingFiles(t, dir), 1, "a delivery interrupted by shutdown stays for the next start")
	})
}
