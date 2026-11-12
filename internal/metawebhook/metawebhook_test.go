package metawebhook

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
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
	"go.kenn.io/kit/atomicfile"
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
	return newHandlerIn(t, dir), dir
}

func newHandlerIn(t *testing.T, dir string) *Handler {
	t.Helper()
	h, err := NewHandler(map[string]App{"9": {Secret: []byte(testSecret), VerifyToken: "synthetic-verify"}}, dir, nil)
	require.NoError(t, err)
	return h
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

// post serves one POST and returns the status. It runs on any goroutine.
func post(t *testing.T, h *Handler, signature string, body io.Reader) int {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/meta/webhook/9", body)
	req.Header.Set("X-Hub-Signature-256", signature)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
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

func TestHandlerRefusesMalformedSignatureBeforeReadingBody(t *testing.T) {
	sum := strings.Repeat("ab", sha256.Size)
	for _, tc := range []struct{ name, header string }{
		{"missing", ""},
		{"short", "sha256=00"},
		{"long", "sha256=" + sum + "00"},
		{"not hex", "sha256=" + strings.Repeat("zz", sha256.Size)},
		{"other algorithm", "sha1=" + sum},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newTestHandler(t)
			body := &readRecorder{Reader: strings.NewReader(`{"object":"page","entry":[]}`)}
			assert.Equal(t, http.StatusUnauthorized, post(t, h, tc.header, body))
			assert.False(t, body.read, "the body is not read")
		})
	}
}

func TestHandlerRefusesPostsAboveTheBodyLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		h, dir := newTestHandler(t)
		writers := make([]*io.PipeWriter, maxConcurrentBodies)
		codes := make(chan int, len(writers))
		for i := range writers {
			r, w := io.Pipe()
			writers[i] = w
			go func() { codes <- post(t, h, sign("other"), r) }()
		}
		synctest.Wait() // every slot holds a request blocked reading its body

		body := `{"object":"page","entry":[]}`
		extra := &readRecorder{Reader: strings.NewReader(body)}
		assert.Equal(http.StatusServiceUnavailable, post(t, h, sign(body), extra))
		assert.False(extra.read, "a refused request's body is not read")

		for _, w := range writers {
			_ = w.Close()
		}
		for range writers {
			assert.Equal(http.StatusUnauthorized, <-codes)
		}
		assert.Equal(http.StatusOK, post(t, h, sign(body), strings.NewReader(body)), "released slots admit new deliveries")
		assert.Len(pendingFiles(t, dir), 1)
	})
}

func TestHandlerAnswersRejectedPostsWithoutWaitingForTheBody(t *testing.T) {
	for _, tc := range []struct {
		name, signature string
		slotsTaken      bool
		status          int
	}{
		{"no signature", "", false, http.StatusUnauthorized},
		{"no free body slot", sign("withheld"), true, http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			h, _ := newTestHandler(t)
			if tc.slotsTaken {
				for range cap(h.bodies) {
					h.bodies <- struct{}{}
				}
			}
			srv := httptest.NewServer(h) // no read timeout: a server that waits for the body never answers
			defer srv.Close()
			var dialer net.Dialer
			conn, err := dialer.DialContext(t.Context(), "tcp", srv.Listener.Addr().String())
			require.NoError(err)
			defer func() { _ = conn.Close() }()
			// The deadline only bounds a failing run.
			require.NoError(conn.SetDeadline(time.Now().Add(5 * time.Second)))

			head := "POST /meta/webhook/9 HTTP/1.1\r\nHost: example.test\r\nContent-Length: 1024\r\n"
			if tc.signature != "" {
				head += "X-Hub-Signature-256: " + tc.signature + "\r\n"
			}
			_, err = io.WriteString(conn, head+"\r\n") // the body is withheld
			require.NoError(err)
			resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
			require.NoError(err, "the response arrives while the body is withheld")
			defer func() { _ = resp.Body.Close() }()
			assert.Equal(tc.status, resp.StatusCode)
		})
	}
}

func TestHandlerRefusesDeliveriesAtTheCountCap(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	dir := filepath.Join(t.TempDir(), "pending")
	require.NoError(os.MkdirAll(filepath.Join(dir, "sub"), 0o700))
	existing := filepath.Join(dir, "00000000000000000001-9-OLD.json")
	require.NoError(os.WriteFile(existing, []byte(`{}`), 0o600))
	require.NoError(os.WriteFile(filepath.Join(dir, ".ignored.set-aside"), []byte(`{}`), 0o600))
	h := newHandlerIn(t, dir)
	h.maxFiles = 2 // the existing delivery counts; dot-names and directories do not

	body := `{"object":"page","entry":[]}`
	assert.Equal(http.StatusOK, post(t, h, sign(body), strings.NewReader(body)))
	before := pendingFiles(t, dir)
	assert.Equal(http.StatusServiceUnavailable, post(t, h, sign(body), strings.NewReader(body)))
	assert.Equal(before, pendingFiles(t, dir), "a refused delivery is not stored")

	require.NoError(os.Remove(existing)) // the worker applied it
	assert.Equal(http.StatusOK, post(t, h, sign(body), strings.NewReader(body)))
	assert.Len(pendingFiles(t, dir), len(before), "the new delivery takes the removed one's place")
}

func TestHandlerRefusesDeliveriesAtTheByteCap(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	dir := filepath.Join(t.TempDir(), "pending")
	require.NoError(os.MkdirAll(dir, 0o700))
	body := `{"object":"page","entry":[]}`
	existing := filepath.Join(dir, "00000000000000000001-9-OLD.json")
	require.NoError(os.WriteFile(existing, []byte(body), 0o600))
	h := newHandlerIn(t, dir)
	h.maxBytes = int64(2*len(body) + len(body)/2) // room for the existing delivery and one more

	assert.Equal(http.StatusOK, post(t, h, sign(body), strings.NewReader(body)))
	before := pendingFiles(t, dir)
	assert.Equal(http.StatusServiceUnavailable, post(t, h, sign(body), strings.NewReader(body)))
	assert.Equal(before, pendingFiles(t, dir), "a refused delivery is not stored")

	require.NoError(os.Remove(existing))
	assert.Equal(http.StatusOK, post(t, h, sign(body), strings.NewReader(body)))
}

func TestNewHandlerCleansStagingAndOrdersAfterPendingDeliveries(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	dir := filepath.Join(t.TempDir(), "pending")
	require.NoError(os.MkdirAll(dir, 0o700))
	existing := "09000000000000000000-9-FUTURE.json" // written before the clock stepped back
	for _, name := range []string{existing, ".x.json.tmp-123", ".y.json.set-aside", "notanumber-9-X.json"} {
		require.NoError(os.WriteFile(filepath.Join(dir, name), []byte(`{}`), 0o600))
	}
	h := newHandlerIn(t, dir)
	assert.NoFileExists(filepath.Join(dir, ".x.json.tmp-123"), "a staging file left by a crash is removed")
	assert.FileExists(filepath.Join(dir, ".y.json.set-aside"), "a set-aside delivery is kept")

	body := `{"object":"page","entry":[]}`
	require.Equal(http.StatusOK, post(t, h, sign(body), strings.NewReader(body)))
	var stored []string
	for _, name := range pendingFiles(t, dir) {
		if !strings.HasPrefix(name, ".") && name != existing && name != "notanumber-9-X.json" {
			stored = append(stored, name)
		}
	}
	require.Len(stored, 1)
	assert.Greater(stored[0], existing, "the new delivery sorts after the pending one")
}

func TestWriteResultAcknowledgesOnlyDurableDeliveries(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		published bool
		status    int
	}{
		{"stored", nil, true, http.StatusOK},
		{"staging cleanup failed", fmt.Errorf("write: %w", atomicfile.ErrPublished), true, http.StatusOK},
		{"directory not synced", fmt.Errorf("write: %w", atomicfile.ErrNotDurable), true, http.StatusServiceUnavailable},
		{"name taken", fmt.Errorf("write: %w", fs.ErrExist), false, http.StatusServiceUnavailable},
		{"spool full", errSpoolFull, false, http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			published, status := writeResult(tc.err)
			assert.Equal(t, tc.published, published, "published")
			assert.Equal(t, tc.status, status)
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

// recorder holds what a worker test's field handler saw. The handler runs on
// the worker's goroutine and the test reads on its own, so every access takes
// the lock and reads return copies.
type recorder struct {
	mu       sync.Mutex
	applied  []string
	attempts map[string][]time.Time
}

// attempt records an attempt at value now and returns how many there were.
func (r *recorder) attempt(value string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.attempts == nil {
		r.attempts = map[string][]time.Time{}
	}
	r.attempts[value] = append(r.attempts[value], time.Now())
	return len(r.attempts[value])
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

func (r *recorder) attemptTimes(value string) []time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.attempts[value])
}

func (r *recorder) attemptCount(value string) int {
	return len(r.attemptTimes(value))
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
		var messages recorder
		w := NewWorker(dir, map[Field]FieldHandler{{"page", "feed"}: feed, {"instagram", "messages"}: func(_ context.Context, change Change) error {
			messages.apply(change.EntryID + ":" + string(change.Values[0]))
			return nil
		}})
		require.NoError(h.store("9", []byte(`{"object":"instagram","entry":[{"id":"200","messaging":[{"message":{"mid":"m_1"}}]}]}`)))
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
		assert.Equal([]string{`200:{"message":{"mid":"m_1"}}`}, messages.appliedValues(), "messaging events reach the messages handler")
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

// skipIfPermissionsIgnored skips when this user can read a file with mode 0
// or write into a read-only directory, as root can and as Windows allows.
func skipIfPermissionsIgnored(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	probe := filepath.Join(dir, "probe")
	require.NoError(t, os.WriteFile(probe, nil, 0))
	_, readErr := os.ReadFile(probe)
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	writeErr := os.WriteFile(filepath.Join(dir, "other"), nil, 0o600)
	if readErr == nil || writeErr == nil {
		t.Skip("file permissions do not stop this user (root, or Windows), so the files cannot be made unreadable or undeletable")
	}
}

// feedDelivery returns a page delivery with one feed change per value.
func feedDelivery(values ...string) []byte {
	changes := make([]string, len(values))
	for i, value := range values {
		changes[i] = `{"field":"feed","value":` + value + `}`
	}
	return []byte(`{"object":"page","entry":[{"id":"100","changes":[` + strings.Join(changes, ",") + `]}]}`)
}

func TestWorkerSetsAsideADeliveryItCannotRead(t *testing.T) {
	skipIfPermissionsIgnored(t)
	synctest.Test(t, func(t *testing.T) {
		assert, require := assert.New(t), require.New(t)
		h, dir := newTestHandler(t)
		var rec recorder
		feed := func(_ context.Context, change Change) error {
			rec.apply(string(change.Values[0]))
			return nil
		}
		w := NewWorker(dir, map[Field]FieldHandler{{"page", "feed"}: feed})
		require.NoError(h.store("9", feedDelivery(`"unreadable"`)))
		names := pendingFiles(t, dir)
		require.Len(names, 1)
		require.NoError(os.Chmod(filepath.Join(dir, names[0]), 0))
		require.NoError(h.store("9", feedDelivery(`"later"`)))
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error)
		go func() { done <- w.Run(ctx) }()

		synctest.Wait()
		assert.Equal([]string{`"later"`}, rec.appliedValues(), "a later delivery still applies")
		assert.Equal([]string{"." + names[0] + ".set-aside"}, pendingFiles(t, dir), "the unreadable delivery is set aside")

		require.NoError(h.store("9", feedDelivery(`"after"`)))
		w.Wake()
		synctest.Wait()
		assert.Equal([]string{`"later"`, `"after"`}, rec.appliedValues(), "the worker keeps running")
		cancel()
		require.ErrorIs(<-done, context.Canceled)
	})
}

func TestWorkerSkipsADeliveryItCannotRemoveOrSetAside(t *testing.T) {
	skipIfPermissionsIgnored(t)
	synctest.Test(t, func(t *testing.T) {
		assert, require := assert.New(t), require.New(t)
		h, dir := newTestHandler(t)
		var rec recorder
		feed := func(_ context.Context, change Change) error {
			rec.apply(string(change.Values[0]))
			return nil
		}
		w := NewWorker(dir, map[Field]FieldHandler{{"page", "feed"}: feed})
		require.NoError(h.store("9", feedDelivery(`"stuck"`)))
		stuck := pendingFiles(t, dir)
		require.NoError(os.Chmod(dir, 0o500)) // neither remove nor rename can succeed
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error)
		go func() { done <- w.Run(ctx) }()

		synctest.Wait() // a worker that re-listed the directory forever would never block here
		assert.Equal([]string{`"stuck"`}, rec.appliedValues())
		assert.Equal(stuck, pendingFiles(t, dir), "the delivery stays where it is")

		require.NoError(os.Chmod(dir, 0o700))
		require.NoError(h.store("9", feedDelivery(`"next"`)))
		w.Wake()
		synctest.Wait()
		assert.Equal([]string{`"stuck"`, `"next"`}, rec.appliedValues(), "the stuck delivery is not applied again and the worker keeps running")
		cancel()
		require.ErrorIs(<-done, context.Canceled)
	})
}

func TestNewHandlerStartsWhenAStagingFileCannotBeRemoved(t *testing.T) {
	skipIfPermissionsIgnored(t)
	require := require.New(t)
	dir := filepath.Join(t.TempDir(), "pending")
	require.NoError(os.MkdirAll(dir, 0o700))
	staging := filepath.Join(dir, ".x.json.tmp-123")
	require.NoError(os.WriteFile(staging, []byte(`{}`), 0o600))
	require.NoError(os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	_, err := NewHandler(map[string]App{"9": {Secret: []byte(testSecret), VerifyToken: "synthetic-verify"}}, dir, nil)
	require.NoError(err, "a staging file that cannot be removed does not stop the daemon")
	assert.FileExists(t, staging)
}

func TestWorkerWaitsOutABusySourceWithoutSpendingTheRetryBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert, require := assert.New(t), require.New(t)
		h, dir := newTestHandler(t)
		start := time.Now()
		var rec recorder
		feed := func(ctx context.Context, change Change) error {
			value := string(change.Values[0])
			rec.attempt(value)
			switch value {
			case `"busy"`:
				if time.Since(start) < 3*time.Hour {
					return fmt.Errorf("%w: polling sync running", ErrBusy)
				}
			case `"down"`:
				return errors.New("graph unavailable")
			case `"busy later"`:
				if time.Since(rec.attemptTimes(value)[0]) < 10*time.Minute {
					return fmt.Errorf("%w: polling sync running", ErrBusy)
				}
			}
			rec.apply(value)
			return nil
		}
		w := NewWorker(dir, map[Field]FieldHandler{{"page", "feed"}: feed})
		body := `{"object":"page","entry":[{"id":"100","changes":[` +
			`{"field":"feed","value":"busy"},{"field":"feed","value":"down"},` +
			`{"field":"feed","value":"busy later"},{"field":"feed","value":"after"}]}]}`
		require.NoError(h.store("9", []byte(body)))
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error)
		go func() { done <- w.Run(ctx) }()

		time.Sleep(6 * time.Hour)
		synctest.Wait()
		assert.Equal([]string{`"busy"`, `"busy later"`, `"after"`}, rec.appliedValues(),
			"busy changes wait for their source, even after the budget is spent; the failing change is skipped")
		downAttempts := rec.attemptTimes(`"down"`)
		require.NotEmpty(downAttempts)
		assert.GreaterOrEqual(downAttempts[len(downAttempts)-1].Sub(downAttempts[0]), 55*time.Minute,
			"hours of waiting for a busy source leave the next failure its full retry budget")
		assert.Empty(pendingFiles(t, dir))
		cancel()
		require.ErrorIs(<-done, context.Canceled)
	})
}

// Time spent inside an attempt, such as waiting for the daemon to finish
// other work, is not charged to the retry budget: only the waits between
// attempts are.
func TestWorkerDoesNotChargeTimeInsideAnAttempt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert, require := assert.New(t), require.New(t)
		h, dir := newTestHandler(t)
		var rec recorder
		feed := func(ctx context.Context, change Change) error {
			value := string(change.Values[0])
			if rec.attempt(value) < 4 {
				// Each failing attempt takes longer than the whole budget.
				time.Sleep(2 * time.Hour)
				return errors.New("graph unavailable")
			}
			rec.apply(value)
			return nil
		}
		w := NewWorker(dir, map[Field]FieldHandler{{"page", "feed"}: feed})
		require.NoError(h.store("9", []byte(`{"object":"page","entry":[{"id":"100","changes":[{"field":"feed","value":"slow"}]}]}`)))
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error)
		go func() { done <- w.Run(ctx) }()

		time.Sleep(7 * time.Hour)
		synctest.Wait()
		assert.Equal([]string{`"slow"`}, rec.appliedValues(), "the change is retried after slow attempts and applies")
		assert.Equal(4, rec.attemptCount(`"slow"`))
		assert.Empty(pendingFiles(t, dir))
		cancel()
		require.ErrorIs(<-done, context.Canceled)
	})
}
