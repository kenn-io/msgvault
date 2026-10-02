package mcp

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"sync"
	"time"
)

const (
	maxConfirmationClients    = 1024
	confirmationClientIdleTTL = 30 * time.Minute
)

type confirmationClientContextKey struct{}
type confirmationClients struct {
	mu     sync.Mutex
	active map[string]time.Time
	now    func() time.Time
	closed bool
}

func newConfirmationClients() *confirmationClients {
	return &confirmationClients{active: make(map[string]time.Time), now: time.Now}
}

func confirmationClientKey(ctx context.Context) string {
	key, _ := ctx.Value(confirmationClientContextKey{}).(string)
	return key
}

// admit validates issued transport identities. Caller-chosen headers never
// become confirmation authority, and live clients are not evicted at capacity.
func (c *confirmationClients) admit(id string) (string, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return "", http.StatusServiceUnavailable
	}
	now := c.now()
	for key, expires := range c.active {
		if !expires.After(now) {
			delete(c.active, key)
		}
	}
	if id != "" {
		if _, ok := c.active[id]; !ok {
			return "", http.StatusNotFound
		}
	} else {
		if len(c.active) >= maxConfirmationClients {
			return "", http.StatusServiceUnavailable
		}
		entropy := make([]byte, 32)
		if _, err := rand.Read(entropy); err != nil {
			return "", http.StatusServiceUnavailable
		}
		id = base64.RawURLEncoding.EncodeToString(entropy)
	}
	c.active[id] = now.Add(confirmationClientIdleTTL)
	return id, 0
}

func (c *confirmationClients) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.Header.Values("Mcp-Session-Id")) > 1 {
			http.Error(w, "invalid confirmation client", http.StatusBadRequest)
			return
		}
		id, status := c.admit(r.Header.Get("Mcp-Session-Id"))
		if status != 0 {
			http.Error(w, "confirmation client unavailable", status)
			return
		}
		w.Header().Set("Mcp-Session-Id", id)
		ctx := context.WithValue(r.Context(), confirmationClientContextKey{}, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (c *confirmationClients) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	clear(c.active)
	c.closed = true
}
