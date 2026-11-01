package store_test

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestChatMembersRefreshVsSourceRemovalPG(t *testing.T) {
	requirements := require.New(t)
	st := requirePostgreSQLStore(t)
	chat := newAttrFixtureOn(t, st, "beeper", "chat-account")
	victim := newAttrFixtureOn(t, st, "gmail", "victim@example.net")
	conversation, err := st.EnsureConversationWithType(chat.source.ID, "refresh-room", "direct_chat", "Refresh Room")
	requirements.NoError(err)
	_, err = st.UpsertMessage(&store.Message{SourceID: chat.source.ID, SourceMessageID: "refresh", ConversationID: conversation,
		MessageType: "beeper", SentAt: sql.NullTime{Time: time.Unix(100, 0), Valid: true}})
	requirements.NoError(err)

	paused := make(chan struct{})
	resume := make(chan struct{})
	var pauseOnce sync.Once
	restore := st.SetChatMembersAfterClaimHookForTest(func() {
		pauseOnce.Do(func() {
			close(paused)
			<-resume
		})
	})
	defer restore()
	resumeOnce := sync.OnceFunc(func() { close(resume) })
	t.Cleanup(resumeOnce)

	searchDone := make(chan error, 1)
	go func() {
		_, err := st.SearchChatsContext(context.Background(), store.ChatDiscoveryQuery{Query: "Refresh"})
		searchDone <- err
	}()
	<-paused
	removeDone := make(chan error, 1)
	go func() {
		_, _, err := st.RemoveSourceSerialized(context.Background(), victim.source.ID)
		removeDone <- err
	}()
	waitForLockWait(t, st, "archive_metadata", "source removal must wait at the identity row, before its table locks")
	resumeOnce()
	assertNoDeadlock(t, waitResult(t, searchDone, "chat discovery refresh"))
	assertNoDeadlock(t, waitResult(t, removeDone, "source removal"))
}

// A writer that changes an already queued chat must leave it queued until a
// refresh can see the change, even when a refresh runs before it commits.
func TestChatMembersWriterCommittingAfterRefreshPG(t *testing.T) {
	requirements := require.New(t)
	st := requirePostgreSQLStore(t)
	chat := newAttrFixtureOn(t, st, "beeper", "writer-account")
	conversation, err := st.EnsureConversationWithType(chat.source.ID, "writer-room", "group_chat", "")
	requirements.NoError(err)
	message, err := st.UpsertMessage(&store.Message{SourceID: chat.source.ID, SourceMessageID: "writer", ConversationID: conversation,
		MessageType: "beeper", SentAt: sql.NullTime{Time: time.Unix(100, 0), Valid: true}})
	requirements.NoError(err)
	member, err := st.EnsureParticipant("member@example.net", "Plain Member", "example.net")
	requirements.NoError(err)
	requirements.NoError(st.ReplaceMessageRecipients(message, "to", []int64{member}, []string{"Kilo Alias"}))

	writer, err := st.DB().BeginTx(t.Context(), nil)
	requirements.NoError(err)
	t.Cleanup(func() { _ = writer.Rollback() })
	_, err = writer.ExecContext(t.Context(), `UPDATE message_recipients SET display_name = 'Lima Alias' WHERE message_id = $1`, message)
	requirements.NoError(err)

	searchDone := make(chan error, 1)
	go func() {
		_, err := st.SearchChatsContext(context.Background(), store.ChatDiscoveryQuery{Query: "Kilo"})
		searchDone <- err
	}()
	requirements.NoError(waitResult(t, searchDone, "chat discovery during an open write"))
	requirements.NoError(writer.Commit())

	page, err := st.SearchChatsContext(t.Context(), store.ChatDiscoveryQuery{Query: "Lima"})
	requirements.NoError(err)
	requirements.Len(page.Results, 1, "the committed alias must reach discovery")
	requirements.Equal(conversation, page.Results[0].ConversationID)
}

// A recipient writer that locks a message and then queues its chat must not
// deadlock with a refresh that already claimed the chat and rebuilds its
// anchor from that message.
func TestChatMembersRefreshVsRecipientWriterPG(t *testing.T) {
	requirements := require.New(t)
	st := requirePostgreSQLStore(t)
	chat := newAttrFixtureOn(t, st, "beeper", "recipient-account")
	conversation, err := st.EnsureConversationWithType(chat.source.ID, "recipient-room", "group_chat", "Recipient Room")
	requirements.NoError(err)
	message, err := st.UpsertMessage(&store.Message{SourceID: chat.source.ID, SourceMessageID: "recipient", ConversationID: conversation,
		MessageType: "beeper", SentAt: sql.NullTime{Time: time.Unix(100, 0), Valid: true}})
	requirements.NoError(err)
	member, err := st.EnsureParticipant("recipient@example.net", "Recipient Member", "example.net")
	requirements.NoError(err)

	paused := make(chan struct{})
	resume := make(chan struct{})
	var pauseOnce sync.Once
	restore := st.SetChatMembersAfterClaimHookForTest(func() {
		pauseOnce.Do(func() {
			close(paused)
			<-resume
		})
	})
	defer restore()
	resumeOnce := sync.OnceFunc(func() { close(resume) })
	t.Cleanup(resumeOnce)

	searchDone := make(chan error, 1)
	go func() {
		_, err := st.SearchChatsContext(context.Background(), store.ChatDiscoveryQuery{Query: "Recipient"})
		searchDone <- err
	}()
	<-paused
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- st.ReplaceMessageRecipients(message, "to", []int64{member}, []string{"Recipient Alias"})
	}()
	waitForLockWait(t, st, "chat_members_dirty", "the recipient writer must wait for the claimed chat")
	resumeOnce()
	assertNoDeadlock(t, waitResult(t, searchDone, "chat discovery refresh"))
	assertNoDeadlock(t, waitResult(t, writeDone, "recipient write"))
}

func TestChatMembersRecipientReplacementVsJournalEnablePG(t *testing.T) {
	requirements := require.New(t)
	st := requirePostgreSQLStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	chat := newAttrFixtureOn(t, st, "beeper", "enable-account")
	conversation, err := st.EnsureConversationWithType(chat.source.ID, "enable-room", "group_chat", "Enable Room")
	requirements.NoError(err)
	message, err := st.UpsertMessageContext(ctx, &store.Message{
		SourceID: chat.source.ID, SourceMessageID: "recipient", ConversationID: conversation, MessageType: "beeper",
	})
	requirements.NoError(err)
	member, err := st.EnsureParticipant("sender@example.test", "Envelope Sender", "example.test")
	requirements.NoError(err)
	requirements.NoError(st.EnableEmbeddingChangeJournal(ctx))

	// Pause after the enqueue's clock lock, before the attribution UPDATE.
	_, err = st.DB().ExecContext(ctx, `
		CREATE FUNCTION pause_recipient_chat_enqueue() RETURNS trigger AS $$
		BEGIN
		    PERFORM pg_advisory_xact_lock(115102);
		    RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER trg_test_pause_recipient_chat_enqueue
		BEFORE INSERT ON chat_members_dirty FOR EACH ROW
		EXECUTE FUNCTION pause_recipient_chat_enqueue()`)
	requirements.NoError(err)
	gate, err := st.DB().BeginTx(ctx, nil)
	requirements.NoError(err)
	defer func() { _ = gate.Rollback() }()
	_, err = gate.ExecContext(ctx, `SELECT pg_advisory_xact_lock(115102)`)
	requirements.NoError(err)

	replaceDone := make(chan error, 1)
	go func() {
		replaceDone <- st.ReplaceMessageRecipientsContext(ctx, message, "from", []int64{member}, []string{"Envelope Sender"})
	}()
	replacePID := waitForLockWait(t, st, "INSERT INTO chat_members_dirty", "recipient replacement must reach the enqueue barrier")
	enableDone := make(chan error, 1)
	go func() { enableDone <- st.EnableEmbeddingChangeJournal(ctx) }()
	requirements.Eventually(func() bool {
		var waiting bool
		err = st.DB().QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity WHERE datname = current_database()
			AND $1 = ANY(pg_blocking_pids(pid)))`, replacePID).Scan(&waiting)
		return err == nil && waiting
	}, pgWaitBudget, 10*time.Millisecond, "journal enable must wait for recipient replacement")
	requirements.NoError(err)
	requirements.NoError(gate.Commit())
	assertNoDeadlock(t, waitResult(t, replaceDone, "recipient replacement"))
	assertNoDeadlock(t, waitResult(t, enableDone, "journal enable"))
	var recipient int64
	requirements.NoError(st.DB().QueryRowContext(ctx,
		`SELECT participant_id FROM message_recipients WHERE message_id = $1 AND recipient_type = 'from'`, message).Scan(&recipient))
	requirements.Equal(member, recipient)
}

func TestChatMembersInsertVsTimestampUpdatePG(t *testing.T) {
	for _, tc := range []struct {
		name string
		body bool
	}{{"bodyless", false}, {"with body", true}} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			st := requirePostgreSQLStore(t)
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			chat := newAttrFixtureOn(t, st, "beeper", "journal-account")
			conversation, err := st.EnsureConversationWithType(chat.source.ID, "journal-room", "group_chat", "Journal Room")
			requirements.NoError(err)
			existing := store.Message{SourceID: chat.source.ID, SourceMessageID: "existing", ConversationID: conversation,
				MessageType: "beeper", Subject: sql.NullString{String: "Existing message", Valid: true},
				SentAt: sql.NullTime{Time: time.Unix(100, 0), Valid: true}}
			message, err := st.UpsertMessageContext(ctx, &existing)
			requirements.NoError(err)
			if tc.body {
				requirements.NoError(st.UpsertMessageBody(message, sql.NullString{String: "Original body", Valid: true}, sql.NullString{}))
			}
			requirements.NoError(st.EnableEmbeddingChangeJournal(ctx))

			// Pause the real fresh-insert path just before it queues the chat,
			// after its journal append. The uncommitted row identifies that
			// transaction without changing either production write operation.
			_, err = st.DB().ExecContext(ctx, `
				CREATE FUNCTION pause_fresh_chat_enqueue() RETURNS trigger AS $$
				BEGIN
				    IF EXISTS (SELECT 1 FROM messages WHERE source_message_id = 'fresh') THEN
				        PERFORM pg_advisory_xact_lock(115101);
				    END IF;
				    RETURN NEW;
				END;
				$$ LANGUAGE plpgsql;
				CREATE TRIGGER trg_test_pause_fresh_chat_enqueue
				BEFORE INSERT ON chat_members_dirty FOR EACH ROW
				EXECUTE FUNCTION pause_fresh_chat_enqueue()`)
			requirements.NoError(err)
			gate, err := st.DB().BeginTx(ctx, nil)
			requirements.NoError(err)
			defer func() { _ = gate.Rollback() }()
			_, err = gate.ExecContext(ctx, `SELECT pg_advisory_xact_lock(115101)`)
			requirements.NoError(err)

			insertDone := make(chan error, 1)
			go func() {
				_, err := st.UpsertMessageContext(ctx, &store.Message{
					SourceID: chat.source.ID, SourceMessageID: "fresh", ConversationID: conversation,
					MessageType: "beeper", SentAt: sql.NullTime{Time: time.Unix(300, 0), Valid: true},
				})
				insertDone <- err
			}()
			insertPID := waitForLockWait(t, st, "INSERT INTO chat_members_dirty", "fresh insert must reach the enqueue barrier")
			updateDone := make(chan error, 1)
			go func() {
				existing.SentAt = sql.NullTime{Time: time.Unix(200, 0), Valid: true}
				_, err := st.UpsertMessageContext(ctx, &existing)
				updateDone <- err
			}()
			requirements.Eventually(func() bool {
				var waiting bool
				err = st.DB().QueryRowContext(ctx, `SELECT EXISTS (
					SELECT 1 FROM pg_stat_activity WHERE datname = current_database()
					AND $1 = ANY(pg_blocking_pids(pid)))`, insertPID).Scan(&waiting)
				return err == nil && waiting
			}, pgWaitBudget, 10*time.Millisecond, "timestamp update must wait for the fresh insert's journal clock")
			requirements.NoError(err)
			requirements.NoError(gate.Commit())
			assertNoDeadlock(t, waitResult(t, insertDone, "fresh message insert"))
			assertNoDeadlock(t, waitResult(t, updateDone, "message timestamp update"))

			var count int
			requirements.NoError(st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE conversation_id = $1`, conversation).Scan(&count))
			assertions.Equal(2, count)
			var sentAt time.Time
			requirements.NoError(st.DB().QueryRowContext(ctx, `SELECT sent_at FROM messages WHERE id = $1`, message).Scan(&sentAt))
			assertions.Equal(time.Unix(200, 0).UTC(), sentAt.UTC())
		})
	}
}

func TestChatMembersRefreshVsParticipantRepairPG(t *testing.T) {
	requirements := require.New(t)
	st := requirePostgreSQLStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	chat := newAttrFixtureOn(t, st, "beeper", "repair-account")
	conversation, err := st.EnsureConversationWithType(chat.source.ID, "repair-room", "direct_chat", "")
	requirements.NoError(err)
	sender, err := st.EnsureParticipant("sender@example.test", "Repair Sender", "example.test")
	requirements.NoError(err)
	message, err := st.UpsertMessageContext(ctx, &store.Message{
		SourceID: chat.source.ID, SourceMessageID: "repair", ConversationID: conversation,
		MessageType: "beeper", SenderID: sql.NullInt64{Int64: sender, Valid: true},
	})
	requirements.NoError(err)
	// Make the repair detect any cycle first. Otherwise a refresh retry can
	// hide the incompatible participant lock by releasing the claimed chat.
	_, err = st.DB().ExecContext(ctx, `
		CREATE FUNCTION defer_chat_refresh_deadlock_detection() RETURNS trigger AS $$
		BEGIN
		    PERFORM set_config('deadlock_timeout', '1h', true);
		    RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER trg_test_defer_chat_refresh_deadlock_detection
		BEFORE INSERT ON chat_members FOR EACH ROW
		EXECUTE FUNCTION defer_chat_refresh_deadlock_detection()`)
	requirements.NoError(err)
	paused := make(chan struct{})
	resume := make(chan struct{})
	var pauseOnce sync.Once
	restore := st.SetChatMembersAfterClaimHookForTest(func() {
		pauseOnce.Do(func() {
			close(paused)
			select {
			case <-resume:
			case <-ctx.Done():
			}
		})
	})
	t.Cleanup(restore)
	resumeOnce := sync.OnceFunc(func() { close(resume) })
	t.Cleanup(resumeOnce)
	searchDone := make(chan error, 1)
	go func() {
		_, err := st.SearchChatsContext(ctx, store.ChatDiscoveryQuery{Query: "Repair Sender"})
		searchDone <- err
	}()
	select {
	case <-paused:
	case <-ctx.Done():
		requirements.FailNow("refresh did not claim the chat", ctx.Err())
	}
	repairDone := make(chan error, 1)
	go func() {
		_, err := st.PersistRepairMessageWithParticipantsContext(ctx,
			store.MessageIdentityGuard{ID: message, SourceID: chat.source.ID, SourceMessageID: "repair"},
			[]store.ParticipantPersistData{{EmailAddress: "sender@example.test", DisplayName: "Repair Sender", Domain: "example.test"}},
			func(ids []int64) *store.MessagePersistData {
				return &store.MessagePersistData{Message: &store.Message{
					SourceID: chat.source.ID, SourceMessageID: "repair", ConversationID: conversation,
					MessageType: "beeper", SenderID: sql.NullInt64{Int64: ids[0], Valid: true},
				}, BodyText: sql.NullString{String: "Repaired body", Valid: true}}
			})
		repairDone <- err
	}()
	waitForLockWait(t, st, "INSERT INTO chat_members_dirty", "repair must wait for the claimed chat after resolving its sender")
	resumeOnce()
	assertNoDeadlock(t, waitResult(t, searchDone, "chat discovery refresh"))
	assertNoDeadlock(t, waitResult(t, repairDone, "participant-resolving message repair"))
	var body string
	requirements.NoError(st.DB().QueryRowContext(ctx, `SELECT body_text FROM message_bodies WHERE message_id = $1`, message).Scan(&body))
	requirements.Equal("Repaired body", body)
}
