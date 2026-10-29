package telegram

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.kenn.io/msgvault/internal/beeper"
	"go.kenn.io/msgvault/internal/beeperidentity"
	"go.kenn.io/msgvault/internal/store"
)

// participantIdentifierType namespaces Telegram participant identifiers so a
// bare numeric ID can never collide with another network's identifier.
const participantIdentifierType = SourceType

// serviceSlug is the communication service Telegram participants are
// observed under. It matches the seeded catalog slug.
const serviceSlug = SourceType

// participantResolver resolves Telegram senders to archive participants and
// records the shared provider-identity observation each numeric user gets
// exactly once per run. resolvedCount tracks the distinct participants the
// run resolved, so the summary's participant line reports the work done,
// not a constant.
type participantResolver struct {
	store    *store.Store
	sourceID int64
	once     sync.Map // sender identifier string → struct{}
	resolved map[int64]struct{}
}

func newParticipantResolver(st *store.Store, sourceID int64) *participantResolver {
	return &participantResolver{store: st, sourceID: sourceID, resolved: map[int64]struct{}{}}
}

// resolvedCount reports how many distinct participants were resolved.
func (r *participantResolver) resolvedCount() int {
	return len(r.resolved)
}

// markResolved records one successful participant resolution.
func (r *participantResolver) markResolved(pid int64) {
	if pid != 0 {
		r.resolved[pid] = struct{}{}
	}
}

// parseSenderID splits a raw from_id value ("user482111") into its type
// prefix and ID. A '-' between the prefix and the digits (a negative chat
// ID, "chat-100…") belongs to neither and is dropped; callers that need the
// original value keep the raw from_id. ok is false for values without the
// <type><id> shape.
func parseSenderID(fromID string) (kind, id string, ok bool) {
	for i, r := range fromID {
		if r >= '0' && r <= '9' {
			if i == 0 {
				return "", "", false
			}
			return strings.TrimSuffix(fromID[:i], "-"), fromID[i:], true
		}
	}
	return "", "", false
}

// senderKind names the from_id prefixes Telegram Desktop documents: "user"
// for accounts, "channel" for channel-authored posts, and "chat" for
// anonymous group admins. Every other prefix is not a sender identity.
func senderKind(kind string) string {
	switch kind {
	case "user", "channel", "chat":
		return kind
	default:
		return ""
	}
}

// resolve returns the participant for one sender. Every documented from_id
// prefix resolves to a stable Telegram-namespaced participant: users under
// their bare numeric ID, channel and chat senders under their full prefixed
// ID, because Telegram's numeric namespaces are separate and a bare channel
// ID could collide with an unrelated user ID.
//
// Only numeric user IDs also record the shared provider-identity observation
// under the service-scoped key a Beeper bridge produces; channel and chat
// senders are conversation-level identities no bridge can corroborate, so
// they get no observation.
func (r *participantResolver) resolve(
	ctx context.Context, fromID, displayName string,
) (int64, error) {
	kind, id, ok := parseSenderID(fromID)
	if !ok || senderKind(kind) == "" {
		return 0, nil
	}
	identifier := fromID
	if kind == "user" {
		if !beeperidentity.NumericTelegramID(id) {
			return 0, nil
		}
		identifier = id
	}
	pid, err := r.store.EnsureParticipantByIdentifier(participantIdentifierType, identifier, displayName)
	if err != nil {
		return 0, fmt.Errorf("telegram: ensure participant %s: %w", fromID, err)
	}
	if kind == "user" {
		if err := r.observe(ctx, pid, id); err != nil {
			return 0, err
		}
	}
	r.markResolved(pid)
	return pid, nil
}

// observe records the shared provider-identity observation once per sender
// per run and runs the stable-provider matcher over it.
func (r *participantResolver) observe(ctx context.Context, participantID int64, nativeID string) error {
	if _, seen := r.once.LoadOrStore(nativeID, struct{}{}); seen {
		return nil
	}
	key := beeperidentity.TelegramStableProviderKey(nativeID)
	if key == "" {
		return nil
	}
	slug := serviceSlug
	sourceID := r.sourceID
	sourceRef := "telegram:" + nativeID
	observedAt := time.Now().UTC()
	result, err := r.store.RecordContactObservationContext(ctx, participantID, store.ParticipantContactObservationInput{
		SourceID:       &sourceID,
		AddressKind:    store.ContactAddressProviderIdentity,
		ServiceSlug:    &slug,
		ProviderUserID: &key,
		OriginalValue:  key,
		ObservedAt:     &observedAt,
		Envelope: store.ValueEnvelopeInput{
			Source:    store.ProvenanceArchiveObservation,
			SourceRef: &sourceRef,
		},
	})
	if err != nil {
		return fmt.Errorf("telegram: record provider identity for user %s: %w", nativeID, err)
	}
	if err := beeper.MatchProviderIdentity(ctx, r.store, participantID, result); err != nil {
		return fmt.Errorf("telegram: match provider identity for user %s: %w", nativeID, err)
	}
	return nil
}

// ensureOwner returns the exporting user's participant, recording the same
// shared observation a sender gets so the owner also links across imports.
func (r *participantResolver) ensureOwner(ctx context.Context, ownerID int64, displayName string) (int64, error) {
	id := strconv.FormatInt(ownerID, 10)
	pid, err := r.store.EnsureParticipantByIdentifier(participantIdentifierType, id, displayName)
	if err != nil {
		return 0, fmt.Errorf("telegram: ensure owner participant: %w", err)
	}
	if err := r.observe(ctx, pid, id); err != nil {
		return 0, err
	}
	r.markResolved(pid)
	return pid, nil
}

// chatMember resolves one message's sender to its chat-member participant.
// The owner's own from_id maps to the owner participant; every documented
// prefix resolves through resolve. isOwner reports the first case.
func (r *participantResolver) chatMember(
	ctx context.Context, ownerID, ownerPID int64, m *Message,
) (pid int64, isOwner bool, err error) {
	if kind, id, ok := parseSenderID(m.FromID); ok && kind == "user" {
		if numeric, err := strconv.ParseInt(id, 10, 64); err == nil && numeric == ownerID {
			return ownerPID, true, nil
		}
	}
	pid, err = r.resolve(ctx, m.FromID, m.FromName)
	return pid, false, err
}
