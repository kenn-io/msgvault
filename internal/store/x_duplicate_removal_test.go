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
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st, source := xPostFixture(t)
	_, err := st.PersistXPostContext(t.Context(), xPostInput(source, "102", "100", "same native content", "100", "102"))
	_testRequire.NoError(err)
	execution, err := st.AcquireSyncExecutionContext(t.Context(), source)
	_testRequire.NoError(err)
	_, err = st.RemoveXPostContentContext(t.Context(), source, "100", "removed")
	_testRequire.NoError(err)
	_testRequire.NoError(execution.Release())
	other, err := st.GetOrCreateSource("x", "2001")
	_testRequire.NoError(err)
	later := xPostInput(other.ID, "104", "100", "same native content", "102", "104")
	later.OwnerID = "2001"
	saved, err := st.PersistXPostContext(t.Context(), later)
	_testRequire.NoError(err)
	_testAssert.True(saved.Skipped)
	target, err := st.ResolveXPostContext(t.Context(), other.ID, "104")
	_testRequire.NoError(err)
	_testAssert.Equal("removed", target.Availability)
	if saved.MessageID != 0 {
		body, err := st.GetMessageBodyText(saved.MessageID)
		_testRequire.NoError(err)
		_testAssert.Empty(body)
	}
}

func TestXRemovalGlobalSuppressionFollowsForeignAliasBridge(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st, source := xPostFixture(t)
	_, err := st.PersistXPostContext(t.Context(), xPostInput(source, "102", "100", "same native content", "100", "102"))
	_testRequire.NoError(err)
	other, err := st.GetOrCreateSource("x", "2001")
	_testRequire.NoError(err)
	bridge := xPostInput(other.ID, "104", "102", "same native content", "102", "104")
	bridge.OwnerID = "2001"
	_, err = st.PersistXPostContext(t.Context(), bridge)
	_testRequire.NoError(err)
	execution, err := st.AcquireSyncExecutionContext(t.Context(), source)
	_testRequire.NoError(err)
	_, err = st.RemoveXPostContentContext(t.Context(), source, "100", "removed")
	_testRequire.NoError(err)
	_testRequire.NoError(execution.Release())
	third, err := st.GetOrCreateSource("x", "3001")
	_testRequire.NoError(err)
	latest := xPostInput(third.ID, "104", "104", "must not cross the bridge", "104")
	latest.OwnerID = "3001"
	result, err := st.PersistXPostContext(t.Context(), latest)
	_testRequire.NoError(err)
	_testAssert.True(result.Skipped)
	target, err := st.ResolveXPostContext(t.Context(), third.ID, "104")
	_testRequire.NoError(err)
	_testAssert.Equal("removed", target.Availability)
}

func TestXRemovalPreservesScopedSyncFence(t *testing.T) {
	_testRequire := require.New(t)

	st, source := xPostFixture(t)
	_, err := st.PersistXPostContext(t.Context(), xPostInput(source, "100", "100", "first occurrence"))
	_testRequire.NoError(err)
	other, err := st.GetOrCreateSource("x", "2001")
	_testRequire.NoError(err)
	duplicate := xPostInput(other.ID, "100", "100", "second occurrence")
	duplicate.OwnerID = "2001"
	saved, err := st.PersistXPostContext(t.Context(), duplicate)
	_testRequire.NoError(err)
	execution, err := st.AcquireSyncExecutionContext(t.Context(), source)
	_testRequire.NoError(err)
	defer func() { assert.NoError(t, execution.Release()) }()
	run, err := execution.StartSyncContext(t.Context(), "full", "")
	_testRequire.NoError(err)
	scoped := st.ScopedToSync(source, run)
	_, err = scoped.RemoveXPostContentContext(t.Context(), other.ID, "100", "removed")
	_testRequire.Error(err)
	body, err := st.GetMessageBodyText(saved.MessageID)
	_testRequire.NoError(err)
	assert.Equal(t, "second occurrence", body)
	_, err = scoped.AcquireXRemovalExecutionContext(t.Context(), source, "100")
	_testRequire.ErrorIs(err, store.ErrXRemovalNeedsOwner)
	_testRequire.NoError(st.CompleteSyncContext(t.Context(), run, ""))
	_, err = scoped.RemoveXPostContentContext(t.Context(), source, "100", "removed")
	_testRequire.ErrorIs(err, store.ErrSyncRunSuperseded)
}

func TestXRemovalGroupUsesOneSessionAndReleasesBusyReservation(t *testing.T) {
	_testRequire := require.New(t)

	st, source := xPostFixture(t)
	_, err := st.PersistXPostContext(t.Context(), xPostInput(source, "100", "100", "same native content"))
	_testRequire.NoError(err)
	other, err := st.GetOrCreateSource("x", "2001")
	_testRequire.NoError(err)
	duplicate := xPostInput(other.ID, "100", "100", "same native content")
	duplicate.OwnerID = "2001"
	_, err = st.PersistXPostContext(t.Context(), duplicate)
	_testRequire.NoError(err)
	st.DB().SetMaxOpenConns(2)
	st.DB().SetMaxIdleConns(2)
	busy, err := st.AcquireSyncExecutionContext(t.Context(), other.ID)
	_testRequire.NoError(err)
	_, err = st.AcquireXRemovalExecutionContext(t.Context(), source, "100")
	_testRequire.ErrorIs(err, store.ErrSyncAlreadyActive)
	_testRequire.NoError(busy.Release())
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	group, err := st.AcquireXRemovalExecutionContext(ctx, source, "100")
	_testRequire.NoError(err)
	defer func() { assert.NoError(t, group.Release()) }()
	_testRequire.Len(group.Targets, 2)
	for _, target := range group.Targets {
		_, err := st.RemoveXPostContentContext(ctx, target.SourceID, target.CanonicalID, "removed")
		_testRequire.NoError(err)
	}
	pending, err := st.XCleanupPendingContext(ctx)
	_testRequire.NoError(err)
	assert.True(t, pending)
}
