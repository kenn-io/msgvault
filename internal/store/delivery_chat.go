package store

import "context"

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
	rows, err := tx.QueryContext(ctx, `SELECT COALESCE(p.vcard_uid, '')
 FROM conversations c JOIN conversation_participants cp ON cp.conversation_id=c.id
 LEFT JOIN person_participants pp ON pp.participant_id=cp.participant_id
 LEFT JOIN persons p ON p.id=pp.person_id
 WHERE c.source_id=? AND c.source_conversation_id=? AND c.conversation_type IN `+chatConversationTypesSQL+` AND cp.left_at IS NULL
 AND NOT EXISTS (SELECT 1 FROM messages m WHERE m.source_id=c.source_id
  AND m.sender_id=cp.participant_id AND m.identity_is_from_me=TRUE)
 AND NOT (`+senderOwnerFallback("cp.participant_id", "c.source_id")+`)
 ORDER BY cp.participant_id`, sourceID, sourceConversationID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	recipients := []DeliveryPolicyQuery{}
	for rows.Next() {
		q := DeliveryPolicyQuery{RouteKind: "chat", SourceID: sourceID, SourceConversationID: sourceConversationID}
		if err := rows.Scan(&q.PersonUID); err != nil {
			return nil, err
		}
		recipients = append(recipients, q)
	}
	return recipients, rows.Err()
}
