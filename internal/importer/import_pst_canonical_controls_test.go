package importer

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestNativePSTMandatoryFailureRetryAndFreshReplay(t *testing.T) {
	for _, table := range []string{"message_raw", "message_recipients", "attachments", "mcp_event_log"} {
		t.Run(table, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			st := testutil.NewTestStore(t)
			configureNativePSTImports(t, st, true, true)
			path := filepath.Join(t.TempDir(), "synthetic-media.pst")
			require.NoError(os.WriteFile(path, syntheticPSTMediaFixture, 0o600))
			if st.IsPostgreSQL() {
				_, err := st.DB().Exec(`CREATE FUNCTION synthetic_pst_abort_required() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic PST required write failure'; END $$`)
				require.NoError(err)
				_, err = st.DB().Exec(fmt.Sprintf(`CREATE TRIGGER synthetic_pst_abort_required BEFORE INSERT ON %s FOR EACH ROW EXECUTE FUNCTION synthetic_pst_abort_required()`, table))
				require.NoError(err)
			} else {
				_, err := st.DB().Exec(fmt.Sprintf(`CREATE TRIGGER synthetic_pst_abort_required BEFORE INSERT ON %s BEGIN SELECT RAISE(ABORT,'synthetic PST required write failure'); END`, table))
				require.NoError(err)
			}
			opts := PstImportOptions{Identifier: "owner@example.net", NotifyImports: true, AttachmentsDir: t.TempDir()}
			first, err := ImportPst(t.Context(), st, path, opts)
			require.NoError(err)
			require.NotNil(first)
			require.True(first.HardErrors)
			for _, relation := range []string{"participants", "participant_identifiers", "conversations", "messages", "message_bodies", "message_raw", "message_recipients", "conversation_participants", "attachments", "import_message_decisions", "mcp_event_log"} {
				var n int
				require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM " + relation).Scan(&n))
				assert.Zero(n, "failed native snapshot must not leak "+relation)
			}
			drop := `DROP TRIGGER synthetic_pst_abort_required`
			if st.IsPostgreSQL() {
				drop += " ON " + table
			}
			_, err = st.DB().Exec(drop)
			require.NoError(err)
			retry, err := ImportPst(t.Context(), st, path, opts)
			require.NoError(err)
			require.NotNil(retry)
			require.False(retry.HardErrors)
			assert.Equal(first.ImportRunID, retry.ImportRunID)
			assert.Equal(int64(1), retry.MessagesAdded)
			fresh, err := ImportPst(t.Context(), st, path, opts)
			require.NoError(err)
			require.NotNil(fresh)
			require.False(fresh.HardErrors)
			assert.NotEqual(retry.ImportRunID, fresh.ImportRunID)
			assert.Equal(int64(1), fresh.MessagesSkipped)
			for query, want := range map[string]int{
				`SELECT COUNT(*) FROM messages`: 1,
				`SELECT COUNT(*) FROM attachments WHERE storage_path<>'' AND content_hash<>''`:       1,
				`SELECT COUNT(*) FROM import_message_decisions`:                                      1,
				`SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'`: 1,
				`SELECT COUNT(*) FROM import_runs WHERE completed=TRUE`:                              2,
			} {
				var got int
				require.NoError(st.DB().QueryRow(query).Scan(&got))
				assert.Equal(want, got, query)
			}
		})
	}
}

func TestNativePSTFormatAuthorityIsIndependentOfCustomSourceType(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := testutil.NewTestStore(t)
	cfg := store.MCPEventsConfig{Enabled: true, Principal: "owner:synthetic-pst", ImportNotifications: true, Capabilities: []store.MCPEventCapability{
		{Family: "msgvault.message_archived", SourceType: "gmail", Kinds: []string{"email"}},
		{Family: "msgvault.import_message_archived", SourceType: "pst", Kinds: []string{"message"}},
	}}
	_, err := st.ConfigureMCPEvents(t.Context(), cfg)
	require.NoError(err)
	result, err := ImportPst(t.Context(), st.WithIngestContext(store.IngestContext{Mode: store.IngestLive}), nativeSyntheticPST(t), PstImportOptions{SourceType: "gmail", Identifier: "owner@example.net", NoResume: true, NotifyImports: true})
	require.NoError(err)
	require.NotNil(result)
	require.False(result.HardErrors)
	var sourceType, format string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT source_type FROM sources WHERE id=?`), result.SourceID).Scan(&sourceType))
	assert.Equal("gmail", sourceType)
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT import_format FROM import_runs WHERE id=?`), result.ImportRunID).Scan(&format))
	assert.Equal("pst", format)
	var live, imports int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.message_archived'`).Scan(&live))
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'`).Scan(&imports))
	assert.Zero(live)
	assert.Equal(1, imports)
}

func TestNativePSTDerivedFTSFailureKeepsReadyImport(t *testing.T) {
	require := Require.New(t)

	st := testutil.NewTestStore(t)
	if st.IsPostgreSQL() {
		t.Skip("SQLite virtual-table failure; PostgreSQL projection covered by native import matrix")
	}
	configureNativePSTImports(t, st, true, true)
	_, err := st.DB().Exec(`DROP TABLE messages_fts`)
	require.NoError(err)
	result, err := ImportPst(t.Context(), st, nativeSyntheticPST(t), PstImportOptions{Identifier: "owner@example.net", NoResume: true, NotifyImports: true})
	require.NoError(err)
	require.NotNil(result)
	require.False(result.HardErrors)
	for _, table := range []string{"messages", "message_bodies", "message_raw", "import_message_decisions", "mcp_event_log"} {
		var n int
		require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n))
		Assert.Equal(t, 1, n, table)
	}
}
