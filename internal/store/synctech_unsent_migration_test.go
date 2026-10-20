package store_test

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/synctechsms"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// rerunSynctechUnsentMigration puts the archive back in the state before the
// migration ran: every row sent, no ledger entry.
func rerunSynctechUnsentMigration(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := t.Context()
	_, err := st.DB().ExecContext(ctx, `UPDATE messages SET is_sent = TRUE`)
	require.NoError(t, err)
	_, err = st.DB().ExecContext(ctx, st.Rebind(`DELETE FROM applied_migrations WHERE name = ?`), "synctech_unsent_texts_v1")
	require.NoError(t, err)
}

func isSentBySnippet(t *testing.T, st *store.Store) map[string]bool {
	t.Helper()
	rows, err := st.DB().QueryContext(t.Context(), `SELECT snippet, is_sent FROM messages`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	result := map[string]bool{}
	for rows.Next() {
		var snippet string
		var sent sql.NullBool
		require.NoError(t, rows.Scan(&snippet, &sent))
		require.True(t, sent.Valid)
		result[snippet] = sent.Bool
	}
	require.NoError(t, rows.Err())
	return result
}

func TestInitSchemaMarksUnsentSynctechTexts(t *testing.T) {
	f := storetest.New(t)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "messages.xml"), []byte(`<smses count="6">
  <sms address="+15551234567" date="1717214400000" type="1" body="received" read="1" status="-1" />
  <sms address="+15551234567" date="1717214401000" type="2" body="sent" read="1" status="-1" />
  <sms address="+15551234567" date="1717214402000" type="3" body="draft" read="1" status="-1" />
  <sms address="+15551234567" date="1717214403000" type="6" body="queued" read="1" status="-1" />
  <sms address="+15551234567" date="1717214404000" type="5" body="no record" read="1" status="-1" />
  <mms date="1717214405000" msg_box="3" read="1" m_id="mms-draft" sub="null">
    <parts><part seq="0" ct="text/plain" text="mms draft" /></parts>
    <addrs><addr address="+15550000001" type="137" charset="106" /><addr address="+15551234567" type="151" charset="106" /></addrs>
  </mms>
</smses>`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "calls.xml"), []byte(`<calls count="1">
  <call number="+15551234567" duration="0" date="1717214406000" type="2" presentation="1" />
</calls>`), 0o600))
	_, err := synctechsms.NewImporter(f.Store, synctechsms.ImportOptions{
		OwnerPhone: "+15550000001", IncludeSMS: true, IncludeMMS: true, IncludeCalls: true,
	}).ImportPath(dir)
	require.NoError(t, err)
	rerunSynctechUnsentMigration(t, f.Store)
	_, err = f.Store.DB().ExecContext(t.Context(), `DELETE FROM message_raw WHERE message_id IN (SELECT id FROM messages WHERE snippet = 'no record')`)
	require.NoError(t, err)

	require.NoError(t, f.Store.InitSchemaContext(t.Context()))

	assert.Equal(t, map[string]bool{
		"received":                 true,
		"sent":                     true,
		"draft":                    false,
		"queued":                   false,
		"no record":                true,
		"mms draft":                false,
		"Call outgoing, 0 seconds": true,
	}, isSentBySnippet(t, f.Store))
	applied, err := f.Store.IsMigrationApplied("synctech_unsent_texts_v1")
	require.NoError(t, err)
	assert.True(t, applied)
}

// The walk reads in batches by id, so unsent rows on both sides of a batch
// boundary are all found.
func TestInitSchemaMarksUnsentSynctechTextsAcrossBatches(t *testing.T) {
	f := storetest.New(t)
	source, err := f.Store.GetOrCreateSource("synctech_sms", "+15550000001")
	require.NoError(t, err)
	conversation, err := f.Store.EnsureConversation(source.ID, "text:batches", "Batches")
	require.NoError(t, err)
	const total = 503
	for i := range total {
		id, err := f.Store.UpsertMessage(&store.Message{
			ConversationID: conversation, SourceID: source.ID, MessageType: "sms",
			SourceMessageID: fmt.Sprintf("batch-%d", i), Snippet: sql.NullString{String: fmt.Sprintf("text %d", i), Valid: true},
		})
		require.NoError(t, err)
		status := 2
		if i%2 == 0 {
			status = 3
		}
		require.NoError(t, f.Store.UpsertMessageRawWithFormat(id, []byte(fmt.Sprintf(`{"Type":%d}`, status)), synctechsms.RawFormat))
	}
	rerunSynctechUnsentMigration(t, f.Store)

	require.NoError(t, f.Store.InitSchemaContext(t.Context()))

	var unsent int
	require.NoError(t, f.Store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM messages WHERE is_sent = FALSE`).Scan(&unsent))
	assert.Equal(t, (total+1)/2, unsent)
}
