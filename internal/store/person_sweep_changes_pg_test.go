package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPersonSweepJournalPostgreSQLSequenceFollowsCommitOrder(t *testing.T) {
	requirements := require.New(t)
	f := newPersonSweepJournalFixture(t, true, false)
	if !f.store.IsPostgreSQL() {
		t.Skip("PostgreSQL-only commit-order test")
	}
	firstSource, err := f.store.GetOrCreateSource("test", "person-sweep-first")
	requirements.NoError(err)
	secondSource, err := f.store.GetOrCreateSource("test", "person-sweep-second")
	requirements.NoError(err)
	firstConversation, err := f.store.EnsureConversationWithType(
		firstSource.ID, "first", "direct_chat", "First")
	requirements.NoError(err)
	secondConversation, err := f.store.EnsureConversationWithType(
		secondSource.ID, "second", "direct_chat", "Second")
	requirements.NoError(err)
	before := latestPersonSweepSequence(t, f.store)

	first, err := f.store.DB().BeginTx(context.Background(), nil)
	requirements.NoError(err)
	firstOpen := true
	t.Cleanup(func() {
		if firstOpen {
			_ = first.Rollback()
		}
	})
	second, err := f.store.DB().BeginTx(context.Background(), nil)
	requirements.NoError(err)
	secondOpen := true
	t.Cleanup(func() {
		if secondOpen {
			_ = second.Rollback()
		}
	})

	_, err = second.Exec(f.store.Rebind(`
		INSERT INTO messages
			(source_id, source_message_id, conversation_id, message_type, sender_id, sent_at)
		VALUES (?, 'second', ?, 'email', ?, ?)`),
		secondSource.ID, secondConversation, f.aliceID,
		time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC))
	requirements.NoError(err)
	requirements.NoError(second.Commit())
	secondOpen = false

	_, err = first.Exec(f.store.Rebind(`
		INSERT INTO messages
			(source_id, source_message_id, conversation_id, message_type, sender_id, sent_at)
		VALUES (?, 'first', ?, 'email', ?, ?)`),
		firstSource.ID, firstConversation, f.aliceID,
		time.Date(2001, 1, 2, 3, 4, 5, 0, time.UTC))
	requirements.NoError(err)
	requirements.NoError(first.Commit())
	firstOpen = false

	changes := personSweepChangesAfter(t, f.store, f.alicePersonID, before)
	requirements.Len(changes, 2)
	assert.Equal(t, []int64{secondSource.ID, firstSource.ID},
		[]int64{changes[0].SourceID, changes[1].SourceID})
}
