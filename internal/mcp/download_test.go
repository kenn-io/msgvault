package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	msgexport "go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type changingAttachmentMetadataEngine struct {
	*querytest.MockEngine

	calls atomic.Int64
}

func (e *changingAttachmentMetadataEngine) GetAttachment(ctx context.Context, id int64) (*query.AttachmentInfo, error) {
	attachment, err := e.MockEngine.GetAttachment(ctx, id)
	if err != nil || attachment == nil {
		return attachment, err
	}
	cloned := *attachment
	sequence := e.calls.Add(1)
	cloned.Filename = fmt.Sprintf("attachment-%d.bin", sequence)
	cloned.MimeType = fmt.Sprintf("application/x-test-%d", sequence)
	return &cloned, nil
}

type attachmentReaderProbe struct {
	read  func(context.Context, string) ([]byte, error)
	check func(context.Context, string) error
}

func (r attachmentReaderProbe) ReadAttachment(ctx context.Context, contentHash string) ([]byte, error) {
	return r.read(ctx, contentHash)
}

func (r attachmentReaderProbe) CheckAttachmentContent(ctx context.Context, contentHash string) error {
	return r.check(ctx, contentHash)
}

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

func TestAttachmentChunkContinuationUsesMetadataAuthorizationAndLocalMetadata(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	data := []byte("0123456789")
	hash := fmt.Sprintf("%x", sha256.Sum256(data))
	engine := &changingAttachmentMetadataEngine{MockEngine: &querytest.MockEngine{
		Attachments: map[int64]*query.AttachmentInfo{
			7: {ID: 7, Filename: "stored.bin", MimeType: "application/octet-stream", Size: int64(len(data)), ContentHash: hash},
		},
	}}
	var reads, authorizations atomic.Int64
	key := downloadKey{attachment: 7}
	h := &handlers{
		delegatedOnly: true,
		downloads:     &downloadCache{},
		engine:        engine,
		attachmentReader: attachmentReaderProbe{
			read: func(_ context.Context, gotHash string) ([]byte, error) {
				if gotHash != hash {
					return nil, errors.New("unexpected attachment hash")
				}
				reads.Add(1)
				return data, nil
			},
			check: func(_ context.Context, gotHash string) error {
				if gotHash != hash {
					return errors.New("unexpected attachment hash")
				}
				authorizations.Add(1)
				return nil
			},
		},
	}
	t.Cleanup(h.downloads.close)
	call := func(args map[string]any) (getAttachmentResponse, error) {
		result, err := h.getAttachment(t.Context(), toolRequest{arguments: args})
		if err != nil {
			return getAttachmentResponse{}, err
		}
		if result.isError {
			return getAttachmentResponse{}, errors.New(result.text)
		}
		var got getAttachmentResponse
		if err := json.Unmarshal([]byte(result.text), &got); err != nil {
			return got, err
		}
		return got, nil
	}

	first, err := call(map[string]any{"attachment_id": float64(7), "offset": float64(0), "length": float64(5)})
	must.NoError(err)
	checks.Equal("attachment-1.bin", first.Filename)
	checks.Equal("application/x-test-1", first.MIMEType)

	const continuationCount = 4
	type continuationResult struct {
		response getAttachmentResponse
		err      error
	}
	results := make(chan continuationResult, continuationCount)
	var workers sync.WaitGroup
	for i := range continuationCount {
		workers.Add(1)
		go func(offset int64) {
			defer workers.Done()
			response, err := call(map[string]any{
				"attachment_id": float64(7), "offset": float64(offset), "length": float64(1), "sha256": hash,
			})
			results <- continuationResult{response: response, err: err}
		}(int64(5 + i))
	}
	workers.Wait()
	close(results)
	seen := make(map[string]struct{}, continuationCount)
	for result := range results {
		must.NoError(result.err)
		var sequence int
		_, scanErr := fmt.Sscanf(result.response.Filename, "attachment-%d.bin", &sequence)
		must.NoError(scanErr)
		checks.Greater(sequence, 1)
		checks.Equal(fmt.Sprintf("application/x-test-%d", sequence), result.response.MIMEType)
		seen[result.response.Filename] = struct{}{}
	}
	checks.Len(seen, continuationCount, "concurrent continuations must get request-local metadata")

	h.downloads.mu.Lock()
	cached := h.downloads.entries[key]
	cachedFilename, cachedMIMEType := "", ""
	if cached != nil && cached.attachment != nil {
		cachedFilename = cached.attachment.metadata.Filename
		cachedMIMEType = cached.attachment.mimeType
	}
	h.downloads.mu.Unlock()
	checks.Equal("attachment-1.bin", cachedFilename, "validation must leave the cached snapshot immutable")
	checks.Equal("application/x-test-1", cachedMIMEType)
	checks.Equal(int64(1), reads.Load(), "continuations must reuse cached bytes")
	checks.Equal(int64(continuationCount), authorizations.Load(), "each continuation must use metadata authorization")
	checks.Equal(int64(continuationCount+1), engine.calls.Load(), "each chunk must reauthorize attachment metadata")
}

func TestEmbeddedReaderAttachmentContinuationUsesPerCallSourceScope(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	attachmentsDir := t.TempDir()
	source, err := st.GetOrCreateSource("gmail", "owner@example.test")
	must.NoError(err)
	otherSource, err := st.GetOrCreateSource("gmail", "other@example.test")
	must.NoError(err)
	conversationID, err := st.EnsureConversation(source.ID, "embedded-download", "Embedded download")
	must.NoError(err)
	messageID, err := st.UpsertMessage(&store.Message{
		SourceID: source.ID, ConversationID: conversationID,
		SourceMessageID: "embedded-message", MessageType: "email",
	})
	must.NoError(err)
	data := []byte("embedded attachment")
	hash := fmt.Sprintf("%x", sha256.Sum256(data))
	path, err := msgexport.StoragePath(attachmentsDir, hash)
	must.NoError(err)
	must.NoError(os.MkdirAll(filepath.Dir(path), 0o700))
	must.NoError(os.WriteFile(path, data, 0o600))
	must.NoError(st.UpsertAttachmentRecord(t.Context(), messageID, store.AttachmentWrite{
		Filename: "embedded.bin", MIMEType: "application/octet-stream", ContentHash: hash,
		StoragePath: path, Size: int64(len(data)),
	}))
	attachments, err := query.NewEngine(st.DB(), st.IsPostgreSQL()).GetAttachmentsByHash(t.Context(), hash)
	must.NoError(err)
	must.Len(attachments, 1)
	attachmentID := attachments[0].ID
	reader := NewEmbeddedReader(st, attachmentsDir)
	t.Cleanup(reader.Close)
	call := func(sourceIDs []int64, args map[string]any) *sdkmcp.CallToolResult {
		t.Helper()
		result, _, err := reader.Call(t.Context(), sourceIDs,
			&sdkmcp.CallToolRequest{Params: &sdkmcp.CallToolParamsRaw{Name: ToolGetAttachment}}, args)
		must.NoError(err)
		return result
	}
	first := call([]int64{source.ID}, map[string]any{
		"attachment_id": float64(attachmentID), "offset": float64(0), "length": float64(4),
	})
	checks.False(first.IsError)
	continued := call([]int64{source.ID}, map[string]any{
		"attachment_id": float64(attachmentID), "offset": float64(4), "length": float64(4), "sha256": hash,
	})
	checks.False(continued.IsError)
	denied := call([]int64{otherSource.ID}, map[string]any{
		"attachment_id": float64(attachmentID), "offset": float64(4), "length": float64(4), "sha256": hash,
	})
	checks.True(denied.IsError, "continuations must reapply the caller's source scope")
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
		for _, validationErr := range []error{context.Canceled, errors.New("synthetic authorization failure")} {
			first.validate = func(context.Context) error { return validationErr }
			_, err = cache.get(t.Context(), key, chunkRequest{}, load)
			must.ErrorIs(err, validationErr)
			checks.Same(first, cache.entries[key])
			checks.Equal(len(first.data), cache.bytes)
		}
		first.validate = nil
		_, err = cache.get(t.Context(), key, chunkRequest{digest: "wrong"}, load)
		must.ErrorIs(err, errDownloadExpired)
		checks.Same(first, cache.entries[key])
		first.validate = func(context.Context) error { return errDownloadExpired }
		_, err = cache.get(t.Context(), key, chunkRequest{offset: 1, digest: first.digest}, load)
		must.ErrorIs(err, errDownloadExpired)
		checks.Zero(cache.bytes)
		first, err = cache.get(t.Context(), key, chunkRequest{}, load)
		must.NoError(err, "invalidated entry can restart immediately")
		first.validate = func(context.Context) error { return fmt.Errorf("snapshot changed: %w", errDownloadExpired) }
		load = func() (*downloadSnapshot, error) { return &downloadSnapshot{data: []byte("updated bytes")}, nil }
		first, err = cache.get(t.Context(), key, chunkRequest{}, load)
		must.NoError(err, "offset zero also replaces an invalidated cached entry directly")
		checks.Equal([]byte("updated bytes"), first.data)
		checks.Equal(len(first.data), cache.bytes)
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
