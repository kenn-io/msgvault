//go:build sqlite_vec

package sqlitevec

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"

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

// currentEmbeddingRevisions returns revisions for the requested messages that
// are still live and eligible for vector search. SQLite stores archive rows
// and vector rows in separate databases, so callers compare this result with
// the revision captured on each vector row after reading it from vectors.db.
func (b *Backend) currentEmbeddingRevisions(ctx context.Context, messageIDs []int64) (map[int64]int64, error) {
	if len(messageIDs) == 0 {
		return map[int64]int64{}, nil
	}
	if b.mainDB == nil {
		return nil, errors.New("current vector revisions require the main message database")
	}
	encoded, err := json.Marshal(messageIDs, json.Deterministic(true))
	if err != nil {
		return nil, fmt.Errorf("encode embedding revision message ids: %w", err)
	}
	rows, err := b.mainDB.QueryContext(ctx, `
		SELECT m.id, m.embedding_revision
		FROM messages m
		WHERE m.id IN (SELECT value FROM json_each(?))
		  AND `+b.liveMessageWhere("m"), string(encoded))
	if err != nil {
		return nil, fmt.Errorf("read current embedding revisions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	revisions := make(map[int64]int64, len(messageIDs))
	for rows.Next() {
		var id, revision int64
		if err := rows.Scan(&id, &revision); err != nil {
			return nil, fmt.Errorf("scan current embedding revision: %w", err)
		}
		revisions[id] = revision
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate current embedding revisions: %w", err)
	}
	return revisions, nil
}

func encodeEmbeddingRevisionPairs(revisions map[int64]int64) (string, error) {
	ids := make([]int64, 0, len(revisions))
	for id := range revisions {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	pairs := make([][2]int64, 0, len(ids))
	for _, id := range ids {
		pairs = append(pairs, [2]int64{id, revisions[id]})
	}
	encoded, err := json.Marshal(pairs, json.Deterministic(true))
	if err != nil {
		return "", fmt.Errorf("encode embedding revision pairs: %w", err)
	}
	return string(encoded), nil
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
 FROM vec.embedding_document_members dm
 WHERE EXISTS (SELECT 1 FROM vec.embedding_documents d WHERE d.generation_id=dm.generation_id AND d.document_key=dm.document_key AND d.state='tombstoned')
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
			return nil, err
		}
		selected = append(selected, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, m := range selected {
		if _, err := tx.ExecContext(ctx, b.policyStore.Rebind(`UPDATE vec.embedding_documents SET state='tombstoned' WHERE generation_id=? AND document_key=? AND state='current'`), m.generation, m.document); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, b.policyStore.Rebind(`DELETE FROM vec.embedding_document_members WHERE generation_id=? AND message_id=?`), m.generation, m.message); err != nil {
			return nil, err
		}
	}
	return selected, nil
}
