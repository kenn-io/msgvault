package store

import (
	"context"
	"time"
)

// CheckConversationPolicyDocumentTokensTx validates exact pending token/source
// snapshots and surviving Full owners on the actual publication transaction.
func (s *Store) CheckConversationPolicyDocumentTokensTx(ctx context.Context, tx ConversationPolicyTx, generationID int64, tokens []string) error {
	if err := s.CheckConversationPolicyPublicationTx(ctx, tx, nil); err != nil {
		return err
	}
	for _, token := range tokens {
		var valid bool
		err := tx.QueryRowContext(ctx, s.Rebind(`SELECT EXISTS(SELECT 1 FROM document_vector_publications v JOIN document_vector_generations g ON g.id=v.generation_id WHERE v.generation_id=? AND v.token=? AND v.state='pending' AND v.lease_until>? AND g.state='building' AND `+documentVectorPublicationSnapshotCurrentSQL()+`)`), generationID, token, s.dialect.TimestampParam(time.Now().UTC())).Scan(&valid)
		if err != nil {
			return err
		}
		if !valid {
			return ErrConversationPolicyStale
		}
	}
	return nil
}

// CheckConversationPolicyVisualTokensTx checks the prepared token's exact live
// claim, source journal fence, message content stamp and Full owner support.
func (s *Store) CheckConversationPolicyVisualTokensTx(ctx context.Context, tx ConversationPolicyTx, tokens []string) error {
	if err := s.CheckConversationPolicyPublicationTx(ctx, tx, nil); err != nil {
		return err
	}
	for _, token := range tokens {
		var valid bool
		err := tx.QueryRowContext(ctx, s.Rebind(`SELECT EXISTS(
 SELECT 1 FROM visual_publications vp
 JOIN visual_generations g ON g.id=vp.generation_id
 JOIN visual_work_claims wc ON wc.generation_id=vp.generation_id AND wc.message_id=vp.message_id AND wc.blob_hash=vp.blob_hash AND wc.media_input_key=vp.media_input_key AND wc.proposed_revision=vp.prepared_revision AND wc.source_fence=vp.source_fence
 JOIN messages m ON m.id=vp.message_id
 JOIN attachments a ON a.id=vp.representative_attachment_id AND a.message_id=m.id
 WHERE vp.pending_vector_token=? AND g.state IN ('building','active') AND wc.lease_expires_at>?
 AND COALESCE(CAST(m.content_changed_at AS TEXT),'')=wc.claimed_content_stamp
 AND `+LiveMessagesWhere("m", true)+` AND `+FullMessageWhere("m")+`
 AND a.attachment_role='standalone' AND a.role_source IN ('mime_disposition','provider_explicit','importer_semantics','raw_mime_repair')
 AND (LOWER(COALESCE(a.content_hash,''))=vp.blob_hash OR (COALESCE(a.content_hash,'')='' AND LOWER(a.storage_path)=SUBSTR(vp.blob_hash,1,2)||'/'||vp.blob_hash))
 AND NOT EXISTS(SELECT 1 FROM attachment_change_log change WHERE change.sequence>wc.source_fence AND (change.old_message_id=m.id OR change.new_message_id=m.id))
 )`), token, s.dialect.TimestampParam(time.Now().UTC())).Scan(&valid)
		if err != nil {
			return err
		}
		if !valid {
			return ErrConversationPolicyStale
		}
	}
	return nil
}

// CheckSQLiteConversationPolicyDocumentTokens checks tokens while callers hold R.
// These short snapshots close before native SQL.
func (s *Store) CheckSQLiteConversationPolicyDocumentTokens(ctx context.Context, generationID int64, tokens []string) error {
	return s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		return s.CheckConversationPolicyDocumentTokensTx(ctx, tx.Tx, generationID, tokens)
	})
}
func (s *Store) CheckSQLiteConversationPolicyVisualTokens(ctx context.Context, tokens []string) error {
	return s.withReadSnapshotContext(ctx, func(tx *loggedTx) error { return s.CheckConversationPolicyVisualTokensTx(ctx, tx.Tx, tokens) })
}

type conversationPolicyMediaKey struct{}
type conversationPolicyMedia struct {
	generationID int64
	tokens       []string
	visual       bool
}

// WithConversationPolicyDocumentTokens binds the exact prepared document inputs
// to each request/retry admission without replacing the original policy revision.
func WithConversationPolicyDocumentTokens(ctx context.Context, generationID int64, tokens []string) context.Context {
	return context.WithValue(ctx, conversationPolicyMediaKey{}, conversationPolicyMedia{generationID: generationID, tokens: append([]string(nil), tokens...)})
}

// WithConversationPolicyVisualTokens binds each prepared visual input snapshot.
func WithConversationPolicyVisualTokens(ctx context.Context, tokens []string) context.Context {
	return context.WithValue(ctx, conversationPolicyMediaKey{}, conversationPolicyMedia{tokens: append([]string(nil), tokens...), visual: true})
}
