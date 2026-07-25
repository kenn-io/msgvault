package store_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestMessageDateRepairsListAndApplyCandidates(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	lower := time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC)
	upper := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

	oldID := f.NewMessage().
		WithSourceMessageID("old").
		WithSentAt(time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)).
		Create(t, f.Store)
	goodID := f.NewMessage().
		WithSourceMessageID("good").
		WithSentAt(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)).
		Create(t, f.Store)
	missingID := f.CreateMessage("missing")

	require.NoError(f.Store.UpsertMessageRaw(oldID, sampleRawMessage), "raw old")
	require.NoError(f.Store.UpsertMessageRaw(goodID, sampleRawMessage), "raw good")
	require.NoError(f.Store.UpsertMessageRaw(missingID, sampleRawMessage), "raw missing")

	candidates, err := f.Store.ListMessageDateRepairCandidates(t.Context(), lower, upper)
	require.NoError(err, "ListMessageDateRepairCandidates")
	require.Len(candidates, 2)
	assert.Equal([]int64{oldID, missingID}, []int64{candidates[0].ID, candidates[1].ID})
	assert.NotEmpty(candidates[0].LastModifiedAt)
	assert.NotEmpty(candidates[1].LastModifiedAt)

	repairedOld := time.Date(2007, 1, 2, 15, 4, 5, 0, time.UTC)
	repairedMissing := time.Date(2015, 5, 5, 12, 0, 0, 0, time.UTC)
	err = f.Store.ApplyMessageDateRepairs(t.Context(), []store.MessageDateRepair{
		{
			ID:                     oldID,
			SentAt:                 repairedOld,
			ExpectedLastModifiedAt: candidates[0].LastModifiedAt,
		},
		{
			ID:                     missingID,
			SentAt:                 repairedMissing,
			ExpectedLastModifiedAt: candidates[1].LastModifiedAt,
		},
	}, lower, upper)
	require.NoError(err, "ApplyMessageDateRepairs")

	for id, want := range map[int64]time.Time{
		oldID:     repairedOld,
		missingID: repairedMissing,
	} {
		var got time.Time
		require.NoError(
			f.Store.DB().QueryRow(
				f.Store.Rebind("SELECT sent_at FROM messages WHERE id = ?"),
				id,
			).Scan(&got),
			"query repaired sent_at",
		)
		assert.Equal(want, got.UTC())
	}
}

func TestApplyMessageDateRepairsRejectsChangedRowsAtomically(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	lower := time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC)
	upper := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

	firstID := f.NewMessage().
		WithSentAt(time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)).
		Create(t, f.Store)
	changedID := f.NewMessage().
		WithSentAt(time.Date(1971, 1, 1, 0, 0, 0, 0, time.UTC)).
		Create(t, f.Store)

	candidates, err := f.Store.ListMessageDateRepairCandidates(t.Context(), lower, upper)
	require.NoError(err)
	require.Len(candidates, 2)

	concurrentSentAt := time.Date(1972, 1, 1, 0, 0, 0, 0, time.UTC)
	_, err = f.Store.DB().Exec(
		f.Store.Rebind(`
			UPDATE messages
			SET sent_at = ?, last_modified = ?
			WHERE id = ?
		`),
		concurrentSentAt,
		"2001-02-03 04:05:06",
		changedID,
	)
	require.NoError(err)

	err = f.Store.ApplyMessageDateRepairs(context.Background(), []store.MessageDateRepair{
		{
			ID:                     firstID,
			SentAt:                 time.Date(2007, 1, 2, 0, 0, 0, 0, time.UTC),
			ExpectedLastModifiedAt: candidates[0].LastModifiedAt,
		},
		{
			ID:                     changedID,
			SentAt:                 time.Date(2008, 1, 2, 0, 0, 0, 0, time.UTC),
			ExpectedLastModifiedAt: candidates[1].LastModifiedAt,
		},
	}, lower, upper)
	require.ErrorContains(err, "changed after date repair planning")

	var sentAt sql.NullTime
	require.NoError(
		f.Store.DB().QueryRow(
			f.Store.Rebind("SELECT sent_at FROM messages WHERE id = ?"),
			firstID,
		).Scan(&sentAt),
		"query rolled back sent_at",
	)
	require.True(sentAt.Valid)
	assert.Equal(time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC), sentAt.Time.UTC())

	require.NoError(
		f.Store.DB().QueryRow(
			f.Store.Rebind("SELECT sent_at FROM messages WHERE id = ?"),
			changedID,
		).Scan(&sentAt),
		"query concurrently changed sent_at",
	)
	require.True(sentAt.Valid)
	assert.Equal(concurrentSentAt, sentAt.Time.UTC())
}
