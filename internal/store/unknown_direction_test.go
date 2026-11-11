package store_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestUnknownDirectionStaysUnknownThroughRefreshAndReads(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store
	senderID := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	persist := func(key string, mutate func(*store.Message)) int64 {
		message := f.NewMessage().WithSourceMessageID(key).Build()
		message.SenderID = sql.NullInt64{Int64: senderID, Valid: true}
		mutate(message)
		id, err := st.UpsertMessage(message)
		require.NoError(err)
		return id
	}
	unknownID := persist("unknown", func(m *store.Message) { m.DirectionUnknown = true })
	sentID := persist("sent", func(m *store.Message) { m.IsFromMe = true })
	receivedID := persist("received", func(*store.Message) {})
	attribution := func(id int64) (effective, source sql.NullBool) {
		require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
			`SELECT is_from_me, source_is_from_me FROM messages WHERE id = ?`), id).Scan(&effective, &source))
		return effective, source
	}

	effective, source := attribution(unknownID)
	assert.False(effective.Valid)
	assert.False(source.Valid)
	effective, _ = attribution(sentID)
	assert.True(effective.Valid && effective.Bool)
	effective, _ = attribution(receivedID)
	assert.True(effective.Valid && !effective.Bool)

	unknown, err := st.GetMessage(unknownID)
	require.NoError(err)
	assert.Nil(unknown.IsFromMe)
	window, err := st.GetConversationWindow(f.ConvID, unknownID, 0, 0)
	require.NoError(err)
	require.Len(window.Messages, 1)
	assert.Nil(window.Messages[0].IsFromMe)
	sent, err := st.GetMessage(sentID)
	require.NoError(err)
	require.NotNil(sent.IsFromMe)
	assert.True(*sent.IsFromMe)
	received, err := st.GetMessage(receivedID)
	require.NoError(err)
	require.NotNil(received.IsFromMe)
	assert.False(*received.IsFromMe)

	require.NoError(st.AddAccountIdentity(f.Source.ID, "owner@example.com", "manual"))
	effective, source = attribution(unknownID)
	assert.True(effective.Valid && effective.Bool, "confirmed owner evidence still resolves ownership")
	assert.False(source.Valid)

	_, err = st.RemoveAccountIdentity(f.Source.ID, "owner@example.com")
	require.NoError(err)
	effective, _ = attribution(unknownID)
	assert.False(effective.Valid, "clearing owner evidence restores unknown")
}

func TestUnknownDirectionNoticeStaysInUnclassifiedRosterScope(t *testing.T) {
	requirements := require.New(t)
	f := newPersonSweepJournalFixture(t, true, false)
	triggers := `SELECT name, '' FROM sqlite_master WHERE type = 'trigger' AND name LIKE 'trg_person_sweep%'`
	if f.store.IsPostgreSQL() {
		triggers = `SELECT t.tgname, c.relname FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid
			WHERE t.tgname LIKE 'trg_person_sweep%' AND NOT t.tgisinternal`
	}
	rows, err := f.store.DB().QueryContext(t.Context(), triggers)
	requirements.NoError(err)
	defer func() { _ = rows.Close() }()
	var drops []string
	for rows.Next() {
		var name, table string
		requirements.NoError(rows.Scan(&name, &table))
		drops = append(drops, "DROP TRIGGER "+name+map[bool]string{true: " ON " + table, false: ""}[table != ""])
	}
	requirements.NoError(rows.Close())
	requirements.NoError(rows.Err())
	requirements.NotEmpty(drops)
	for _, drop := range drops {
		_, err = f.store.DB().ExecContext(t.Context(), drop)
		requirements.NoError(err)
	}
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(
		`DELETE FROM applied_migrations WHERE name = ?`), "person_sweep_unknown_direction_scope_v1")
	requirements.NoError(err)
	requirements.NoError(f.store.InitSchema())
	messageID, err := f.store.UpsertMessage(&store.Message{
		SourceID: f.sourceID, SourceMessageID: "senderless-notice",
		ConversationID: f.conversationID, MessageType: "whatsapp",
		SentAt:           sql.NullTime{Time: time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC), Valid: true},
		DirectionUnknown: true,
	})
	requirements.NoError(err)
	before := latestPersonSweepSequence(t, f.store)

	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		INSERT INTO conversation_participants (conversation_id, participant_id, role)
		VALUES (?, ?, 'member')`), f.conversationID, f.aliceID)
	requirements.NoError(err)

	changes := personSweepChangesAfter(t, f.store, f.alicePersonID, before)
	requirements.Len(changes, 1)
	assert.Equal(t, messageID, changes[0].MessageID)
}
