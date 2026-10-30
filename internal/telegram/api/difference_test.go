package api

import (
	"context"
	"errors"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/telegram"
)

func TestCommonDifferenceSlicePreservesEffectsAndProposesIntermediateState(t *testing.T) {
	mock := tgmock.NewRequire(t)
	mock.ExpectFunc(func(input bin.Encoder) {
		req, ok := input.(*tg.UpdatesGetDifferenceRequest)
		require.True(t, ok)
		assert.Equal(t, 10, req.Pts)
		assert.Equal(t, 2, req.Qts)
		assert.Equal(t, 100, req.Date)
	}).ThenResult(&tg.UpdatesDifferenceSlice{NewMessages: []tg.MessageClass{&tg.MessageEmpty{ID: 8}}, OtherUpdates: []tg.UpdateClass{&tg.UpdateDeleteMessages{Messages: []int{7}, Pts: 11, PtsCount: 1}}, IntermediateState: tg.UpdatesState{Pts: 11, Qts: 2, Date: 101, Seq: 5}})
	prior := telegram.CommonUpdateState{Pts: 10, Qts: 2, Date: 100, Seq: 4}
	page, err := GetCommonDifferencePage(context.Background(), tg.NewClient(mock), prior)
	{
		require := require.New(t)
		require.NoError(err)
	}
	{
		assert := assert.New(t)
		assert.True(page.More)
	}
	require.NotNil(t, page.Next)
	assert.Equal(t, 11, page.Next.Pts)
	assert.Equal(t, 10, prior.Pts)
	slice, ok := page.Response.(*tg.UpdatesDifferenceSlice)
	require.True(t, ok)
	assert.Len(t, slice.NewMessages, 1)
	assert.Len(t, slice.OtherUpdates, 1)
}

func TestCommonDifferenceTooLongDoesNotProposeCheckpoint(t *testing.T) {
	assert := assert.New(t)

	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.UpdatesDifferenceTooLong{Pts: 99})
	page, err := GetCommonDifferencePage(context.Background(), tg.NewClient(mock), telegram.CommonUpdateState{Pts: 10, Date: 100})
	require.NoError(t, err)
	assert.True(page.TooLong)
	assert.Nil(page.Next)
	assert.Equal(99, page.RecoveryPTS)
}

func TestCommonDifferenceEmptyPreservesPtsAndQts(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.UpdatesDifferenceEmpty{Date: 110, Seq: 7})
	prior := telegram.CommonUpdateState{Pts: 10, Qts: 2, Date: 100, Seq: 4}
	page, err := GetCommonDifferencePage(context.Background(), tg.NewClient(mock), prior)
	require.NoError(err)
	require.NotNil(page.Next)
	assert.Equal(telegram.CommonUpdateState{Pts: 10, Qts: 2, Date: 110, Seq: 7}, *page.Next)
	assert.False(page.More)
}

func TestCommonDifferenceRejectsRegressingRecoveryPosition(t *testing.T) {
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.UpdatesDifferenceTooLong{Pts: 9})
	_, err := GetCommonDifferencePage(context.Background(), tg.NewClient(mock), telegram.CommonUpdateState{Pts: 10, Date: 100})
	require.ErrorContains(t, err, "recovery")
}

func TestCommonDifferenceRejectsNonAdvancingSlice(t *testing.T) {
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.UpdatesDifferenceSlice{IntermediateState: tg.UpdatesState{Pts: 10, Date: 100}})
	_, err := GetCommonDifferencePage(context.Background(), tg.NewClient(mock), telegram.CommonUpdateState{Pts: 10, Date: 100})
	require.ErrorContains(t, err, "advance")
}

func TestCommitCommonDifferenceWritesBeforeCheckpoint(t *testing.T) {
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.UpdatesDifference{State: tg.UpdatesState{Pts: 11, Qts: 2, Date: 101, Seq: 5}})
	state := telegram.CommonUpdateState{Pts: 10, Qts: 2, Date: 100, Seq: 4}
	page, err := GetCommonDifferencePage(context.Background(), tg.NewClient(mock), state)
	require.NoError(t, err)
	wrote := false
	err = CommitCommonDifference(context.Background(), page, &state,
		func(context.Context, tg.UpdatesDifferenceClass) error { wrote = true; return nil },
		func(_ context.Context, next telegram.CommonUpdateState) error {
			assert.True(t, wrote)
			assert.Equal(t, 11, next.Pts)
			assert.Equal(t, 10, state.Pts)
			return nil
		})
	require.NoError(t, err)
	assert.Equal(t, 11, state.Pts)
}

func TestCommitCommonDifferenceFailureLeavesCheckpointUnchanged(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.UpdatesDifference{State: tg.UpdatesState{Pts: 11, Qts: 2, Date: 101, Seq: 5}})
	state := telegram.CommonUpdateState{Pts: 10, Qts: 2, Date: 100, Seq: 4}
	page, err := GetCommonDifferencePage(context.Background(), tg.NewClient(mock), state)
	require.NoError(err)
	fail := errors.New("synthetic failure")
	saved := false
	err = CommitCommonDifference(context.Background(), page, &state,
		func(context.Context, tg.UpdatesDifferenceClass) error { return fail },
		func(context.Context, telegram.CommonUpdateState) error { saved = true; return nil })
	require.ErrorIs(err, fail)
	assert.False(saved)
	assert.Equal(10, state.Pts)
}

func TestCommitCommonDifferenceRejectsStaleOrTooLongPage(t *testing.T) {
	for _, tooLong := range []bool{false, true} {
		mock := tgmock.NewRequire(t)
		if tooLong {
			mock.Expect().ThenResult(&tg.UpdatesDifferenceTooLong{Pts: 20})
		} else {
			mock.Expect().ThenResult(&tg.UpdatesDifference{State: tg.UpdatesState{Pts: 11, Qts: 2, Date: 101, Seq: 5}})
		}
		original := telegram.CommonUpdateState{Pts: 10, Qts: 2, Date: 100, Seq: 4}
		page, err := GetCommonDifferencePage(context.Background(), tg.NewClient(mock), original)
		require.NoError(t, err)
		state := original
		if !tooLong {
			state.Pts++
		}
		applied := false
		err = CommitCommonDifference(context.Background(), page, &state,
			func(context.Context, tg.UpdatesDifferenceClass) error { applied = true; return nil },
			func(context.Context, telegram.CommonUpdateState) error { return nil })
		require.Error(t, err)
		assert.False(t, applied)
	}
}
