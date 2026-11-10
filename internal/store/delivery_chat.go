package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
)

// ChatRouteRecipientsContext resolves the current canonical roster to one query
// per non-owner member. An unbound member has an empty PersonUID and must not be
// dropped. Admission resolves the roster again immediately before the attempt.
func (s *Store) ChatRouteRecipientsContext(ctx context.Context, sourceID int64, sourceConversationID string) ([]DeliveryPolicyQuery, error) {
	var recipients []DeliveryPolicyQuery
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		var err error
		recipients, err = s.chatRouteRecipientsTx(ctx, tx, sourceID, sourceConversationID)
		return err
	})
	return recipients, err
}

func (s *Store) chatRouteRecipientsTx(ctx context.Context, tx *loggedTx, sourceID int64, sourceConversationID string) ([]DeliveryPolicyQuery, error) {
	if sourceID <= 0 || !deliveryStringValid(sourceConversationID) {
		return nil, ErrDeliveryPolicyInvalid
	}

	r := MessagingRoute{SourceID: sourceID, ProviderChatID: sourceConversationID}
	var raw, sourceFailure string
	var oversized, reanchorRequired bool
	err := tx.QueryRowContext(ctx, `SELECT c.id,src.identifier,c.conversation_type,
 `+s.boundedRouteMetadataSQL()+`, COALESCE(srf.failure,''),
 EXISTS(SELECT 1 FROM archive_metadata am WHERE am.key='`+beeperReanchorMarkerPrefix+`' || CAST(src.id AS TEXT))
 FROM conversations c JOIN sources src ON src.id=c.source_id
 LEFT JOIN source_messaging_route_failures srf ON srf.source_id=src.id
 WHERE c.source_id=? AND c.source_conversation_id=? AND c.conversation_type IN `+chatConversationTypesSQL, sourceID, sourceConversationID).Scan(&r.ConversationID, &r.AccountID, &r.ConversationType, &raw, &oversized, &sourceFailure, &reanchorRequired)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrDeliveryPolicyInvalid
	}
	if err != nil {
		return nil, err
	}
	roster, _, err := messagingRoster(ctx, tx, r.ConversationID, 0)
	if err != nil {
		return nil, err
	}
	metadata := readRouteMetadata(&r, raw, oversized)
	e := metadata.Route
	if e == nil {
		return nil, fmt.Errorf("chat route proof missing: %w", ErrDeliveryPolicyInvalid)
	}
	checkRouteBindings(&r, e)
	checkRouteRosterProof(&r, e, roster)
	if len(r.Reasons) > 0 || e.Merged || e.MergedIntoChatID != "" || e.Failure != "" || sourceFailure != "" || reanchorRequired {
		return nil, fmt.Errorf("chat route proof invalid: %w", ErrDeliveryPolicyInvalid)
	}
	rows, err := tx.QueryContext(ctx, `SELECT cp.participant_id, COALESCE(p.vcard_uid, '')
 FROM conversations c JOIN conversation_participants cp ON cp.conversation_id=c.id
 LEFT JOIN person_participants pp ON pp.participant_id=cp.participant_id
 LEFT JOIN persons p ON p.id=pp.person_id
 WHERE c.source_id=? AND c.source_conversation_id=? AND c.conversation_type IN `+chatConversationTypesSQL+` AND cp.left_at IS NULL
 AND NOT (`+s.chatOwnerSQL("cp.participant_id", "c.source_id")+`)
 ORDER BY cp.participant_id`, sourceID, sourceConversationID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	recipients := []DeliveryPolicyQuery{}
	for rows.Next() {
		q := DeliveryPolicyQuery{RouteKind: "chat", SourceID: sourceID, SourceConversationID: sourceConversationID}
		var participantID int64
		if err := rows.Scan(&participantID, &q.PersonUID); err != nil {
			return nil, err
		}
		if !slices.Contains(e.SelfParticipantIDs, participantID) {
			recipients = append(recipients, q)
		}
	}
	return recipients, rows.Err()
}
