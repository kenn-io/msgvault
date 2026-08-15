package store_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// TestEnsureConversationConflictDoesNotRequeueThread guards the sync hot path:
// EnsureConversation runs once per synced message, and on SQLite any
// conversations UPDATE requeues every message in the conversation. A
// get-or-create that hits the conflict path must leave the queue untouched, or
// an N-message thread costs ~N^2/2 queue writes per full sync.
func TestEnsureConversationConflictDoesNotRequeueThread(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	messageID := f.NewMessage().Create(t, f.Store)
	before := activityQueueRevision(t, f.Store, messageID)

	conversationID, err := f.Store.EnsureConversation(
		f.Source.ID, "default-thread", "Default Thread")
	require.NoError(err)
	assert.Equal(f.ConvID, conversationID,
		"conflict path must return the existing conversation")
	assert.Equal(before, activityQueueRevision(t, f.Store, messageID),
		"a get-or-create conflict must not requeue the thread")
}

// TestEnsureConversationWithTypeRequeuesOnlyOnRealChange pins the persist
// path's conflict guard: re-persisting identical conversation data must not
// requeue the thread, while a conversation_type change is classification input
// and must.
func TestEnsureConversationWithTypeRequeuesOnlyOnRealChange(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	conversationID, err := f.Store.EnsureConversationWithType(
		f.Source.ID, "typed-thread", "group_chat", "Typed Thread")
	require.NoError(err)
	messageID := storetest.NewMessage(f.Source.ID, conversationID).
		WithSourceMessageID("typed-thread-message").
		Create(t, f.Store)
	before := activityQueueRevision(t, f.Store, messageID)

	sameID, err := f.Store.EnsureConversationWithType(
		f.Source.ID, "typed-thread", "group_chat", "Typed Thread")
	require.NoError(err)
	assert.Equal(conversationID, sameID)
	assert.Equal(before, activityQueueRevision(t, f.Store, messageID),
		"an idempotent re-persist must not requeue the thread")

	changedID, err := f.Store.EnsureConversationWithType(
		f.Source.ID, "typed-thread", "channel", "Typed Thread")
	require.NoError(err)
	assert.Equal(conversationID, changedID)
	assert.Greater(activityQueueRevision(t, f.Store, messageID), before,
		"a conversation_type change reclassifies and must requeue the thread")
}

// TestConversationParticipantSnapshotRequeuesOnlyOnRealChange pins the
// set-difference membership update: chat importers re-persist the full member
// snapshot once per message, and a delete-and-reinsert of an unchanged
// snapshot would requeue every message in the conversation twice per member.
func TestConversationParticipantSnapshotRequeuesOnlyOnRealChange(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	first := f.EnsureParticipant("member-one@example.com", "Member One", "example.com")
	second := f.EnsureParticipant("member-two@example.com", "Member Two", "example.com")
	messageID := f.NewMessage().Create(t, f.Store)

	snapshot := []store.ConversationParticipantRef{
		{ParticipantID: first, Role: "member"},
		{ParticipantID: second, Role: "member"},
	}
	require.NoError(f.Store.ReplaceConversationParticipants(f.ConvID, snapshot))
	before := activityQueueRevision(t, f.Store, messageID)

	require.NoError(f.Store.ReplaceConversationParticipants(f.ConvID, snapshot))
	assert.Equal(before, activityQueueRevision(t, f.Store, messageID),
		"an unchanged membership snapshot must write nothing")

	require.NoError(f.Store.ReplaceConversationParticipants(f.ConvID,
		[]store.ConversationParticipantRef{{ParticipantID: first, Role: "member"}}))
	assert.Greater(activityQueueRevision(t, f.Store, messageID), before,
		"a removed member changes co-presence and must requeue the thread")
}

// TestConversationStatsRecomputeDoesNotRequeueActivity pins the targeted
// conversation trigger: routine per-source statistics recomputation rewrites
// every conversation row after each import and must not reproject the archive
// or hold the contact-state freshness barrier open.
func TestConversationStatsRecomputeDoesNotRequeueActivity(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	messageID := f.NewMessage().Create(t, f.Store)
	before := activityQueueRevision(t, f.Store, messageID)

	require.NoError(f.Store.RecomputeConversationStats(f.Source.ID))
	assert.Equal(before, activityQueueRevision(t, f.Store, messageID),
		"statistics recomputation must not requeue the archive")
}

// TestReplaceConversationParticipantsHandlesHugeMemberships syncs a channel
// with more members than SQLite's 32,766 bound-variable cap. The removal
// DELETE binds the desired set as one JSON parameter; a placeholder per
// member made large Slack/Teams channel synchronization fail outright.
// SQLite-only: the regression targets SQLite's variable cap and PostgreSQL
// shares the same statement shape, while 66k round trips to a PostgreSQL
// server would dominate the suite's runtime.
func TestReplaceConversationParticipantsHandlesHugeMemberships(t *testing.T) {
	testutil.SkipIfPostgres(t, "SQLite bound-variable cap regression; shared statement shape")
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)

	const members = 33_000
	ids := make([]int64, 0, members)
	for start := 0; start < members; start += 500 {
		count := min(500, members-start)
		values := make([]string, count)
		for index := range values {
			values[index] = "('Member', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)"
		}
		func() {
			rows, err := f.Store.DB().Query(
				`INSERT INTO participants (display_name, created_at, updated_at) VALUES ` +
					strings.Join(values, ",") + ` RETURNING id`)
			require.NoError(err, "seed participants")
			defer func() { require.NoError(rows.Close()) }()
			for rows.Next() {
				var id int64
				require.NoError(rows.Scan(&id))
				ids = append(ids, id)
			}
			require.NoError(rows.Err())
		}()
	}
	require.Len(ids, members)

	refs := make([]store.ConversationParticipantRef, members)
	for index, id := range ids {
		refs[index] = store.ConversationParticipantRef{ParticipantID: id, Role: "member"}
	}
	require.NoError(f.Store.ReplaceConversationParticipants(f.ConvID, refs),
		"membership beyond the bound-variable cap must sync")

	require.NoError(f.Store.ReplaceConversationParticipants(f.ConvID, refs[:100]),
		"shrinking the membership must delete through the JSON set parameter")
	var remaining int
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(
		`SELECT COUNT(*) FROM conversation_participants WHERE conversation_id = ?`),
		f.ConvID).Scan(&remaining))
	assert.Equal(100, remaining)
}
