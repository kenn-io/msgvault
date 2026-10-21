package query_test

import (
	"database/sql"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestMessageSnapshotKeepsAllDetailReadsTogether(t *testing.T) {
	for _, raw := range []bool{false, true} {
		name := "stored body"
		if raw {
			name = "raw MIME fallback"
		}
		t.Run(name, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			f := storetest.New(t)
			id := f.CreateMessage("synthetic-snapshot-message")
			participant := f.EnsureParticipant("recipient@example.net", "Synthetic Recipient", "example.net")
			label := f.EnsureLabels(map[string]string{"synthetic-label": "Original label"}, "user")["synthetic-label"]
			exec := func(statement string, args ...any) {
				t.Helper()
				_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(statement), args...)
				Require.NoError(t, err)
			}
			exec(`UPDATE messages SET subject='Original subject' WHERE id=?`, id)
			exec(`INSERT INTO message_recipients (message_id,participant_id,recipient_type,display_name) VALUES (?,?,'to','Original recipient')`, id, participant)
			exec(`INSERT INTO message_labels (message_id,label_id) VALUES (?,?)`, id, label)
			exec(`INSERT INTO attachments (message_id,filename,mime_type,storage_path) VALUES (?,'original.txt','text/plain','synthetic/original')`, id)
			if raw {
				exec(`INSERT INTO message_raw (message_id,raw_data,raw_format,compression) VALUES (?,?,'mime','none')`, id, []byte("Content-Type: text/plain; charset=utf-8\r\n\r\nOriginal body"))
			} else {
				exec(`INSERT INTO message_bodies (message_id,body_text,body_html) VALUES (?,'Original body','<p>Original body</p>')`, id)
			}
			tx, err := f.Store.DB().BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
			require.NoError(err)
			t.Cleanup(func() { _ = tx.Rollback() })
			// This is the snapshot established by the Events authorization guard.
			var present int64
			require.NoError(tx.QueryRowContext(t.Context(), f.Store.Rebind(`SELECT id FROM messages WHERE id=?`), id).Scan(&present))
			writer, err := f.Store.DB().BeginTx(t.Context(), nil)
			require.NoError(err)
			t.Cleanup(func() { _ = writer.Rollback() })
			for _, change := range []struct {
				statement string
				args      []any
			}{
				{`UPDATE messages SET subject='Replacement subject' WHERE id=?`, []any{id}},
				{`UPDATE message_recipients SET display_name='Replacement recipient' WHERE message_id=?`, []any{id}},
				{`UPDATE labels SET name='Replacement label' WHERE id=?`, []any{label}},
				{`UPDATE attachments SET filename='replacement.txt' WHERE message_id=?`, []any{id}},
			} {
				_, err := writer.ExecContext(t.Context(), f.Store.Rebind(change.statement), change.args...)
				require.NoError(err)
			}
			if raw {
				_, err = writer.ExecContext(t.Context(), f.Store.Rebind(`UPDATE message_raw SET raw_data=? WHERE message_id=?`), []byte("Content-Type: text/plain; charset=utf-8\r\n\r\nReplacement body"), id)
			} else {
				_, err = writer.ExecContext(t.Context(), f.Store.Rebind(`UPDATE message_bodies SET body_text='Replacement body',body_html='<p>Replacement body</p>' WHERE message_id=?`), id)
			}
			require.NoError(err)
			require.NoError(writer.Commit())
			detail, err := query.GetMessageInSnapshot(t.Context(), tx, f.Store.Rebind, id)
			require.NoError(err)
			require.NotNil(detail)
			assert.Equal("Original subject", detail.Subject)
			assert.Equal("Original body", detail.BodyText)
			assert.Equal([]query.Address{{Email: "recipient@example.net", Name: "Original recipient"}}, detail.To)
			assert.Equal([]string{"Original label"}, detail.Labels)
			require.Len(detail.Attachments, 1)
			assert.Equal("original.txt", detail.Attachments[0].Filename)
			require.NoError(tx.Commit())
			latest, err := f.Store.DB().BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
			require.NoError(err)
			t.Cleanup(func() { _ = latest.Rollback() })
			current, err := query.GetMessageInSnapshot(t.Context(), latest, f.Store.Rebind, id)
			require.NoError(err)
			require.NotNil(current)
			assert.Equal("Replacement subject", current.Subject)
			assert.Equal("Replacement body", current.BodyText)
			assert.Equal([]string{"Replacement label"}, current.Labels)
			require.NoError(latest.Commit())
		})
	}
}
