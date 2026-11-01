//go:build cgo

package archive_test

import (
	"database/sql"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/pkg/archive"
)

func TestSQLiteArchiveReadsExistingMail(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "archive.db")
	require.NoError(archive.SetupSQLite(t.Context(), path))
	st, err := store.Open(path)
	require.NoError(err)
	source, err := st.GetOrCreateSource("gmail", "reader@example.com")
	require.NoError(err)
	conversation, err := st.EnsureConversation(source.ID, "mail-thread", "Observatory")
	require.NoError(err)
	id, err := st.UpsertMessage(&store.Message{
		SourceID: source.ID, ConversationID: conversation, SourceMessageID: "mail-1", MessageType: "email",
		Subject: sql.NullString{String: "Observatory", Valid: true},
	})
	require.NoError(err)
	require.NoError(st.UpsertMessageBody(id, sql.NullString{String: "The telescope is ready.", Valid: true}, sql.NullString{}))
	require.NoError(st.UpsertFTS(id, "Observatory", "The telescope is ready.", "", "", ""))
	require.NoError(st.Close())
	runtime, err := archive.OpenSQLite(t.Context(), path)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(runtime.Close()) })
	searchIDs(t, runtime, "telescope", 1)
	require.NoError(runtime.PurgeSource(t.Context(), source.ID))
	require.NoError(runtime.PurgeSource(t.Context(), source.ID), "retry after successful purge")
	searchIDs(t, runtime, "telescope", 0)
}

func TestOpenSQLiteRequiresExistingArchive(t *testing.T) {
	require := require.New(t)
	dir := filepath.Join(t.TempDir(), "missing")
	_, err := archive.OpenSQLite(t.Context(), filepath.Join(dir, "archive.db"))
	require.ErrorIs(err, fs.ErrNotExist)
	_, err = os.Stat(dir)
	require.ErrorIs(err, fs.ErrNotExist, "opening must not create the archive directory")
	_, err = archive.OpenSQLite(t.Context(), "postgres://archive.invalid/msgvault")
	require.ErrorContains(err, "use Open for PostgreSQL")
	require.ErrorContains(archive.SetupSQLite(t.Context(), "postgres://archive.invalid/msgvault"), "use Setup for PostgreSQL")
}

func TestSQLiteArchiveRequiresSourceLifecycleSetup(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	path := filepath.Join(t.TempDir(), "archive.db")
	require.NoError(archive.SetupSQLite(t.Context(), path))
	st, err := store.Open(path)
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(st.Close()) })
	source, err := st.GetOrCreateSource("mbox", "history@example.com")
	require.NoError(err)
	// Version 8 predates source lifecycle tables. Keep that historical marker
	// literal so omitting the version bump makes runtime opening fail this test.
	_, err = st.DB().Exec("UPDATE archive_metadata SET value = '8' WHERE key = 'schema_version'")
	require.NoError(err)
	_, err = st.DB().Exec("DROP TABLE source_settings")
	require.NoError(err)

	runtime, err := archive.OpenSQLite(t.Context(), path)
	if runtime != nil {
		t.Cleanup(func() { assert.NoError(runtime.Close()) })
	}
	require.ErrorContains(err, "run setup")
	var version string
	require.NoError(st.DB().QueryRow("SELECT value FROM archive_metadata WHERE key = 'schema_version'").Scan(&version))
	assert.Equal("8", version, "runtime opening must not certify an upgrade")
	var tables int
	require.NoError(st.DB().QueryRow("SELECT count(*) FROM sqlite_master WHERE name = 'source_settings'").Scan(&tables))
	assert.Zero(tables, "runtime opening must not create the missing table")

	require.NoError(archive.SetupSQLite(t.Context(), path))
	runtime, err = archive.OpenSQLite(t.Context(), path)
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(runtime.Close()) })
	sources, err := runtime.Store().ListSources("")
	require.NoError(err)
	require.Len(sources, 1)
	assert.Equal(source.ID, sources[0].ID)
	assert.Equal("history@example.com", sources[0].Identifier)
}

func TestSlackCallerSelectsPrivateConversation(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "archive.db")
	require.NoError(archive.SetupSQLite(t.Context(), path))
	runtime, err := archive.OpenSQLite(t.Context(), path)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(runtime.Close()) })
	peer := &slackPeer{rootTS: fmt.Sprintf("%d.000001", time.Now().Add(-time.Hour).Unix())}
	server := httptest.NewServer(http.HandlerFunc(peer.serve))
	defer server.Close()
	credential := archive.SlackCredential{Token: "broad", BaseURL: server.URL}
	channels, err := archive.SlackChannels(t.Context(), credential, "TPUBLIC")
	require.NoError(err)
	require.Contains(channels, archive.Channel{ID: "CPRIVATE", Name: "private", IsPrivate: true})
	opts := archive.SlackSync{Credential: credential, Options: archive.SlackOptions{
		ChannelIDs: []string{"CPRIVATE"}, NoThreads: true, NoMedia: true, ExcludePrivateChannels: true,
	}}
	summary, err := runtime.SyncSlack(t.Context(), opts)
	require.NoError(err)
	require.Zero(summary.MessagesAdded)
	opts.Options.ExcludePrivateChannels = false
	summary, err = runtime.SyncSlack(t.Context(), opts)
	require.NoError(err)
	require.Equal(1, summary.MessagesAdded)
	searchIDs(t, runtime, "telescope", 1)
}

func TestSQLiteRuntimeCollectsAcrossCredentialReplacement(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "archive.db")
	require.NoError(archive.SetupSQLite(t.Context(), path))
	st, err := store.Open(path)
	require.NoError(err)
	source, err := st.GetOrCreateSource("slack", "TEXAMPLE:UEXAMPLE")
	require.NoError(err)
	conversation, err := st.EnsureConversation(source.ID, "CEXAMPLE", "Announcements")
	require.NoError(err)
	id, err := st.UpsertMessage(&store.Message{
		SourceID: source.ID, ConversationID: conversation, SourceMessageID: "1704110400.000001", MessageType: "slack",
		Subject: sql.NullString{String: "Launch", Valid: true},
	})
	require.NoError(err)
	require.NoError(st.UpsertMessageBody(id, sql.NullString{String: "The observatory opens tomorrow.", Valid: true}, sql.NullString{}))
	require.NoError(st.UpsertFTS(id, "Launch", "The observatory opens tomorrow.", "", "", ""))
	require.NoError(st.Close())
	runtime, err := archive.OpenSQLite(t.Context(), path)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(runtime.Close()) })
	exerciseSlackReplacement(t, runtime)
	exerciseDiscordCollection(t, runtime)
}

// An archive set up before chat discovery has no chat member index. Runtime
// opens must refuse it until setup runs the migration that builds the index.
func TestOpenSQLiteRequiresSetupForChatMembers(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "archive.db")
	require.NoError(archive.SetupSQLite(t.Context(), path))
	st, err := store.Open(path)
	require.NoError(err)
	source, err := st.GetOrCreateSource("beeper", "upgrade-account")
	require.NoError(err)
	chat, err := st.EnsureConversationWithType(source.ID, "upgrade-room", "direct_chat", "")
	require.NoError(err)
	sender, err := st.EnsureParticipant("sender@example.com", "Upgrade Sender", "example.com")
	require.NoError(err)
	_, err = st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: chat, SourceMessageID: "before",
		MessageType: "beeper", SenderID: sql.NullInt64{Int64: sender, Valid: true}})
	require.NoError(err)
	for _, statement := range []string{
		"DROP TRIGGER trg_chat_members_messages_update", "DROP TRIGGER trg_chat_members_messages_delete",
		"DROP TRIGGER trg_chat_members_recipients_update", "DROP TRIGGER trg_chat_members_recipients_delete",
		"DROP TRIGGER trg_chat_members_conversation_type", "DROP INDEX idx_messages_source_owner",
		"DROP TABLE chat_members", "DROP TABLE chat_members_dirty", "DROP TABLE chat_anchors",
		"DELETE FROM applied_migrations WHERE name = 'chat_members_v1'",
		// Schema version 11 is the last one without the chat member index.
		"UPDATE archive_metadata SET value = '11' WHERE key = 'schema_version'",
	} {
		_, err = st.DB().Exec(statement)
		require.NoError(err, statement)
	}
	require.NoError(st.Close())

	_, err = archive.OpenSQLite(t.Context(), path)
	require.ErrorContains(err, "run setup")
	require.NoError(archive.SetupSQLite(t.Context(), path))
	runtime, err := archive.OpenSQLite(t.Context(), path)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(runtime.Close()) })
	page, err := runtime.Store().SearchChatsContext(t.Context(), store.ChatDiscoveryQuery{Query: "Upgrade Sender"})
	require.NoError(err)
	require.Len(page.Results, 1)
	require.Equal(chat, page.Results[0].ConversationID)
	_, err = runtime.Store().UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: chat,
		SourceMessageID: "after", MessageType: "beeper"})
	require.NoError(err)
}
