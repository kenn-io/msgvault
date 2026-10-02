package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestSearchMessageIDsQueryPreservesRankScopeAndTotal(t *testing.T) {
	t.Parallel()
	requirements := require.New(t)
	f := storetest.New(t)
	day := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, externalID := range []string{"older", "newer", "deleted"} {
		id := f.NewMessage().WithSourceMessageID(externalID).WithSubject("glacier").
			WithSentAt(day.Add(time.Duration(i)*24*time.Hour)).Create(t, f.Store)
		requirements.NoError(f.Store.UpsertFTS(id, "glacier", "", "", "", ""))
	}
	requirements.NoError(f.Store.MarkMessageDeleted(f.Source.ID, "deleted"))
	other, err := f.Store.GetOrCreateSource("gmail", "other@example.test")
	requirements.NoError(err)
	conversation, err := f.Store.EnsureConversation(other.ID, "other", "Other thread")
	requirements.NoError(err)
	id := storetest.NewMessage(other.ID, conversation).WithSubject("glacier").
		WithSentAt(day.Add(3*24*time.Hour)).Create(t, f.Store)
	requirements.NoError(f.Store.UpsertFTS(id, "glacier", "", "", "", ""))

	for _, tc := range []struct {
		name  string
		query search.Query
		limit int
		ids   []int64
		total int64
	}{
		{"bounded ranked pool", search.Query{TextTerms: []string{"glacier"}, DeletionScope: search.DeletionScopeAny}, 2, []int64{4, 3}, 4},
		{"active", search.Query{TextTerms: []string{"glacier"}}, 10, []int64{4, 2, 1}, 3},
		{"deleted", search.Query{TextTerms: []string{"glacier"}, DeletionScope: search.DeletionScopeDeleted}, 10, []int64{3}, 1},
		{"source before cap", search.Query{TextTerms: []string{"glacier"}, AccountIDs: []int64{f.Source.ID}}, 1, []int64{2}, 2},
		{"date before cap", search.Query{TextTerms: []string{"glacier"}, BeforeDate: new(day.Add(24 * time.Hour))}, 1, []int64{1}, 1},
		{"empty", search.Query{TextTerms: []string{"xylophone"}}, 10, []int64{}, 0},
		{"tokenless", search.Query{TextTerms: []string{"!!!"}}, 10, []int64{}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ids, total, err := f.Store.SearchMessageIDsQueryContext(t.Context(), &tc.query, tc.limit)
			require.NoError(t, err)
			assert.Equal(t, tc.ids, ids)
			assert.Equal(t, tc.total, total)
		})
	}
	t.Run("relevance precedes date", func(t *testing.T) {
		requirements := require.New(t)
		assertions := assert.New(t)
		subjectHit := f.NewMessage().WithSubject("alpine").WithSentAt(day).Create(t, f.Store)
		requirements.NoError(f.Store.UpsertFTS(subjectHit, "alpine", "", "", "", ""))
		bodyHit := f.NewMessage().WithSubject("update").WithSentAt(day.Add(24*time.Hour)).Create(t, f.Store)
		requirements.NoError(f.Store.UpsertFTS(bodyHit, "update", "alpine", "", "", ""))
		ids, total, err := f.Store.SearchMessageIDsQueryContext(t.Context(), &search.Query{TextTerms: []string{"alpine"}}, 1)
		requirements.NoError(err)
		assertions.Equal([]int64{subjectHit}, ids)
		assertions.Equal(int64(2), total)
	})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err = f.Store.SearchMessageIDsQueryContext(ctx, &search.Query{TextTerms: []string{"glacier"}}, 10)
	assert.ErrorIs(t, err, context.Canceled)
}
