//go:build pgvector

package pgvector

import (
	"context"
	"errors"

	"go.kenn.io/msgvault/internal/store"
)

type visualPolicyCursor struct {
	publication store.VisualPolicyCleanupCursor
	obsolete    store.VisualPolicyToken
	native      string
}

// PruneIneligibleVisuals revokes current-state publication tokens, retries the
// obsolete ledger, then removes orphan native tokens, releasing W each batch.
func (b *VisualBackend) PruneIneligibleVisuals(ctx context.Context) (int64, error) {
	if b.backend.policyStore == nil {
		return 0, errors.New("visual policy cleanup requires an archive")
	}
	var total int64
	for phase := range 3 {
		cursor := visualPolicyCursor{}
		for {
			n, next, more, err := b.pruneVisualPolicyBatch(ctx, cursor, phase)
			total += n
			if err != nil {
				return total, err
			}
			if !more {
				break
			}
			cursor = next
		}
	}
	return total, nil
}

func (b *VisualBackend) pruneVisualPolicyBatch(ctx context.Context, cursor visualPolicyCursor, phase int) (int64, visualPolicyCursor, bool, error) {
	tx, err := b.backend.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, cursor, false, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := b.backend.policyStore.LockPostgresConversationPolicyTx(ctx, tx, store.ConversationPolicyExclusive); err != nil {
		return 0, cursor, false, err
	}
	next, more := cursor, false
	var selected, tokens []store.VisualPolicyToken
	switch phase {
	case 0:
		page, err := b.backend.policyStore.RevokeIneligibleVisualTokens(ctx, tx, cursor.publication)
		if err != nil {
			return 0, cursor, false, err
		}
		tokens, next.publication, more = page.Tokens, page.After, page.More
	case 1:
		var err error
		selected, err = b.backend.policyStore.ListVisualPolicyObsoleteTokens(ctx, tx, cursor.obsolete)
		if err != nil {
			return 0, cursor, false, err
		}
		more = len(selected) == 500
		if len(selected) > 0 {
			next.obsolete = selected[len(selected)-1]
		}
	case 2:
		rows, err := tx.QueryContext(ctx, b.backend.policyStore.Rebind(`SELECT vector_token FROM visual_vectors WHERE vector_token>? ORDER BY vector_token LIMIT 500`), cursor.native)
		if err != nil {
			return 0, cursor, false, err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var token store.VisualPolicyToken
			if err := rows.Scan(&token.Token); err != nil {
				return 0, cursor, false, err
			}
			selected = append(selected, token)
		}
		if err := rows.Err(); err != nil {
			return 0, cursor, false, err
		}
		more = len(selected) == 500
		if len(selected) > 0 {
			next.native = selected[len(selected)-1].Token
		}
	}
	for _, token := range selected {
		current, err := b.backend.policyStore.VisualPolicyTokenCurrent(ctx, tx, token.Token)
		if err != nil {
			return 0, cursor, false, err
		}
		if !current {
			tokens = append(tokens, token)
		}
	}
	var count int64
	for _, token := range tokens {
		result, err := tx.ExecContext(ctx, b.backend.policyStore.Rebind(`DELETE FROM visual_vectors WHERE vector_token=?`), token.Token)
		if err != nil {
			return 0, cursor, false, err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return 0, cursor, false, err
		}
		count += n
	}
	if err := b.backend.policyStore.FinalizeVisualPolicyTokens(ctx, tx, tokens); err != nil {
		return 0, cursor, false, err
	}
	if err := tx.Commit(); err != nil {
		return 0, cursor, false, err
	}
	return count, next, more, nil
}
