package store

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// refreshConfirmedIdentityAttributionTx is shared by batch confirmations and
// provider snapshots. The transaction holds the identity writer lock.
func (s *Store) refreshConfirmedIdentityAttributionTx(ctx context.Context, tx *loggedTx, sourceID int64, inserted []normalizedIdentityConfirmation) error {
	if len(inserted) == 0 {
		return nil
	}
	if _, err := s.bumpIdentityRevisionContext(ctx, tx); err != nil {
		return err
	}
	if err := s.bumpAccountIdentityRevisionContext(ctx, tx); err != nil {
		return err
	}
	return refreshIdentityMessageAttributionContext(ctx, tx, sourceID, inserted, "")
}

// refreshIdentityMessageAttributionContext visits only indexed From-envelope
// hits and legacy messages sent by matching participants. An envelope remains
// authoritative after participant merges, including rows with no sender_id.
func refreshIdentityMessageAttributionContext(ctx context.Context, tx *loggedTx, sourceID int64, confirmations []normalizedIdentityConfirmation, excludeSourceMessageID string) error {
	addresses := make([]string, 0, len(confirmations))
	for _, confirmation := range confirmations {
		addresses = append(addresses, strings.TrimSpace(confirmation.identifier))
	}
	messageIDs := make(map[int64]struct{})
	scanID := func(rows *loggedRows) error {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		messageIDs[id] = struct{}{}
		return nil
	}
	if err := queryInChunksWithValueExprContext(ctx, tx, addresses, []any{sourceID}, `
		SELECT mr.message_id FROM message_recipients mr CROSS JOIN messages m
		WHERE m.id = mr.message_id AND m.source_id = ?
		  AND mr.recipient_type = 'from' AND LOWER(mr.email_address) IN (%s)
	`, "LOWER(?)", scanID); err != nil {
		return fmt.Errorf("resolve identity envelope mentions: %w", err)
	}
	participants, err := participantIDsForConfirmationsContext(ctx, tx, confirmations)
	if err != nil {
		return err
	}
	if err := queryInChunksContext(ctx, tx, participants, []any{sourceID}, `
		SELECT m.id FROM messages m WHERE m.source_id = ? AND m.sender_id IN (%s)
		AND NOT EXISTS (SELECT 1 FROM message_recipients mr
		  WHERE mr.message_id = m.id AND mr.recipient_type = 'from'
		    AND mr.email_address IS NOT NULL AND TRIM(mr.email_address) <> '')
	`, scanID); err != nil {
		return fmt.Errorf("resolve legacy identity sender mentions: %w", err)
	}
	ids := make([]int64, 0, len(messageIDs))
	for id := range messageIDs {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	update := `UPDATE messages SET identity_is_from_me = ` + messageIdentityAttributionMatch + `,
		is_from_me = (` + messageSourceAttribution + ` OR ` + messageIdentityAttributionMatch + `)
		WHERE (? = '' OR source_message_id <> ?) AND id IN (%s)
		AND (identity_is_from_me <> ` + messageIdentityAttributionMatch + `
		  OR is_from_me IS NULL OR is_from_me <> (` + messageSourceAttribution + ` OR ` + messageIdentityAttributionMatch + `))`
	if err := execInChunksContext(ctx, tx, ids, []any{excludeSourceMessageID, excludeSourceMessageID}, update); err != nil {
		return fmt.Errorf("refresh identity-mentioned messages: %w", err)
	}
	return nil
}
