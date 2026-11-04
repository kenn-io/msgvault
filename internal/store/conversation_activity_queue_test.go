package store_test

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"testing"
	"time"

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
	expandActivityConversations(t, f.Store)
	before := activityQueueRevision(t, f.Store, messageID)

	require.NoError(f.Store.ReplaceConversationParticipants(f.ConvID, snapshot))
	expandActivityConversations(t, f.Store)
	assert.Equal(before, activityQueueRevision(t, f.Store, messageID),
		"an unchanged membership snapshot must write nothing")

	require.NoError(f.Store.ReplaceConversationParticipants(f.ConvID,
		[]store.ConversationParticipantRef{{ParticipantID: first, Role: "member"}}))
	expandActivityConversations(t, f.Store)
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

func expandActivityConversations(t *testing.T, st *store.Store) {
	t.Helper()
	for {
		expanded, err := st.ExpandActivityConversationQueueContext(t.Context(), func() error { return nil })
		require.NoError(t, err)
		if !expanded {
			return
		}
	}
}

func TestConversationActivityExpansionBoundsAndRestarts(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	ids := f.CreateMessages(1001)
	firstBefore := activityQueueRevision(t, f.Store, ids[0])
	lastBefore := activityQueueRevision(t, f.Store, ids[1000])
	member := f.EnsureParticipant("member@example.com", "Member", "example.com")
	require.NoError(f.Store.EnsureConversationParticipant(f.ConvID, member, "member"))
	var count int
	require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM activity_projection_conversation_queue`).Scan(&count))
	assert.Equal(1, count, "fresh schema installs the membership marker trigger")
	assert.Equal(firstBefore, activityQueueRevision(t, f.Store, ids[0]))
	assert.Equal(lastBefore, activityQueueRevision(t, f.Store, ids[1000]))

	expanded, err := f.Store.ExpandActivityConversationQueueContext(t.Context(), func() error { return nil })
	require.NoError(err)
	assert.True(expanded)
	assert.Equal(firstBefore+1, activityQueueRevision(t, f.Store, ids[0]))
	assert.Equal(lastBefore, activityQueueRevision(t, f.Store, ids[1000]))
	var cursor int64
	require.NoError(f.Store.DB().QueryRow(`SELECT after_message_id FROM activity_projection_conversation_queue`).Scan(&cursor))
	assert.Equal(ids[999], cursor)

	// A membership change restarts the chunk already expanded.
	second := f.EnsureParticipant("second@example.com", "Second", "example.com")
	require.NoError(f.Store.EnsureConversationParticipant(f.ConvID, second, "member"))
	require.NoError(f.Store.DB().QueryRow(`SELECT after_message_id FROM activity_projection_conversation_queue`).Scan(&cursor))
	assert.Zero(cursor)
	expandActivityConversations(t, f.Store)
	assert.Equal(firstBefore+2, activityQueueRevision(t, f.Store, ids[0]))
	assert.Equal(lastBefore+1, activityQueueRevision(t, f.Store, ids[1000]))
	expandActivityConversations(t, f.Store)
	assert.Equal(firstBefore+2, activityQueueRevision(t, f.Store, ids[0]))
	assert.Equal(lastBefore+1, activityQueueRevision(t, f.Store, ids[1000]))
}

func TestConversationActivityExpansionSkipsSnapshotPersistencePG(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	if !f.Store.IsPostgreSQL() {
		t.Skip("PostgreSQL row-lock regression")
	}
	id := f.CreateMessage("snapshot")
	member := f.EnsureParticipant("member@example.com", "Member", "example.com")
	require.NoError(f.Store.EnsureConversationParticipant(f.ConvID, member, "member"))
	second := f.EnsureParticipant("second@example.com", "Second", "example.com")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	blocker, err := f.Store.DB().BeginTx(ctx, nil)
	require.NoError(err)
	_, err = blocker.ExecContext(ctx, f.Store.Rebind(`SELECT message_id FROM activity_projection_queue WHERE message_id = ? FOR UPDATE`), id)
	require.NoError(err)
	var wg sync.WaitGroup
	defer func() {
		_ = blocker.Rollback()
		cancel()
		wg.Wait()
	}()
	done := make(chan error, 1)
	wg.Go(func() {
		_, err := f.Store.PersistMessageContext(ctx, &store.MessagePersistData{
			Message: &store.Message{
				SourceID: f.Source.ID, SourceMessageID: "snapshot", MessageType: "email",
				Subject: sql.NullString{String: "after", Valid: true},
				SentAt:  sql.NullTime{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Valid: true},
			},
			Conversation: &store.ConversationPersistData{
				SourceConversationID: "default-thread", ConversationType: "email_thread", Title: "Default Thread",
				Participants: []store.ConversationParticipantRef{{ParticipantID: member, Role: "member"}, {ParticipantID: second, Role: "member"}},
			},
		})
		done <- err
	})
	waitForLockWait(t, f.Store, "WITH attribution", "snapshot must reach the message queue while holding its membership marker")
	expanded, err := f.Store.ExpandActivityConversationQueueContext(ctx, func() error { return nil })
	require.NoError(err)
	assert.False(expanded, "a locked marker is skipped before touching message queue rows")
	require.NoError(blocker.Rollback())
	require.NoError(<-done, "snapshot must commit without a deadlock")
	var subject string
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT subject FROM messages WHERE id = ?`), id).Scan(&subject))
	assert.Equal("after", subject)
}

func TestConversationActivityExpansionWithSourceRemoval(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	f.CreateMessage("removal-race")
	member := f.EnsureParticipant("member@example.com", "Member", "example.com")
	require.NoError(f.Store.EnsureConversationParticipant(f.ConvID, member, "member"))
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	var wg sync.WaitGroup
	var blocker *sql.Tx
	defer func() {
		if blocker != nil {
			_ = blocker.Rollback()
		}
		cancel()
		wg.Wait()
	}()
	var blockerPID int
	if f.Store.IsPostgreSQL() {
		_, err := f.Store.DB().ExecContext(ctx, `INSERT INTO archive_metadata (key, value) VALUES ('identity_revision', '0') ON CONFLICT DO NOTHING`)
		require.NoError(err)
		blocker, err = f.Store.DB().BeginTx(ctx, nil)
		require.NoError(err)
		require.NoError(blocker.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID))
		_, err = blocker.ExecContext(ctx, `UPDATE archive_metadata SET value = value WHERE key = 'identity_revision'`)
		require.NoError(err)
	}
	removeDone := make(chan error, 1)
	wg.Go(func() {
		_, _, err := f.Store.RemoveSourceSerialized(ctx, f.Source.ID)
		removeDone <- err
	})
	if blocker != nil {
		waitForLockWait(t, f.Store, "archive_metadata", "source removal must reach the identity lock")
	}
	expandDone := make(chan error, 1)
	wg.Go(func() {
		_, err := f.Store.ExpandActivityConversationQueueContext(ctx, func() error { return nil })
		expandDone <- err
	})
	if blocker != nil {
		// Both operations must wait before touching the queues. Without this
		// coordination, expansion can hold a marker while removal locks the
		// message queue first, producing a cycle in PostgreSQL's lock graph.
		completed := false
		require.Eventually(func() bool {
			select {
			case err := <-expandDone:
				assert.NoError(err)
				expandDone <- err
				completed = true
				return true
			default:
			}
			var waiting int
			err := f.Store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM pg_stat_activity
				WHERE datname = current_database() AND $1 = ANY(pg_blocking_pids(pid))`, blockerPID).Scan(&waiting)
			return err == nil && waiting == 2
		}, pgWaitBudget, 10*time.Millisecond, "expansion and removal must reach their coordination lock")
		assert.False(completed, "expansion must wait for the identity lock before claiming a membership marker")
		require.NoError(blocker.Rollback())
	}
	require.NoError(<-expandDone, "expansion must complete without a deadlock")
	require.NoError(<-removeDone, "source removal must complete without a deadlock")
	var remaining int
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT COUNT(*) FROM sources WHERE id = ?`), f.Source.ID).Scan(&remaining))
	assert.Zero(remaining, "source removal must commit")
}
