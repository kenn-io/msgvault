//go:build sqlite_vec

package sqlitevec

import (
	"context"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/store"
)

// PruneIneligiblePersons reconciles exact semantic versions in all generations.
// Each batch owns W and reselects current versions; no earlier hint authorizes deletion.
func (b *Backend) PruneIneligiblePersons(ctx context.Context) (int64, error) {
	if b.policyStore == nil {
		return 0, errors.New("person policy cleanup requires an archive")
	}
	var total, generation, person int64
	for {
		n, g, p, more, err := b.prunePersonPolicyBatch(ctx, generation, person)
		if err != nil {
			return total, err
		}
		total += n
		if !more {
			return total, nil
		}
		generation, person = g, p
	}
}

type personPolicyRow struct {
	generation, person int64
	dimension          int
	revision           string
}

func (b *Backend) prunePersonPolicyBatch(ctx context.Context, generation, person int64) (int64, int64, int64, bool, error) {
	release, err := b.policyStore.AcquireSQLiteConversationPolicyFence(ctx, store.ConversationPolicyExclusive)
	if err != nil {
		return 0, generation, person, false, err
	}
	defer release()
	rows, err := b.db.QueryContext(ctx, b.policyStore.Rebind(`SELECT generation_id,person_id,dimension,published_revision FROM person_embeddings WHERE generation_id>? OR (generation_id=? AND person_id>?) ORDER BY generation_id,person_id LIMIT 500`), generation, generation, person)
	if err != nil {
		return 0, generation, person, false, err
	}
	defer func() { _ = rows.Close() }()
	var selected []personPolicyRow
	for rows.Next() {
		var row personPolicyRow
		if err := rows.Scan(&row.generation, &row.person, &row.dimension, &row.revision); err != nil {
			return 0, generation, person, false, err
		}
		selected = append(selected, row)
	}
	if err := rows.Err(); err != nil {
		return 0, generation, person, false, err
	}
	var obsolete []personPolicyRow
	for _, row := range selected {
		doc, err := b.policyStore.LoadPersonSemanticDocumentContext(ctx, row.person)
		if err != nil && !errors.Is(err, store.ErrPersonNotFound) {
			return 0, generation, person, false, err
		}
		if err == nil && doc.Text != "" && doc.Revision == row.revision {
			continue
		}
		obsolete = append(obsolete, row)
	}
	// Main snapshot reads are closed before the native write transaction starts.
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, generation, person, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var count int64
	for _, row := range obsolete {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE generation_id=? AND embedding_id IN (SELECT embedding_id FROM person_embeddings WHERE generation_id=? AND person_id=? AND published_revision=?)`, PersonVectorTableName(row.dimension)), row.generation, row.generation, row.person, row.revision); err != nil {
			return 0, generation, person, false, err
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM person_embeddings WHERE generation_id=? AND person_id=? AND published_revision=?`, row.generation, row.person, row.revision)
		if err != nil {
			return 0, generation, person, false, err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return 0, generation, person, false, err
		}
		count += n
	}
	if err := tx.Commit(); err != nil {
		return 0, generation, person, false, err
	}
	if len(selected) > 0 {
		last := selected[len(selected)-1]
		generation, person = last.generation, last.person
	}
	return count, generation, person, len(selected) == 500, nil
}
