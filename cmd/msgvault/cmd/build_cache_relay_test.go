package cmd

import (
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/importer"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/senderauth"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestBuildCacheRelayedAuthors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "archive.db")
	analytics := filepath.Join(dir, "analytics")
	st := testutil.NewSQLiteTestStoreAt(t, path)
	src, err := st.GetOrCreateSource("gmail", "reader@example.test")
	require.NoError(t, err)
	for _, author := range []struct{ name, login string }{{"Alice", "alice"}, {"Build Bot", "build[bot]"}} {
		raw := []byte("From: " + author.name + " <notifications@github.com>\r\nTo: reader@example.test\r\nX-GitHub-Sender: " + author.login + "\r\nAuthentication-Results: mx.example.test; dmarc=pass header.from=github.com\r\nDate: Mon, 01 Jan 2024 12:00:00 +0000\r\nSubject: Notice\r\n\r\nHello")
		require.NoError(t, importer.IngestRawMessage(t.Context(), st, src.ID, src.Identifier, "", nil, author.login, author.login, raw, time.Time{}, slog.Default()))
		var id int64
		require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages WHERE source_message_id=?`, author.login).Scan(&id))
		message, err := st.GetMessage(id)
		require.NoError(t, err)
		assert.Equal(t, "notifications@github.com", message.FromEmail)
		archived, err := st.GetMessageRawContext(t.Context(), id)
		require.NoError(t, err)
		assert.Equal(t, raw, archived)
		assert.True(t, senderauth.Evaluate(archived, []string{"mx.example.test"}, message.FromEmail).Verified)
	}
	require.NoError(t, st.Close())
	_, err = buildCache(t.Context(), path, analytics, false)
	require.NoError(t, err)
	engine, err := query.NewArchiveDuckDBEngine(t.Context(), analytics)
	require.NoError(t, err)
	t.Cleanup(func() { _ = engine.Close() })
	ids := make(map[int64]bool)
	for _, login := range []string{"alice", "build[bot]"} {
		people, err := engine.SearchPeople(t.Context(), query.PersonSearchRequest{Query: login, Page: query.PageSpec{Limit: 10}})
		require.NoError(t, err)
		require.Len(t, people.Rows, 1)
		assert.Equal(t, int64(1), people.Rows[0].ActivityCount)
		ids[people.Rows[0].CanonicalID()] = true
	}
	assert.Len(t, ids, 2)
	filter := query.MessageFilter{Sender: "notifications@github.com", Domain: "github.com"}
	messages, err := engine.ListMessages(t.Context(), filter)
	require.NoError(t, err)
	assert.Len(t, messages, 2)
	targets, err := engine.GetDeletionTargetsByFilter(t.Context(), filter)
	require.NoError(t, err)
	assert.Len(t, targets, 2)
}
