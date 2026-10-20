package store_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/synctechsms"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// runSynctechUnsentMigration puts the archive back before the migration, with
// every row sent, and upgrades it again.
func runSynctechUnsentMigration(t *testing.T, st *store.Store) {
	t.Helper()
	_, err := st.DB().ExecContext(t.Context(), `UPDATE messages SET is_sent = TRUE`)
	require.NoError(t, err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`DELETE FROM applied_migrations WHERE name = ?`), "synctech_unsent_texts_v1")
	require.NoError(t, err)
	require.NoError(t, st.InitSchemaContext(t.Context()))
}

func TestInitSchemaMarksUnsentSynctechTexts(t *testing.T) {
	f := storetest.New(t)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "messages.xml"), []byte(`<smses>
  <sms address="+15551234567" date="1717214401000" type="2" body="sent" read="1" status="-1" />
  <sms address="+15551234567" date="1717214402000" type="3" body="draft" read="1" status="-1" />
  <sms address="+15551234567" date="1717214403000" type="5" body="no record" read="1" status="-1" />
  <mms date="1717214405000" msg_box="3" read="1" m_id="mms-draft" sub="null"><parts><part seq="0" ct="text/plain" text="mms draft" /></parts><addrs><addr address="+15550000001" type="137" charset="106" /><addr address="+15551234567" type="151" charset="106" /></addrs></mms>
  <mms date="1717214407000" msg_box="5" read="1" m_id="mms-failed" sub="null"><parts><part seq="0" ct="text/plain" text="mms failed" /></parts><addrs><addr address="+15550000001" type="137" charset="106" /><addr address="+15551234567" type="151" charset="106" /></addrs></mms>
</smses>`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "calls.xml"), []byte(`<calls>
  <call number="+15551234567" duration="0" date="1717214406000" type="2" presentation="1" />
</calls>`), 0o600))
	_, err := synctechsms.NewImporter(f.Store, synctechsms.ImportOptions{
		OwnerPhone: "+15550000001", IncludeSMS: true, IncludeMMS: true, IncludeCalls: true,
	}).ImportPath(dir)
	require.NoError(t, err)
	_, err = f.Store.DB().ExecContext(t.Context(), `DELETE FROM message_raw WHERE message_id IN (SELECT id FROM messages WHERE snippet = 'no record')`)
	require.NoError(t, err)

	runSynctechUnsentMigration(t, f.Store)

	rows, err := f.Store.DB().QueryContext(t.Context(), `SELECT snippet, is_sent FROM messages`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	got := map[string]bool{}
	for rows.Next() {
		var snippet string
		var sent bool
		require.NoError(t, rows.Scan(&snippet, &sent))
		got[snippet] = sent
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, map[string]bool{
		"sent": true, "draft": false, "mms draft": false, "mms failed": false,
		"no record": true, "Call outgoing, 0 seconds": true,
	}, got)
}
