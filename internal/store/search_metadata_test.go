package store_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestSearchMetadataUsesIndexAndAttachments(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := storetest.New(t)
	id := f.NewMessage().WithSubject("Plan").WithSnippet("prefix without match").Create(t, f.Store)
	require.NoError(f.Store.UpsertFTS(id, "Plan", strings.Repeat("界", 300)+" needle context", "", "", ""))
	_, err := f.Store.DB().Exec(f.Store.Rebind(`INSERT INTO attachments(message_id,filename,mime_type,size,storage_path,content_hash) VALUES (?,'plan.pdf','application/pdf',10,'synthetic',NULL),(?,'budget.csv','text/csv',20,'synthetic',NULL)`), id, id)
	require.NoError(err)
	// A third attachment the sync hasn't written yet: the stored count wins over the row count.
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET attachment_count = 3 WHERE id = ?`), id)
	require.NoError(err)
	emptyID := f.NewMessage().WithSubject("Empty").Create(t, f.Store)
	metadata, err := f.Store.GetSearchMetadata(t.Context(), []int64{id, emptyID, -1}, search.Parse("needle"), true)
	require.NoError(err)
	assert.NotContains(metadata, int64(-1))
	require.Contains(metadata, emptyID)
	assert.Equal([]string{}, metadata[emptyID].AttachmentNames)
	assert.Zero(metadata[emptyID].AttachmentCount)
	assert.Equal([]string{"plan.pdf", "budget.csv"}, metadata[id].AttachmentNames)
	assert.Equal(3, metadata[id].AttachmentCount)
	if !f.Store.IsPostgreSQL() {
		assert.Contains(metadata[id].MatchSnippet, "needle")
		assert.True(utf8.ValidString(metadata[id].MatchSnippet))
		assert.LessOrEqual(utf8.RuneCountInString(metadata[id].MatchSnippet), 162)
	} else {
		assert.Empty(metadata[id].MatchSnippet)
	}
	if !f.Store.IsPostgreSQL() {
		_, err = f.Store.DB().Exec(`DROP TABLE messages_fts`)
		require.NoError(err)
		var logs bytes.Buffer
		previousLogger := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
		t.Cleanup(func() { slog.SetDefault(previousLogger) })
		metadata, err = f.Store.GetSearchMetadata(t.Context(), []int64{id}, search.Parse("needle"), true)
		require.NoError(err)
		assert.Equal([]string{"plan.pdf", "budget.csv"}, metadata[id].AttachmentNames)
		assert.Equal(3, metadata[id].AttachmentCount)
		assert.Empty(metadata[id].MatchSnippet, "unavailable context falls back to the stored preview")
		assert.Contains(logs.String(), `level=WARN msg="sql error"`)
		assert.Contains(logs.String(), "no such table: messages_fts")
	}
}

func TestSearchMetadataSkipsUnnamedAttachments(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	id := f.NewMessage().Create(t, f.Store)
	_, err := f.Store.DB().Exec(f.Store.Rebind(`INSERT INTO attachments(message_id,filename,mime_type,size,storage_path) VALUES (?,NULL,'application/octet-stream',10,'synthetic'),(?,'','application/octet-stream',10,'synthetic')`), id, id)
	require.NoError(err)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET attachment_count = 2 WHERE id = ?`), id)
	require.NoError(err)
	metadata, err := f.Store.GetSearchMetadata(t.Context(), []int64{id}, nil, false)
	require.NoError(err)
	assert.Equal([]string{}, metadata[id].AttachmentNames)
	assert.Equal(2, metadata[id].AttachmentCount)
}
