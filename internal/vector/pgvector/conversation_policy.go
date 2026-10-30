//go:build pgvector

package pgvector

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/store"
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

type policyMember struct {
	generation, message int64
	document            string
}

// releasedMemberWhere matches the vectors of released document members. A
// contextual document's vectors carry every member's context, so a member
// that leaves takes its siblings' vectors in that generation with it.
func releasedMemberWhere(alias string, released []policyMember) string {
	if len(released) == 0 {
		return ""
	}
	values := make([]string, 0, len(released))
	for _, m := range released {
		values = append(values, fmt.Sprintf("(%d,%d)", m.generation, m.message))
	}
	return fmt.Sprintf("(%s.generation_id,%s.message_id) IN (VALUES %s)", alias, alias, strings.Join(values, ","))
}

// prunePolicyLedgerBatch bounds both member and document mutations, including
// zero-chunk documents. Its caller owns W and releases it after this batch and
// deletes the released members' vectors in the same transaction.
func (b *Backend) prunePolicyLedgerBatch(ctx context.Context, tx *sql.Tx) ([]policyMember, error) {
	rows, err := tx.QueryContext(ctx, `SELECT dm.generation_id,dm.message_id,dm.document_key
 FROM embedding_document_members dm
 WHERE EXISTS (SELECT 1 FROM embedding_documents d WHERE d.generation_id=dm.generation_id AND d.document_key=dm.document_key AND d.state='tombstoned')
 OR NOT EXISTS (SELECT 1 FROM messages m WHERE m.id=dm.message_id AND `+store.FullMessageWhere("m")+`)
 ORDER BY dm.generation_id,dm.message_id LIMIT 500`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var selected []policyMember
	for rows.Next() {
		var m policyMember
		if err := rows.Scan(&m.generation, &m.message, &m.document); err != nil {
			_ = rows.Close()
			return nil, err
		}
		selected = append(selected, m)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, m := range selected {
		if _, err := tx.ExecContext(ctx, b.policyStore.Rebind(`UPDATE embedding_documents SET state='tombstoned' WHERE generation_id=? AND document_key=? AND state='current'`), m.generation, m.document); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, b.policyStore.Rebind(`DELETE FROM embedding_document_members WHERE generation_id=? AND message_id=?`), m.generation, m.message); err != nil {
			return nil, err
		}
	}
	return selected, nil
}
