package store

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMetaCommentDeletionUsesTombstoneIndex(t *testing.T) {
	st := openTestStore(t)
	plan := explainPlan(t, st, succeededMetaCommentDeletionQuery, 1, "example-source", "300")
	assert.Contains(t, plan, "USING COVERING INDEX idx_meta_comment_actions_deleted", "polling must search the exact source/target tombstone index: %s", plan)
}

func TestMetaCommentDeletionUsesTombstoneIndexPG(t *testing.T) {
	require := require.New(t)
	st := newUninitializedPGStoreInternal(t, skipUnlessPostgresInternal(t))
	require.NoError(st.InitSchema())
	tx, err := st.db.BeginTx(t.Context(), nil)
	require.NoError(err)
	defer func() { _ = tx.Rollback() }()
	// A tiny fixture can legitimately favor a sequential scan. Disable it
	// within this rolled-back transaction to prove the index matches the query.
	_, err = tx.ExecContext(t.Context(), "SET LOCAL enable_seqscan = off")
	require.NoError(err)
	rows, err := tx.QueryContext(t.Context(), "EXPLAIN "+st.Rebind(succeededMetaCommentDeletionQuery), 1, "example-source", "300")
	require.NoError(err)
	defer func() { _ = rows.Close() }()
	var plan strings.Builder
	for rows.Next() {
		var line string
		require.NoError(rows.Scan(&line))
		plan.WriteString(line)
		plan.WriteByte('\n')
	}
	require.NoError(rows.Err())
	assert.Contains(t, plan.String(), "Index Only Scan using idx_meta_comment_actions_deleted", "polling must use the exact source/target tombstone index")
}
