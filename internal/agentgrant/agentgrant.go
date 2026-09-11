package agentgrant

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
)

type Permission string

const (
	PermissionDraftCreate Permission = "draft.create"
	PermissionDraftRead   Permission = "draft.read"
	PermissionDraftEdit   Permission = "draft.edit"
	PermissionDraftDelete Permission = "draft.delete"
)

var knownPermissions = map[string]Permission{
	string(PermissionDraftCreate): PermissionDraftCreate,
	string(PermissionDraftRead):   PermissionDraftRead,
	string(PermissionDraftEdit):   PermissionDraftEdit,
	string(PermissionDraftDelete): PermissionDraftDelete,
}

func KnownPermission(s string) (Permission, bool) {
	p, ok := knownPermissions[s]
	return p, ok
}

func AllPermissions() []Permission {
	return []Permission{PermissionDraftCreate, PermissionDraftRead, PermissionDraftEdit, PermissionDraftDelete}
}

// SourceRef carries the repo's durable source identity: (id, type, identifier).
// SourceRef carries ID as a diagnostic field only; matching uses (Type, Identifier)
// which are portable across re-adds (manifest.go:99-101).
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
}

// Allows returns true only when p is in the grant AND some SourceRef matches Type and Identifier.
func (g Grant) Allows(p Permission, src SourceRef) bool {
	if !slices.Contains(g.Permissions, p) {
		return false
	}
	for _, s := range g.Sources {
		if s.Type == src.Type && s.Identifier == src.Identifier {
			return true
		}
	}
	return false
}

const secretBytes = 32
const secretPrefix = "mva1_"

type entry struct {
	digest [32]byte
	grant  Grant
}

type Registry struct {
	mu      sync.Mutex
	entries map[string]entry // keyed by grant ID
}

func NewRegistry() *Registry {
	return &Registry{entries: make(map[string]entry)}
}

func (r *Registry) Issue(label string, perms []Permission, sources []SourceRef) (id, secret string, g Grant, err error) {
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
	seen := make(map[string]struct{})
	for _, s := range sources {
		if s.ID <= 0 {
			return "", "", Grant{}, fmt.Errorf("agentgrant: source ID must be positive, got %d", s.ID)
		}
		if s.Type == "" {
			return "", "", Grant{}, errors.New("agentgrant: source Type must not be empty")
		}
		if s.Identifier == "" {
			return "", "", Grant{}, errors.New("agentgrant: source Identifier must not be empty")
		}
		key := s.Type + "\x00" + s.Identifier
		if _, dup := seen[key]; dup {
			return "", "", Grant{}, fmt.Errorf("agentgrant: duplicate source (Type=%q, Identifier=%q)", s.Type, s.Identifier)
		}
		seen[key] = struct{}{}
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

	g = Grant{
		ID:          id,
		Label:       label,
		Permissions: append([]Permission(nil), perms...),
		Sources:     append([]SourceRef(nil), sources...),
		CreatedAt:   time.Now(),
	}

	r.mu.Lock()
	r.entries[id] = entry{digest: digest, grant: g}
	r.mu.Unlock()

	return id, secretPlain, g, nil
}

func (r *Registry) Lookup(secret string) (Grant, bool) {
	digest := sha256.Sum256([]byte(secret))

	r.mu.Lock()
	defer r.mu.Unlock()

	for _, e := range r.entries {
		if subtle.ConstantTimeCompare(digest[:], e.digest[:]) == 1 {
			g := e.grant
			return Grant{
				ID:          g.ID,
				Label:       g.Label,
				Permissions: append([]Permission(nil), g.Permissions...),
				Sources:     append([]SourceRef(nil), g.Sources...),
				CreatedAt:   g.CreatedAt,
			}, true
		}
	}
	return Grant{}, false
}

func (r *Registry) List() []Grant {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Grant
	for _, e := range r.entries {
		g := e.grant
		out = append(out, Grant{
			ID:          g.ID,
			Label:       g.Label,
			Permissions: append([]Permission(nil), g.Permissions...),
			Sources:     append([]SourceRef(nil), g.Sources...),
			CreatedAt:   g.CreatedAt,
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
