package importer

import (
	"os"
	"path/filepath"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportEMLDirRecordsFailureWhenLogicalViewIsRevoked(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st, tmp := testutil.NewTestStore(t), t.TempDir()
	// A native SQL transition between durable attempt linking and validated
	// view construction must return a failure, with no unscoped fallback.
	if st.IsPostgreSQL() {
		_, err := st.DB().ExecContext(t.Context(), `CREATE FUNCTION synthetic_expire_import_view() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN UPDATE import_runs SET completed=TRUE WHERE id=NEW.import_run_id; RETURN NEW; END $$`)
		require.NoError(err)
		_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER synthetic_expire_import_view AFTER INSERT ON import_run_attempts FOR EACH ROW EXECUTE FUNCTION synthetic_expire_import_view()`)
		require.NoError(err)
	} else {
		_, err := st.DB().ExecContext(t.Context(), `CREATE TRIGGER synthetic_expire_import_view AFTER INSERT ON import_run_attempts BEGIN UPDATE import_runs SET completed=TRUE WHERE id=NEW.import_run_id; END`)
		require.NoError(err)
	}
	root := filepath.Join(tmp, "Synthetic.mailbox")
	require.NoError(os.MkdirAll(root, 0o700))
	raw := []byte("From: sender@example.net\r\nTo: owner@example.net\r\nMessage-ID: <view-revoked@example.net>\r\nSubject: Synthetic revoked view\r\n\r\nSynthetic body\r\n")
	require.NoError(os.WriteFile(filepath.Join(root, "message.eml"), raw, 0o600))
	var summary *EMLImportSummary
	var err error
	require.NotPanics(func() {
		summary, err = ImportEMLDir(t.Context(), st, root, EMLImportOptions{Identifier: "owner@example.net", NoResume: true})
	})
	require.Error(err)
	assert.Nil(summary)
	var count int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM messages`).Scan(&count))
	assert.Zero(count)
	var status string
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT status FROM sync_runs ORDER BY id DESC LIMIT 1`).Scan(&status))
	assert.Equal("failed", status, "native execution must record terminal failure")
}
