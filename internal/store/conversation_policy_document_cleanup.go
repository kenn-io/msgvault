package store

import (
	"context"
	"database/sql"
	"fmt"
)

// DocumentPolicyCleanupPage is a bounded current-state ledger scan. Native
// callers own W for selection, revocation, physical deletion and finalization.
type DocumentPolicyCleanupPage struct {
	Tokens     []DocumentVectorCleanupToken
	AfterToken string
	More       bool
}

// RevokeIneligibleDocumentTokens selects at most 500 ledger rows and revokes
// invalid incarnations before native deletion. PostgreSQL supplies its actual
// native transaction; SQLite passes nil and closes this main transaction before
// beginning the native write, retaining W throughout both operations.
func (s *Store) RevokeIneligibleDocumentTokens(ctx context.Context, tx *sql.Tx, after string) (DocumentPolicyCleanupPage, error) {
	return s.revokeDocumentPolicyTokens(ctx, tx, after, false)
}

func (s *Store) revokeDocumentPolicyTokens(ctx context.Context, tx *sql.Tx, after string, missing bool) (DocumentPolicyCleanupPage, error) {
	if tx == nil {
		var page DocumentPolicyCleanupPage
		err := s.withTxContext(ctx, func(main *loggedTx) error {
			var err error
			page, err = s.revokeDocumentPolicyTokens(ctx, main.Tx, after, missing)
			return err
		})
		return page, err
	}
	rows, err := tx.QueryContext(ctx, s.Rebind(`SELECT v.generation_id,v.token,
  (g.state<>'retired' AND NOT(v.state='failed' AND COALESCE(v.error_code,'')='source_changed') AND `+documentVectorPublicationSnapshotCurrentSQL()+`) AS eligible
  FROM document_vector_publications v JOIN document_vector_generations g ON g.id=v.generation_id
  WHERE v.backend_cleaned_at IS NULL AND v.token>? ORDER BY v.token LIMIT 500`), after)
	if err != nil {
		return DocumentPolicyCleanupPage{}, err
	}
	defer func() { _ = rows.Close() }()
	var page DocumentPolicyCleanupPage
	scanned := 0
	for rows.Next() {
		var token DocumentVectorCleanupToken
		var eligible bool
		if err := rows.Scan(&token.GenerationID, &token.Token, &eligible); err != nil {
			_ = rows.Close()
			return page, err
		}
		scanned++
		page.AfterToken = token.Token
		if missing || !eligible {
			page.Tokens = append(page.Tokens, token)
		}
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return page, err
	}
	if closeErr != nil {
		return page, closeErr
	}
	page.More = scanned == 500
	for _, token := range page.Tokens {
		if _, err := tx.ExecContext(ctx, s.Rebind(`UPDATE document_vector_publications SET state='failed',error_code='source_changed',lease_owner=NULL,lease_until=NULL,next_retry_at=NULL WHERE generation_id=? AND token=?`), token.GenerationID, token.Token); err != nil {
			return page, err
		}
	}
	return page, nil
}

// DocumentPolicyTokenCurrent checks current Full support and immutable source
// identity; an earlier work revision or cleanup hint is never deletion authority.
func (s *Store) DocumentPolicyTokenCurrent(ctx context.Context, tx *sql.Tx, generation int64, token string) (bool, error) {
	var q ConversationPolicyTx = s.DB()
	if tx != nil {
		q = tx
	}
	var valid bool
	err := q.QueryRowContext(ctx, s.Rebind(`SELECT EXISTS(SELECT 1 FROM document_vector_publications v JOIN document_vector_generations g ON g.id=v.generation_id WHERE v.generation_id=? AND v.token=? AND v.backend_cleaned_at IS NULL AND g.state<>'retired' AND NOT(v.state='failed' AND COALESCE(v.error_code,'')='source_changed') AND `+documentVectorPublicationSnapshotCurrentSQL()+`)`), generation, token).Scan(&valid)
	return valid, err
}

// FinalizeDocumentPolicyTokens follows successful physical deletion under the
// same W fence. Exact token predicates cannot remove a recreated incarnation.
func (s *Store) FinalizeDocumentPolicyTokens(ctx context.Context, tx *sql.Tx, tokens []DocumentVectorCleanupToken) error {
	if tx == nil {
		return s.withTxContext(ctx, func(main *loggedTx) error { return s.FinalizeDocumentPolicyTokens(ctx, main.Tx, tokens) })
	}
	for _, token := range tokens {
		if _, err := tx.ExecContext(ctx, s.Rebind(`DELETE FROM document_vector_publications WHERE generation_id=? AND token=? AND state='failed' AND error_code='source_changed' AND EXISTS(SELECT 1 FROM document_vector_generations g WHERE g.id=document_vector_publications.generation_id AND g.state<>'retired')`), token.GenerationID, token.Token); err != nil {
			return fmt.Errorf("finalize document policy token: %w", err)
		}
		if _, err := tx.ExecContext(ctx, s.Rebind(`UPDATE document_vector_publications SET backend_cleaned_at=`+s.dialect.Now()+` WHERE generation_id=? AND token=? AND EXISTS(SELECT 1 FROM document_vector_generations g WHERE g.id=document_vector_publications.generation_id AND g.state='retired')`), token.GenerationID, token.Token); err != nil {
			return err
		}
	}
	return nil
}

// DocumentPolicyTokenPublished protects a live ready publication from stale
// cleanup hints while leaving unpublished abort tokens deletable. Caller owns W.
func (s *Store) DocumentPolicyTokenPublished(ctx context.Context, tx *sql.Tx, generation int64, token string) (bool, error) {
	current, err := s.DocumentPolicyTokenCurrent(ctx, tx, generation, token)
	if err != nil || !current {
		return false, err
	}
	var q ConversationPolicyTx = s.DB()
	if tx != nil {
		q = tx
	}
	var published bool
	err = q.QueryRowContext(ctx, s.Rebind(`SELECT EXISTS(SELECT 1 FROM document_vector_publications WHERE generation_id=? AND token=? AND state='ready')`), generation, token).Scan(&published)
	return published, err
}
