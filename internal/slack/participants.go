package slack

import (
	"context"
	"errors"
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

// loadUserSnapshot seeds identity resolution from an offline Slack export.
func (r *participantResolver) loadUserSnapshot(users []User) {
	for _, user := range users {
		r.users[user.ID] = user
	}
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

// resolveID uses fetched directory metadata only after live room admission.
func (r *participantResolver) resolveID(ctx context.Context, userID string, room int64) (int64, error) {
	if userID == "" {
		return 0, nil
	}
	u := r.users[userID]
	email := ""
	if !u.IsBot && strings.Contains(u.Profile.Email, "@") {
		email = u.Profile.Email
	}
	return r.resolveIdentity(ctx, room, userID, u.DisplayName(), email)
}

func (r *participantResolver) resolveBot(ctx context.Context, botID, username string, room int64) (int64, error) {
	if botID == "" {
		return 0, nil
	}
	return r.resolveIdentity(ctx, room, "bot:"+botID, username, "")
}

func (r *participantResolver) resolveIdentity(ctx context.Context, room int64, key, name, email string) (int64, error) {
	input := store.ConversationParticipantIdentity{IdentifierType: participantIdentifierType, IdentifierValue: r.identifierValue(key), DisplayName: name, Email: email}
	result, err := r.store.ResolveConversationParticipantContext(ctx, room, input)
	if errors.Is(err, store.ErrConversationPolicyStale) || errors.Is(err, store.ErrPersonBindingConflict) {
		pid, fallbackErr := r.store.EnsureParticipantByIdentifierContext(ctx, participantIdentifierType, input.IdentifierValue, "")
		if fallbackErr == nil {
			r.fallback[key] = pid
		}
		return pid, fallbackErr
	}
	if err != nil {
		return 0, err
	}
	// A cached PID is never admission authority. Publish only committed results.
	if email != "" {
		r.rich[key] = result.ParticipantID
		delete(r.fallback, key)
	} else {
		r.fallback[key] = result.ParticipantID
	}
	return result.ParticipantID, nil
}
