package discord

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

const (
	mediaTestChannelID    = "301"
	mediaTestMessageID    = "501"
	mediaTestAttachmentID = "401"
)

type mediaFixture struct {
	store     *store.Store
	messageID int64
	dir       string
}

func newMediaFixture(t *testing.T) mediaFixture {
	t.Helper()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("discord", "201")
	require.NoError(t, err)
	conversationID, err := st.EnsureConversationWithType(
		source.ID, mediaTestChannelID, "channel", "synthetic channel",
	)
	require.NoError(t, err)
	messageID, err := st.UpsertMessage(&store.Message{
		SourceID:        source.ID,
		ConversationID:  conversationID,
		SourceMessageID: mediaTestMessageID,
		MessageType:     "discord",
		SentAt: sql.NullTime{
			Time:  time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC),
			Valid: true,
		},
	})
	require.NoError(t, err)
	return mediaFixture{store: st, messageID: messageID, dir: t.TempDir()}
}

func testDiscordAttachment(rawURL string, size int64) Attachment {
	width, height := 640, 480
	return Attachment{
		ID:          mediaTestAttachmentID,
		Filename:    "archive image.png",
		ContentType: "image/png",
		Size:        size,
		URL:         rawURL,
		Width:       &width,
		Height:      &height,
	}
}

func pendingDiscordRef(rawURL string) store.AttachmentRef {
	return mapAttachments([]Attachment{testDiscordAttachment(rawURL, 0)})[0]
}

func requirePendingDiscordAttachment(t *testing.T, f mediaFixture, rawURL string) {
	t.Helper()
	refs, err := f.store.MessageDiscordAttachments(f.messageID)
	require.NoError(t, err)
	require.Contains(t, refs, "discord:"+mediaTestAttachmentID)
	ref := refs["discord:"+mediaTestAttachmentID]
	assert.Empty(t, ref.ContentHash)
	assert.Equal(t, rawURL, ref.StoragePath)
}

func newTestArchiver(t *testing.T, f mediaFixture, api API, maxBytes int64, cdn *httptest.Server) *MediaArchiver {
	t.Helper()
	archiver, err := newTestMediaArchiver(f.store, api, f.dir, maxBytes, cdn.URL)
	require.NoError(t, err)
	return archiver
}

func TestMediaArchiverStoresAttachmentAfterDurableMarker(t *testing.T) {
	f := newMediaFixture(t)
	content := []byte("synthetic attachment bytes")
	type observation struct {
		authorization string
		markerSeen    bool
	}
	observed := make(chan observation, 1)
	var rawURL string
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refs, err := f.store.MessageDiscordAttachments(f.messageID)
		marker := refs["discord:"+mediaTestAttachmentID]
		observed <- observation{
			authorization: r.Header.Get("Authorization"),
			markerSeen:    err == nil && marker.ContentHash == "" && marker.StoragePath == rawURL,
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		_, _ = w.Write(content)
	}))
	defer cdn.Close()

	rawURL = cdn.URL + "/attachments/301/401/archive%20image.png?ex=123&is=456&hm=secret-signature"
	archiver := newTestArchiver(t, f, nil, 1<<20, cdn)
	result, err := archiver.PersistAttachments(
		context.Background(), f.messageID, []Attachment{testDiscordAttachment(rawURL, int64(len(content)))},
	)
	require.NoError(t, err)
	require.Len(t, result.Items, 1)
	assert.Equal(t, MediaDownloaded, result.Items[0].Outcome)
	require.NoError(t, result.Items[0].Err)

	requestObservation := <-observed
	assert.Empty(t, requestObservation.authorization, "CDN requests must never receive the bot token")
	assert.True(t, requestObservation.markerSeen, "pending metadata must exist before binary work starts")

	refs, err := f.store.MessageDiscordAttachments(f.messageID)
	require.NoError(t, err)
	ref := refs["discord:"+mediaTestAttachmentID]
	assert.NotEmpty(t, ref.ContentHash)
	assert.Equal(t, len(content), ref.Size)
	assert.Equal(t, "image", ref.MediaType)
	assert.EqualValues(t, 640, ref.Width)
	assert.EqualValues(t, 480, ref.Height)
	stored, err := os.ReadFile(filepath.Join(f.dir, filepath.FromSlash(ref.StoragePath)))
	require.NoError(t, err)
	assert.Equal(t, content, stored)
}

func TestMediaArchiverEnforcesSizeCapBeforeAndDuringStreaming(t *testing.T) {
	tests := []struct {
		name           string
		attachmentSize int64
		serve          func(http.ResponseWriter)
		wantRequests   int32
	}{
		{
			name:           "API declared size",
			attachmentSize: 11,
			serve: func(w http.ResponseWriter) {
				_, _ = w.Write([]byte("not requested"))
			},
			wantRequests: 0,
		},
		{
			name: "HTTP content length",
			serve: func(w http.ResponseWriter) {
				w.Header().Set("Content-Length", "11")
				_, _ = w.Write([]byte("12345678901"))
			},
			wantRequests: 1,
		},
		{
			name: "stream without content length",
			serve: func(w http.ResponseWriter) {
				flusher, ok := w.(http.Flusher)
				if ok {
					flusher.Flush()
				}
				_, _ = w.Write([]byte("12345678901"))
			},
			wantRequests: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newMediaFixture(t)
			var requests atomic.Int32
			cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				tt.serve(w)
			}))
			defer cdn.Close()

			rawURL := cdn.URL + "/attachments/301/401/capped.bin?hm=size-secret"
			archiver := newTestArchiver(t, f, nil, 10, cdn)
			result, err := archiver.PersistAttachments(
				context.Background(), f.messageID, []Attachment{testDiscordAttachment(rawURL, tt.attachmentSize)},
			)
			require.NoError(t, err)
			require.Len(t, result.Items, 1)
			assert.Equal(t, MediaPending, result.Items[0].Outcome)
			require.ErrorIs(t, result.Items[0].Err, ErrMediaTooLarge)
			assert.Equal(t, tt.wantRequests, requests.Load())
			requirePendingDiscordAttachment(t, f, rawURL)
		})
	}
}

func TestMediaArchiverPreservesPendingMarkerOnHTTPAndStorageFailures(t *testing.T) {
	tests := []struct {
		name          string
		status        int
		breakStorage  bool
		wantErrorKind error
	}{
		{name: "CDN HTTP failure", status: http.StatusForbidden, wantErrorKind: ErrMediaDownload},
		{name: "attachment store failure", status: http.StatusOK, breakStorage: true, wantErrorKind: ErrMediaStorage},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newMediaFixture(t)
			cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte("download body"))
			}))
			defer cdn.Close()
			rawURL := cdn.URL + "/attachments/301/401/failure.bin?hm=do-not-return-this"
			if tt.breakStorage {
				blockedPath := filepath.Join(f.dir, "not-a-directory")
				require.NoError(t, os.WriteFile(blockedPath, []byte("file"), 0600))
				f.dir = blockedPath
			}

			archiver := newTestArchiver(t, f, nil, 1<<20, cdn)
			result, err := archiver.PersistAttachments(
				context.Background(), f.messageID, []Attachment{testDiscordAttachment(rawURL, 0)},
			)
			require.NoError(t, err, "binary failure must not invalidate the durable message boundary")
			require.Len(t, result.Items, 1)
			assert.Equal(t, MediaPending, result.Items[0].Outcome)
			require.ErrorIs(t, result.Items[0].Err, tt.wantErrorKind)
			assert.NotContains(t, result.Items[0].Err.Error(), "do-not-return-this")
			requirePendingDiscordAttachment(t, f, rawURL)
		})
	}
}

func TestMediaArchiverCancellationLeavesPendingMarker(t *testing.T) {
	f := newMediaFixture(t)
	started := make(chan struct{})
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if ok {
			flusher.Flush()
		}
		close(started)
		<-r.Context().Done()
	}))
	defer cdn.Close()
	rawURL := cdn.URL + "/attachments/301/401/cancel.bin?hm=cancel-secret"
	archiver := newTestArchiver(t, f, nil, 1<<20, cdn)

	ctx, cancel := context.WithCancel(context.Background())
	type persistResult struct {
		media MediaResult
		err   error
	}
	done := make(chan persistResult, 1)
	go func() {
		result, err := archiver.PersistAttachments(ctx, f.messageID, []Attachment{testDiscordAttachment(rawURL, 0)})
		done <- persistResult{media: result, err: err}
	}()
	<-started
	cancel()
	completed := <-done
	require.NoError(t, completed.err)
	result := completed.media
	require.Len(t, result.Items, 1)
	assert.Equal(t, MediaPending, result.Items[0].Outcome)
	require.ErrorIs(t, result.Items[0].Err, context.Canceled)
	assert.NotContains(t, result.Items[0].Err.Error(), "cancel-secret")
	requirePendingDiscordAttachment(t, f, rawURL)
}

func TestMediaArchiverRejectsUnapprovedOriginsAndRedirects(t *testing.T) {
	t.Run("production origin policy", func(t *testing.T) {
		f := newMediaFixture(t)
		archiver, err := NewMediaArchiver(f.store, nil, f.dir, 1<<20)
		require.NoError(t, err)
		rawURL := "https://example.invalid/attachments/301/401/private.bin?hm=origin-secret"
		result, err := archiver.PersistAttachments(
			context.Background(), f.messageID, []Attachment{testDiscordAttachment(rawURL, 0)},
		)
		require.NoError(t, err)
		require.Len(t, result.Items, 1)
		assert.Equal(t, MediaPending, result.Items[0].Outcome)
		require.ErrorIs(t, result.Items[0].Err, ErrInvalidMediaURL)
		assert.NotContains(t, result.Items[0].Err.Error(), "origin-secret")
		requirePendingDiscordAttachment(t, f, rawURL)
	})

	t.Run("redirect", func(t *testing.T) {
		f := newMediaFixture(t)
		cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/attachments/301/401/redirected.bin?hm=redirected-secret", http.StatusFound)
		}))
		defer cdn.Close()
		rawURL := cdn.URL + "/attachments/301/401/original.bin?hm=original-secret"
		archiver := newTestArchiver(t, f, nil, 1<<20, cdn)
		result, err := archiver.PersistAttachments(
			context.Background(), f.messageID, []Attachment{testDiscordAttachment(rawURL, 0)},
		)
		require.NoError(t, err)
		require.Len(t, result.Items, 1)
		assert.Equal(t, MediaPending, result.Items[0].Outcome)
		require.ErrorIs(t, result.Items[0].Err, ErrMediaRedirect)
		assert.NotContains(t, result.Items[0].Err.Error(), "original-secret")
		requirePendingDiscordAttachment(t, f, rawURL)
	})
}

func TestMediaArchiverRejectsMalformedAttachmentPathsBeforeRequest(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{name: "encoded traversal filename", path: "/attachments/301/401/%2e%2e"},
		{name: "path ID differs from stable attachment ID", path: "/attachments/301/999/file.bin"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newMediaFixture(t)
			var requests atomic.Int32
			cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				_, _ = w.Write([]byte("must not be requested"))
			}))
			defer cdn.Close()
			rawURL := cdn.URL + tt.path + "?hm=path-secret"
			archiver := newTestArchiver(t, f, nil, 1<<20, cdn)

			result, err := archiver.PersistAttachments(
				context.Background(), f.messageID, []Attachment{testDiscordAttachment(rawURL, 0)},
			)
			require.NoError(t, err)
			require.Len(t, result.Items, 1)
			assert.Equal(t, MediaPending, result.Items[0].Outcome)
			require.ErrorIs(t, result.Items[0].Err, ErrInvalidMediaURL)
			assert.Zero(t, requests.Load())
			requirePendingDiscordAttachment(t, f, rawURL)
		})
	}
}

func TestMediaBackfillRefreshesSignedURLThroughMessageEndpoint(t *testing.T) {
	f := newMediaFixture(t)
	content := []byte("fresh signed URL bytes")
	var cdnAuthorization atomic.Value
	var refreshedMarkerSeen atomic.Bool
	var freshURL string
	cdnAuthorization.Store("")
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cdnAuthorization.Store(r.Header.Get("Authorization"))
		refs, err := f.store.MessageDiscordAttachments(f.messageID)
		if err == nil && refs["discord:"+mediaTestAttachmentID].StoragePath == freshURL {
			refreshedMarkerSeen.Store(true)
		}
		_, _ = w.Write(content)
	}))
	defer cdn.Close()
	freshURL = cdn.URL + "/attachments/301/401/fresh.bin?ex=new&hm=fresh-secret"
	oldURL := cdn.URL + "/attachments/301/401/old.bin?ex=old&hm=old-secret"
	require.NoError(t, f.store.ReplaceMessageDiscordAttachments(f.messageID, []store.AttachmentRef{pendingDiscordRef(oldURL)}))

	var apiRequests atomic.Int32
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiRequests.Add(1)
		assert.Equal(t, "/api/v10/channels/301/messages/501", r.URL.Path)
		assert.Equal(t, "Bot synthetic-token", r.Header.Get("Authorization"))
		writeDiscordJSON(w, http.StatusOK, map[string]any{
			"id": mediaTestMessageID, "channel_id": mediaTestChannelID,
			"author": map[string]any{"id": "101"}, "timestamp": "2026-07-18T12:00:00Z",
			"attachments": []map[string]any{{
				"id": mediaTestAttachmentID, "filename": "fresh.bin", "content_type": "application/octet-stream",
				"size": len(content), "url": freshURL,
			}},
		})
	}))
	defer apiServer.Close()
	client, err := NewClient(apiServer.URL+"/api/v10", "synthetic-token")
	require.NoError(t, err)
	archiver := newTestArchiver(t, f, client, 1<<20, cdn)

	result, err := archiver.BackfillMessage(context.Background(), f.messageID, mediaTestChannelID, mediaTestMessageID)
	require.NoError(t, err)
	require.Len(t, result.Items, 1)
	assert.Equal(t, MediaDownloaded, result.Items[0].Outcome)
	assert.EqualValues(t, 1, apiRequests.Load())
	assert.Empty(t, cdnAuthorization.Load())
	assert.True(t, refreshedMarkerSeen.Load(), "fresh signed provenance must be durable before its download")

	refs, err := f.store.MessageDiscordAttachments(f.messageID)
	require.NoError(t, err)
	ref := refs["discord:"+mediaTestAttachmentID]
	assert.NotEmpty(t, ref.ContentHash)
	assert.NotEqual(t, oldURL, ref.StoragePath)
	stored, err := os.ReadFile(filepath.Join(f.dir, filepath.FromSlash(ref.StoragePath)))
	require.NoError(t, err)
	assert.Equal(t, content, stored)
}

func TestMediaBackfillReturnsUnrecoverableOutcomeWhenSourceCannotRefresh(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   any
	}{
		{
			name: "source message deleted", status: http.StatusNotFound,
			body: map[string]any{"code": 10008, "message": "Unknown Message with signed URL hm=must-not-leak"},
		},
		{
			name: "attachment no longer present", status: http.StatusOK,
			body: map[string]any{
				"id": mediaTestMessageID, "channel_id": mediaTestChannelID,
				"author": map[string]any{"id": "101"}, "timestamp": "2026-07-18T12:00:00Z",
				"attachments": []any{},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newMediaFixture(t)
			oldURL := "https://cdn.discordapp.com/attachments/301/401/old.bin?hm=stored-secret"
			require.NoError(t, f.store.ReplaceMessageDiscordAttachments(f.messageID, []store.AttachmentRef{pendingDiscordRef(oldURL)}))
			apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeDiscordJSON(w, tt.status, tt.body)
			}))
			defer apiServer.Close()
			client, err := NewClient(apiServer.URL+"/api/v10", "synthetic-token")
			require.NoError(t, err)
			archiver, err := NewMediaArchiver(f.store, client, f.dir, 1<<20)
			require.NoError(t, err)

			result, err := archiver.BackfillMessage(context.Background(), f.messageID, mediaTestChannelID, mediaTestMessageID)
			require.NoError(t, err)
			require.Len(t, result.Items, 1)
			assert.Equal(t, MediaUnrecoverable, result.Items[0].Outcome)
			require.ErrorIs(t, result.Items[0].Err, ErrMediaUnrecoverable)
			assert.NotContains(t, result.Items[0].Err.Error(), "must-not-leak")
			assert.NotContains(t, result.Items[0].Err.Error(), "stored-secret")
			requirePendingDiscordAttachment(t, f, oldURL)
		})
	}
}

func TestMediaBackfillPreservesPendingMarkerOnRefreshFailure(t *testing.T) {
	f := newMediaFixture(t)
	oldURL := "https://cdn.discordapp.com/attachments/301/401/old.bin?hm=stored-secret"
	require.NoError(t, f.store.ReplaceMessageDiscordAttachments(f.messageID, []store.AttachmentRef{pendingDiscordRef(oldURL)}))
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeDiscordJSON(w, http.StatusForbidden, map[string]any{
			"code": 50013, "message": "Missing Permissions hm=api-secret",
		})
	}))
	defer apiServer.Close()
	client, err := NewClient(apiServer.URL+"/api/v10", "synthetic-token")
	require.NoError(t, err)
	archiver, err := NewMediaArchiver(f.store, client, f.dir, 1<<20)
	require.NoError(t, err)

	result, err := archiver.BackfillMessage(context.Background(), f.messageID, mediaTestChannelID, mediaTestMessageID)
	require.NoError(t, err)
	require.Len(t, result.Items, 1)
	assert.Equal(t, MediaPending, result.Items[0].Outcome)
	require.ErrorIs(t, result.Items[0].Err, ErrMediaRefresh)
	assert.NotContains(t, result.Items[0].Err.Error(), "api-secret")
	assert.NotContains(t, result.Items[0].Err.Error(), "stored-secret")
	requirePendingDiscordAttachment(t, f, oldURL)
}

func TestMediaArchiverDefaultsCapFromConfiguration(t *testing.T) {
	f := newMediaFixture(t)
	archiver, err := NewMediaArchiver(f.store, nil, f.dir, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(50<<20), archiver.maxBytes)
}
