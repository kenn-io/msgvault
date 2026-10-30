//go:build sqlite_vec

package sqlitevec

import (
	"context"
	"database/sql"
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
	release, err := b.backend.policyStore.AcquireSQLiteConversationPolicyFence(ctx, store.ConversationPolicyExclusive)
	if err != nil {
		return 0, cursor, false, err
	}
	defer release()
	next, more := cursor, false
	var selected, tokens []store.VisualPolicyToken
	switch phase {
	case 0:
		page, err := b.backend.policyStore.RevokeIneligibleVisualTokens(ctx, nil, cursor.publication)
		if err != nil {
			return 0, cursor, false, err
		}
		tokens, next.publication, more = page.Tokens, page.After, page.More
	case 1:
		var err error
		selected, err = b.backend.policyStore.ListVisualPolicyObsoleteTokens(ctx, nil, cursor.obsolete)
		if err != nil {
			return 0, cursor, false, err
		}
		more = len(selected) == 500
		if len(selected) > 0 {
			next.obsolete = selected[len(selected)-1]
		}
	case 2:
		rows, err := b.backend.db.QueryContext(ctx, b.backend.policyStore.Rebind(`SELECT vector_token FROM visual_vectors WHERE vector_token>? ORDER BY vector_token LIMIT 500`), cursor.native)
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
		current, err := b.backend.policyStore.VisualPolicyTokenCurrent(ctx, nil, token.Token)
		if err != nil {
			return 0, cursor, false, err
		}
		if !current {
			tokens = append(tokens, token)
		}
	}
	tx, err := b.backend.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, cursor, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var count int64
	for _, token := range tokens {
		var id int64
		err := tx.QueryRowContext(ctx, `SELECT vector_id FROM visual_vectors WHERE vector_token=?`, token.Token).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return 0, cursor, false, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM visual_vectors_vec WHERE rowid=?`, id); err != nil {
			return 0, cursor, false, err
		}
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
	if err := tx.Commit(); err != nil {
		return 0, cursor, false, err
	}
	if err := b.backend.policyStore.FinalizeVisualPolicyTokens(ctx, nil, tokens); err != nil {
		return count, cursor, false, err
	}
	return count, next, more, nil
}
