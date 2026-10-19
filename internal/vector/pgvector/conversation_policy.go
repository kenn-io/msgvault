//go:build pgvector

package pgvector

import (
	"context"
	"database/sql"
	"fmt"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
)

func (b *Backend) checkPolicyPublicationTx(ctx context.Context, tx *sql.Tx, ids []int64) error {
	if b.policyStore == nil {
		return nil
	}
	if err := b.policyStore.LockPostgresConversationPolicyTx(ctx, tx, store.ConversationPolicyShared); err != nil {
		return err
	}
	return b.policyStore.CheckConversationPolicyPublicationTx(ctx, tx, ids)
}

func (b *Backend) liveMessageWhere(alias string) string {
	where := store.LiveMessagesWhere(alias, true)
	if b.policyStore != nil {
		if alias == "" {
			alias = "messages"
		}
		where += " AND " + store.FullMessageWhere(alias)
	}
	return where
}

// embeddedMessageWhere admits only live, policy-eligible message vectors
// whose input revision still matches the archive row. embed_gen tracks work
// coverage for one build generation and is not a publication-validity token.
func (b *Backend) embeddedMessageWhere(embeddingAlias string) string {
	return b.liveMessageWhere("m") + fmt.Sprintf(" AND %s.embedding_revision = m.embedding_revision", embeddingAlias)
}

// prunePolicyLedgerBatch bounds both member and document mutations, including
// zero-chunk documents. Its caller owns W and releases it after this batch.
func (b *Backend) prunePolicyLedgerBatch(ctx context.Context, tx *sql.Tx) (int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT dm.generation_id,dm.message_id,dm.document_key
 FROM embedding_document_members dm
 WHERE EXISTS (SELECT 1 FROM embedding_documents d WHERE d.generation_id=dm.generation_id AND d.document_key=dm.document_key AND d.state='tombstoned')
 OR NOT EXISTS (SELECT 1 FROM messages m WHERE m.id=dm.message_id AND `+store.FullMessageWhere("m")+`)
 ORDER BY dm.generation_id,dm.message_id LIMIT 500`)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()
	type member struct {
		generation, message int64
		document            string
	}
	var selected []member
	for rows.Next() {
		var m member
		if err := rows.Scan(&m.generation, &m.message, &m.document); err != nil {
			_ = rows.Close()
			return 0, err
		}
		selected = append(selected, m)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, m := range selected {
		if _, err := tx.ExecContext(ctx, b.policyStore.Rebind(`UPDATE embedding_documents SET state='tombstoned' WHERE generation_id=? AND document_key=? AND state='current'`), m.generation, m.document); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, b.policyStore.Rebind(`DELETE FROM embedding_document_members WHERE generation_id=? AND message_id=?`), m.generation, m.message); err != nil {
			return 0, err
		}
	}
	return int64(len(selected)), nil
}
