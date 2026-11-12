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
	"strings"
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

func TestWorkerRetriesEachChangeInOrderAndAppliesTheRest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert, require := assert.New(t), require.New(t)
		h, dir := newTestHandler(t)
		var applied []string
		flaky, invalid, down, alsoDown := 0, 0, 0, 0
		feed := func(ctx context.Context, change Change) error {
			value := string(change.Values[0])
			switch value {
			case `"flaky"`:
				flaky++
				if flaky < 3 {
					return errors.New("source busy")
				}
			case `"invalid"`:
				invalid++
				return fmt.Errorf("%w: not archivable", ErrUnrecoverable)
			case `"down"`:
				down++
				return errors.New("graph unavailable")
			case `"also down"`:
				alsoDown++
				return errors.New("graph unavailable")
			}
			applied = append(applied, change.App+":"+change.EntryID+":"+value)
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
		assert.Equal([]string{`9:100:"first"`, `9:100:"flaky"`}, applied,
			"a retried change does not apply the changes before it again; a failing change blocks the rest")
		assert.Equal(1, invalid, "an unrecoverable change is skipped at once, without retries")
		assert.Equal(3, flaky)
		assert.Len(pendingFiles(t, dir), 2, "the failing delivery and the one after it wait")

		time.Sleep(time.Hour)
		synctest.Wait()
		assert.Equal([]string{`9:100:"first"`, `9:100:"flaky"`, `9:100:"later"`, `9:100:"after"`}, applied,
			"after an hour the failing change is skipped and the rest of its delivery and later deliveries apply")
		assert.Equal(1, alsoDown, "with the retry budget spent, a later failing change gets a single attempt")
		assert.Equal(1, invalid)
		assert.Empty(pendingFiles(t, dir), "applied and dropped deliveries are deleted")
		attempts := down

		store(`"down"`)
		w.Wake()
		time.Sleep(10 * time.Minute)
		synctest.Wait()
		assert.Greater(down, attempts+1, "the new delivery is being retried")
		cancel()
		require.ErrorIs(<-done, context.Canceled)
		assert.Len(pendingFiles(t, dir), 1, "a delivery interrupted by shutdown stays for the next start")
	})
}
