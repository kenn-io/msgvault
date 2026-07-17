package teams

import (
	"context"
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
}

func newParticipantResolver(s *store.Store, lookup userLookup) *participantResolver {
	return &participantResolver{store: s, lookup: lookup, cache: map[string]int64{}}
}

func (r *participantResolver) resolve(ctx context.Context, id *Identity) (int64, error) {
	if id == nil || id.ID == "" {
		return 0, nil
	}
	if pid, ok := r.cache[id.ID]; ok {
		return pid, nil
	}
	var pid int64
	var err error
	switch id.UserIdentityType {
	case "emailUser":
		pid, err = r.byEmail(id.ID, id.DisplayName)
	case "aadUser", "onPremiseAadUser":
		email := r.lookupMail(ctx, id.ID)
		if email != "" {
			pid, err = r.byEmail(email, id.DisplayName)
		} else {
			pid, err = r.store.EnsureParticipantByIdentifier("teams", id.ID, id.DisplayName)
		}
	default:
		pid, err = r.store.EnsureParticipantByIdentifier("teams", id.ID, id.DisplayName)
	}
	if err != nil {
		return 0, err
	}
	r.cache[id.ID] = pid
	return pid, nil
}

func (r *participantResolver) byEmail(email, displayName string) (int64, error) {
	domain := ""
	if at := strings.LastIndex(email, "@"); at >= 0 {
		domain = strings.ToLower(email[at+1:])
	}
	return r.store.EnsureParticipant(strings.ToLower(email), displayName, domain)
}

func (r *participantResolver) lookupMail(ctx context.Context, objectID string) string {
	if r.lookup == nil {
		return ""
	}
	u, err := r.lookup.GetUser(ctx, objectID)
	if err != nil || u == nil {
		return ""
	}
	if u.Mail != "" {
		return u.Mail
	}
	if strings.Contains(u.UserPrincipalName, "@") && !strings.Contains(u.UserPrincipalName, "#EXT#") {
		return u.UserPrincipalName
	}
	return ""
}

// resolveMember resolves a ChatMember to a participant ID, using the member's
// email when available and falling back to identifier-based resolution otherwise.
// The resolved ID is cached by the member's user object ID to enable mention
// resolution to find the same participant via the participant cache.
func (r *participantResolver) resolveMember(ctx context.Context, m ChatMember) (int64, error) {
	id := m.UserID
	if id == "" {
		id = m.ID
	}
	if id == "" {
		return 0, nil
	}
	if pid, ok := r.cache[id]; ok {
		return pid, nil
	}
	var pid int64
	var err error
	if m.Email != "" {
		pid, err = r.byEmail(m.Email, m.DisplayName)
	} else {
		pid, err = r.resolve(ctx, &Identity{ID: id, DisplayName: m.DisplayName, UserIdentityType: "aadUser"})
	}
	if err != nil {
		return 0, err
	}
	if pid != 0 {
		r.cache[id] = pid
	}
	return pid, nil
}
