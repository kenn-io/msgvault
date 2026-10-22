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

func TestLoopMessageEvidenceInvalidation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	id := f.PersistEmail(storetest.EmailSpec{From: "alex@example.test", To: []string{storetest.OwnerAddress}, Body: "Can you send the report?", SentAt: time.Now().Add(-48 * time.Hour)})
	ctx := t.Context()
	expected := store.LoopMessageEvidence{Asks: true}
	save := func() {
		t.Helper()
		_, token, _, err := f.Store.LoopMessageEvidenceContext(ctx, id, 1)
		require.NoError(err)
		require.NoError(f.Store.SaveLoopMessageEvidenceContext(ctx, id, 1, token, expected))
		got, _, hit, err := f.Store.LoopMessageEvidenceContext(ctx, id, 1)
		require.NoError(err)
		require.True(hit)
		assert.Equal(expected, got)
	}
	save()
	_, _, hit, err := f.Store.LoopMessageEvidenceContext(ctx, id, 2)
	require.NoError(err)
	assert.False(hit, "detector rule changes invalidate older evidence")
	for _, mutation := range []struct {
		statement string
		args      []any
	}{
		{`UPDATE message_bodies SET body_text = 'Thanks!' WHERE message_id = ?`, []any{id}},
		{`DELETE FROM message_bodies WHERE message_id = ?`, []any{id}},
		{`INSERT INTO message_bodies(message_id,body_text) VALUES (?, 'Can you send it?')`, []any{id}},
		{`DELETE FROM message_raw WHERE message_id = ?`, []any{id}},
		{`INSERT INTO message_raw(message_id,raw_data,raw_format,compression) VALUES (?, ?, 'mime', 'none')`, []any{id, []byte("Content-Type: text/plain\r\n\r\nCan you send it?")}},
		{`UPDATE message_raw SET raw_data = ? WHERE message_id = ?`, []any{[]byte("Auto-Submitted: auto-replied\r\n\r\nCan you send it?"), id}},
	} {
		save()
		_, err := f.Store.DB().ExecContext(ctx, f.Store.Rebind(mutation.statement), mutation.args...)
		require.NoError(err)
		_, _, hit, err := f.Store.LoopMessageEvidenceContext(ctx, id, 1)
		require.NoError(err)
		assert.False(hit, mutation.statement)
	}
	save()
	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`DELETE FROM messages WHERE id = ?`), id)
	require.NoError(err)
	var count int
	require.NoError(f.Store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM loop_message_evidence`).Scan(&count))
	assert.Zero(count, "deleting a message cascades its cached facts")
}

func TestLoopEvidenceSchemaUpgrade(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	_, err := f.Store.DB().ExecContext(t.Context(), `DROP TABLE loop_message_evidence`)
	require.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), `UPDATE archive_metadata SET value='10' WHERE key='schema_version'`)
	require.NoError(err)
	require.NoError(f.Store.InitSchemaContext(t.Context()))
	require.NoError(f.Store.ValidateSchemaContext(t.Context()))
	var count int
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM loop_message_evidence`).Scan(&count))
	assert.Zero(count)
}

func TestLoopEvidenceCannotRestoreInvalidatedComputation(t *testing.T) {
	for _, table := range []string{"message_bodies", "message_raw"} {
		t.Run(table, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := storetest.New(t)
			id := f.PersistEmail(storetest.EmailSpec{From: "alex@example.test", To: []string{storetest.OwnerAddress}, Body: "Can you send it?", SentAt: time.Now().Add(-48 * time.Hour)})
			_, staleToken, _, err := f.Store.LoopMessageEvidenceContext(t.Context(), id, 1)
			require.NoError(err)
			mutation := `UPDATE message_bodies SET body_text = ? WHERE message_id = ?`
			var value any = "Thanks!"
			expected := store.LoopMessageEvidence{Acknowledgement: true}
			if table == "message_raw" {
				mutation = `UPDATE message_raw SET raw_data = ?, compression = 'none' WHERE message_id = ?`
				value = []byte("Auto-Submitted: auto-replied\r\nContent-Type: text/plain\r\n\r\nCan you send it?")
				expected = store.LoopMessageEvidence{Asks: true, Automated: true}
			}
			_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(mutation), value, id)
			require.NoError(err)
			_, currentToken, _, err := f.Store.LoopMessageEvidenceContext(t.Context(), id, 1)
			require.NoError(err)
			require.NoError(f.Store.SaveLoopMessageEvidenceContext(t.Context(), id, 1, currentToken, expected))
			require.NoError(f.Store.SaveLoopMessageEvidenceContext(t.Context(), id, 1, staleToken, store.LoopMessageEvidence{Asks: true}))
			got, _, hit, err := f.Store.LoopMessageEvidenceContext(t.Context(), id, 1)
			require.NoError(err)
			require.True(hit)
			assert.Equal(expected, got, "a completed read from before invalidation cannot overwrite fresh evidence")
		})
	}
}

func TestLoopEvidenceSurvivesUnchangedImports(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	conv, err := f.Store.EnsureConversation(f.Source.ID, "identical-import", "Question")
	require.NoError(err)
	data := &store.MessagePersistData{
		Message:  &store.Message{SourceID: f.Source.ID, ConversationID: conv, SourceMessageID: "stable-question", MessageType: store.MessageTypeEmail, SentAt: sql.NullTime{Time: time.Now().Add(-48 * time.Hour), Valid: true}},
		BodyText: sql.NullString{String: "Can you send it?", Valid: true},
		RawMIME:  []byte("Content-Type: text/plain\r\n\r\nCan you send it?"),
	}
	id, err := f.Store.PersistMessageContext(t.Context(), data)
	require.NoError(err)
	save := func() {
		t.Helper()
		_, token, _, err := f.Store.LoopMessageEvidenceContext(t.Context(), id, 1)
		require.NoError(err)
		require.NoError(f.Store.SaveLoopMessageEvidenceContext(t.Context(), id, 1, token, store.LoopMessageEvidence{Asks: true}))
	}
	save()
	sameID, err := f.Store.PersistMessageContext(t.Context(), data)
	require.NoError(err)
	require.Equal(id, sameID)
	_, _, hit, err := f.Store.LoopMessageEvidenceContext(t.Context(), id, 1)
	require.NoError(err)
	assert.True(hit, "an unchanged import must retain evidence progress")
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET last_modified = '2020-01-01 00:00:00+00:00' WHERE id = ?`), id)
	require.NoError(err)
	_, _, hit, err = f.Store.LoopMessageEvidenceContext(t.Context(), id, 1)
	require.NoError(err)
	assert.True(hit, "message metadata timestamps do not version body/MIME facts")
}
