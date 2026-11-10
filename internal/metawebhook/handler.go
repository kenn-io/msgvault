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
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"go.kenn.io/kit/atomicfile"
)

// maxBodyBytes bounds one delivery. Meta batches at most 1,000 changes.
const maxBodyBytes = 8 << 20

var (
	appPattern       = regexp.MustCompile(`^[1-9][0-9]*$`)
	challengePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
)

// App holds one Meta app's webhook credentials.
type App struct {
	Secret      []byte
	VerifyToken string
}

// Handler serves /meta/webhook/{app_id}. GET answers Meta's subscription
// challenge. POST verifies the delivery signature and writes the body to the
// pending directory before it answers 200, so an accepted delivery survives a
// restart.
type Handler struct {
	apps    map[string]App
	pending string
	wake    func()
}

// NewHandler stores deliveries in pending. wake, when set, is called after
// each stored delivery.
func NewHandler(apps map[string]App, pending string, wake func()) (*Handler, error) {
	for app, credentials := range apps {
		if !appPattern.MatchString(app) || len(credentials.Secret) == 0 || credentials.VerifyToken == "" {
			return nil, fmt.Errorf("meta webhook app %q needs a secret and a verify token", app)
		}
	}
	if err := os.MkdirAll(pending, 0o700); err != nil {
		return nil, fmt.Errorf("create Meta webhook directory: %w", err)
	}
	return &Handler{apps: apps, pending: pending, wake: wake}, nil
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
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
		if err != nil {
			if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
				w.WriteHeader(http.StatusRequestEntityTooLarge)
			} else {
				w.WriteHeader(http.StatusBadRequest)
			}
			return
		}
		if !validSignature(credentials.Secret, body, r.Header.Get("X-Hub-Signature-256")) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if err := h.store(app, body); err != nil {
			slog.Error("store Meta webhook delivery", "app", app, "error", err)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if h.wake != nil {
			h.wake()
		}
		w.WriteHeader(http.StatusOK)
	default:
		w.Header().Set("Allow", "GET, POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
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
// carries the app ID. The delivery becomes visible only once complete.
func (h *Handler) store(app string, body []byte) error {
	name := fmt.Sprintf("%020d-%s-%s.json", time.Now().UnixNano(), app, rand.Text())
	return atomicfile.WriteNew(filepath.Join(h.pending, name), body, atomicfile.WithPrivate())
}
