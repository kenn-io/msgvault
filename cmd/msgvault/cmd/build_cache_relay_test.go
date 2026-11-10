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
	"go.kenn.io/msgvault/internal/testutil"
)

func TestBuildCacheRelayedAuthors(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "archive.db")
	analytics := filepath.Join(dir, "analytics")
	st := testutil.NewSQLiteTestStoreAt(t, path)
	src, err := st.GetOrCreateSource("gmail", "reader@example.test")
	requirements.NoError(err)
	for _, author := range []struct{ name, login string }{{"Alice", "alice"}, {"Build Bot", "build[bot]"}} {
		raw := []byte("From: " + author.name + " <notifications@github.com>\r\nTo: reader@example.test\r\nX-GitHub-Sender: " + author.login + "\r\nDate: Mon, 01 Jan 2024 12:00:00 +0000\r\nSubject: Notice\r\n\r\nHello")
		requirements.NoError(importer.IngestRawMessage(t.Context(), st, src.ID, src.Identifier, "", nil, author.login, author.login, raw, time.Time{}, slog.Default()))
	}
	author, err := st.EnsureParticipantByIdentifier("github", "alice", "Alice")
	requirements.NoError(err)
	raw := []byte("From: Alice <old@old.test>\r\nTo: reader@example.test\r\nDate: Mon, 01 Jan 2024 12:00:00 +0000\r\nSubject: Ordinary\r\n\r\nHello")
	requirements.NoError(importer.IngestRawMessage(t.Context(), st, src.ID, src.Identifier, "", nil, "ordinary", "ordinary", raw, time.Time{}, slog.Default()))
	alias, err := st.EnsureParticipant("old@old.test", "Alice", "old.test")
	requirements.NoError(err)
	contact, err := st.EnsureParticipant("alice@example.test", "Alice", "example.test")
	requirements.NoError(err)
	requirements.NoError(st.MergeParticipants(author, contact))
	requirements.NoError(st.MergeParticipants(alias, contact))
	requirements.NoError(st.Close())
	_, err = buildCache(t.Context(), path, analytics, false)
	requirements.NoError(err)
	engine, err := query.NewArchiveDuckDBEngine(t.Context(), analytics)
	requirements.NoError(err)
	t.Cleanup(func() { _ = engine.Close() })
	ids := make(map[int64]bool)
	for _, login := range []string{"alice", "build[bot]"} {
		people, err := engine.SearchPeople(t.Context(), query.PersonSearchRequest{Query: login, Page: query.PageSpec{Limit: 10}})
		requirements.NoError(err)
		requirements.Len(people.Rows, 1)
		want := int64(1)
		if login == "alice" {
			want = 2
		}
		assertions.Equal(want, people.Rows[0].ActivityCount)
		ids[people.Rows[0].CanonicalID()] = true
	}
	assertions.Len(ids, 2)
}
