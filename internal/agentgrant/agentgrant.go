package agentgrant

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"time"
)

type Permission string

const (
	PermissionDraftCreate Permission = "draft.create"
	PermissionMessageRead Permission = "message.read"
)

var knownPermissions = map[string]Permission{
	string(PermissionDraftCreate): PermissionDraftCreate,
	string(PermissionMessageRead): PermissionMessageRead,
}

func KnownPermission(s string) (Permission, bool) {
	p, ok := knownPermissions[s]
	return p, ok
}

func AllPermissions() []Permission {
	return []Permission{PermissionDraftCreate, PermissionMessageRead}
}

// SourceRef is the repo's durable source identity: (id, type, identifier).
// Matches internal/deletion/manifest.go:99-106.
type SourceRef struct {
	ID         int64
	Type       string
	Identifier string
}

type Grant struct {
	ID          string
	Label       string
	Permissions []Permission
	Sources     []SourceRef
	CreatedAt   time.Time
	ExpiresAt   time.Time
}

// Allows returns true only when p is in the grant AND some SourceRef matches all three fields.
func (g Grant) Allows(p Permission, src SourceRef) bool {
	hasPerm := false
	for _, gp := range g.Permissions {
		if gp == p {
			hasPerm = true
			break
		}
	}
	if !hasPerm {
		return false
	}
	for _, s := range g.Sources {
		if s.ID == src.ID && s.Type == src.Type && s.Identifier == src.Identifier {
			return true
		}
	}
	return false
}

const DefaultLifetime = 24 * time.Hour
const secretBytes = 32
const secretPrefix = "mva1_"

type entry struct {
	digest [32]byte
	grant  Grant
}

type Registry struct {
	mu      sync.Mutex
	entries map[string]entry // keyed by grant ID
	now     func() time.Time
}

func NewRegistry(now func() time.Time) *Registry {
	return &Registry{entries: make(map[string]entry), now: now}
}

func (r *Registry) Issue(label string, perms []Permission, sources []SourceRef, lifetime time.Duration) (id, secret string, g Grant, err error) {
	if label == "" {
		return "", "", Grant{}, errors.New("agentgrant: label must not be empty")
	}
	if len(perms) == 0 {
		return "", "", Grant{}, errors.New("agentgrant: permission set must not be empty")
	}
	if len(sources) == 0 {
		return "", "", Grant{}, errors.New("agentgrant: source set must not be empty")
	}
	// validate perms
	for _, p := range perms {
		if p == "*" || string(p) == "" {
			return "", "", Grant{}, fmt.Errorf("agentgrant: invalid permission %q", p)
		}
		if _, ok := knownPermissions[string(p)]; !ok {
			return "", "", Grant{}, fmt.Errorf("agentgrant: unknown permission %q", p)
		}
	}
	// validate sources
	seen := make(map[int64]struct{})
	for _, s := range sources {
		if s.ID <= 0 {
			return "", "", Grant{}, fmt.Errorf("agentgrant: source ID must be positive, got %d", s.ID)
		}
		if _, dup := seen[s.ID]; dup {
			return "", "", Grant{}, fmt.Errorf("agentgrant: duplicate source ID %d", s.ID)
		}
		seen[s.ID] = struct{}{}
	}
	if lifetime <= 0 {
		lifetime = DefaultLifetime
	}

	// generate ID
	var idBuf [16]byte
	if _, err = rand.Read(idBuf[:]); err != nil {
		return "", "", Grant{}, fmt.Errorf("agentgrant: generate id: %w", err)
	}
	id = base64.RawURLEncoding.EncodeToString(idBuf[:])

	// generate secret
	var secretBuf [secretBytes]byte
	if _, err = rand.Read(secretBuf[:]); err != nil {
		return "", "", Grant{}, fmt.Errorf("agentgrant: generate secret: %w", err)
	}
	secretPlain := secretPrefix + base64.RawURLEncoding.EncodeToString(secretBuf[:])
	digest := sha256.Sum256([]byte(secretPlain))

	now := r.now()
	g = Grant{
		ID:          id,
		Label:       label,
		Permissions: append([]Permission(nil), perms...),
		Sources:     append([]SourceRef(nil), sources...),
		CreatedAt:   now,
		ExpiresAt:   now.Add(lifetime),
	}

	r.mu.Lock()
	r.entries[id] = entry{digest: digest, grant: g}
	r.mu.Unlock()

	return id, secretPlain, g, nil
}

func (r *Registry) Lookup(secret string) (Grant, bool) {
	digest := sha256.Sum256([]byte(secret))
	now := r.now()

	r.mu.Lock()
	defer r.mu.Unlock()

	for id, e := range r.entries {
		if !e.grant.ExpiresAt.After(now) {
			delete(r.entries, id)
			continue
		}
		if subtle.ConstantTimeCompare(digest[:], e.digest[:]) == 1 {
			g := e.grant
			return Grant{
				ID:          g.ID,
				Label:       g.Label,
				Permissions: append([]Permission(nil), g.Permissions...),
				Sources:     append([]SourceRef(nil), g.Sources...),
				CreatedAt:   g.CreatedAt,
				ExpiresAt:   g.ExpiresAt,
			}, true
		}
	}
	return Grant{}, false
}

func (r *Registry) List() []Grant {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Grant
	for id, e := range r.entries {
		if !e.grant.ExpiresAt.After(now) {
			delete(r.entries, id)
			continue
		}
		g := e.grant
		out = append(out, Grant{
			ID:          g.ID,
			Label:       g.Label,
			Permissions: append([]Permission(nil), g.Permissions...),
			Sources:     append([]SourceRef(nil), g.Sources...),
			CreatedAt:   g.CreatedAt,
			ExpiresAt:   g.ExpiresAt,
		})
	}
	return out
}

func (r *Registry) Revoke(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.entries[id]
	delete(r.entries, id)
	return ok
}

func (r *Registry) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = make(map[string]entry)
}
