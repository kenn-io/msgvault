// Package metawebhook receives signed Meta webhook deliveries, stores each one
// before acknowledging it, and applies stored deliveries in arrival order.
package metawebhook

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/pack"
)

const (
	// maxBodyBytes bounds one delivery. Meta batches at most 1,000 changes.
	maxBodyBytes = 8 << 20
	// maxConcurrentBodies bounds the POST bodies read at once, so request
	// bodies hold at most 64 MiB.
	maxConcurrentBodies = 8
	// maxPendingFiles and maxPendingBytes bound the deliveries waiting in the
	// pending directory. Above them a POST gets 503 and Meta retries later.
	maxPendingFiles = 10_000
	maxPendingBytes = 1 << 30
)

// errSpoolFull refuses a delivery that would take the pending directory past
// its caps.
var errSpoolFull = errors.New("too many pending Meta webhook deliveries")

var (
	appPattern       = regexp.MustCompile(`^[1-9][0-9]*$`)
	challengePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	signaturePattern = regexp.MustCompile(`^sha256=[0-9A-Fa-f]{64}$`)
)

// App holds one Meta app's webhook credentials.
type App struct {
	Secret      []byte
	VerifyToken string
}

// Handler serves /meta/webhook/{app_id}. GET answers Meta's subscription
// challenge. POST verifies the delivery signature and writes the body to the
// pending directory before it answers 200, so an accepted delivery survives a
// restart. A POST without a well-formed signature header gets 401 before its
// body is read, and one that finds maxConcurrentBodies bodies being read gets
// 503 at once. A delivery that would take the pending directory past
// maxPendingFiles or maxPendingBytes also gets 503.
type Handler struct {
	apps    map[string]App
	pending string
	wake    func()
	bodies  chan struct{}

	// maxFiles and maxBytes are maxPendingFiles and maxPendingBytes; tests
	// lower them.
	maxFiles int
	maxBytes int64

	// mu makes deliveries visible in the order of their names, so the
	// worker never sees a newer delivery before an older one. It also guards
	// files and bytes, the pending totals.
	mu    sync.Mutex
	last  int64
	files int
	bytes int64
}

// NewHandler stores deliveries in pending. It removes staging files left by
// a crash, counts the deliveries already there toward the caps, and names
// new deliveries after them even if the clock stepped back. wake, when set,
// is called after each stored delivery.
func NewHandler(apps map[string]App, pending string, wake func()) (*Handler, error) {
	for app, credentials := range apps {
		if !appPattern.MatchString(app) || len(credentials.Secret) == 0 || credentials.VerifyToken == "" {
			return nil, fmt.Errorf("meta webhook app %q needs a secret and a verify token", app)
		}
	}
	if err := pack.MkdirAllSynced(pending); err != nil {
		return nil, fmt.Errorf("create Meta webhook directory: %w", err)
	}
	if err := removeStaging(pending); err != nil {
		return nil, err
	}
	files, bytes, last, err := scanPending(pending)
	if err != nil {
		return nil, err
	}
	return &Handler{
		apps: apps, pending: pending, wake: wake, bodies: make(chan struct{}, maxConcurrentBodies),
		maxFiles: maxPendingFiles, maxBytes: maxPendingBytes, last: last, files: files, bytes: bytes,
	}, nil
}

// removeStaging deletes atomicfile staging files, named
// ".<final name>.tmp-<random>", that a crash left in dir. Other dot-names,
// such as set-aside deliveries, stay.
func removeStaging(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read Meta webhook directory: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, ".") || !strings.Contains(name, ".tmp-") {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove Meta webhook staging file: %w", err)
		}
	}
	return nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	app, ok := strings.CutPrefix(r.URL.Path, "/meta/webhook/")
	credentials, known := h.apps[app]
	if !ok || !known {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		q := r.URL.Query()
		challenge := q.Get("hub.challenge")
		if q.Get("hub.mode") != "subscribe" || subtle.ConstantTimeCompare([]byte(q.Get("hub.verify_token")), []byte(credentials.VerifyToken)) != 1 || !challengePattern.MatchString(challenge) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, challenge) // #nosec G705 -- challengePattern allows only letters, digits, '-' and '_'; the response is text/plain.
	case http.MethodPost:
		// The early rejections close the connection, so the server answers
		// at once instead of first draining a body the client may withhold.
		signature := r.Header.Get("X-Hub-Signature-256")
		if !signaturePattern.MatchString(signature) {
			w.Header().Set("Connection", "close")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		select {
		case h.bodies <- struct{}{}:
			defer func() { <-h.bodies }()
		default:
			w.Header().Set("Connection", "close")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
		if err != nil {
			if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
				w.WriteHeader(http.StatusRequestEntityTooLarge)
			} else {
				w.WriteHeader(http.StatusBadRequest)
			}
			return
		}
		if !validSignature(credentials.Secret, body, signature) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		err = h.store(app, body)
		published, status := writeResult(err)
		switch {
		case errors.Is(err, errSpoolFull):
			slog.Warn("Meta webhook delivery refused; too many deliveries pending", "app", app)
		case err != nil && status == http.StatusOK:
			slog.Warn("Meta webhook delivery stored; staging cleanup failed", "app", app, "error", err)
		case err != nil:
			slog.Error("store Meta webhook delivery", "app", app, "error", err)
		}
		if published && h.wake != nil {
			h.wake()
		}
		w.WriteHeader(status)
	default:
		w.Header().Set("Allow", "GET, POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// writeResult reports whether store's error leaves the delivery in the
// pending directory, and the status to answer.
func writeResult(err error) (published bool, status int) {
	switch {
	case err == nil:
		return true, http.StatusOK
	case errors.Is(err, atomicfile.ErrNotDurable):
		// The worker will apply the visible file, but a crash could still
		// lose it, so 200 would break "durable before 200". Meta redelivers
		// after 503, and applying a delivery twice is harmless.
		return true, http.StatusServiceUnavailable
	case errors.Is(err, atomicfile.ErrPublished):
		// The delivery is durable; only removing the staging name failed.
		return true, http.StatusOK
	default:
		return false, http.StatusServiceUnavailable
	}
}

// validSignature checks Meta's HMAC-SHA256 of the exact received bytes.
func validSignature(secret, body []byte, header string) bool {
	hexSum, ok := strings.CutPrefix(header, "sha256=")
	got, err := hex.DecodeString(hexSum)
	if !ok || err != nil || len(got) != sha256.Size {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(body)
	return hmac.Equal(mac.Sum(nil), got)
}

// store writes the delivery under a name that sorts in arrival order and
// carries the app ID. The delivery becomes visible only once complete. It
// returns errSpoolFull, without writing, when the delivery would take the
// pending directory past maxFiles or maxBytes. A WriteNew error that wraps
// atomicfile.ErrPublished still counts the delivery; see writeResult.
func (h *Handler) store(app string, body []byte) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	size := int64(len(body))
	full := func() bool { return h.files+1 > h.maxFiles || h.bytes+size > h.maxBytes }
	if full() {
		// The worker deletes applied deliveries without telling the handler,
		// so the totals are an upper bound. Recount before refusing.
		files, bytes, _, err := scanPending(h.pending)
		if err != nil {
			return err
		}
		h.files, h.bytes = files, bytes
		if full() {
			return errSpoolFull
		}
	}
	h.last = max(time.Now().UnixNano(), h.last+1)
	name := fmt.Sprintf("%020d-%s-%s.json", h.last, app, rand.Text())
	err := atomicfile.WriteNew(filepath.Join(h.pending, name), body, atomicfile.WithPrivate())
	if published, _ := writeResult(err); published {
		h.files++
		h.bytes += size
	}
	return err
}

// scanPending returns the number and total size of the deliveries in dir,
// regular files whose names do not start with a dot, and the largest
// timestamp among their names.
func scanPending(dir string) (files int, bytes, last int64, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("read Meta webhook directory: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.Type().IsRegular() || strings.HasPrefix(name, ".") {
			continue
		}
		info, err := entry.Info()
		if errors.Is(err, fs.ErrNotExist) {
			continue // applied and deleted since ReadDir
		}
		if err != nil {
			return 0, 0, 0, fmt.Errorf("read Meta webhook delivery: %w", err)
		}
		files++
		bytes += info.Size()
		prefix, _, _ := strings.Cut(name, "-")
		if stamp, err := strconv.ParseInt(prefix, 10, 64); err == nil {
			last = max(last, stamp)
		}
	}
	return files, bytes, last, nil
}
