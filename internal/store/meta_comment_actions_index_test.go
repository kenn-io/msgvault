package store

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var metaCommentPollingIndexCases = []struct {
	name, query, index string
}{
	{"deletion", succeededMetaCommentDeletionQuery, "idx_meta_comment_actions_deleted"},
	{"public-reply", succeededMetaCommentReplyQuery, "idx_meta_comment_actions_sent_reply"},
}

func TestMetaCommentPollingUsesReceiptIndexes(t *testing.T) {
	st := openTestStore(t)
	for _, tc := range metaCommentPollingIndexCases {
		t.Run(tc.name, func(t *testing.T) {
			plan := explainPlan(t, st, tc.query, 1, "example-source", "300")
			assert.Contains(t, plan, "USING COVERING INDEX "+tc.index, "polling must search the exact source/object index: %s", plan)
		})
	}
}

func TestMetaCommentPollingUsesReceiptIndexesPG(t *testing.T) {
	requirements := require.New(t)
	st := newUninitializedPGStoreInternal(t, skipUnlessPostgresInternal(t))
	requirements.NoError(st.InitSchema())
	tx, err := st.db.BeginTx(t.Context(), nil)
	requirements.NoError(err)
	defer func() { _ = tx.Rollback() }()
	// A tiny fixture can legitimately favor a sequential scan. Disable it
	// within this rolled-back transaction to prove the index matches the query.
	_, err = tx.ExecContext(t.Context(), "SET LOCAL enable_seqscan = off")
	requirements.NoError(err)
	for _, tc := range metaCommentPollingIndexCases {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			rows, err := tx.QueryContext(t.Context(), "EXPLAIN "+st.Rebind(tc.query), 1, "example-source", "300")
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
			assert.Contains(t, plan.String(), "Index Only Scan using "+tc.index, "polling must use the exact source/object index")
		})
	}
}
