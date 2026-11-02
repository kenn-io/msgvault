//go:build sqlite_vec

package sqlitevec

import (
	"context"
	"database/sql"
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
	release, err := b.policyStore.AcquireSQLiteConversationPolicyFence(ctx, store.ConversationPolicyExclusive)
	if err != nil {
		return 0, after, false, err
	}
	defer release()
	var tokens []store.DocumentVectorCleanupToken
	next := after
	var more bool
	if ledger {
		page, err := b.policyStore.RevokeIneligibleDocumentTokens(ctx, nil, after)
		if err != nil {
			return 0, after, false, err
		}
		tokens, next, more = page.Tokens, page.AfterToken, page.More
	} else {
		rows, err := b.db.QueryContext(ctx, b.policyStore.Rebind(`SELECT generation_id,token FROM document_vector_embeddings WHERE token>? ORDER BY token LIMIT 500`), after)
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
			current, err := b.policyStore.DocumentPolicyTokenCurrent(ctx, nil, row.GenerationID, row.Token)
			if err != nil {
				return 0, after, false, err
			}
			if !current {
				tokens = append(tokens, row)
			}
		}
	}
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, after, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var count int64
	for _, row := range tokens {
		var id int64
		var dimension int
		err := tx.QueryRowContext(ctx, `SELECT document_vector_id,dimension FROM document_vector_embeddings WHERE generation_id=? AND token=?`, row.GenerationID, row.Token).Scan(&id, &dimension)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return 0, after, false, err
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE generation_id=? AND document_vector_id=?`, DocumentVectorTableName(dimension)), row.GenerationID, id); err != nil {
			return 0, after, false, err
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM document_vector_embeddings WHERE generation_id=? AND token=?`, row.GenerationID, row.Token)
		if err != nil {
			return 0, after, false, err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return 0, after, false, err
		}
		count += n
	}
	if err := tx.Commit(); err != nil {
		return 0, after, false, err
	}
	if err := b.policyStore.FinalizeDocumentPolicyTokens(ctx, nil, tokens); err != nil {
		return count, after, false, err
	}
	return count, next, more, nil
}
