package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/store"
)

func TestXRemovalSuppressionBlocksAnotherSourceImport(t *testing.T) {
	st, source := xPostFixture(t)
	_, err := st.PersistXPostContext(t.Context(), xPostInput(source, "102", "100", "same native content", "100", "102"))
	require.NoError(t, err)
	execution, err := st.AcquireSyncExecutionContext(t.Context(), source)
	require.NoError(t, err)
	_, err = st.RemoveXPostContentContext(t.Context(), source, "100", "removed")
	require.NoError(t, err)
	require.NoError(t, execution.Release())
	other, err := st.GetOrCreateSource("x", "2001")
	require.NoError(t, err)
	later := xPostInput(other.ID, "104", "100", "same native content", "102", "104")
	later.OwnerID = "2001"
	saved, err := st.PersistXPostContext(t.Context(), later)
	require.NoError(t, err)
	assert.True(t, saved.Skipped)
	target, err := st.ResolveXPostContext(t.Context(), other.ID, "104")
	require.NoError(t, err)
	assert.Equal(t, "removed", target.Availability)
	if saved.MessageID != 0 {
		body, err := st.GetMessageBodyText(saved.MessageID)
		require.NoError(t, err)
		assert.Empty(t, body)
	}
}

func TestXRemovalGlobalSuppressionFollowsForeignAliasBridge(t *testing.T) {
	st, source := xPostFixture(t)
	_, err := st.PersistXPostContext(t.Context(), xPostInput(source, "102", "100", "same native content", "100", "102"))
	require.NoError(t, err)
	other, err := st.GetOrCreateSource("x", "2001")
	require.NoError(t, err)
	bridge := xPostInput(other.ID, "104", "102", "same native content", "102", "104")
	bridge.OwnerID = "2001"
	_, err = st.PersistXPostContext(t.Context(), bridge)
	require.NoError(t, err)
	execution, err := st.AcquireSyncExecutionContext(t.Context(), source)
	require.NoError(t, err)
	_, err = st.RemoveXPostContentContext(t.Context(), source, "100", "removed")
	require.NoError(t, err)
	require.NoError(t, execution.Release())
	third, err := st.GetOrCreateSource("x", "3001")
	require.NoError(t, err)
	latest := xPostInput(third.ID, "104", "104", "must not cross the bridge", "104")
	latest.OwnerID = "3001"
	result, err := st.PersistXPostContext(t.Context(), latest)
	require.NoError(t, err)
	assert.True(t, result.Skipped)
	target, err := st.ResolveXPostContext(t.Context(), third.ID, "104")
	require.NoError(t, err)
	assert.Equal(t, "removed", target.Availability)
}

func TestXRemovalPreservesScopedSyncFence(t *testing.T) {
	st, source := xPostFixture(t)
	_, err := st.PersistXPostContext(t.Context(), xPostInput(source, "100", "100", "first occurrence"))
	require.NoError(t, err)
	other, err := st.GetOrCreateSource("x", "2001")
	require.NoError(t, err)
	duplicate := xPostInput(other.ID, "100", "100", "second occurrence")
	duplicate.OwnerID = "2001"
	saved, err := st.PersistXPostContext(t.Context(), duplicate)
	require.NoError(t, err)
	execution, err := st.AcquireSyncExecutionContext(t.Context(), source)
	require.NoError(t, err)
	defer func() { assert.NoError(t, execution.Release()) }()
	run, err := execution.StartSyncContext(t.Context(), "full", "")
	require.NoError(t, err)
	scoped := st.ScopedToSync(source, run)
	_, err = scoped.RemoveXPostContentContext(t.Context(), other.ID, "100", "removed")
	require.Error(t, err)
	body, err := st.GetMessageBodyText(saved.MessageID)
	require.NoError(t, err)
	assert.Equal(t, "second occurrence", body)
	_, err = scoped.AcquireXRemovalExecutionContext(t.Context(), source, "100")
	require.ErrorIs(t, err, store.ErrXRemovalNeedsOwner)
	require.NoError(t, st.CompleteSyncContext(t.Context(), run, ""))
	_, err = scoped.RemoveXPostContentContext(t.Context(), source, "100", "removed")
	require.ErrorIs(t, err, store.ErrSyncRunSuperseded)
}

func TestXRemovalGroupUsesOneSessionAndReleasesBusyReservation(t *testing.T) {
	st, source := xPostFixture(t)
	_, err := st.PersistXPostContext(t.Context(), xPostInput(source, "100", "100", "same native content"))
	require.NoError(t, err)
	other, err := st.GetOrCreateSource("x", "2001")
	require.NoError(t, err)
	duplicate := xPostInput(other.ID, "100", "100", "same native content")
	duplicate.OwnerID = "2001"
	_, err = st.PersistXPostContext(t.Context(), duplicate)
	require.NoError(t, err)
	st.DB().SetMaxOpenConns(2)
	st.DB().SetMaxIdleConns(2)
	busy, err := st.AcquireSyncExecutionContext(t.Context(), other.ID)
	require.NoError(t, err)
	_, err = st.AcquireXRemovalExecutionContext(t.Context(), source, "100")
	require.ErrorIs(t, err, store.ErrSyncAlreadyActive)
	require.NoError(t, busy.Release())
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	group, err := st.AcquireXRemovalExecutionContext(ctx, source, "100")
	require.NoError(t, err)
	defer func() { assert.NoError(t, group.Release()) }()
	require.Len(t, group.Targets, 2)
	for _, target := range group.Targets {
		_, err := st.RemoveXPostContentContext(ctx, target.SourceID, target.CanonicalID, "removed")
		require.NoError(t, err)
	}
	pending, err := st.XCleanupPendingContext(ctx)
	require.NoError(t, err)
	assert.True(t, pending)
}
