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
// cache by user ID. Rich (phone/email) resolutions and bare-ID fallbacks are
// cached separately: a fallback cached in one chat must not block a later
// chat's richer metadata from deduping the person across archives.
// richEntry is a cached phone/email-based resolution. Email entries stay
// upgradeable: phone outranks email in the ladder, so a user first seen with
// only an email must still dedupe by phone when one appears later.
type richEntry struct {
	pid     int64
	byPhone bool
}

type participantResolver struct {
	store    *store.Store
	rich     map[string]richEntry // phone/email-based resolutions
	fallback map[string]int64     // bare-ID fallbacks; upgraded when metadata appears
}

func newParticipantResolver(s *store.Store) *participantResolver {
	return &participantResolver{store: s, rich: map[string]richEntry{}, fallback: map[string]int64{}}
}

func (r *participantResolver) resolveUser(u *User) (int64, error) {
	if u == nil || u.ID == "" {
		return 0, nil
	}
	hasPhone := strings.HasPrefix(u.PhoneNumber, "+")
	if e, ok := r.rich[u.ID]; ok && (e.byPhone || !hasPhone) {
		return e.pid, nil
	}
	switch {
	case hasPhone:
		pid, err := r.store.EnsureParticipantByPhone(u.PhoneNumber, u.FullName, participantIdentifierType)
		if err != nil {
			return 0, err
		}
		// Upgrading an earlier email-based resolution: both rows are the
		// same Beeper user, so fold the email participant's history into the
		// phone one (which dedupes with SMS/WhatsApp archives).
		if e, ok := r.rich[u.ID]; ok && e.pid != pid {
			if err := r.store.MergeParticipants(e.pid, pid); err != nil {
				return 0, err
			}
		}
		return pid, r.recordRich(u.ID, pid, true)
	case strings.Contains(u.Email, "@"):
		// Never downgrade: if a prior run already resolved this Beeper user
		// by phone, an email-only sighting must keep pointing at that
		// participant instead of re-pointing the identifier to a weaker row.
		prev, hasPhone, err := r.store.ParticipantByIdentifier(participantIdentifierType, u.ID)
		if err != nil {
			return 0, err
		}
		if prev != 0 && hasPhone {
			r.rich[u.ID] = richEntry{pid: prev, byPhone: true}
			delete(r.fallback, u.ID)
			return prev, nil
		}
		pid, err := r.byEmail(u.Email, u.FullName)
		if err != nil {
			return 0, err
		}
		return pid, r.recordRich(u.ID, pid, false)
	default:
		return r.resolveID(u.ID, u.FullName)
	}
}

// recordRich caches a phone/email-based resolution and persists the Beeper
// user ID as an identifier of that participant, so later runs (fresh caches)
// resolve bare sender IDs to the same person. When a weak (bare-ID-only)
// participant already owns the identifier, its history — messages, reactions,
// membership — is merged into the rich participant rather than left split.
func (r *participantResolver) recordRich(userID string, pid int64, byPhone bool) error {
	prev, hasPhone, err := r.store.ParticipantByIdentifier(participantIdentifierType, userID)
	if err != nil {
		return err
	}
	// A prior owner without a phone — a bare-ID fallback or an email-only
	// resolution — is the same Beeper user seen with weaker metadata: fold
	// its history into this resolution. A phone-bearing owner whose number
	// differs is left un-merged (conservative: numbers can be recycled).
	if prev != 0 && prev != pid && !hasPhone {
		if err := r.store.MergeParticipants(prev, pid); err != nil {
			return err
		}
	}
	if err := r.store.SetParticipantIdentifier(pid, participantIdentifierType, userID); err != nil {
		return err
	}
	r.rich[userID] = richEntry{pid: pid, byPhone: byPhone}
	delete(r.fallback, userID)
	return nil
}

// resolveID resolves a bare Beeper user ID (message sender, mention, reactor)
// with an optional display name. Cache misses fall through to rung 3 of the
// ladder since no phone/email is available on bare IDs.
func (r *participantResolver) resolveID(userID, displayName string) (int64, error) {
	if userID == "" {
		return 0, nil
	}
	if e, ok := r.rich[userID]; ok {
		return e.pid, nil
	}
	if pid, ok := r.fallback[userID]; ok {
		return pid, nil
	}
	pid, err := r.store.EnsureParticipantByIdentifier(participantIdentifierType, userID, displayName)
	if err != nil {
		return 0, err
	}
	r.fallback[userID] = pid
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
