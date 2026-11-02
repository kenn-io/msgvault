package store_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestXSuppressedReplayCannotRecreateErasedAuthorProfile(t *testing.T) {
	for _, mode := range []string{"same alias", "new edit", "other source"} {
		t.Run(mode, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st, source := xPostFixture(t)
			original := xPostInput(source, "100", "100", "erased post")
			_, err := st.PersistXPostContext(t.Context(), original)
			require.NoError(err)
			work, err := st.RemoveXPostContentContext(t.Context(), source, "100", "deleted")
			require.NoError(err)
			work.DerivedComplete = true
			require.NoError(st.FinishXRemovalContext(t.Context(), source, work))
			checkErased := func() {
				t.Helper()
				var names int
				require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM participant_contact_observations WHERE address_kind='username' AND source='archive_observation'`).Scan(&names))
				assert.Zero(names, "completed suppression must not republish an erased username")
				var name sql.NullString
				require.NoError(st.DB().QueryRow(`SELECT p.display_name FROM participants p WHERE EXISTS(SELECT 1 FROM participant_identifiers pi WHERE pi.participant_id=p.id AND pi.identifier_type='x_user_id' AND pi.identifier_value='1002')`).Scan(&name))
				assert.Empty(name.String, "numeric anchors do not authorize profile text")
			}
			checkErased()
			replay := xPostInput(source, "100", "100", "must stay erased")
			replay.FetchedAt = original.FetchedAt.Add(time.Hour)
			if mode == "new edit" {
				replay.PostID = "102"
				replay.EditHistory = []string{"100", "102"}
			}
			if mode == "other source" {
				other, err := st.GetOrCreateSource("x", "1003")
				require.NoError(err)
				replay.SourceID, replay.OwnerID = other.ID, "1003"
				replay.Author.SourceID = other.ID
			}
			result, err := st.PersistXPostContext(t.Context(), replay)
			require.NoError(err)
			assert.True(result.Skipped)
			checkErased()
			pending, err := st.XCleanupPendingContext(t.Context())
			require.NoError(err)
			assert.False(pending, "replaying completed removal needs no repeated cleanup")
		})
	}
}

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
