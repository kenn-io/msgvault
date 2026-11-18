package store

import (
	"context"
	"database/sql"
	"fmt"
)

// VisualPolicyCleanupCursor follows the existing publication primary key.
type VisualPolicyCleanupCursor struct {
	GenerationID int64
	Owner        VisualOwner
}
type VisualPolicyToken struct {
	GenerationID int64
	Token        string
}
type VisualPolicyCleanupPage struct {
	Tokens []VisualPolicyToken
	After  VisualPolicyCleanupCursor
	More   bool
}

func visualPolicyOwnerCurrentSQL(alias string) string {
	return fmt.Sprintf(`g.state<>'retired' AND EXISTS(SELECT 1 FROM messages m WHERE m.id=%[1]s.message_id AND `+LiveMessagesWhere("m", true)+` AND `+FullMessageWhere("m")+` AND EXISTS(SELECT 1 FROM attachments a WHERE a.message_id=m.id AND a.attachment_role='standalone' AND a.role_source IN ('mime_disposition','provider_explicit','importer_semantics','raw_mime_repair') AND (LOWER(COALESCE(a.content_hash,''))=%[1]s.blob_hash OR (COALESCE(a.content_hash,'')='' AND LOWER(a.storage_path)=SUBSTR(%[1]s.blob_hash,1,2)||'/'||%[1]s.blob_hash))))`, alias)
}

// VisualPolicyTokenCurrent uses current publication/claim authority under W.
func (s *Store) VisualPolicyTokenCurrent(ctx context.Context, tx *sql.Tx, token string) (bool, error) {
	var q ConversationPolicyTx = s.DB()
	if tx != nil {
		q = tx
	}
	var valid bool
	err := q.QueryRowContext(ctx, s.Rebind(`SELECT EXISTS(SELECT 1 FROM visual_publications vp JOIN visual_generations g ON g.id=vp.generation_id
 WHERE `+visualPolicyOwnerCurrentSQL("vp")+` AND (
 (vp.current_vector_token=? AND vp.state='current') OR
 (vp.pending_vector_token=? AND EXISTS(SELECT 1 FROM visual_work_claims wc JOIN messages m ON m.id=wc.message_id
 WHERE wc.generation_id=vp.generation_id AND wc.message_id=vp.message_id AND wc.blob_hash=vp.blob_hash AND wc.media_input_key=vp.media_input_key
 AND wc.proposed_revision=vp.prepared_revision AND wc.source_fence=vp.source_fence
 AND COALESCE(CAST(m.content_changed_at AS TEXT),'')=wc.claimed_content_stamp
 AND NOT EXISTS(SELECT 1 FROM attachment_change_log change WHERE change.sequence>wc.source_fence AND (change.old_message_id=m.id OR change.new_message_id=m.id))))))`), token, token).Scan(&valid)
	return valid, err
}

// RevokeIneligibleVisualTokens scans 250 owners (at most 500 native tokens).
// The caller owns W. SQLite passes nil to close the main transaction before
// native writes; PostgreSQL passes the actual native transaction.
func (s *Store) RevokeIneligibleVisualTokens(ctx context.Context, tx *sql.Tx, after VisualPolicyCleanupCursor) (VisualPolicyCleanupPage, error) {
	return s.revokeVisualPolicyTokens(ctx, tx, after, false)
}

func (s *Store) revokeVisualPolicyTokens(ctx context.Context, tx *sql.Tx, after VisualPolicyCleanupCursor, missing bool) (VisualPolicyCleanupPage, error) {
	if tx == nil {
		var page VisualPolicyCleanupPage
		err := s.withTxContext(ctx, func(main *loggedTx) error {
			var err error
			page, err = s.revokeVisualPolicyTokens(ctx, main.Tx, after, missing)
			return err
		})
		return page, err
	}
	rows, err := tx.QueryContext(ctx, s.Rebind(`SELECT vp.generation_id,vp.message_id,vp.blob_hash,vp.media_input_key,COALESCE(vp.current_vector_token,''),COALESCE(vp.pending_vector_token,''),(`+visualPolicyOwnerCurrentSQL("vp")+`)
 FROM visual_publications vp JOIN visual_generations g ON g.id=vp.generation_id
 WHERE (vp.generation_id,vp.message_id,vp.blob_hash,vp.media_input_key)>(?,?,?,?)
 ORDER BY vp.generation_id,vp.message_id,vp.blob_hash,vp.media_input_key LIMIT 250`), after.GenerationID, after.Owner.MessageID, after.Owner.BlobHash, after.Owner.MediaInputKey)
	if err != nil {
		return VisualPolicyCleanupPage{}, err
	}
	defer func() { _ = rows.Close() }()
	type row struct {
		cursor           VisualPolicyCleanupCursor
		current, pending string
		eligible         bool
	}
	var selected []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.cursor.GenerationID, &r.cursor.Owner.MessageID, &r.cursor.Owner.BlobHash, &r.cursor.Owner.MediaInputKey, &r.current, &r.pending, &r.eligible); err != nil {
			_ = rows.Close()
			return VisualPolicyCleanupPage{}, err
		}
		selected = append(selected, r)
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return VisualPolicyCleanupPage{}, err
	}
	if closeErr != nil {
		return VisualPolicyCleanupPage{}, closeErr
	}
	page := VisualPolicyCleanupPage{After: after, More: len(selected) == 250}
	for _, row := range selected {
		page.After = row.cursor
		for _, token := range []string{row.current, row.pending} {
			if token == "" {
				continue
			}
			valid, err := s.VisualPolicyTokenCurrent(ctx, tx, token)
			if err != nil {
				return page, err
			}
			if valid && !missing {
				continue
			}
			if _, err := tx.ExecContext(ctx, s.Rebind(`INSERT INTO visual_obsolete_tokens(generation_id,vector_token) VALUES(?,?) ON CONFLICT(generation_id,vector_token) DO NOTHING`), row.cursor.GenerationID, token); err != nil {
				return page, err
			}
			if _, err := tx.ExecContext(ctx, s.Rebind(`UPDATE visual_publications SET state=CASE WHEN current_vector_token=? THEN 'stale' ELSE state END,current_vector_token=CASE WHEN current_vector_token=? THEN NULL ELSE current_vector_token END,pending_vector_token=CASE WHEN pending_vector_token=? THEN NULL ELSE pending_vector_token END WHERE generation_id=? AND message_id=? AND blob_hash=? AND media_input_key=?`), token, token, token, row.cursor.GenerationID, row.cursor.Owner.MessageID, row.cursor.Owner.BlobHash, row.cursor.Owner.MediaInputKey); err != nil {
				return page, err
			}
			page.Tokens = append(page.Tokens, VisualPolicyToken{GenerationID: row.cursor.GenerationID, Token: token})
		}
		if missing || !row.eligible {
			args := []any{row.cursor.GenerationID, row.cursor.Owner.MessageID, row.cursor.Owner.BlobHash, row.cursor.Owner.MediaInputKey}
			if _, err := tx.ExecContext(ctx, s.Rebind(`UPDATE visual_publications SET state='tombstoned',outcome_kind=NULL,outcome_reason=NULL WHERE generation_id=? AND message_id=? AND blob_hash=? AND media_input_key=?`), args...); err != nil {
				return page, err
			}
			if _, err := tx.ExecContext(ctx, s.Rebind(`DELETE FROM visual_work_claims WHERE generation_id=? AND message_id=? AND blob_hash=? AND media_input_key=?`), args...); err != nil {
				return page, err
			}
		}
	}
	return page, nil
}

// ListVisualPolicyObsoleteTokens retries previously revoked tokens after a
// physical deletion failure. Each token is revalidated before deletion.
func (s *Store) ListVisualPolicyObsoleteTokens(ctx context.Context, tx *sql.Tx, after VisualPolicyToken) ([]VisualPolicyToken, error) {
	var q interface {
		QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	} = s.DB()
	if tx != nil {
		q = tx
	}
	rows, err := q.QueryContext(ctx, s.Rebind(`SELECT generation_id,vector_token FROM visual_obsolete_tokens WHERE (generation_id,vector_token)>(?,?) ORDER BY generation_id,vector_token LIMIT 500`), after.GenerationID, after.Token)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var tokens []VisualPolicyToken
	for rows.Next() {
		var token VisualPolicyToken
		if err := rows.Scan(&token.GenerationID, &token.Token); err != nil {
			return nil, err
		}
		tokens = append(tokens, token)
	}
	return tokens, rows.Err()
}

func (s *Store) FinalizeVisualPolicyTokens(ctx context.Context, tx *sql.Tx, tokens []VisualPolicyToken) error {
	if tx == nil {
		return s.withTxContext(ctx, func(main *loggedTx) error { return s.FinalizeVisualPolicyTokens(ctx, main.Tx, tokens) })
	}
	for _, token := range tokens {
		if _, err := tx.ExecContext(ctx, s.Rebind(`DELETE FROM visual_obsolete_tokens WHERE generation_id=? AND vector_token=?`), token.GenerationID, token.Token); err != nil {
			return err
		}
	}
	return nil
}

// VisualPolicyTokenPublished protects restored current publications, while
// allowing rollback of an unpublished pending token. Caller owns W.
func (s *Store) VisualPolicyTokenPublished(ctx context.Context, tx *sql.Tx, token string) (bool, error) {
	current, err := s.VisualPolicyTokenCurrent(ctx, tx, token)
	if err != nil || !current {
		return false, err
	}
	var q ConversationPolicyTx = s.DB()
	if tx != nil {
		q = tx
	}
	var published bool
	err = q.QueryRowContext(ctx, s.Rebind(`SELECT EXISTS(SELECT 1 FROM visual_publications WHERE current_vector_token=? AND state='current')`), token).Scan(&published)
	return published, err
}
