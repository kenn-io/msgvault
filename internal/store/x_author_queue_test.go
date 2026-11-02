package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestXAuthorQueueCoversForeignOnlyChainsWithoutForeignPurge(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st := testutil.NewTestStore(t)
	selected, err := st.GetOrCreateSource("x", "1001")
	_testRequire.NoError(err)
	foreign, err := st.GetOrCreateSource("x", "1003")
	_testRequire.NoError(err)
	wrong, err := st.GetOrCreateSource("mbox", "example@example.test")
	_testRequire.NoError(err)
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	var messages []int64
	for _, entry := range []struct {
		source            int64
		owner, id, author string
	}{{selected.ID, "1001", "301", "1001"}, {foreign.ID, "1003", "601", "1001"}, {foreign.ID, "1003", "701", "1002"}} {
		saved, err := st.PersistXPostContext(t.Context(), store.XPersistPostInput{SourceID: entry.source, OwnerID: entry.owner, PostID: entry.id, AuthorID: entry.author, ConversationID: entry.id, EditHistory: []string{entry.id}, Text: "synthetic queued author body", CreatedAt: at, FetchedAt: at, Availability: "public"})
		_testRequire.NoError(err)
		messages = append(messages, saved.MessageID)
	}
	_, err = st.ResolveXPostContext(t.Context(), selected.ID, "601")
	_testRequire.ErrorIs(err, store.ErrXPostNotFound)
	err = st.QueueXAuthorRemovalContext(t.Context(), wrong.ID, "1001", at)
	_testRequire.Error(err, "a non-X source cannot authorize global X author suppression")
	execution, err := st.AcquireSyncExecutionContext(t.Context(), selected.ID)
	_testRequire.NoError(err)
	defer func() { assert.NoError(t, execution.Release()) }()
	run, err := execution.StartSyncContext(t.Context(), "x", "")
	_testRequire.NoError(err)
	scoped := st.ScopedToSync(selected.ID, run)
	err = scoped.QueueXAuthorRemovalContext(t.Context(), foreign.ID, "1001", at)
	_testRequire.Error(err, "retain the selected source generation guard")
	pending, err := st.XCleanupPendingContext(t.Context())
	_testRequire.NoError(err)
	_testAssert.False(pending)
	_testRequire.NoError(scoped.QueueXAuthorRemovalContext(t.Context(), selected.ID, "1001", at))
	// Pending work must keep its durable reason even if row availability differs.
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE x_post_versions SET availability='removed' WHERE source_id=? AND post_id='601'`), foreign.ID)
	_testRequire.NoError(err)
	works, err := st.PendingXRemovalsContext(t.Context(), 0)
	_testRequire.NoError(err)
	_testAssert.Len(works, 2, "both known author chains must be durably queued")
	var foreignPending, unrelatedPending, foreignReasonChecked bool
	for _, work := range works {
		if work.SourceID == foreign.ID {
			_testAssert.Equal("protected", work.Reason, "pending cleanup must keep its suppression reason even if a row's availability differs")
			foreignReasonChecked = true
		}
	}
	_testAssert.True(foreignReasonChecked)
	_testRequire.NoError(st.DB().QueryRow(st.Rebind("SELECT cleanup_pending FROM x_post_versions WHERE source_id=? AND post_id=?"), foreign.ID, "601").Scan(&foreignPending))
	_testRequire.NoError(st.DB().QueryRow(st.Rebind("SELECT cleanup_pending FROM x_post_versions WHERE source_id=? AND post_id=?"), foreign.ID, "701").Scan(&unrelatedPending))
	_testAssert.True(foreignPending)
	_testAssert.False(unrelatedPending)
	_, err = scoped.RemoveXPostContentContext(t.Context(), foreign.ID, "601", "protected")
	_testRequire.Error(err)
	for _, id := range messages {
		var body string
		_testRequire.NoError(st.DB().QueryRow(st.Rebind("SELECT body_text FROM message_bodies WHERE message_id=?"), id).Scan(&body))
		_testAssert.Equal("synthetic queued author body", body, "queueing never purges under another source's execution")
	}
	_testRequire.NoError(st.CompleteSyncContext(t.Context(), run, ""))
}
