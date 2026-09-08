package remoteimage

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestArchivePersistsDistinctURLIdentitiesAndReusesBytes(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("eml", "user@example.com")
	require.NoError(err)
	conversation, err := st.EnsureConversation(src.ID, "thread", "Images")
	require.NoError(err)
	original := `<p>Receipt</p><img src="http://images.example/a?tracking=secret"><img src="http://images.example/a?tracking=secret"><img src="http://images.example/b"><img src="http://images.example/missing"><img src="cid:original">`
	raw := []byte("original MIME bytes")
	id, err := st.PersistMessage(&store.MessagePersistData{
		Message:  &store.Message{SourceID: src.ID, SourceMessageID: "message", ConversationID: conversation, MessageType: "email"},
		BodyHTML: sql.NullString{String: original, Valid: true}, RawMIME: raw,
	})
	require.NoError(err)
	image := []byte("\x89PNG\r\n\x1a\nsynthetic")
	requests := map[string]int{}
	var requestsMu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestsMu.Lock()
		requests[r.URL.Path]++
		requestsMu.Unlock()
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(image)
	}))
	defer upstream.Close()
	f := NewFetcher()
	f.LookupNetIP = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}
	f.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(upstream.URL, "http://"))
	}
	dir := t.TempDir()
	legacyBytes, err := export.StoreAttachmentFileDurable(dir, &mime.Attachment{ContentType: "image/png", Content: image})
	require.NoError(err)
	require.NoError(st.UpsertAttachment(id, "receipt.png", "image/png", legacyBytes.StoragePath, legacyBytes.ContentHash, len(image)))
	var legacyID int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT id FROM attachments WHERE message_id = ?`), id).Scan(&legacyID))
	legacy, err := st.GetFileMetadata(t.Context(), legacyID)
	require.NoError(err)
	require.NotNil(legacy)

	result := f.Archive(t.Context(), st, dir, id, original)
	assert.Equal(2, result.Downloaded)
	require.Len(result.Errors, 1)
	assert.NotContains(result.Errors[0].Error(), "tracking")
	refs, err := st.MessageRemoteImages(id)
	require.NoError(err)
	require.Len(refs, 2)
	paths := map[string]bool{}
	for _, ref := range refs {
		assert.Equal(store.AttachmentRoleInline, ref.Role)
		assert.Equal(store.AttachmentRoleSourceImporterSemantics, ref.RoleSource)
		bytes, err := os.ReadFile(filepath.Join(dir, ref.StoragePath))
		require.NoError(err)
		assert.Equal(image, bytes)
		paths[ref.StoragePath] = true
	}
	assert.Len(paths, 1, "CAS deduplicates content, not source URL occurrences")
	rendered := RewriteHTML(original, refs)
	assert.Equal(3, strings.Count(rendered, `src="cid:remote-image:`))
	assert.Contains(rendered, `src="http://images.example/missing"`)
	assert.Contains(rendered, `src="cid:original"`)
	// Attachment rows can persist before the separate statistics update.
	_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET attachment_count = 0, has_attachments = FALSE WHERE id = ?`), id)
	require.NoError(err)
	result = f.Archive(t.Context(), st, dir, id, original)
	assert.Zero(result.Downloaded)
	assert.Equal(2, result.Reused)
	requestsMu.Lock()
	assert.Equal(map[string]int{"/a": 1, "/b": 1, "/missing": 2}, requests)
	requestsMu.Unlock()
	saved, err := st.GetMessage(id)
	require.NoError(err)
	assert.Equal(original, saved.BodyHTML)
	assert.True(saved.HasAttachments, "reusing archived images must repair the attachment flag")
	var storedAttachmentCount int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT attachment_count FROM messages WHERE id = ?`), id).Scan(&storedAttachmentCount))
	assert.Equal(3, storedAttachmentCount, "reusing archived images must recount all attachment occurrences")
	savedRaw, err := st.GetMessageRaw(id)
	require.NoError(err)
	assert.Equal(raw, savedRaw)
	preservedLegacy, err := st.GetFileMetadata(t.Context(), legacyID)
	require.NoError(err)
	assert.Equal(legacy, preservedLegacy, "remote image downloads must not relabel a MIME attachment with identical bytes")
	var attachmentCount int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM attachments WHERE message_id = ?`), id).Scan(&attachmentCount))
	assert.Equal(3, attachmentCount, "the MIME part and both remote URL occurrences must coexist after retry")
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	backfill, err := f.Backfill(t.Context(), st, dir, src.ID, 1, log)
	require.NoError(err)
	assert.Equal(1, backfill.Messages)
	assert.Zero(backfill.Downloaded)
	assert.Equal(2, backfill.Reused)
	assert.Equal(1, backfill.Errors)
	assert.Contains(logs.String(), "level=WARN")
	assert.Contains(logs.String(), fmt.Sprintf("message=%d", id))
	assert.Contains(logs.String(), "Remote image host returned status 404")
	replacement := []store.AttachmentWrite{}
	_, err = st.PersistMessage(&store.MessagePersistData{
		Message:  &store.Message{SourceID: src.ID, SourceMessageID: "message", ConversationID: conversation, MessageType: "email"},
		BodyHTML: sql.NullString{String: original, Valid: true}, RawMIME: raw, MIMEAttachmentReplacement: &replacement,
	})
	require.NoError(err)
	preserved, err := st.MessageRemoteImages(id)
	require.NoError(err)
	assert.Equal(refs, preserved, "MIME repair must preserve archived remote image occurrences")
}

func TestArchiveBoundsNetworkRequestsByCountAndBytes(t *testing.T) {
	for _, tc := range []struct {
		name             string
		urls, size, want int
	}{
		{"count", 65, 16, 64},
		{"bytes", 5, 10 << 20, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := testutil.NewTestStore(t)
			src, err := st.GetOrCreateSource("eml", "user@example.com")
			require.NoError(err)
			conv, err := st.EnsureConversation(src.ID, "thread", "Images")
			require.NoError(err)
			id, err := st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: src.ID, SourceMessageID: "message", ConversationID: conv, MessageType: "email"}})
			require.NoError(err)
			image := make([]byte, tc.size)
			copy(image, "\x89PNG\r\n\x1a\n")
			var requests atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "image/png")
				_, _ = w.Write(image)
			}))
			defer upstream.Close()
			f := NewFetcher()
			f.LookupNetIP = func(context.Context, string) ([]netip.Addr, error) {
				return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
			}
			f.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(upstream.URL, "http://"))
			}
			var body strings.Builder
			for i := range tc.urls {
				fmt.Fprintf(&body, `<img src="http://images.example/%d">`, i)
			}
			result := f.Archive(t.Context(), st, t.TempDir(), id, body.String())
			assert.Equal(tc.want, result.Downloaded)
			assert.Equal(int64(tc.want), requests.Load())
			assert.NotEmpty(result.Errors)
		})
	}
}

func TestBackfillHonorsSourceAndLimitAndSkipsNonEmail(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	for _, source := range []string{"one@example.com", "two@example.com"} {
		src, err := st.GetOrCreateSource("eml", source)
		require.NoError(err)
		conv, err := st.EnsureConversation(src.ID, "thread", "Empty")
		require.NoError(err)
		for i, kind := range []string{"email", "email", "slack"} {
			_, err := st.PersistMessage(&store.MessagePersistData{
				Message: &store.Message{SourceID: src.ID, SourceMessageID: fmt.Sprintf("%s-%d", kind, i), ConversationID: conv, MessageType: kind},
			})
			require.NoError(err)
		}
		result, err := NewFetcher().Backfill(t.Context(), st, t.TempDir(), src.ID, 1, slog.Default())
		require.NoError(err)
		assert.Equal(1, result.Messages)
		result, err = NewFetcher().Backfill(t.Context(), st, t.TempDir(), src.ID, 0, slog.Default())
		require.NoError(err)
		assert.Equal(2, result.Messages)
	}
	result, err := NewFetcher().Backfill(t.Context(), st, t.TempDir(), 0, 0, slog.Default())
	require.NoError(err)
	assert.Equal(4, result.Messages)
}

func TestArchiveCancellationAndEmptyStorageDoNotFetch(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	f := NewFetcher()
	f.LookupNetIP = func(context.Context, string) ([]netip.Addr, error) {
		assert.Fail("unexpected DNS request")
		return nil, nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result := f.Archive(ctx, st, t.TempDir(), 1, `<img src="http://images.example/a">`)
	require.NotEmpty(result.Errors)
	require.ErrorIs(result.Errors[0], context.Canceled)
	result = f.Archive(t.Context(), st, "", 1, `<img src="http://images.example/a">`)
	require.NotEmpty(result.Errors)
}

func TestArchiveJPEGContentTypes(t *testing.T) {
	for _, tc := range []struct {
		name, contentType string
		body              []byte
		wantDownloaded    int
	}{
		{"jpeg", "image/jpeg", []byte("\xff\xd8\xffsynthetic"), 1},
		{"jpg", "image/jpg", []byte("\xff\xd8\xffsynthetic"), 1},
		{"jpg with non-JPEG bytes", "image/jpg", []byte("\x89PNG\r\n\x1a\nsynthetic"), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := testutil.NewTestStore(t)
			src, err := st.GetOrCreateSource("eml", "user@example.com")
			require.NoError(err)
			conv, err := st.EnsureConversation(src.ID, "thread", "Images")
			require.NoError(err)
			id, err := st.PersistMessage(&store.MessagePersistData{
				Message: &store.Message{SourceID: src.ID, SourceMessageID: "message", ConversationID: conv, MessageType: "email"},
			})
			require.NoError(err)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				_, _ = w.Write(tc.body)
			}))
			defer upstream.Close()
			f := NewFetcher()
			f.LookupNetIP = func(context.Context, string) ([]netip.Addr, error) {
				return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
			}
			f.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
			}
			dir := t.TempDir()
			result := f.Archive(t.Context(), st, dir, id, `<img src="http://images.example/photo">`)
			require.Equal(tc.wantDownloaded, result.Downloaded)
			refs, err := st.MessageRemoteImages(id)
			require.NoError(err)
			require.Len(refs, tc.wantDownloaded)
			for _, ref := range refs {
				assert.Equal("image/jpeg", ref.MimeType)
				assert.Equal(".jpg", filepath.Ext(ref.Filename))
				body, err := os.ReadFile(filepath.Join(dir, ref.StoragePath))
				require.NoError(err)
				assert.Equal(tc.body, body)
			}
		})
	}
}
