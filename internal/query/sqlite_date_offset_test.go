package query_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestSQLiteQueryEngineDateBoundsCompareMixedOffsetsAsInstants(t *testing.T) {
	t.Parallel()
	req := require.New(t)
	f := storetest.New(t)
	if f.Store.IsPostgreSQL() {
		t.Skip("mixed textual timestamp encodings are specific to SQLite archives")
	}

	earlierID := f.NewMessage().
		WithSourceMessageID("mixed-offset-earlier").
		WithSubject("earlier").
		WithSentAt(time.Date(2024, 1, 15, 14, 0, 0, 0, time.UTC)).
		Create(t, f.Store)
	insideID := f.NewMessage().
		WithSourceMessageID("mixed-offset-inside").
		WithSubject("inside").
		WithSentAt(time.Date(2024, 1, 15, 16, 0, 0, 0, time.UTC)).
		Create(t, f.Store)
	laterID := f.NewMessage().
		WithSourceMessageID("mixed-offset-later").
		WithSubject("later").
		WithSentAt(time.Date(2024, 1, 15, 17, 30, 0, 0, time.UTC)).
		Create(t, f.Store)

	// Preserve the same instants using encodings that do not share lexical
	// order with their UTC values. Historical SQLite archives can contain both.
	_, err := f.Store.DB().ExecContext(t.Context(), `
		UPDATE messages
		SET sent_at = CASE id
			WHEN ? THEN '2024-01-15 14:00:00+00:00'
			WHEN ? THEN '2024-01-15 11:00:00-05:00'
			WHEN ? THEN '2024-01-15 12:30:00-05:00'
		END
		WHERE id IN (?, ?, ?)
	`, earlierID, insideID, laterID, earlierID, insideID, laterID)
	req.NoError(err)

	eng := query.NewEngine(f.Store.DB(), false)
	after := time.Date(2024, 1, 15, 15, 30, 0, 0, time.UTC)
	before := time.Date(2024, 1, 15, 17, 0, 0, 0, time.UTC)

	ids := func(messages []query.MessageSummary) []int64 {
		result := make([]int64, len(messages))
		for i, message := range messages {
			result[i] = message.ID
		}
		return result
	}

	for _, tc := range []struct {
		name  string
		bound *time.Time
		want  []int64
	}{
		{name: "after", bound: &after, want: []int64{insideID, laterID}},
		{name: "before", bound: &before, want: []int64{earlierID, insideID}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			var afterBound, beforeBound *time.Time
			if tc.name == "after" {
				afterBound = tc.bound
			} else {
				beforeBound = tc.bound
			}

			listed, err := eng.ListMessages(t.Context(), query.MessageFilter{
				After:  afterBound,
				Before: beforeBound,
				Sorting: query.MessageSorting{
					Field: query.MessageSortByDate,
				},
			})
			require.NoError(err, "ListMessages")
			assert.ElementsMatch(tc.want, ids(listed), "ListMessages")

			searched, err := eng.SearchFast(context.Background(), &search.Query{
				AfterDate:  afterBound,
				BeforeDate: beforeBound,
			}, query.MessageFilter{}, 50, 0)
			require.NoError(err, "SearchFast")
			assert.ElementsMatch(tc.want, ids(searched), "SearchFast")

			opts := query.DefaultAggregateOptions()
			opts.After = afterBound
			opts.Before = beforeBound
			rows, err := eng.Aggregate(t.Context(), query.ViewTime, opts)
			require.NoError(err, "Aggregate")
			require.Len(rows, 1, "Aggregate")
			assert.Equal(int64(len(tc.want)), rows[0].Count, "Aggregate")
		})
	}
}

func TestSQLiteInstantDatePredicateUsesExpressionIndex(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	if f.Store.IsPostgreSQL() {
		t.Skip("SQLite query-plan regression")
	}

	f.NewMessage().
		WithSourceMessageID("indexed-date-bound").
		WithSentAt(time.Date(2024, 1, 15, 16, 0, 0, 0, time.UTC)).
		Create(t, f.Store)

	predicate := (query.SQLiteQueryDialect{}).DateComparison("m.sent_at", ">=")
	rows, err := f.Store.DB().QueryContext(t.Context(),
		"EXPLAIN QUERY PLAN SELECT m.id FROM messages m WHERE "+predicate,
		"2024-01-15 15:30:00",
	)
	require.NoError(err)
	defer func() { require.NoError(rows.Close()) }()

	var plan strings.Builder
	for rows.Next() {
		var selectID, order, from int
		var detail string
		require.NoError(rows.Scan(&selectID, &order, &from, &detail))
		plan.WriteString(detail)
		plan.WriteByte('\n')
	}
	require.NoError(rows.Err())
	assert.Contains(plan.String(), "idx_messages_sent_at_julianday", plan.String())
}

func TestSQLiteListThreadUsesExpressionIndex(t *testing.T) {
	t.Parallel()
	f := storetest.New(t)
	if f.Store.IsPostgreSQL() {
		t.Skip("SQLite query-plan regression")
	}
	f.CreateMessage("indexed-thread-page")
	// Give the planner a source containing many conversations, so selecting
	// one thread is cheaper than reading every message from that source.
	otherConv, err := f.Store.EnsureConversation(f.Source.ID, "other-thread", "Other thread")
	require.NoError(t, err)
	_, err = f.Store.DB().ExecContext(t.Context(), `
		WITH RECURSIVE n(id) AS (SELECT 1 UNION ALL SELECT id + 1 FROM n WHERE id < 1000)
		INSERT INTO messages (source_id, conversation_id, source_message_id, message_type, sent_at)
		SELECT ?, CASE WHEN id <= 100 THEN ? ELSE ? END, 'thread-plan-' || id, 'email', '2026-01-15 14:00:00+00:00' FROM n
	`, f.Source.ID, f.ConvID, otherConv)
	require.NoError(t, err)
	_, err = f.Store.DB().ExecContext(t.Context(), "ANALYZE")
	require.NoError(t, err)

	for _, scoped := range []bool{false, true} {
		where := "m.conversation_id = ? AND " + store.LiveMessagesWhere("m", false)
		args := []any{f.ConvID}
		if scoped {
			where += " AND m.source_id IN (?)"
			args = append(args, f.Source.ID)
		}
		// Match ListThread's count and page statements, including the raw MIME lookup.
		for name, statement := range map[string]string{
			"count": "SELECT COUNT(*) FROM messages m WHERE " + where,
			"page": `SELECT m.id, COALESCE(m.source_message_id, ''), COALESCE(m.subject, ''), m.sent_at,
			       COALESCE(m.attachment_count, 0), m.deleted_from_source_at,
			       CASE WHEN EXISTS (
			           SELECT 1 FROM message_raw mr
			           WHERE mr.message_id = m.id AND mr.raw_format = 'mime'
			       ) THEN 1 ELSE 0 END
			FROM messages m WHERE ` + where + `
			ORDER BY ` + (query.SQLiteQueryDialect{}).DateOrderExpression("m.sent_at") + ` NULLS LAST, m.id LIMIT ? OFFSET ?`,
		} {
			t.Run(fmt.Sprintf("%s/scoped=%t", name, scoped), func(t *testing.T) {
				require := require.New(t)
				assert := assert.New(t)
				queryArgs := append([]any{}, args...)
				if name == "page" {
					queryArgs = append(queryArgs, 100, 50000)
				}
				rows, err := f.Store.DB().QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+statement, queryArgs...)
				require.NoError(err)
				defer func() { require.NoError(rows.Close()) }()
				var plan strings.Builder
				for rows.Next() {
					var selectID, order, from int
					var detail string
					require.NoError(rows.Scan(&selectID, &order, &from, &detail))
					plan.WriteString(detail)
					plan.WriteByte('\n')
				}
				require.NoError(rows.Err())
				assert.Contains(plan.String(), "idx_messages_conversation_sent_at_julianday", plan.String())
				assert.NotContains(plan.String(), "TEMP B-TREE", plan.String())
			})
		}
	}
}
