package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrParticipantIdentifierNotFound reports that no participant_identifiers
// row matches a (type, value) pair. Callers distinguish it from an internal
// failure with errors.Is so a stale identifier is a skip, not a 500.
var ErrParticipantIdentifierNotFound = errors.New("participant identifier not found")

const (
	// DefaultIdentityObservationLookupLimit bounds one fan-out lookup so a
	// pathological address (a shared support username seen on thousands of
	// participants) cannot turn one import into a quadratic matcher run.
	DefaultIdentityObservationLookupLimit = 50
	// MaxIdentityObservationLookupLimit is the hard cap a caller can raise to.
	MaxIdentityObservationLookupLimit = 500
)

// observationLookupLimit clamps a caller-supplied limit.
func observationLookupLimit(limit int) int {
	switch {
	case limit <= 0:
		return DefaultIdentityObservationLookupLimit
	case limit > MaxIdentityObservationLookupLimit:
		return MaxIdentityObservationLookupLimit
	default:
		return limit
	}
}

// FindObservationsByProviderUserIDContext returns the current observations
// whose provider_user_id equals providerUserID, ordered by participant then
// id. This is the stable-ID rung of the identity matching ladder: a repeated
// provider or Beeper user ID is the ONLY basis on which a link may resolve
// automatically, so the lookup is exact — never a prefix, never a fold.
//
// Reads go through idx_participant_observations_provider_user.
func (s *Store) FindObservationsByProviderUserIDContext(
	ctx context.Context, providerUserID string, limit int,
) ([]ParticipantContactObservation, error) {
	providerUserID = strings.TrimSpace(providerUserID)
	if providerUserID == "" {
		return nil, nil
	}
	query := participantObservationSelect + `
		WHERE o.provider_user_id = ?
		  AND o.active_until IS NULL AND o.superseded_at IS NULL
		ORDER BY o.participant_id, o.id
		LIMIT ?
	`
	// No s.Rebind here: loggedDB.QueryContext rebinds placeholders itself
	// (internal/store/db_logger.go). Rebinding twice corrupts the query on
	// PostgreSQL. Only tests, which reach the raw handle through st.DB(), call
	// st.Rebind explicitly.
	return s.queryParticipantObservationsContext(
		ctx, query, providerUserID, observationLookupLimit(limit))
}

// FindObservationsByServiceValueContext returns the current observations of
// one address kind and service whose normalized value matches, ACROSS every
// scope.
//
// PR 3's FindObservationsByAddressContext takes a ContactPointQuery whose nil
// ScopeKind/ScopeValue are matched with NULL-safe predicates, so nil there
// means "scope IS NULL" rather than "any scope". The cross-scope username
// suggestion in the matcher needs "any scope", so it reads through here
// instead of relying on an interpretation of that function.
func (s *Store) FindObservationsByServiceValueContext(
	ctx context.Context,
	addressKind ContactAddressKind,
	serviceSlug string,
	normalizedValue string,
	limit int,
) ([]ParticipantContactObservation, error) {
	if !addressKind.Valid() {
		return nil, fmt.Errorf("%q: %w", addressKind, ErrInvalidContactAddressKind)
	}
	serviceSlug = strings.TrimSpace(serviceSlug)
	normalizedValue = strings.TrimSpace(normalizedValue)
	if serviceSlug == "" || normalizedValue == "" {
		return nil, nil
	}
	query := participantObservationSelect + `
		WHERE o.address_kind = ? AND cs.slug = ? AND o.normalized_value = ?
		  AND o.active_until IS NULL AND o.superseded_at IS NULL
		ORDER BY o.participant_id, o.id
		LIMIT ?
	`
	return s.queryParticipantObservationsContext(ctx, query,
		string(addressKind), serviceSlug, normalizedValue, observationLookupLimit(limit))
}

// ClassifyParticipantIdentifierServiceContext stamps the communication
// service and scope classification onto one existing participant_identifiers
// anchor row. It never inserts, never repoints an identifier to a different
// participant, and never changes identifier_type or identifier_value: the
// global UNIQUE(identifier_type, identifier_value) anchor semantics are
// unchanged.
//
// LOCK ORDERING: this transaction writes participant_identifiers, which is in
// exclusiveLockTables, but deliberately does NOT touch the identity-revision
// row. The ordering contract in participant_links.go only constrains
// transactions that do both, so this one simply queues on the table lock and
// cannot deadlock against a serialized source removal. Do not add
// lockIdentityMutationTx here. Classification is people-layer metadata: it
// changes no cluster, no owner evidence, and no analytics dataset, so it also
// bumps no revision.
func (s *Store) ClassifyParticipantIdentifierServiceContext(
	ctx context.Context,
	identifierType, identifierValue string,
	serviceID *int64,
	scopeKind, scopeValue *string,
) error {
	identifierType = strings.TrimSpace(identifierType)
	identifierValue = strings.TrimSpace(identifierValue)
	if identifierType == "" || identifierValue == "" {
		return errors.New("classify participant identifier: type and value are required")
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		result, err := tx.ExecContext(ctx, `
			UPDATE participant_identifiers
			SET service_id = ?, scope_kind = ?, scope_value = ?
			WHERE identifier_type = ? AND identifier_value = ?
		`, serviceID, scopeKind, scopeValue, identifierType, identifierValue)
		if err != nil {
			return fmt.Errorf("classify participant identifier: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("classify participant identifier rows affected: %w", err)
		}
		if affected == 0 {
			return fmt.Errorf("%s %q: %w",
				identifierType, identifierValue, ErrParticipantIdentifierNotFound)
		}
		return nil
	})
}

// ParticipantDisplayNamesContext returns the non-empty display names of the
// given participants. Display-name equality is evidence for an identity match
// and never a basis for one, so this read is deliberately plain: no folding,
// no fuzzy comparison, no normalization the caller cannot see.
func (s *Store) ParticipantDisplayNamesContext(
	ctx context.Context, participantIDs []int64,
) (map[int64]string, error) {
	names := make(map[int64]string, len(participantIDs))
	if len(participantIDs) == 0 {
		return names, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(participantIDs)), ",")
	args := make([]any, 0, len(participantIDs))
	for _, id := range participantIDs {
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT id, display_name FROM participants
		WHERE id IN (%s) AND display_name IS NOT NULL AND display_name != ''
	`, placeholders), args...)
	if err != nil {
		return nil, fmt.Errorf("read participant display names: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			id   int64
			name string
		)
		if err := rows.Scan(&id, &name); err != nil {
			return nil, fmt.Errorf("scan participant display name: %w", err)
		}
		names[id] = name
	}
	return names, rows.Err()
}

type SharedConversationEvidence struct {
	ConversationID       int64
	SourceID             int64
	SourceConversationID string
}

// SharedConversationsContext lists the immutable conversation identities that
// both participants belong to. Co-membership is evidence only: the roadmap
// forbids turning group-chat co-presence into a declared relationship or an
// identity link.
//
// Two EXISTS semi-joins rather than SELECT DISTINCT with a join, per the
// repository SQL rules.
func (s *Store) SharedConversationsContext(
	ctx context.Context, a, b int64,
) ([]SharedConversationEvidence, error) {
	if a == b || a <= 0 || b <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id, c.source_id, COALESCE(c.source_conversation_id, '')
		FROM conversations c
		WHERE EXISTS (
			SELECT 1 FROM conversation_participants cp
			WHERE cp.conversation_id = c.id AND cp.participant_id = ?
		) AND EXISTS (
			SELECT 1 FROM conversation_participants cp
			WHERE cp.conversation_id = c.id AND cp.participant_id = ?
		)
		ORDER BY c.id
	`, a, b)
	if err != nil {
		return nil, fmt.Errorf("list shared conversations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	shared := make([]SharedConversationEvidence, 0)
	for rows.Next() {
		var conversation SharedConversationEvidence
		if err := rows.Scan(
			&conversation.ConversationID, &conversation.SourceID,
			&conversation.SourceConversationID,
		); err != nil {
			return nil, fmt.Errorf("scan shared conversation: %w", err)
		}
		shared = append(shared, conversation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate shared conversations: %w", err)
	}
	return shared, nil
}

// SharedConversationCountContext counts conversations both participants join.
func (s *Store) SharedConversationCountContext(ctx context.Context, a, b int64) (int, error) {
	shared, err := s.SharedConversationsContext(ctx, a, b)
	if err != nil {
		return 0, err
	}
	return len(shared), nil
}
