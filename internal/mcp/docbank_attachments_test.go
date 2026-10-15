package mcp

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestAttachmentDocbankViewsThroughDaemon(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Parallel()

	type receipt struct {
		NodeID int64 `json:"node_id"`
	}
	type view struct {
		Docbank     []receipt `json:"docbank"`
		Attachments []struct {
			Docbank []receipt `json:"docbank"`
		} `json:"attachments"`
	}
	f := storetest.New(t)
	_, _, err := f.Store.RegisterAttachmentChangeConsumer(t.Context(), store.DocbankAttachmentConsumerKey("destination"))
	require.NoError(err)
	mid := f.CreateMessage("mirror-message")
	data := []byte("synthetic document")
	hash := fmt.Sprintf("%x", sha256.Sum256(data))
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	createAttachmentFixture(t, cfg.AttachmentsDir(), hash, data)
	require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), mid, store.AttachmentWrite{Filename: "report.pdf", MIMEType: "application/pdf", ContentHash: hash, StoragePath: hash[:2] + "/" + hash, Size: int64(len(data))}))
	var id int64
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind("SELECT id FROM attachments WHERE message_id=?"), mid).Scan(&id))
	require.NoError(f.Store.RecordDocbankAttachment(t.Context(), "destination", store.DocbankAttachmentDelivery{ContentHash: hash}, store.DocbankAttachmentRef{NodeID: 8, VersionID: "version"}, ""))
	engine := query.NewSQLiteEngine(f.Store.DB())
	if f.Store.IsPostgreSQL() {
		engine = query.NewEngineWithDialect(f.Store.DB(), query.PostgreSQLQueryDialect{})
	}
	daemon := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: f.Store, Engine: engine, Logger: slog.New(slog.DiscardHandler)}).Router())
	defer daemon.Close()
	client, err := daemonclient.New(daemonclient.Config{URL: daemon.URL, AllowInsecure: true, HTTPClient: daemon.Client()})
	require.NoError(err)
	defer func() { assert.NoError(client.Close()) }()
	remote := daemonclient.NewEngineAdapter(client)
	server := newMCPHTTPServer(ServeOptions{Engine: remote, AttachmentReader: client}, HTTPOptions{})
	defer func() { assert.NoError(server.Shutdown(t.Context())) }()
	for _, call := range []struct {
		name string
		args map[string]any
	}{{ToolGetAttachment, map[string]any{"attachment_id": id, "offset": 0, "length": 18}}, {ToolGetMessage, map[string]any{"id": mid}}} {
		response, raw := task4RawRequest(t, server.Handler, "tools/call", map[string]any{"name": call.name, "arguments": call.args}, nil)
		require.Empty(response.Error, raw)
		assert.NotEqual(true, response.Result["isError"], raw)
		encoded, err := json.Marshal(response.Result["structuredContent"])
		require.NoError(err)
		var body view
		require.NoError(json.Unmarshal(encoded, &body))
		refs := body.Docbank
		if call.name == ToolGetMessage {
			require.Len(body.Attachments, 1)
			refs = body.Attachments[0].Docbank
		}
		require.Len(refs, 1, raw)
		assert.Equal(int64(8), refs[0].NodeID)
	}
}
