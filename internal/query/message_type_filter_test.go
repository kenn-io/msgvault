package query

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/testutil/dbtest"
)

// TestSearch_MessageTypeFilterCoversMultipleTypes is a regression test for the local FTS search
// path honoring the message_type: filter. Before this fix, buildSearchQueryParts
// (and the DuckDB Search fallback) silently dropped q.MessageTypes, so
// `msgvault search --mode=fts` ignored message_type scoping for every non-email
// type (sms, whatsapp, calendar_event, ...). The store/api.go path already
// filtered correctly; this brings the query engine in line.
func TestSearch_MessageTypeFilterCoversMultipleTypes(t *testing.T) {
	env := newTestEnv(t)

	// A unique term so we only count the rows we add, independent of the
	// standard seed data set.
	const term = "zzmsgtypeterm"

	emailID := env.AddMessage(dbtest.MessageOpts{
		Subject:     term + " email edition",
		SentAt:      "2024-05-01 10:00:00",
		MessageType: "email",
	})
	eventID := env.AddMessage(dbtest.MessageOpts{
		Subject:     term + " calendar edition",
		SentAt:      "2024-05-02 10:00:00",
		MessageType: "calendar_event",
	})
	smsID := env.AddMessage(dbtest.MessageOpts{
		Subject:     term + " sms edition",
		SentAt:      "2024-05-03 10:00:00",
		MessageType: messageTypeSMS,
	})

	// Index everything (one-time populate, so add rows first).
	env.EnableFTS()

	idsOf := func(results []MessageSummary) map[int64]bool {
		m := make(map[int64]bool, len(results))
		for _, r := range results {
			m[r.ID] = true
		}
		return m
	}

	t.Run("no type filter returns all three (email unaffected)", func(t *testing.T) {
		results := env.MustSearch(&search.Query{TextTerms: []string{term}}, 100, 0)
		ids := idsOf(results)
		assert.True(t, ids[emailID], "email should match when no type filter is set")
		assert.True(t, ids[eventID], "calendar_event should match when no type filter is set")
		assert.True(t, ids[smsID], "sms should match when no type filter is set")
	})

	t.Run("scope to calendar_event", func(t *testing.T) {
		results := env.MustSearch(&search.Query{
			TextTerms:    []string{term},
			MessageTypes: []string{"calendar_event"},
		}, 100, 0)
		require.Len(t, results, 1)
		assert.Equal(t, eventID, results[0].ID)
	})

	t.Run("scope to email excludes calendar/sms", func(t *testing.T) {
		results := env.MustSearch(&search.Query{
			TextTerms:    []string{term},
			MessageTypes: []string{"email"},
		}, 100, 0)
		require.Len(t, results, 1)
		assert.Equal(t, emailID, results[0].ID)
	})

	t.Run("multi-type IN filter", func(t *testing.T) {
		assert := assert.New(t)
		results := env.MustSearch(&search.Query{
			TextTerms:    []string{term},
			MessageTypes: []string{"calendar_event", messageTypeSMS},
		}, 100, 0)
		ids := idsOf(results)
		assert.Len(results, 2)
		assert.True(ids[eventID])
		assert.True(ids[smsID])
		assert.False(ids[emailID], "email must be excluded by the IN filter")
	})

	t.Run("structured-only (no text term) still scopes by type", func(t *testing.T) {
		results := env.MustSearch(&search.Query{
			SubjectTerms: []string{term},
			MessageTypes: []string{"calendar_event"},
		}, 100, 0)
		require.Len(t, results, 1)
		assert.Equal(t, eventID, results[0].ID)
	})
}

// TestMergeFilterIntoQuery_MessageType verifies that the SearchFast drill-down
// path (MergeFilterIntoQuery) carries a MessageFilter.MessageType into the
// query's MessageTypes, so type-scoped views (e.g. Texts mode) don't silently
// widen back to all message types during an in-view search.
func TestMergeFilterIntoQuery_MessageType(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	env := newTestEnv(t)
	const term = "zzmergeterm"

	_ = env.AddMessage(dbtest.MessageOpts{
		Subject:     term + " email",
		MessageType: "email",
	})
	smsID := env.AddMessage(dbtest.MessageOpts{
		Subject:     term + " text",
		MessageType: messageTypeSMS,
	})
	env.EnableFTS()

	base := &search.Query{TextTerms: []string{term}}
	merged := MergeFilterIntoQuery(base, MessageFilter{MessageType: messageTypeSMS})

	// The original query must not be mutated.
	assert.Empty(base.MessageTypes, "base query MessageTypes must not be mutated")
	require.Equal([]string{messageTypeSMS}, merged.MessageTypes)

	results := env.MustSearch(merged, 100, 0)
	require.Len(results, 1)
	assert.Equal(smsID, results[0].ID)
}

func TestMergeFilterIntoQuery_MessageTypeIntersectsExistingQueryTypes(t *testing.T) {
	base := &search.Query{MessageTypes: []string{"email", messageTypeSMS}}
	merged := MergeFilterIntoQuery(base, MessageFilter{MessageType: messageTypeSMS})

	assert.Equal(t, []string{"email", messageTypeSMS}, base.MessageTypes,
		"base query MessageTypes must not be mutated")
	assert.Equal(t, []string{messageTypeSMS}, merged.MessageTypes)
}

func TestSearchFast_MessageTypeConflictReturnsNoMatches(t *testing.T) {
	env := newTestEnv(t)
	const term = "zzconflictingtype"

	_ = env.AddMessage(dbtest.MessageOpts{
		Subject:     term + " email",
		MessageType: "email",
	})
	_ = env.AddMessage(dbtest.MessageOpts{
		Subject:     term + " sms",
		MessageType: messageTypeSMS,
	})
	env.EnableFTS()

	q := &search.Query{
		TextTerms:    []string{term},
		MessageTypes: []string{"email"},
	}
	results, err := env.Engine.SearchFast(env.Ctx, q, MessageFilter{MessageType: messageTypeSMS}, 100, 0)

	require.NoError(t, err)
	assert.Empty(t, results)
	assert.Equal(t, []string{"email"}, q.MessageTypes, "base query MessageTypes must not be mutated")
}
