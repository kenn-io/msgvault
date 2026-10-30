package telegram

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestLiveSyncResumesOnlyLastSavedLiveCheckpoint(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, OwnerSourceIdentifier(999))
	{
		require := require.New(t)
		require.NoError(err)
	}
	importRun, err := st.StartSync(source.ID, "import-telegram")
	require.NoError(t, err)
	require.NoError(t, st.CompleteSync(importRun, `{"not_live_state":true}`))
	failure := errors.New("synthetic failure")
	err = WithLiveSyncState(ctx, st, source.ID, func(ctx context.Context, _ *store.Store, state *SyncState, save func(context.Context, *SyncState) error) error {
		assert.Empty(t, state.Dialogs)
		state.Dialogs["-123"] = DialogState{Frontier: 7}
		require.NoError(t, save(ctx, state))
		state.Dialogs["-123"] = DialogState{Frontier: 900}
		return failure
	})
	require.ErrorIs(t, err, failure)
	err = WithLiveSyncState(ctx, st, source.ID, func(_ context.Context, _ *store.Store, state *SyncState, _ func(context.Context, *SyncState) error) error {
		assert.Equal(t, 7, state.Dialogs["-123"].Frontier)
		state.Dialogs["-123"] = DialogState{Frontier: 900}
		return nil
	})
	require.NoError(t, err)
	completed, err := st.GetLastSuccessfulSyncByTypeContext(ctx, source.ID, "sync-telegram")
	require.NoError(t, err)
	decoded, err := LoadSyncState(completed.CursorAfter.String)
	require.NoError(t, err)
	assert.Equal(t, 7, decoded.Dialogs["-123"].Frontier)
}

func TestLiveSyncPublishesCheckpointBeforeWorkAndExcludesOverlap(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, OwnerSourceIdentifier(999))
	require.NoError(t, err)
	err = WithLiveSyncState(ctx, st, source.ID, func(ctx context.Context, _ *store.Store, _ *SyncState, _ func(context.Context, *SyncState) error) error {
		checkpoint, err := st.GetLatestCheckpointedSyncByTypeContext(ctx, source.ID, liveSyncType)
		require.NoError(t, err)
		assert.NotEmpty(t, checkpoint.CursorBefore.String)
		return WithLiveSyncState(ctx, st, source.ID, func(context.Context, *store.Store, *SyncState, func(context.Context, *SyncState) error) error {
			return nil
		})
	})
	require.ErrorIs(t, err, store.ErrSyncAlreadyActive)
}
