package teams

import (
	"context"
	"errors"
	"strings"

	"go.kenn.io/msgvault/internal/store"
)

type userLookup interface {
	GetUser(ctx context.Context, id string) (*GraphUser, error)
}

type participantResolver struct {
	store  *store.Store
	lookup userLookup
	cache  map[string]int64
	// mail contains only committed rich results; cached IDs never grant admission.
	mail map[string]string
	// fetched retains raw lookup results, including misses, for this resolver only.
	// These values never grant profile admission or confirmed-self authority.
	fetched map[string]string
}

func newParticipantResolver(s *store.Store, lookup userLookup) *participantResolver {
	return &participantResolver{store: s, lookup: lookup, cache: map[string]int64{}, mail: map[string]string{}, fetched: map[string]string{}}
}

func (r *participantResolver) resolve(ctx context.Context, id *Identity, room int64) (int64, error) {
	if id == nil || id.ID == "" {
		return 0, nil
	}
	email := ""
	switch id.UserIdentityType {
	case "emailUser":
		email = id.ID
	case "aadUser", "onPremiseAadUser":
		email = r.lookupMail(ctx, id.ID)
	}
	return r.resolveIdentity(ctx, room, id.ID, id.DisplayName, email)
}

func (r *participantResolver) resolveIdentity(ctx context.Context, room int64, id, name, email string) (int64, error) {
	result, err := r.store.ResolveConversationParticipantContext(ctx, room, store.ConversationParticipantIdentity{IdentifierType: "teams", IdentifierValue: id, DisplayName: name, Email: email})
	if errors.Is(err, store.ErrConversationPolicyStale) || errors.Is(err, store.ErrPersonBindingConflict) {
		pid, fallbackErr := r.store.EnsureParticipantByIdentifierContext(ctx, "teams", id, "")
		if fallbackErr == nil {
			r.cache[id] = pid
		}
		return pid, fallbackErr
	}
	if err != nil {
		return 0, err
	}
	r.cache[id] = result.ParticipantID
	r.mail[id] = email
	return result.ParticipantID, nil
}

func (r *participantResolver) lookupMail(ctx context.Context, objectID string) string {
	if email, ok := r.fetched[objectID]; ok {
		return email
	}
	if email, ok := r.mail[objectID]; ok {
		return email
	}
	email := ""
	if r.lookup != nil {
		u, err := r.lookup.GetUser(ctx, objectID)
		if err == nil && u != nil {
			if u.Mail != "" {
				email = u.Mail
			} else if strings.Contains(u.UserPrincipalName, "@") && !strings.Contains(u.UserPrincipalName, "#EXT#") {
				email = u.UserPrincipalName
			}
		}
	}
	r.fetched[objectID] = email
	return email
}

// resolveMember retains raw roster metadata separately from admitted profiles.
// Shared rich cache publication still requires this concrete room's admission.
func (r *participantResolver) resolveMember(ctx context.Context, m ChatMember, room int64) (int64, error) {
	id := m.UserID
	if id == "" {
		id = m.ID
	}
	if id == "" {
		return 0, nil
	}
	// Only concrete fetched roster proof may normalize existing confirmed self.
	// Ordinary profile lookup cannot call this path or publish its mail cache.
	if m.UserID != "" && m.Email != "" {
		if _, _, err := r.store.NormalizeTeamsConversationSelfContext(ctx, room, m.UserID, m.Email); err != nil {
			return 0, err
		}
	}
	email := m.Email
	if email == "" {
		email = r.lookupMail(ctx, id)
	} else {
		r.fetched[id] = email
	}
	return r.resolveIdentity(ctx, room, id, m.DisplayName, email)
}
