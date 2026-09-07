package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentstore"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/remoteimage"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestArchivedRemoteImagesRenderOfflineAndAreMessageScoped(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	src, err := st.GetOrCreateSource("eml", "user@example.com")
	require.NoError(err)
	conv, err := st.EnsureConversation(src.ID, "thread", "Images")
	require.NoError(err)
	original := `<img src="http://images.example/chart.png">`
	largeImage := append(append([]byte(nil), fakePNG...), make([]byte, 6<<20)...)
	id, err := st.PersistMessage(&store.MessagePersistData{
		Message:  &store.Message{SourceID: src.ID, SourceMessageID: "message", ConversationID: conv, MessageType: "email"},
		BodyHTML: sql.NullString{String: original, Valid: true}, RawMIME: []byte("From: user@example.com\r\n\r\nOriginal"),
	})
	require.NoError(err)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(largeImage)
	}))
	defer upstream.Close()
	fetcher := remoteimage.NewFetcher()
	fetcher.LookupNetIP = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}
	fetcher.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(upstream.URL, "http://"))
	}
	result := fetcher.Archive(t.Context(), st, cfg.AttachmentsDir(), id, original)
	require.Empty(result.Errors)
	require.Equal(1, result.Downloaded)
	upstream.Close() // The original image host is now unavailable.
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, Logger: testLogger()})
	detailResponse := httptest.NewRecorder()
	srv.Router().ServeHTTP(detailResponse, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/messages/%d", id), nil))
	require.Equal(http.StatusOK, detailResponse.Code, detailResponse.Body.String())
	var detail MessageDetail
	require.NoError(json.Unmarshal(detailResponse.Body.Bytes(), &detail))
	assert.NotContains(detail.BodyHTML, "http://images.example")
	assert.Contains(detail.BodyHTML, "cid:remote-image:")
	conversationResponse := httptest.NewRecorder()
	srv.Router().ServeHTTP(conversationResponse, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/conversations/%d?anchor=%d", conv, id), nil))
	require.Equal(http.StatusOK, conversationResponse.Code, conversationResponse.Body.String())
	var conversation ConversationResponse
	require.NoError(json.Unmarshal(conversationResponse.Body.Bytes(), &conversation))
	require.Len(conversation.Messages, 1)
	assert.Contains(conversation.Messages[0].BodyHTML, "cid:remote-image:")
	refs, err := st.MessageRemoteImages(id)
	require.NoError(err)
	require.Len(refs, 1)
	for cid := range refs {
		response := httptest.NewRecorder()
		srv.Router().ServeHTTP(response, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/messages/%d/inline?cid=%s", id, cid), nil))
		require.Equal(http.StatusOK, response.Code, response.Body.String())
		assert.Equal(largeImage, response.Body.Bytes())
		assert.Equal("image/png", response.Header().Get("Content-Type"))
		response = httptest.NewRecorder()
		srv.Router().ServeHTTP(response, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/messages/%d/inline?cid=%s", id+1, cid), nil))
		assert.Equal(http.StatusNotFound, response.Code)
	}
	saved, err := st.GetMessage(id)
	require.NoError(err)
	assert.Equal(original, saved.BodyHTML)

	// Compact the same bytes into a real pack, then remove only the test's
	// loose copy: the reader must resolve through the configured blob store.
	entry := buildTestPack(t, cfg.AttachmentsDir(), largeImage)
	require.NoError(st.RecordPackedBlobs(store.PackRecord{
		PackID: entry.PackID, EntryCount: 1, StoredBytes: entry.StoredLen, CreatedAt: time.Now(),
	}, []store.PackIndexEntry{entry}))
	path, err := export.StoragePath(cfg.AttachmentsDir(), entry.BlobHash)
	require.NoError(err)
	require.NoError(os.Remove(path))
	bs, err := attachmentstore.New(store.NewPackCatalog(st), cfg.AttachmentsDir())
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(bs.Close()) })
	srv = NewServerWithOptions(ServerOptions{Config: cfg, Store: st, BlobStore: bs, Logger: testLogger()})
	for cid := range refs {
		response := httptest.NewRecorder()
		srv.Router().ServeHTTP(response, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/messages/%d/inline?cid=%s", id, cid), nil))
		require.Equal(http.StatusOK, response.Code, response.Body.String())
		assert.Equal(largeImage, response.Body.Bytes())
	}
}
