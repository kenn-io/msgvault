package query_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestSQLiteQueryEngineDateBoundsCompareMixedOffsetsAsInstants(t *testing.T) {
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
