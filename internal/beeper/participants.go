package beeper

import (
	"strings"

	"go.kenn.io/msgvault/internal/store"
)

// participantIdentifierType namespaces Beeper user IDs in
// participant_identifiers. The IDs are Beeper-local Matrix-style IDs
// (@x:beeper.local), not federated Matrix IDs.
const participantIdentifierType = "beeper"

// participantResolver resolves Beeper users to msgvault participant IDs,
// cached per run by Beeper user ID.
//
// Resolution ladder (exclusive — EnsureParticipantByIdentifier creates a new
// participant for unseen identifiers, so calling two rungs for one person
// would fork them):
//  1. E.164 phone number → EnsureParticipantByPhone, deduping people across
//     Beeper and native SMS/WhatsApp archives.
//  2. Email → EnsureParticipant, deduping against mail archives.
//  3. Beeper user ID → EnsureParticipantByIdentifier("beeper", …).
//
// Phone/email are only present on chat participant entries, so the resolver
// is seeded from each chat's participant list; message senders then hit the
// cache by user ID. Senders absent from the list (departed members) fall
// through to rung 3.
type participantResolver struct {
	store *store.Store
	cache map[string]int64
}

func newParticipantResolver(s *store.Store) *participantResolver {
	return &participantResolver{store: s, cache: map[string]int64{}}
}

func (r *participantResolver) resolveUser(u *User) (int64, error) {
	if u == nil || u.ID == "" {
		return 0, nil
	}
	if pid, ok := r.cache[u.ID]; ok {
		return pid, nil
	}
	var pid int64
	var err error
	switch {
	case strings.HasPrefix(u.PhoneNumber, "+"):
		pid, err = r.store.EnsureParticipantByPhone(u.PhoneNumber, u.FullName, participantIdentifierType)
	case strings.Contains(u.Email, "@"):
		pid, err = r.byEmail(u.Email, u.FullName)
	default:
		pid, err = r.store.EnsureParticipantByIdentifier(participantIdentifierType, u.ID, u.FullName)
	}
	if err != nil {
		return 0, err
	}
	r.cache[u.ID] = pid
	return pid, nil
}

// resolveID resolves a bare Beeper user ID (message sender, mention, reactor)
// with an optional display name. Cache misses fall through to rung 3 of the
// ladder since no phone/email is available on bare IDs.
func (r *participantResolver) resolveID(userID, displayName string) (int64, error) {
	if userID == "" {
		return 0, nil
	}
	if pid, ok := r.cache[userID]; ok {
		return pid, nil
	}
	pid, err := r.store.EnsureParticipantByIdentifier(participantIdentifierType, userID, displayName)
	if err != nil {
		return 0, err
	}
	r.cache[userID] = pid
	return pid, nil
}

func (r *participantResolver) byEmail(email, displayName string) (int64, error) {
	email = strings.ToLower(email)
	domain := ""
	if at := strings.LastIndex(email, "@"); at >= 0 {
		domain = email[at+1:]
	}
	return r.store.EnsureParticipant(email, displayName, domain)
}
