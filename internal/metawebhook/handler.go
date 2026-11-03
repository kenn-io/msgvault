package metawebhook

import (
	"crypto/subtle"
	"errors"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

type AppCredentials struct {
	Secret      []byte
	VerifyToken string
}
type HandlerOptions struct {
	Apps         map[string]AppCredentials
	Spool        *Spool
	MaxBodyBytes int64
	MaxInFlight  int
	Wake         func()
}
type Handler struct {
	options                                                        HandlerOptions
	mux                                                            *http.ServeMux
	slots                                                          chan struct{}
	accepted, duplicates, signatureFailures, tooLarge, unavailable atomic.Uint64
	lastAccepted                                                   atomic.Int64
}

var challengePattern = regexp.MustCompile(`^[a-zA-Z0-9]{1,64}$`)

func NewHandler(o HandlerOptions) (*Handler, error) {
	if o.Spool == nil || o.MaxBodyBytes < 0 || o.MaxInFlight < 0 {
		return nil, errors.New("invalid webhook handler options")
	}
	for app, credentials := range o.Apps {
		if !appPattern.MatchString(app) || len(credentials.Secret) == 0 || credentials.VerifyToken == "" {
			return nil, errors.New("missing webhook app credentials")
		}
	}
	if o.MaxBodyBytes == 0 {
		o.MaxBodyBytes = 8 << 20
	}
	if o.MaxInFlight == 0 {
		o.MaxInFlight = 16
	}
	h := &Handler{options: o, mux: http.NewServeMux(), slots: make(chan struct{}, o.MaxInFlight)}
	h.mux.HandleFunc("/meta/webhook/{app_id}", h.deliver)
	return h, nil
}
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Reject malformed paths before ServeMux can redirect and reflect them.
	if !strings.HasPrefix(r.URL.Path, "/meta/webhook/") || !appPattern.MatchString(strings.TrimPrefix(r.URL.Path, "/meta/webhook/")) {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	default:
		h.unavailable.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	h.mux.ServeHTTP(w, r)
}
func (h *Handler) deliver(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("app_id")
	credentials, ok := h.options.Apps[app]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
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
		_, _ = io.WriteString(w, challenge) // #nosec G705 -- challengePattern permits only bounded ASCII letters and digits; response is text/plain.
	case http.MethodPost:
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.options.MaxBodyBytes))
		if err != nil {
			if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
				h.tooLarge.Add(1)
				w.WriteHeader(http.StatusRequestEntityTooLarge)
			} else {
				w.WriteHeader(http.StatusBadRequest)
			}
			return
		}
		if VerifySignature(credentials.Secret, body, r.Header.Get("X-Hub-Signature-256")) != nil {
			h.signatureFailures.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if value := r.Header.Get("Content-Type"); value != "" {
			kind, _, err := mime.ParseMediaType(value)
			if err != nil || kind != "application/json" {
				w.WriteHeader(http.StatusUnsupportedMediaType)
				return
			}
		}
		_, duplicate, err := h.options.Spool.Accept(app, body)
		if err != nil {
			h.unavailable.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if duplicate {
			h.duplicates.Add(1)
		} else {
			h.accepted.Add(1)
		}
		h.lastAccepted.Store(time.Now().UnixNano())
		if h.options.Wake != nil {
			h.options.Wake()
		}
		w.WriteHeader(http.StatusOK)
	default:
		w.Header().Set("Allow", "GET, POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}
func (h *Handler) Counters() Counters {
	c := Counters{Accepted: h.accepted.Load(), Duplicates: h.duplicates.Load(), SignatureFailures: h.signatureFailures.Load(), TooLarge: h.tooLarge.Load(), Unavailable: h.unavailable.Load()}
	if stamp := h.lastAccepted.Load(); stamp != 0 {
		c.LastAccepted = time.Unix(0, stamp).UTC()
	}
	return c
}
