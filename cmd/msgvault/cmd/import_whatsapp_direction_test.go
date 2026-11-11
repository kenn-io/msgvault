package cmd

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/identityindex"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

func TestWhatsAppTextUnknownDirectionPeople(t *testing.T) {
	oldPhone, oldMediaDir, oldContacts := importPhone, importMediaDir, importContacts
	oldLimit, oldDisplayName, oldNoDefaultIdentity := importLimit, importDisplayName, noDefaultIdentityImportWhatsApp
	t.Cleanup(func() {
		importPhone, importMediaDir, importContacts = oldPhone, oldMediaDir, oldContacts
		importLimit, importDisplayName, noDefaultIdentityImportWhatsApp = oldLimit, oldDisplayName, oldNoDefaultIdentity
	})
	markDaemonCLISubprocessForTest(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	path := filepath.Join(t.TempDir(), "chat.txt")
	require.NoError(t, os.WriteFile(path, []byte("13/6/26, 10:00 - You: hello\n13/6/26, 10:01 - Mira: hi\n"), 0600))
	command := &cobra.Command{Use: "test"}
	command.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	command.AddCommand(newImportWhatsAppCmd())
	command.SetArgs([]string{"import-whatsapp", path, "--format", "text", "--phone", "+12025550123", "--chat-id", "garden", "--chat-type", "direct", "--timezone", "UTC", "--date-order", "dmy"})
	require.NoError(t, command.Execute())
	st, err := store.Open(cfg.DatabaseDSN())
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, st.Close()) })
	var messageID, senderID, sourceID int64
	var ownership, sourceOwnership sql.NullBool
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT id,sender_id,source_id,is_from_me,source_is_from_me FROM messages WHERE snippet='hello'").Scan(&messageID, &senderID, &sourceID, &ownership, &sourceOwnership))
	sqlite := query.NewSQLiteEngine(st.DB())
	detail, err := sqlite.GetMessage(t.Context(), messageID)
	require.NoError(t, err)
	assert.Nil(t, detail.IsFromMe)
	engine, err := query.NewDuckDBEngine(t.Context(), cfg.AnalyticsDir(), "", st.DB())
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, engine.Close()) })
	detail, err = engine.GetMessage(t.Context(), messageID)
	require.NoError(t, err)
	assert.Nil(t, detail.IsFromMe)
	inboxes, err := engine.ListPersonInboxes(t.Context(), query.PersonInboxRequest{CanonicalID: senderID})
	require.NoError(t, err)
	require.Len(t, inboxes.Rows, 1)
	for _, filter := range []query.Context{{}, {SourceIDs: []int64{sourceID}}} {
		result, err := engine.Relationships(t.Context(), query.RelationshipsRequest{ShowAll: true, Context: filter})
		require.NoError(t, err)
		require.Len(t, result.Rows, 2)
		for _, row := range result.Rows {
			assert.Zero(t, row.Signals.SentToThem)
			assert.Zero(t, row.Signals.ReceivedFromThem)
			assert.Zero(t, row.Score)
		}
	}
	assert.False(t, ownership.Valid)
	assert.False(t, sourceOwnership.Valid)
	assert.EqualValues(t, 1, inboxes.Rows[0].ConversationCount)
	assert.Zero(t, inboxes.Rows[0].SentCount)
	assert.Zero(t, inboxes.Rows[0].ReceivedCount)
}

func TestUnknownDirectionIncrementalCacheAndNoticeMedia(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "msgvault.db")
	analyticsDir := filepath.Join(t.TempDir(), "analytics")
	st, err := store.Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, st.Close()) })
	require.NoError(t, st.InitSchema())
	source, err := st.GetOrCreateSource("whatsapp", "synthetic")
	require.NoError(t, err)
	conversation, err := st.EnsureConversationWithType(source.ID, "chat", "direct_chat", "Chat")
	require.NoError(t, err)
	person, err := st.EnsureParticipant("person@example.test", "Person", "example.test")
	require.NoError(t, err)
	_, err = st.DB().ExecContext(t.Context(), `INSERT INTO conversation_participants (conversation_id, participant_id, role) VALUES (?, ?, 'member')`, conversation, person)
	require.NoError(t, err)
	_, err = st.UpsertMessage(&store.Message{SourceID: source.ID, SourceMessageID: "known", ConversationID: conversation, MessageType: "whatsapp", SenderID: sql.NullInt64{Int64: person, Valid: true}, IsFromMe: true, SentAt: sql.NullTime{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Valid: true}})
	require.NoError(t, err)
	_, err = buildCache(t.Context(), dbPath, analyticsDir, true)
	require.NoError(t, err)
	noticeID, err := st.UpsertMessage(&store.Message{SourceID: source.ID, SourceMessageID: "notice", ConversationID: conversation, MessageType: "whatsapp", DirectionUnknown: true, SentAt: sql.NullTime{Time: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), Valid: true}})
	require.NoError(t, err)
	require.NoError(t, st.UpsertAttachment(t.Context(), noticeID, "notice.txt", "text/plain", "notice.txt", "", 5))
	stale := cacheNeedsBuild(dbPath, analyticsDir)
	require.True(t, stale.NeedsBuild)
	require.False(t, stale.FullRebuild)
	_, err = buildCacheAuto(t.Context(), dbPath, analyticsDir)
	require.NoError(t, err)
	db, err := sql.Open("duckdb", "")
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })
	var direction sql.NullBool
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT is_from_me FROM read_parquet(?, hive_partitioning=true) WHERE id = ?`, filepath.Join(analyticsDir, "messages", "**", "*.parquet"), noticeID).Scan(&direction))
	assert.False(t, direction.Valid)
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT is_from_me FROM read_parquet(?) WHERE anchor_message_id = ? LIMIT 1`, filepath.Join(analyticsDir, identityindex.DatasetLogicalContributions, "*.parquet"), noticeID).Scan(&direction))
	assert.False(t, direction.Valid)
	engine, err := query.NewDuckDBEngine(t.Context(), analyticsDir, "", st.DB())
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, engine.Close()) })
	files, err := engine.SearchFiles(t.Context(), query.FileSearchRequest{Person: &query.PersonFileScope{ParticipantIDs: []int64{person}, Directions: []query.PersonFileDirection{query.PersonFileFromPerson, query.PersonFileToPerson, query.PersonFileGroup}, IncludeUnclassifiedRosterRows: true}})
	require.NoError(t, err)
	require.Len(t, files.Files, 1)
	assert.Equal(t, noticeID, files.Files[0].MessageID)
	assert.Equal(t, "notice.txt", files.Files[0].Filename)
}
