package importer

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

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
			require.NotNil(first)
			require.True(first.HardErrors)
			for _, relation := range []string{"participants", "participant_identifiers", "conversations", "messages", "message_bodies", "message_raw", "message_recipients", "conversation_participants", "attachments", "import_message_decisions", "mcp_event_log"} {
				var n int
				require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM " + relation).Scan(&n))
				if table == "mcp_event_log" {
					if relation == "mcp_event_log" {
						assert.Zero(n, "the failed publication transaction does not leak event rows")
					}
				} else {
					assert.Zero(n, "failed native snapshot must not leak "+relation)
				}
			}
			if table == "mcp_event_log" {
				require.Error(err, "the staged notification failure is returned after message finalization")
				assert.Contains(err.Error(), "publish PST import events")
				for query, want := range map[string]int{
					`SELECT COUNT(*) FROM messages`:                                      1,
					`SELECT COUNT(*) FROM message_bodies`:                                1,
					`SELECT COUNT(*) FROM message_raw`:                                   1,
					`SELECT COUNT(*) FROM import_message_decisions WHERE notified=FALSE`: 1,
				} {
					var got int
					require.NoError(st.DB().QueryRow(query).Scan(&got))
					assert.Equal(want, got, query)
				}
			} else {
				require.NoError(err)
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
			if table == "mcp_event_log" {
				assert.Zero(retry.MessagesAdded)
				assert.Equal(int64(1), retry.MessagesSkipped)
			} else {
				assert.Equal(int64(1), retry.MessagesAdded)
			}
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
		{Family: "msgvault.message_archived", SourceType: "gmail", Kinds: []string{"message"}},
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
	assert.Zero(live, "the PST import does not inherit the caller's live ingestion mode")
	assert.Equal(1, imports)
	sourceResource, err := st.GetMCPResource(t.Context(), "source", result.SourceID)
	require.NoError(err)
	require.NoError(st.ObserveNativeSourcePermission(t.Context(), store.NativeSourcePermission{
		SourceID: result.SourceID, SourceReference: sourceResource.Reference, SourceType: "gmail",
		NativeIdentity: "owner@example.net", Known: true, MessageRead: true,
	}))
	liveStore := st.WithIngestContext(store.IngestContext{Mode: store.IngestLive})
	require.NoError(IngestRawMessage(t.Context(), liveStore, result.SourceID, "owner@example.net", "", nil,
		"gmail-live-synthetic", "gmail-live-synthetic-hash",
		[]byte("From: sender@example.net\r\nTo: owner@example.net\r\nSubject: Synthetic live mail\r\nMessage-ID: <live@example.net>\r\n\r\nSynthetic live body"),
		time.Now().UTC(), slog.Default()))
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.message_archived'`).Scan(&live))
	assert.Equal(1, live, "the configured live capability accepts a normal live write")
}

func TestNativePSTReplyNotificationsWaitForThreadReconciliation(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)

	st := testutil.NewTestStore(t).WithIngestContext(store.IngestContext{Mode: store.IngestBackfill})
	configureNativePSTImports(t, st, true, true)
	source, err := st.GetOrCreateSource("pst", "owner@example.net")
	require.NoError(err)
	syncID, err := st.StartSyncContext(t.Context(), source.ID, "import-pst")
	require.NoError(err)
	scoped := st.ScopedToSync(source.ID, syncID)
	run, err := scoped.BeginNativeImportAttempt(t.Context(), source.ID, "pst", true, syncID, 0)
	require.NoError(err)
	importStore, err := scoped.WithImportRun(t.Context(), run.ID)
	require.NoError(err)
	ingest := pstMessageIngester(nil, true)
	root := []byte("From: sender@example.net\r\nTo: owner@example.net\r\nSubject: Synthetic PST root\r\nMessage-ID: <root@example.test>\r\nDate: Mon, 01 Jan 2024 00:00:00 +0000\r\n\r\nRoot body")
	reply := []byte("From: owner@example.net\r\nTo: sender@example.net\r\nSubject: Synthetic PST reply\r\nMessage-ID: <reply@example.test>\r\nIn-Reply-To: <root@example.test>\r\nDate: Tue, 02 Jan 2024 00:00:00 +0000\r\n\r\nReply body")
	for _, message := range []struct {
		sourceID, hash string
		raw            []byte
	}{
		{"pst-root", "pst-root-hash", root},
		{"pst-reply", "pst-reply-hash", reply},
	} {
		require.NoError(ingest(t.Context(), importStore, source.ID, "owner@example.net", "", nil,
			message.sourceID, message.hash, message.raw, time.Time{}, slog.Default()))
	}
	var eventCount int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'`).Scan(&eventCount))
	assert.Zero(eventCount, "PST events stay unpublished until thread identities are final")

	require.NoError(finishPstThreads(t.Context(), importStore, source.ID))
	var rootID, rootConversation, replyID, replyConversation, replyParent int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT id,conversation_id FROM messages WHERE source_id=? AND source_message_id='pst-root'`), source.ID).Scan(&rootID, &rootConversation))
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT id,conversation_id,COALESCE(reply_to_message_id,0) FROM messages WHERE source_id=? AND source_message_id='pst-reply'`), source.ID).Scan(&replyID, &replyConversation, &replyParent))
	assert.NotZero(replyParent, "the PST reply header resolves to the archived root")
	assert.Equal(rootConversation, replyConversation, "PST reconciliation places the reply with its root")
	require.NoError(importStore.PublishPSTImportEventsContext(t.Context()))
	rows, err := st.DB().Query(st.Rebind(`SELECT message_id,conversation_id FROM mcp_event_log WHERE family='msgvault.import_message_archived' AND source_id=? ORDER BY message_id`), source.ID)
	require.NoError(err)
	defer func() { require.NoError(rows.Close()) }()
	eventConversations := make(map[int64]int64)
	for rows.Next() {
		var messageID, conversationID int64
		require.NoError(rows.Scan(&messageID, &conversationID))
		eventConversations[messageID] = conversationID
	}
	require.NoError(rows.Err())
	assert.Equal(map[int64]int64{rootID: rootConversation, replyID: replyConversation}, eventConversations)
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
