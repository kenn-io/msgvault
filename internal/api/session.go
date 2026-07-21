package api

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"io"
	"sync"
	"time"
)

const (
	defaultSessionTTL = 24 * time.Hour
	randomTokenBytes  = 32
	// Keep login latency independent of the total number of stored sessions.
	// New logins opportunistically reclaim expired entries in bounded batches.
	sessionPurgeScanLimit = 16
)

type browserSession struct {
	CSRFToken string
	ExpiresAt time.Time
}

type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]browserSession
	ttl      time.Duration
	now      func() time.Time
	random   io.Reader
}

func newSessionStore(ttl time.Duration) *sessionStore {
	return &sessionStore{
		sessions: make(map[string]browserSession),
		ttl:      ttl,
		now:      time.Now,
		random:   rand.Reader,
	}
}

func (s *sessionStore) create() (string, browserSession, error) {
	id, err := s.randomToken()
	if err != nil {
		return "", browserSession{}, fmt.Errorf("generate session ID: %w", err)
	}
	csrfToken, err := s.randomToken()
	if err != nil {
		return "", browserSession{}, fmt.Errorf("generate CSRF token: %w", err)
	}
	now := s.now()
	session := browserSession{
		CSRFToken: csrfToken,
		ExpiresAt: now.Add(s.ttl),
	}
	s.mu.Lock()
	s.purgeExpiredLocked(now)
	s.sessions[id] = session
	s.mu.Unlock()
	return id, session, nil
}

func (s *sessionStore) purgeExpiredLocked(now time.Time) {
	inspected := 0
	for id, session := range s.sessions {
		if inspected == sessionPurgeScanLimit {
			return
		}
		inspected++
		if !now.Before(session.ExpiresAt) {
			delete(s.sessions, id)
		}
	}
}

func (s *sessionStore) lookup(id string) (browserSession, bool) {
	if id == "" {
		return browserSession{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[id]
	if !ok {
		return browserSession{}, false
	}
	if !s.now().Before(session.ExpiresAt) {
		delete(s.sessions, id)
		return browserSession{}, false
	}
	return session, true
}

func (s *sessionStore) delete(id string) {
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
}

func (s *sessionStore) Close() {
	s.mu.Lock()
	clear(s.sessions)
	s.mu.Unlock()
}

func (s *sessionStore) randomToken() (string, error) {
	raw := make([]byte, randomTokenBytes)
	if _, err := io.ReadFull(s.random, raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func constantTimeAPIKeyEqual(got, want string) bool {
	gotHash := sha256.Sum256([]byte(got))
	wantHash := sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(gotHash[:], wantHash[:]) == 1
}
