package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
)

// MessageSenderIdentity contains archive-owned inputs for sender
// classification. Address is the message's exact historical From envelope.
// Missing or ambiguous author snapshots leave Address empty. SourceIsFromMe never
// uses identity-derived ownership.
type MessageSenderIdentity struct {
	MessageID      int64
	SourceID       int64
	ParticipantID  int64
	Address        string
	SourceIsFromMe bool
}

// MessageSenderIdentities reads metadata for an already authorized page. It
// additionally checks source scope so a stale or mismatched page cannot reveal
// identities outside that scope. nil scope means the owner's whole archive;
// nonnil empty scope means no evidence.
func (s *Store) MessageSenderIdentities(ctx context.Context, messageIDs, allowedSourceIDs []int64) (map[int64]MessageSenderIdentity, error) {
	if len(messageIDs) > 500 || len(allowedSourceIDs) > maxSenderRelationshipSources {
		return nil, ErrSenderRelationshipLimit
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ids := slices.Clone(messageIDs)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	for _, id := range ids {
		if id <= 0 {
			return nil, errors.New("message sender id must be positive")
		}
	}
	for _, id := range allowedSourceIDs {
		if id <= 0 {
			return nil, errors.New("message sender source id must be positive")
		}
	}
	result := make(map[int64]MessageSenderIdentity, len(ids))
	if len(ids) == 0 || (allowedSourceIDs != nil && len(allowedSourceIDs) == 0) {
		return result, nil
	}
	args := personMergeSnapshotIDArgs(ids)
	scope, sourceArgs := senderSourceScope(allowedSourceIDs, 0)
	args = append(args, sourceArgs...)
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		rows, err := tx.QueryContext(ctx, `SELECT m.id,m.source_id,COALESCE(m.sender_id,0),(`+s.senderNativeOwnershipSQL()+`),
		COALESCE((SELECT CASE WHEN COUNT(*)=1 THEN MIN(LOWER(TRIM(mr.email_address))) ELSE '' END
		FROM message_recipients mr WHERE mr.message_id=m.id AND mr.recipient_type='from'),'')
		FROM messages m JOIN sources src ON src.id=m.source_id
		WHERE m.id IN (`+int64Placeholders(len(ids))+`)`+scope, args...)
		if err != nil {
			return fmt.Errorf("read message sender identities: %w", err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var identity MessageSenderIdentity
			if err := rows.Scan(&identity.MessageID, &identity.SourceID, &identity.ParticipantID, &identity.SourceIsFromMe, &identity.Address); err != nil {
				return fmt.Errorf("scan message sender identity: %w", err)
			}
			result[identity.MessageID] = identity
		}
		return rows.Err()
	})
	return result, err
}
