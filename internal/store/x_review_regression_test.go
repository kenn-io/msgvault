package store_test

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestXIDOnlyRemovalJoinsArchivedEditChain(t *testing.T) {
	for _, archived := range []string{"100", "102"} {
		for _, completed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/completed=%t", archived, completed), func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				st, source := xPostFixture(t)
				saved, err := st.PersistXPostContext(t.Context(), xPostInput(source, archived, archived, "retained edit text"))
				require.NoError(err)
				removed := "102"
				if archived == removed {
					removed = "100"
				}
				work, err := st.RemoveXPostContentContext(t.Context(), source, removed, "deleted")
				require.NoError(err)
				if completed {
					work.DerivedComplete = true
					require.NoError(st.FinishXRemovalContext(t.Context(), source, work))
				}
				input := xPostInput(source, "102", "100", "must stay suppressed", "100", "102")
				result, err := st.PersistXPostContext(t.Context(), input)
				require.NoError(err)
				assert.True(result.Skipped)
				assert.Equal(saved.MessageID, result.MessageID)
				for _, id := range input.EditHistory {
					target, err := st.ResolveXPostContext(t.Context(), source, id)
					require.NoError(err)
					assert.Equal(archived, target.CanonicalID)
					assert.Equal("removed", target.Availability)
				}
				pending, err := st.PendingXRemovalsContext(t.Context(), source)
				require.NoError(err)
				require.Len(pending, 1, "joining a completed ID-only removal must queue retained content too")
				assert.Equal([]int64{saved.MessageID}, pending[0].MessageIDs)
				work, err = st.RemoveXPostContentContext(t.Context(), source, pending[0].CanonicalID, pending[0].Reason)
				require.NoError(err)
				work.DerivedComplete = true
				require.NoError(st.FinishXRemovalContext(t.Context(), source, work))
				body, err := st.GetMessageBodyText(t.Context(), saved.MessageID)
				require.NoError(err)
				assert.Empty(body)
				_, err = st.GetMessageRaw(saved.MessageID)
				require.ErrorIs(err, sql.ErrNoRows)
				input.PostID, input.EditHistory = "104", []string{"100", "102", "104"}
				result, err = st.PersistXPostContext(t.Context(), input)
				require.NoError(err)
				assert.True(result.Skipped)
			})
		}
	}
}

func TestXRemovalDoesNotPermitJoiningIndependentArchivedChains(t *testing.T) {
	require := require.New(t)
	st, source := xPostFixture(t)
	for _, id := range []string{"100", "102"} {
		_, err := st.PersistXPostContext(t.Context(), xPostInput(source, id, id, "independent message"))
		require.NoError(err)
	}
	_, err := st.RemoveXPostContentContext(t.Context(), source, "102", "deleted")
	require.NoError(err)
	_, err = st.PersistXPostContext(t.Context(), xPostInput(source, "102", "100", "conflicting chain", "100", "102"))
	require.ErrorIs(err, store.ErrXPostConflict)
}

func TestXCompletedForeignRemovalQueuesNewlyLinkedEdit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, source := xPostFixture(t)
	saved, err := st.PersistXPostContext(t.Context(), xPostInput(source, "100", "100", "retained edit text"))
	require.NoError(err)
	other, err := st.GetOrCreateSource("x", "1003")
	require.NoError(err)
	work, err := st.RemoveXPostContentContext(t.Context(), other.ID, "102", "deleted")
	require.NoError(err)
	work.DerivedComplete = true
	require.NoError(st.FinishXRemovalContext(t.Context(), other.ID, work))
	pending, err := st.XCleanupPendingContext(t.Context())
	require.NoError(err)
	assert.False(pending)
	result, err := st.PersistXPostContext(t.Context(), xPostInput(source, "102", "100", "must stay suppressed", "100", "102"))
	require.NoError(err)
	assert.True(result.Skipped)
	workItems, err := st.PendingXRemovalsContext(t.Context(), source)
	require.NoError(err)
	require.Len(workItems, 1, "completed foreign evidence must queue newly connected retained content")
	assert.Equal([]int64{saved.MessageID}, workItems[0].MessageIDs)
	work, err = st.RemoveXPostContentContext(t.Context(), source, workItems[0].CanonicalID, workItems[0].Reason)
	require.NoError(err)
	work.DerivedComplete = true
	require.NoError(st.FinishXRemovalContext(t.Context(), source, work))
	body, err := st.GetMessageBodyText(t.Context(), saved.MessageID)
	require.NoError(err)
	assert.Empty(body)
}

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
