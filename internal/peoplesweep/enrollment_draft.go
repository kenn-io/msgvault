package peoplesweep

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

const EnrollmentDraftLifetime = 10 * time.Minute

var (
	ErrEnrollmentDraftNotFound = errors.New("people provider enrollment draft was not found")
	ErrEnrollmentDraftActive   = errors.New("a people provider enrollment draft is already active")
	ErrEnrollmentDraftInvalid  = errors.New("people provider enrollment draft input is invalid")
)

// EnrollmentDraft is a short lived, model-less enrollment context. It carries
// no Codex token or API key and cannot authorize an archive request.
type EnrollmentDraft struct {
	ID        string    `json:"id"`
	Provider  string    `json:"provider"`
	Model     string    `json:"model,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
	owner     string
}

// EnrollmentDrafts owns transient setup state for one daemon. A browser
// session or terminal session supplies its own opaque owner identifier.
type EnrollmentDrafts struct {
	mu      sync.Mutex
	now     func() time.Time
	byID    map[string]EnrollmentDraft
	byOwner map[string]string
}

func NewEnrollmentDrafts(now func() time.Time) *EnrollmentDrafts {
	if now == nil {
		now = time.Now
	}
	return &EnrollmentDrafts{
		now: now, byID: make(map[string]EnrollmentDraft), byOwner: make(map[string]string),
	}
}

// Create allocates one active Codex draft per owner before model discovery.
func (d *EnrollmentDrafts) Create(owner, provider string) (EnrollmentDraft, error) {
	if strings.TrimSpace(owner) != owner || owner == "" || len(owner) > 128 || provider != "codex" {
		return EnrollmentDraft{}, ErrEnrollmentDraftInvalid
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now().UTC()
	d.expire(now)
	if _, exists := d.byOwner[owner]; exists {
		return EnrollmentDraft{}, ErrEnrollmentDraftActive
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return EnrollmentDraft{}, fmt.Errorf("generate enrollment draft ID: %w", err)
	}
	draft := EnrollmentDraft{
		ID:       base64.RawURLEncoding.EncodeToString(random[:]),
		Provider: provider, ExpiresAt: now.Add(EnrollmentDraftLifetime), owner: owner,
	}
	d.byID[draft.ID] = draft
	d.byOwner[owner] = draft.ID
	return draft, nil
}

// Get returns a draft only to its owner. Expired drafts are removed first.
func (d *EnrollmentDrafts) Get(owner, id string) (EnrollmentDraft, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.expire(d.now().UTC())
	draft, exists := d.byID[id]
	if !exists || draft.owner != owner {
		return EnrollmentDraft{}, ErrEnrollmentDraftNotFound
	}
	return draft, nil
}

// Cancel removes only the owner's exact draft.
func (d *EnrollmentDrafts) Cancel(owner, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.expire(d.now().UTC())
	draft, exists := d.byID[id]
	if !exists || draft.owner != owner {
		return ErrEnrollmentDraftNotFound
	}
	delete(d.byID, id)
	delete(d.byOwner, owner)
	return nil
}

// expire runs under mu. Draft state is never persisted or logged.
func (d *EnrollmentDrafts) expire(now time.Time) {
	for id, draft := range d.byID {
		if !now.Before(draft.ExpiresAt) {
			delete(d.byID, id)
			delete(d.byOwner, draft.owner)
		}
	}
}
