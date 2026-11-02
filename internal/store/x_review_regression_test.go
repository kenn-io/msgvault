package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestXProtectedRefreshRetainsAuthorEvidence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, source := xPostFixture(t)
	input := xPostInput(source, "100", "100", "original public text")
	_, err := st.PersistXPostContext(t.Context(), input)
	require.NoError(err)
	protectedAt := input.FetchedAt.Add(time.Hour)
	// Refresh queues known aliases before projecting the returned post.
	require.NoError(st.QueueXAuthorRemovalContext(t.Context(), source, "1002", protectedAt))
	input.Availability = "protected"
	input.FetchedAt = protectedAt
	input.Author.Protected = true
	_, err = st.PersistXPostContext(t.Context(), input)
	require.NoError(err)
	work, err := st.RemoveXPostContentContext(t.Context(), source, "100", "protected")
	require.NoError(err)
	work.DerivedComplete = true
	require.NoError(st.FinishXRemovalContext(t.Context(), source, work))
	unseen := xPostInput(source, "200", "200", "must not become readable")
	unseen.Author = nil
	unseen.Availability = "unverified"
	unseen.FetchedAt = protectedAt.Add(time.Hour)
	saved, err := st.PersistXPostContext(t.Context(), unseen)
	require.NoError(err)
	assert.True(saved.Skipped, "refresh must retain protected evidence even when its alias is already suppressed")
	assert.Zero(saved.MessageID)
}

func TestXFirstSeenWithheldQueuesCrossSourceCleanupAtomically(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, source := xPostFixture(t)
	_, err := st.PersistXPostContext(t.Context(), xPostInput(source, "100", "100", "connected public copy"))
	require.NoError(err)
	other, err := st.GetOrCreateSource("x", "1003")
	require.NoError(err)
	input := xPostInput(other.ID, "100", "100", "withheld text")
	input.OwnerID = "1003"
	input.Availability = "withheld"
	saved, err := st.PersistXPostContext(t.Context(), input)
	require.NoError(err)
	assert.Zero(saved.MessageID)
	// Simulate interruption before the importer can run its next cleanup step.
	pending, err := st.PendingXRemovalsContext(t.Context(), other.ID)
	require.NoError(err)
	require.Len(pending, 1)
	assert.Equal("withheld", pending[0].Reason)
	targets, err := st.XRemovalTargetsContext(t.Context(), other.ID, "100")
	require.NoError(err)
	assert.Len(targets, 2, "startup cleanup must discover the previously archived foreign copy")
	third, err := st.GetOrCreateSource("x", "1004")
	require.NoError(err)
	replay := xPostInput(third.ID, "100", "100", "must remain suppressed")
	replay.OwnerID = "1004"
	saved, err = st.PersistXPostContext(t.Context(), replay)
	require.NoError(err)
	assert.True(saved.Skipped)
}
