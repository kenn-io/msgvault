package query_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestDocbankAttachmentQueryReceipts(t *testing.T) {
	f := storetest.New(t)
	_, _, err := f.Store.RegisterAttachmentChangeConsumer(t.Context(), store.DocbankAttachmentConsumerKey("dest"))
	require.NoError(t, err)
	hash := strings.Repeat("a", 64)
	mid := f.CreateMessage("stored")
	require.NoError(t, f.Store.UpsertAttachmentRecord(t.Context(), mid, store.AttachmentWrite{ContentHash: hash, Size: 20, StoragePath: "aa/" + hash}))
	require.NoError(t, f.Store.RecordDocbankAttachment(t.Context(), "dest", store.DocbankAttachmentDelivery{ContentHash: hash}, store.DocbankAttachmentRef{Endpoint: "https://docbank.example.com", Folder: "/msgvault", NodeID: 8, VersionID: "v1"}, ""))
	t.Run("DuckDB scanner", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		if f.Store.IsPostgreSQL() {
			t.Skip("DuckDB scanner reads SQLite")
		}
		var sequence int
		var name, path string
		require.NoError(f.Store.DB().QueryRow(`PRAGMA database_list`).Scan(&sequence, &name, &path))
		engine, err := query.NewDuckDBEngine("", path, nil)
		require.NoError(err)
		t.Cleanup(func() { _ = engine.Close() })
		if !query.HasSQLiteScanner(engine) {
			t.Skip("DuckDB sqlite_scanner extension unavailable")
		}
		msg, err := engine.GetMessage(t.Context(), mid)
		require.NoError(err)
		require.Len(msg.Attachments, 1)
		assert.Equal(hash, msg.Attachments[0].ContentHash)
		assert.Equal([]store.DocbankAttachmentRef{{Endpoint: "https://docbank.example.com", Folder: "/msgvault", NodeID: 8, VersionID: "v1", BlobHash: hash}}, msg.Attachments[0].Docbank)
	})
}

func TestDocbankAttachmentQueryAliasReceipts(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	_, _, err := f.Store.RegisterAttachmentChangeConsumer(t.Context(), store.DocbankAttachmentConsumerKey("dest"))
	require.NoError(err)
	hash := strings.Repeat("a", 64)
	require.NoError(f.Store.RecordDocbankAttachment(t.Context(), "dest", store.DocbankAttachmentDelivery{ContentHash: hash}, store.DocbankAttachmentRef{NodeID: 8, VersionID: "v1"}, ""))
	engine := query.NewSQLiteEngine(f.Store.DB())
	if f.Store.IsPostgreSQL() {
		engine = query.NewEngineWithDialect(f.Store.DB(), query.PostgreSQLQueryDialect{})
	}
	mid := f.CreateMessage("slack:alias")
	require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), mid, store.AttachmentWrite{StoragePath: "aa/" + hash, SourceAttachmentID: "slack:alias"}))
	msg, err := engine.GetMessage(t.Context(), mid)
	require.NoError(err)
	require.Len(msg.Attachments, 1)
	assert.Equal(hash, msg.Attachments[0].ContentHash)
	assert.Len(msg.Attachments[0].Docbank, 1)
	att, err := engine.GetAttachment(t.Context(), msg.Attachments[0].ID)
	require.NoError(err)
	require.NotNil(att)
	assert.Equal(hash, att.ContentHash)
	assert.Len(att.Docbank, 1)
	for _, provider := range []string{"discord:alias", "beeper:pending"} {
		require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), f.CreateMessage(provider), store.AttachmentWrite{StoragePath: "aa/" + hash, SourceAttachmentID: provider}))
	}
	require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), f.CreateMessage("slack:invalid"), store.AttachmentWrite{StoragePath: "ab/" + hash, SourceAttachmentID: "slack:invalid"}))
	byHash, err := engine.GetAttachmentsByHash(t.Context(), hash)
	require.NoError(err)
	require.Len(byHash, 3)
	for _, att := range byHash {
		assert.Equal(hash, att.ContentHash)
	}
}
