package tiktokwebhook

import (
	"io"
	"net/http"
	"strings"
	"time"
)

type Handler struct {
	Apps         map[string][]byte
	Spool        *Spool
	MaxBodyBytes int64
	Now          func() time.Time
	Wake         func()
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	app, ok := strings.CutPrefix(r.URL.Path, "/webhooks/tiktok/")
	secret, configured := h.Apps[app]
	if !ok || !configured {
		http.NotFound(w, r)
		return
	}
	maximum := h.MaxBodyBytes
	if maximum <= 0 {
		maximum = 1 << 20
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maximum+1))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if int64(len(body)) > maximum {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}
	now := time.Now()
	if h.Now != nil {
		now = h.Now()
	}
	headers := r.Header.Values("Tiktok-Signature")
	if len(headers) != 1 || !VerifySignature(secret, headers[0], body, now) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if h.Spool == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if _, _, err := h.Spool.Accept(r.Context(), app, body); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if h.Wake != nil {
		h.Wake()
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"code":0,"message":"success"}`)
}
