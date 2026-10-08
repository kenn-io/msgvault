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
