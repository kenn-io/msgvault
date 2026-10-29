//go:build pgvector

package pgvector

import (
	"context"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/store"
)

// PruneIneligibleDocuments reconciles every generation in bounded W batches.
func (b *DocumentBackend) PruneIneligibleDocuments(ctx context.Context) (int64, error) {
	if b.policyStore == nil {
		return 0, errors.New("document policy cleanup requires an archive")
	}
	var total int64
	for _, ledger := range []bool{true, false} {
		after := ""
		for {
			n, next, more, err := b.pruneDocumentPolicyBatch(ctx, after, ledger)
			total += n
			if err != nil {
				return total, err
			}
			if !more {
				break
			}
			after = next
		}
	}
	return total, nil
}

func (b *DocumentBackend) pruneDocumentPolicyBatch(ctx context.Context, after string, ledger bool) (int64, string, bool, error) {
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, after, false, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := b.policyStore.LockPostgresConversationPolicyTx(ctx, tx, store.ConversationPolicyExclusive); err != nil {
		return 0, after, false, err
	}
	var tokens []store.DocumentVectorCleanupToken
	next := after
	var more bool
	if ledger {
		page, err := b.policyStore.RevokeIneligibleDocumentTokens(ctx, tx, after)
		if err != nil {
			return 0, after, false, err
		}
		tokens, next, more = page.Tokens, page.AfterToken, page.More
	} else {
		rows, err := tx.QueryContext(ctx, b.policyStore.Rebind(`SELECT generation_id,token FROM document_vector_embeddings WHERE token>? ORDER BY token LIMIT 500`), after)
		if err != nil {
			return 0, after, false, err
		}
		defer func() { _ = rows.Close() }()
		var selected []store.DocumentVectorCleanupToken
		for rows.Next() {
			var row store.DocumentVectorCleanupToken
			if err := rows.Scan(&row.GenerationID, &row.Token); err != nil {
				_ = rows.Close()
				return 0, after, false, err
			}
			selected = append(selected, row)
		}
		err = rows.Err()
		closeErr := rows.Close()
		if err != nil {
			return 0, after, false, err
		}
		if closeErr != nil {
			return 0, after, false, closeErr
		}
		more = len(selected) == 500
		for _, row := range selected {
			next = row.Token
			current, err := b.policyStore.DocumentPolicyTokenCurrent(ctx, tx, row.GenerationID, row.Token)
			if err != nil {
				return 0, after, false, err
			}
			if !current {
				tokens = append(tokens, row)
			}
		}
	}
	var count int64
	for _, row := range tokens {
		result, err := tx.ExecContext(ctx, `DELETE FROM document_vector_embeddings WHERE generation_id=$1 AND token=$2`, row.GenerationID, row.Token)
		if err != nil {
			return 0, after, false, err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return 0, after, false, err
		}
		count += n
	}
	if err := b.policyStore.FinalizeDocumentPolicyTokens(ctx, tx, tokens); err != nil {
		return 0, after, false, err
	}
	if err := tx.Commit(); err != nil {
		return 0, after, false, fmt.Errorf("commit document policy cleanup: %w", err)
	}
	return count, next, more, nil
}
