package api

import (
	"context"
	"errors"
	"testing"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/telegram"
)

func TestBootstrapUpdateStatePersistsBeforeUse(t *testing.T) {
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.UpdatesState{Pts: 12, Qts: 3, Date: 44, Seq: 5})
	state := telegram.NewSyncState()
	var persisted *telegram.CommonUpdateState
	err := BootstrapUpdates(context.Background(), tg.NewClient(mock), state, func(_ context.Context, next *telegram.SyncState) error {
		require.NotNil(t, next.Common)
		assert.Nil(t, state.Common)
		copy := *next.Common
		persisted = &copy
		return nil
	})
	{
		require := require.New(t)
		require.NoError(err)
	}
	require.NotNil(t, persisted)
	{
		assert := assert.New(t)
		assert.Equal(12, persisted.Pts)
	}
	assert.Equal(t, persisted, state.Common)
}

func TestBootstrapUpdateStateSaveFailureLeavesUninitialized(t *testing.T) {
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.UpdatesState{Pts: 12, Date: 44})
	state := telegram.NewSyncState()
	fail := errors.New("synthetic save failure")
	err := BootstrapUpdates(context.Background(), tg.NewClient(mock), state, func(context.Context, *telegram.SyncState) error { return fail })
	require.ErrorIs(t, err, fail)
	assert.Nil(t, state.Common)
}

func TestBootstrapUpdateStateDoesNotReplaceAppliedPosition(t *testing.T) {
	mock := tgmock.NewRequire(t)
	state := telegram.NewSyncState()
	state.Common = &telegram.CommonUpdateState{Pts: 12, Date: 44}
	err := BootstrapUpdates(context.Background(), tg.NewClient(mock), state, func(context.Context, *telegram.SyncState) error { return errors.New("unexpected save") })
	require.NoError(t, err)
	assert.Equal(t, 12, state.Common.Pts)
}
