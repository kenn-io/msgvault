package mcp

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	msgexport "go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestChunkDownloadsThroughDaemon(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	engine := query.NewSQLiteEngine(st.DB())
	cfg := &config.Config{}
	cfg.Data.DataDir = t.TempDir()
	source, err := st.GetOrCreateSource("gmail", "owner@example.com")
	must.NoError(err)
	conversation, err := st.EnsureConversation(source.ID, "download-thread", "Download")
	must.NoError(err)
	id, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: "download-message", MessageType: "email"})
	must.NoError(err)
	var originalReads, attachmentReads atomic.Int64
	router := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: st, Engine: engine, Logger: slog.New(slog.DiscardHandler)}).Router()
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/cli/message/original":
			checks.Equal("268435456", r.URL.Query().Get("max_bytes"), "bound the daemon read before allocating original MIME")
			originalReads.Add(1)
		case "/api/v1/cli/attachment":
			attachmentReads.Add(1)
		}
		router.ServeHTTP(w, r)
	}))
	t.Cleanup(daemon.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: daemon.URL, AllowInsecure: true, HTTPClient: daemon.Client()})
	must.NoError(err)
	t.Cleanup(func() { checks.NoError(client.Close()) })
	server := newMCPHTTPServer(ServeOptions{Engine: daemonclient.NewEngineAdapter(client), AttachmentReader: client}, HTTPOptions{})
	t.Cleanup(func() { checks.NoError(server.Shutdown(t.Context())) })
	chunk := func(t *testing.T, name string, args map[string]any) exportChunkResp {
		t.Helper()
		must := require.New(t)
		response, raw := task4RawRequest(t, server.Handler, "tools/call", map[string]any{"name": name, "arguments": args}, nil)
		must.Empty(response.Error, "%s", raw)
		must.NotEqual(true, response.Result["isError"], "%s", raw)
		encoded, err := json.Marshal(response.Result["structuredContent"])
		must.NoError(err)
		var result exportChunkResp
		must.NoError(json.Unmarshal(encoded, &result))
		return result
	}

	t.Run("original snapshot survives raw replacement", func(t *testing.T) {
		checks := assert.New(t)
		must := require.New(t)
		data := bytes.Repeat([]byte("original"), (40<<20)/8)
		must.NoError(st.UpsertMessageRaw(id, data))
		digest := fmt.Sprintf("%x", sha256.Sum256(data))
		for offset := 0; offset < len(data); offset += 1 << 20 {
			args := map[string]any{"id": id, "offset": offset, "length": 1 << 20}
			if offset > 0 {
				args["sha256"] = digest
			}
			got := chunk(t, ToolExportEML, args)
			part, err := base64.StdEncoding.DecodeString(got.DataBase64)
			must.NoError(err)
			must.Equal(data[offset:offset+(1<<20)], part)
			checks.Equal(digest, got.SHA256)
			checks.Equal(offset+(1<<20) == len(data), got.Complete)
			if offset == 0 {
				must.NoError(st.UpsertMessageRaw(id, []byte("replacement MIME")))
				response, raw := task4RawRequest(t, server.Handler, "tools/call", map[string]any{
					"name":      ToolExportEML,
					"arguments": map[string]any{"id": id, "offset": 1, "sha256": fmt.Sprintf("%064x", 0)},
				}, nil)
				must.Empty(response.Error)
				checks.Equal(true, response.Result["isError"])
				checks.Contains(raw, "restart at offset 0")
			}
		}
		checks.Equal(int64(1), originalReads.Load())
	})

	t.Run("large attachment chunks retain full response cap", func(t *testing.T) {
		checks := assert.New(t)
		must := require.New(t)
		data := bytes.Repeat([]byte("attachment"), (80<<20)/10+1)[:80<<20]
		digest := fmt.Sprintf("%x", sha256.Sum256(data))
		path, err := msgexport.StoragePath(cfg.AttachmentsDir(), digest)
		must.NoError(err)
		must.NoError(os.MkdirAll(filepath.Dir(path), 0o700))
		must.NoError(os.WriteFile(path, data, 0o600))
		must.NoError(st.UpsertAttachmentRecord(t.Context(), id, store.AttachmentWrite{Filename: "large.bin", MIMEType: "application/octet-stream", ContentHash: digest, StoragePath: path, Size: int64(len(data))}))
		attachments, err := engine.GetAttachmentsByHash(t.Context(), digest)
		must.NoError(err)
		must.Len(attachments, 1)
		attachmentID := attachments[0].ID
		for offset := 0; offset < 2<<20; offset += 1 << 20 {
			got := chunk(t, ToolGetAttachment, map[string]any{"attachment_id": attachmentID, "offset": offset, "length": 1 << 20, "sha256": digest})
			part, err := base64.StdEncoding.DecodeString(got.DataBase64)
			must.NoError(err)
			checks.Equal(data[offset:offset+(1<<20)], part)
			checks.Equal(int64(80<<20), got.Size)
			checks.Equal(digest, got.SHA256)
		}
		checks.Equal(int64(1), attachmentReads.Load())
		response, raw := task4RawRequest(t, server.Handler, "tools/call", map[string]any{"name": ToolGetAttachment, "arguments": map[string]any{"attachment_id": attachmentID}}, nil)
		must.Empty(response.Error)
		checks.Equal(true, response.Result["isError"])
		checks.Contains(raw, "attachment too large: 83886080 bytes (max 52428800)")
		response, _ = task4RawRequest(t, server.Handler, "resources/read", map[string]any{"uri": attachmentResourceURI(attachmentID)}, nil)
		checks.NotEmpty(response.Error)
	})
}

func TestDownloadSnapshotExpiryAndShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		checks := assert.New(t)
		must := require.New(t)
		var cache downloadCache
		defer cache.close()
		key := downloadKey{attachment: 1}
		load := func() (*downloadSnapshot, error) {
			return &downloadSnapshot{data: []byte("first version")}, nil
		}
		first, err := cache.get(t.Context(), key, chunkRequest{}, load)
		must.NoError(err)
		time.Sleep(downloadLifetime)
		synctest.Wait()
		_, err = cache.get(t.Context(), key, chunkRequest{offset: 1, digest: first.digest}, load)
		must.ErrorIs(err, errDownloadExpired)
		checks.Zero(cache.bytes, "expiry releases the retained download budget")
		_, err = cache.get(t.Context(), key, chunkRequest{}, load)
		must.NoError(err, "offset zero can start a new download")
		cache.close()
		_, err = cache.get(t.Context(), key, chunkRequest{}, load)
		must.ErrorIs(err, errDownloadExpired)
		checks.Zero(cache.bytes, "shutdown releases snapshots without waiting for expiry")
	})
}

func TestDownloadSnapshotEviction(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		checks := assert.New(t)
		must := require.New(t)
		var cache downloadCache
		defer cache.close()
		load := func() (*downloadSnapshot, error) {
			return &downloadSnapshot{data: []byte("a")}, nil
		}
		var first *downloadSnapshot
		for i := int64(1); i <= maxDownloads+1; i++ {
			snapshot, err := cache.get(t.Context(), downloadKey{attachment: i}, chunkRequest{}, load)
			must.NoError(err)
			if i == 1 {
				first = snapshot
			}
			// Distinct virtual timestamps identify the oldest snapshot on every OS.
			time.Sleep(time.Second)
		}
		_, err := cache.get(t.Context(), downloadKey{attachment: 1}, chunkRequest{offset: 1, digest: first.digest}, load)
		must.ErrorIs(err, errDownloadExpired)
		checks.Equal(maxDownloads, cache.bytes)
	})
}

func TestDownloadSnapshotByteBudget(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	var cache downloadCache
	defer cache.close()
	data := make([]byte, maxDownloadBytes/2+1)
	load := func() (*downloadSnapshot, error) {
		return &downloadSnapshot{data: data}, nil
	}
	first, err := cache.get(t.Context(), downloadKey{attachment: 1}, chunkRequest{}, load)
	must.NoError(err)
	_, err = cache.get(t.Context(), downloadKey{attachment: 2}, chunkRequest{}, load)
	must.NoError(err)
	_, err = cache.get(t.Context(), downloadKey{attachment: 1}, chunkRequest{offset: 1, digest: first.digest}, load)
	must.ErrorIs(err, errDownloadExpired, "byte budget evicts before the entry limit")
	checks.Equal(len(data), cache.bytes)
}
