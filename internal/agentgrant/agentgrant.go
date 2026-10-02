package agentgrant

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
)

type Permission string

const (
	PermissionSearchRead     Permission = "search.read"
	PermissionMessageRead    Permission = "message.read"
	PermissionAttachmentRead Permission = "attachment.read"
	PermissionStatsRead      Permission = "stats.read"
	PermissionDraftCreate    Permission = "draft.create"
	PermissionDraftEdit      Permission = "draft.edit"
	PermissionDraftDelete    Permission = "draft.delete"
)

var knownPermissions = map[string]Permission{
	string(PermissionSearchRead):     PermissionSearchRead,
	string(PermissionMessageRead):    PermissionMessageRead,
	string(PermissionAttachmentRead): PermissionAttachmentRead,
	string(PermissionStatsRead):      PermissionStatsRead,
	string(PermissionDraftCreate):    PermissionDraftCreate,
	string(PermissionDraftEdit):      PermissionDraftEdit,
	string(PermissionDraftDelete):    PermissionDraftDelete,
}

func KnownPermission(s string) (Permission, bool) {
	p, ok := knownPermissions[s]
	return p, ok
}

// SourceRef carries the repo's durable source identity: (id, type, identifier).
// SourceRef carries ID as a diagnostic field only; matching uses (Type, Identifier)
// which are portable across re-adds (manifest.go:99-101).
type SourceRef struct {
	ID         int64
	Type       string
	Identifier string
	SenderKeys []string
}

type Grant struct {
	ID          string
	Label       string
	Permissions []Permission
	Sources     []SourceRef
	CreatedAt   time.Time
	ExpiresAt   time.Time
}

func (g Grant) HasPermission(p Permission) bool {
	return slices.Contains(g.Permissions, p)
}

// Allows returns true only when p is in the grant AND some SourceRef matches Type and Identifier.
func (g Grant) Allows(p Permission, src SourceRef) bool {
	if !g.HasPermission(p) {
		return false
	}
	for _, s := range g.Sources {
		if s.Type == src.Type && s.Identifier == src.Identifier {
			return true
		}
	}
	return false
}

// AllowsSender reports whether the grant permits one canonical sender on a
// source. An empty SenderKeys set grants no sender authority.
func (g Grant) AllowsSender(p Permission, src SourceRef, senderKey string) bool {
	if senderKey == "" || !g.HasPermission(p) {
		return false
	}
	for _, source := range g.Sources {
		if source.Type == src.Type && source.Identifier == src.Identifier && slices.Contains(source.SenderKeys, senderKey) {
			return true
		}
	}
	return false
}

func cloneGrant(g Grant) Grant {
	clone := Grant{
		ID:          g.ID,
		Label:       g.Label,
		Permissions: append([]Permission(nil), g.Permissions...),
		CreatedAt:   g.CreatedAt,
		ExpiresAt:   g.ExpiresAt,
		Sources:     make([]SourceRef, len(g.Sources)),
	}
	for i, source := range g.Sources {
		clone.Sources[i] = SourceRef{
			ID:         source.ID,
			Type:       source.Type,
			Identifier: source.Identifier,
			SenderKeys: append([]string(nil), source.SenderKeys...),
		}
	}
	return clone
}

const secretBytes = 32
const secretPrefix = "mva1_"

type entry struct {
	digest [32]byte
	grant  Grant
}

type Registry struct {
	store   Persistence
	mu      sync.Mutex
	entries map[string]entry // keyed by grant ID
}

func NewRegistry() *Registry {
	return &Registry{entries: make(map[string]entry)}
}

func (r *Registry) Issue(label string, perms []Permission, sources []SourceRef) (id, secret string, g Grant, err error) {
	return r.IssueExpires(context.Background(), label, perms, sources, time.Time{})
}
func (r *Registry) IssueExpires(ctx context.Context, label string, perms []Permission, sources []SourceRef, expires time.Time) (id, secret string, g Grant, err error) {
	if !expires.IsZero() && !expires.After(time.Now()) {
		return "", "", Grant{}, errors.New("agentgrant: expiry must be in the future")
	}

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
		ExpiresAt:   expires,
	}

	if r.store != nil {
		if err := r.store.SaveAgentGrant(ctx, Record{Digest: hex.EncodeToString(digest[:]), Grant: cloneGrant(g)}); err != nil {
			return "", "", Grant{}, fmt.Errorf("%w: %w", ErrPersistence, err)
		}
		return id, secretPlain, cloneGrant(g), nil
	}
	r.mu.Lock()
	r.entries[id] = entry{digest: digest, grant: cloneGrant(g)}
	r.mu.Unlock()

	return id, secretPlain, cloneGrant(g), nil
}

func (r *Registry) Lookup(secret string) (Grant, bool) {
	return r.LookupContext(context.Background(), secret)
}
func (r *Registry) LookupContext(ctx context.Context, secret string) (Grant, bool) {
	digest := sha256.Sum256([]byte(secret))
	if r.store != nil {
		record, ok, err := r.store.FindAgentGrant(ctx, hex.EncodeToString(digest[:]))
		if err != nil || !ok || (!record.Grant.ExpiresAt.IsZero() && !time.Now().Before(record.Grant.ExpiresAt)) {
			return Grant{}, false
		}
		return cloneGrant(record.Grant), true
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	for _, e := range r.entries {
		if subtle.ConstantTimeCompare(digest[:], e.digest[:]) == 1 && (e.grant.ExpiresAt.IsZero() || time.Now().Before(e.grant.ExpiresAt)) {
			return cloneGrant(e.grant), true
		}
	}
	return Grant{}, false
}

func (r *Registry) listMemory() []Grant {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Grant
	for _, e := range r.entries {
		out = append(out, cloneGrant(e.grant))
	}
	return out
}

func (r *Registry) Revoke(id string) bool {
	if r.store != nil {
		rows, err := r.store.ListAgentGrants(context.Background())
		if err != nil {
			return false
		}
		for _, row := range rows {
			if row.Grant.ID == id {
				return r.RevokeContext(context.Background(), id) == nil
			}
		}
		return false
	}
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
