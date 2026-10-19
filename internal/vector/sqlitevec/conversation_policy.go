//go:build sqlite_vec

package sqlitevec

import (
	"context"
	"database/sql"
	"fmt"

	"go.kenn.io/msgvault/internal/store"
)

func (b *Backend) acquirePolicyPublication(ctx context.Context, ids []int64) (func(), error) {
	if b.policyStore == nil {
		return func() {}, nil
	}
	release, err := b.policyStore.AcquireSQLiteConversationPolicyFence(ctx, store.ConversationPolicyShared)
	if err != nil {
		return nil, err
	}
	if err := b.policyStore.CheckSQLiteConversationPolicyPublication(ctx, ids); err != nil {
		release()
		return nil, err
	}
	return release, nil
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

// embeddedMessageWhere hides vectors that no longer match the message's
// current embed_gen watermark. Conversation-policy lifecycle changes clear
// that watermark before a changed room roster can leave old contextual text
// visible through vector search.
func (b *Backend) embeddedMessageWhere(alias string, gen vector.GenerationID) string {
	where := b.liveMessageWhere(alias)
	if b.policyStore != nil {
		if alias == "" {
			alias = "messages"
		}
		where += fmt.Sprintf(" AND %s.embed_gen = %d", alias, int64(gen))
	}
	return where
}

// prunePolicyLedgerBatch bounds both member and document mutations, including
// zero-chunk documents. Its caller owns W and releases it after this batch.
func (b *Backend) prunePolicyLedgerBatch(ctx context.Context, tx *sql.Tx) (int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT dm.generation_id,dm.message_id,dm.document_key
 FROM vec.embedding_document_members dm
 WHERE EXISTS (SELECT 1 FROM vec.embedding_documents d WHERE d.generation_id=dm.generation_id AND d.document_key=dm.document_key AND d.state='tombstoned')
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
			return 0, err
		}
		selected = append(selected, m)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, m := range selected {
		if _, err := tx.ExecContext(ctx, b.policyStore.Rebind(`UPDATE vec.embedding_documents SET state='tombstoned' WHERE generation_id=? AND document_key=? AND state='current'`), m.generation, m.document); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, b.policyStore.Rebind(`DELETE FROM vec.embedding_document_members WHERE generation_id=? AND message_id=?`), m.generation, m.message); err != nil {
			return 0, err
		}
	}
	return int64(len(selected)), nil
}
