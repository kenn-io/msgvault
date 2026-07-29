package slack

import (
	"context"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

// participantIdentifierType namespaces Slack user IDs in
// participant_identifiers. IDs are only unique per workspace, so the stored
// identifier value is "<team_id>:<user_id>".
const participantIdentifierType = "slack"

// participantResolver resolves Slack user IDs to msgvault participant IDs,
// seeded once per run from users.list and cached thereafter.
//
// Resolution ladder (Beeper precedent, minus the phone rung — Slack profiles
// carry emails, not reliable E.164 phones):
//  1. Profile email → EnsureParticipant, deduping the person against mail
//     archives (and Teams' AAD→email resolutions).
//  2. Bare Slack user ID → EnsureParticipantByIdentifier("slack", …).
type participantResolver struct {
	store  *store.Store
	teamID string
	// users caches users.list rows by user ID for display names and titles.
	users map[string]User
	// rich caches email-based resolutions; fallback caches bare-ID ones.
	// Kept separate so a fallback cached before the users refresh cannot
	// block a later email-based dedup (Beeper precedent).
	rich     map[string]int64
	fallback map[string]int64
}

func newParticipantResolver(s *store.Store, teamID string) *participantResolver {
	return &participantResolver{
		store: s, teamID: teamID,
		users: map[string]User{}, rich: map[string]int64{}, fallback: map[string]int64{},
	}
}

// identifierValue namespaces a Slack user ID with its workspace.
func (r *participantResolver) identifierValue(userID string) string {
	return r.teamID + ":" + userID
}

// loadUsers refreshes the workspace member cache. Identity resolution is
// load-bearing for cross-archive dedup, so callers treat failure as fatal
// for the run.
func (r *participantResolver) loadUsers(ctx context.Context, c *Client) error {
	return c.AllUsers(ctx, func(u User) error {
		r.users[u.ID] = u
		return nil
	})
}

// tzLocation returns the cached user's timezone as a *time.Location: the
// IANA zone when its name loads (search date modifiers follow the zone's
// historical DST rules — probed live), falling back to a fixed zone at the
// current tz_offset (correct for non-DST zones; the pre-probe behavior
// otherwise).
func (r *participantResolver) tzLocation(userID string) *time.Location {
	u := r.users[userID]
	if u.TZ != "" {
		if loc, err := time.LoadLocation(u.TZ); err == nil {
			return loc
		}
	}
	return time.FixedZone("user", u.TZOffset)
}

// tzOffset returns the cached user's tz_offset in seconds (0 when unknown).
// Read fresh each run: search date modifiers evaluate in the user's CURRENT
// profile timezone.
func (r *participantResolver) tzOffset(userID string) int {
	return r.users[userID].TZOffset
}

// displayName returns the best-known name for a user ID ("" when unknown).
func (r *participantResolver) displayName(userID string) string {
	if u, ok := r.users[userID]; ok {
		return u.DisplayName()
	}
	return ""
}

// resolveID resolves a Slack user ID to a participant ID, creating the
// participant if needed. Unknown IDs (Slack Connect guests, departed users
// missing from users.list) resolve by bare identifier with no display name.
func (r *participantResolver) resolveID(userID string) (int64, error) {
	if userID == "" {
		return 0, nil
	}
	if pid, ok := r.rich[userID]; ok {
		return pid, nil
	}
	if pid, ok := r.fallback[userID]; ok {
		return pid, nil
	}
	u, known := r.users[userID]
	if known && strings.Contains(u.Profile.Email, "@") && !u.IsBot {
		pid, err := r.byEmail(u.Profile.Email, u.DisplayName())
		if err != nil {
			return 0, err
		}
		if err := r.recordRich(userID, pid); err != nil {
			return 0, err
		}
		return pid, nil
	}
	name := ""
	if known {
		name = u.DisplayName()
	}
	pid, err := r.store.EnsureParticipantByIdentifier(participantIdentifierType, r.identifierValue(userID), name)
	if err != nil {
		return 0, err
	}
	r.fallback[userID] = pid
	return pid, nil
}

// recordRich persists the Slack user ID as an identifier of the email-based
// participant, so later runs resolve bare sightings to the same person. A
// prior weak (bare-ID) owner is the same Slack user seen with less metadata:
// its history is merged into the rich participant rather than left split.
func (r *participantResolver) recordRich(userID string, pid int64) error {
	value := r.identifierValue(userID)
	prev, _, err := r.store.ParticipantByIdentifier(participantIdentifierType, value)
	if err != nil {
		return err
	}
	if prev != 0 && prev != pid {
		if err := r.store.MergeParticipants(prev, pid); err != nil {
			return err
		}
	}
	if err := r.store.SetParticipantIdentifier(pid, participantIdentifierType, value); err != nil {
		return err
	}
	r.rich[userID] = pid
	delete(r.fallback, userID)
	return nil
}

// resolveBot resolves a bot sender (bot_id + optional username). Bots never
// dedup by email; they are namespaced by bot ID.
func (r *participantResolver) resolveBot(botID, username string) (int64, error) {
	if botID == "" {
		return 0, nil
	}
	key := "bot:" + botID
	if pid, ok := r.fallback[key]; ok {
		return pid, nil
	}
	pid, err := r.store.EnsureParticipantByIdentifier(participantIdentifierType, r.identifierValue(key), username)
	if err != nil {
		return 0, err
	}
	r.fallback[key] = pid
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
