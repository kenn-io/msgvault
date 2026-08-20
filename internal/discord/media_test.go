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
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	internalexport "go.kenn.io/msgvault/internal/export"
	internalmime "go.kenn.io/msgvault/internal/mime"
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
	sourceID  int64
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
	return mediaFixture{store: st, sourceID: source.ID, messageID: messageID, dir: t.TempDir()}
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
	assert := assert.New(t)
	require := require.New(t)
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
	require.NoError(err)
	require.Len(result.Items, 1)
	assert.Equal(MediaDownloaded, result.Items[0].Outcome)
	require.NoError(result.Items[0].Err)

	requestObservation := <-observed
	assert.Empty(requestObservation.authorization, "CDN requests must never receive the bot token")
	assert.True(requestObservation.markerSeen, "pending metadata must exist before binary work starts")

	refs, err := f.store.MessageDiscordAttachments(f.messageID)
	require.NoError(err)
	ref := refs["discord:"+mediaTestAttachmentID]
	assert.NotEmpty(ref.ContentHash)
	assert.Equal(len(content), ref.Size)
	assert.Equal("image", ref.MediaType)
	assert.Equal(store.AttachmentRoleStandalone, ref.Role)
	assert.Equal(store.AttachmentRoleSourceProviderExplicit, ref.RoleSource)
	assert.EqualValues(640, ref.Width)
	assert.EqualValues(480, ref.Height)
	stored, err := os.ReadFile(filepath.Join(f.dir, filepath.FromSlash(ref.StoragePath)))
	require.NoError(err)
	assert.Equal(content, stored)
}

func TestMediaArchiverPolicySkipsWithoutFetch(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newMediaFixture(t)
	var requests atomic.Int64
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte("must not download"))
	}))
	defer cdn.Close()

	archiver := newTestArchiver(t, f, nil, 1<<20, cdn)
	archiver.SetPolicy(
		attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeDirect, MaxBytes: 1 << 20},
		attachmentpolicy.Conversation{Type: "channel", ParticipantCount: 20},
	)
	result, err := archiver.PersistAttachments(context.Background(), f.messageID, []Attachment{
		testDiscordAttachment(cdn.URL+"/attachments/301/401/policy.png", 5),
	})
	require.NoError(err)
	require.Len(result.Items, 1)
	assert.Equal(MediaSkipped, result.Items[0].Outcome)
	assert.Zero(requests.Load())

	refs, err := f.store.MessageDiscordAttachments(f.messageID)
	require.NoError(err)
	ref := refs["discord:"+mediaTestAttachmentID]
	assert.Equal(attachmentpolicy.StateSkipped, ref.State)
	assert.Equal(attachmentpolicy.SkipPolicyScope, ref.SkipReason)
}

func TestMediaArchiverReuseKeepsStoredState(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newMediaFixture(t)
	content := []byte("stored once")
	var requests atomic.Int32
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write(content)
	}))
	defer cdn.Close()
	rawURL := cdn.URL + "/attachments/301/401/stored.bin"
	archiver := newTestArchiver(t, f, nil, 1<<20, cdn)

	_, err := archiver.PersistAttachments(t.Context(), f.messageID, []Attachment{
		testDiscordAttachment(rawURL, int64(len(content))),
	})
	require.NoError(err)
	_, err = archiver.PersistAttachments(t.Context(), f.messageID, []Attachment{
		testDiscordAttachment(rawURL, 1),
	})
	require.NoError(err)
	assert.EqualValues(1, requests.Load())

	refs, err := f.store.MessageDiscordAttachments(f.messageID)
	require.NoError(err)
	assert.Equal(attachmentpolicy.StateStored, refs["discord:"+mediaTestAttachmentID].State)
	assert.Equal(len(content), refs["discord:"+mediaTestAttachmentID].Size,
		"a smaller refreshed declaration must not erase the known stored size")
	candidates, err := f.store.ListAttachmentPolicyCandidates(t.Context())
	require.NoError(err)
	assert.Len(candidates, 1, "reused stored media must remain eligible for policy purge")
}

func TestMediaArchiverStoresEphemeralAttachment(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newMediaFixture(t)
	content := []byte("synthetic ephemeral attachment")
	var requests atomic.Int32
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write(content)
	}))
	t.Cleanup(cdn.Close)
	rawURL := cdn.URL + "/ephemeral-attachments/301/401/archive%20image.png"
	attachment := testDiscordAttachment(rawURL, int64(len(content)))
	attachment.Ephemeral = true
	archiver := newTestArchiver(t, f, nil, 1<<20, cdn)

	result, err := archiver.PersistAttachments(t.Context(), f.messageID, []Attachment{attachment})
	require.NoError(err)
	require.Len(result.Items, 1)
	assert.Equal(MediaDownloaded, result.Items[0].Outcome)
	assert.EqualValues(1, requests.Load())
	refs, err := f.store.MessageDiscordAttachments(f.messageID)
	require.NoError(err)
	assert.NotEmpty(refs["discord:"+mediaTestAttachmentID].ContentHash)
}

func TestMediaArchiverRejectsEphemeralPathWithoutEphemeralFlag(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newMediaFixture(t)
	var requests atomic.Int32
	cdn := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	t.Cleanup(cdn.Close)
	rawURL := cdn.URL + "/ephemeral-attachments/301/401/archive%20image.png"
	archiver := newTestArchiver(t, f, nil, 1<<20, cdn)

	result, err := archiver.PersistAttachments(
		t.Context(), f.messageID, []Attachment{testDiscordAttachment(rawURL, 1)},
	)
	require.NoError(err)
	require.Len(result.Items, 1)
	assert.Equal(MediaPending, result.Items[0].Outcome)
	require.ErrorIs(result.Items[0].Err, ErrInvalidMediaURL)
	assert.Zero(requests.Load())
	requirePendingDiscordAttachment(t, f, rawURL)
}

func TestMediaArchiverPreservesDuplicateContentAttachmentIDs(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newMediaFixture(t)
	content := []byte("identical attachment bytes")
	var requests atomic.Int32
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write(content)
	}))
	defer cdn.Close()
	attachments := []Attachment{
		{
			ID: "401", Filename: "first.bin", ContentType: "application/octet-stream",
			Size: int64(len(content)), URL: cdn.URL + "/attachments/301/401/first.bin",
		},
		{
			ID: "402", Filename: "second.bin", ContentType: "application/x-second",
			Size: int64(len(content)), URL: cdn.URL + "/attachments/301/402/second.bin",
		},
	}
	archiver := newTestArchiver(t, f, nil, 1<<20, cdn)

	result, err := archiver.PersistAttachments(context.Background(), f.messageID, attachments)
	require.NoError(err)
	require.Len(result.Items, 2)
	assert.Equal(MediaDownloaded, result.Items[0].Outcome)
	assert.Equal(MediaDownloaded, result.Items[1].Outcome)
	assert.EqualValues(2, requests.Load())

	refs, err := f.store.MessageDiscordAttachments(f.messageID)
	require.NoError(err)
	require.Len(refs, 2)
	first := refs["discord:401"]
	second := refs["discord:402"]
	assert.Equal("first.bin", first.Filename)
	assert.Equal("second.bin", second.Filename)
	assert.Equal(first.StoragePath, second.StoragePath)
	assert.NotEmpty(first.StoragePath)
	assert.Len(first.ContentHash, 64)
	assert.Equal(first.ContentHash, second.ContentHash,
		"store reads derive the hash for a duplicate CAS alias")

	pending, err := f.store.ListDiscordPendingAttachmentMessages(f.sourceID)
	require.NoError(err)
	assert.Empty(pending)

	requests.Store(0)
	result, err = archiver.PersistAttachments(context.Background(), f.messageID, attachments)
	require.NoError(err)
	require.Len(result.Items, 2)
	assert.Equal(MediaDownloaded, result.Items[0].Outcome)
	assert.Equal(MediaDownloaded, result.Items[1].Outcome)
	assert.Zero(requests.Load(), "repersist must recognize both local CAS references")

	result, err = archiver.BackfillMessage(context.Background(), f.messageID, mediaTestChannelID, mediaTestMessageID)
	require.NoError(err)
	assert.Empty(result.Items, "backfill must not revisit a local hashless CAS alias")
	refs, err = f.store.MessageDiscordAttachments(f.messageID)
	require.NoError(err)
	assert.Len(refs, 2)
}

func TestMediaArchiverRepeatedPayloadRefreshesSetWithoutRetryingKnownPendingRows(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newMediaFixture(t)
	cdn := httptest.NewServer(nil)
	t.Cleanup(cdn.Close)
	archiver := newTestArchiver(t, f, nil, 1<<20, cdn)

	first := []Attachment{{ID: "401", Filename: "old.bin", Size: 1}}
	result, err := archiver.persistAttachments(context.Background(), f.messageID, first, true)
	require.NoError(err)
	require.Len(result.Items, 1)
	assert.Equal(MediaPending, result.Items[0].Outcome)

	newer := []Attachment{
		{ID: "401", Filename: "renamed.bin", Size: 2},
		{ID: "402", Filename: "new.bin", Size: 3},
	}
	result, err = archiver.persistAttachments(context.Background(), f.messageID, newer, false)
	require.NoError(err)
	require.Len(result.Items, 1, "only the newly observed attachment is attempted and reported")
	assert.Equal("discord:402", result.Items[0].SourceAttachmentID)

	refs, err := f.store.MessageDiscordAttachments(f.messageID)
	require.NoError(err)
	require.Len(refs, 2)
	assert.Equal("renamed.bin", refs["discord:401"].Filename)
	assert.Equal(2, refs["discord:401"].Size)
	assert.Equal("new.bin", refs["discord:402"].Filename)
}

func TestMediaArchiverRepeatedPayloadPreservesSuppressedOutcome(t *testing.T) {
	tests := []struct {
		name       string
		state      attachmentpolicy.DownloadState
		skipReason attachmentpolicy.SkipReason
	}{
		{name: "skipped", state: attachmentpolicy.StateSkipped, skipReason: attachmentpolicy.SkipSizeCap},
		{name: "failed", state: attachmentpolicy.StateFailed, skipReason: attachmentpolicy.SkipFetchFailure},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := newMediaFixture(t)
			ref := pendingDiscordRef("")
			ref.State = tt.state
			ref.SkipReason = tt.skipReason
			require.NoError(f.store.ReplaceMessageDiscordAttachments(f.messageID, []store.AttachmentRef{ref}))
			cdn := httptest.NewServer(nil)
			t.Cleanup(cdn.Close)
			archiver := newTestArchiver(t, f, nil, 1<<20, cdn)

			result, err := archiver.persistAttachments(
				context.Background(), f.messageID, []Attachment{testDiscordAttachment("", 0)}, false,
			)
			require.NoError(err)
			assert.Empty(result.Items)
			refs, err := f.store.MessageDiscordAttachments(f.messageID)
			require.NoError(err)
			assert.Equal(tt.state, refs["discord:"+mediaTestAttachmentID].State)
			assert.Equal(tt.skipReason, refs["discord:"+mediaTestAttachmentID].SkipReason)
		})
	}
}

func TestMediaArchiverPersistsPendingMetadataForEmptyURL(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newMediaFixture(t)
	var requests atomic.Int32
	cdn := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer cdn.Close()
	archiver := newTestArchiver(t, f, nil, 1<<20, cdn)
	attachment := testDiscordAttachment("", 42)
	attachment.Filename = "unavailable.bin"
	attachment.ContentType = "application/octet-stream"

	result, err := archiver.PersistAttachments(context.Background(), f.messageID, []Attachment{attachment})
	require.NoError(err)
	require.Len(result.Items, 1)
	assert.Equal(MediaPending, result.Items[0].Outcome)
	require.ErrorIs(result.Items[0].Err, ErrInvalidMediaURL)
	assert.Zero(requests.Load())

	refs, err := f.store.MessageDiscordAttachments(f.messageID)
	require.NoError(err)
	assert.Equal(store.AttachmentRef{
		Filename: "unavailable.bin", MimeType: "application/octet-stream", Size: 42,
		StoragePath: "discord:pending:401", SourceAttachmentID: "discord:401", MediaType: "document",
		Width: 640, Height: 480,
		Role:          store.AttachmentRoleUnknown,
		RoleSource:    store.AttachmentRoleSourceUnknown,
		SourcePartKey: "discord:401",
		State:         attachmentpolicy.StateFailed,
		SkipReason:    attachmentpolicy.SkipFetchFailure,
	}, refs["discord:401"])
	pending, err := f.store.ListDiscordPendingAttachmentMessages(f.sourceID)
	require.NoError(err)
	assert.Equal([]store.DiscordPendingAttachmentMessage{{
		MessageID: f.messageID, SourceMessageID: mediaTestMessageID, ChatID: mediaTestChannelID,
	}}, pending)
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
			assert := assert.New(t)
			require := require.New(t)
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
			require.NoError(err)
			require.Len(result.Items, 1)
			assert.Equal(MediaSkipped, result.Items[0].Outcome)
			require.ErrorIs(result.Items[0].Err, ErrMediaTooLarge)
			assert.Equal(tt.wantRequests, requests.Load())
			requirePendingDiscordAttachment(t, f, rawURL)
			refs, err := f.store.MessageDiscordAttachments(f.messageID)
			require.NoError(err)
			assert.Greater(refs["discord:"+mediaTestAttachmentID].Size, 10,
				"stream-detected oversize must remain excluded under the unchanged cap")
		})
	}
}

func TestMediaBackfillPreservesKnownSizeSkipAndChecksRefreshedSize(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newMediaFixture(t)
	knownID := "discord:known-large"
	retryID := "discord:retry"
	require.NoError(f.store.ReplaceMessageDiscordAttachments(f.messageID, []store.AttachmentRef{
		{SourceAttachmentID: knownID, StoragePath: "old-known", Size: 11,
			State: attachmentpolicy.StateSkipped, SkipReason: attachmentpolicy.SkipSizeCap},
		{SourceAttachmentID: retryID, StoragePath: "old-retry", State: attachmentpolicy.StatePending},
	}))
	var cdnRequests atomic.Int32
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cdnRequests.Add(1)
		_, _ = w.Write([]byte("must not download"))
	}))
	defer cdn.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeDiscordJSON(w, http.StatusOK, map[string]any{
			"id": mediaTestMessageID, "channel_id": mediaTestChannelID,
			"author": map[string]any{"id": "101"}, "timestamp": "2026-07-18T12:00:00Z",
			"attachments": []map[string]any{
				{"id": "known-large", "filename": "known.bin", "size": 11,
					"url": cdn.URL + "/attachments/301/known-large/known.bin"},
				{"id": "retry", "filename": "retry.bin", "size": 11,
					"url": cdn.URL + "/attachments/301/retry/retry.bin"},
			},
		})
	}))
	defer api.Close()
	client, err := NewClient(api.URL+"/api/v10", "synthetic-token")
	require.NoError(err)
	archiver := newTestArchiver(t, f, client, 10, cdn)
	archiver.SetPolicy(attachmentpolicy.Policy{MaxBytes: 10}, attachmentpolicy.Conversation{Type: "channel"})

	result, err := archiver.BackfillMessage(t.Context(), f.messageID, mediaTestChannelID, mediaTestMessageID)
	require.NoError(err)
	assert.Zero(cdnRequests.Load())
	require.Len(result.Items, 2)
	for _, item := range result.Items {
		assert.Equal(MediaSkipped, item.Outcome)
	}
	refs, err := f.store.MessageDiscordAttachments(f.messageID)
	require.NoError(err)
	assert.Equal(attachmentpolicy.StateSkipped, refs[knownID].State)
	assert.Equal(attachmentpolicy.SkipSizeCap, refs[knownID].SkipReason)
	assert.Equal(attachmentpolicy.StateSkipped, refs[retryID].State)
	assert.Equal(attachmentpolicy.SkipSizeCap, refs[retryID].SkipReason)
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
			assert := assert.New(t)
			require := require.New(t)
			f := newMediaFixture(t)
			cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte("download body"))
			}))
			defer cdn.Close()
			rawURL := cdn.URL + "/attachments/301/401/failure.bin?hm=do-not-return-this"
			if tt.breakStorage {
				blockedPath := filepath.Join(f.dir, "not-a-directory")
				require.NoError(os.WriteFile(blockedPath, []byte("file"), 0600))
				f.dir = blockedPath
			}

			archiver := newTestArchiver(t, f, nil, 1<<20, cdn)
			result, err := archiver.PersistAttachments(
				context.Background(), f.messageID, []Attachment{testDiscordAttachment(rawURL, 0)},
			)
			require.NoError(err, "binary failure must not invalidate the durable message boundary")
			require.Len(result.Items, 1)
			assert.Equal(MediaPending, result.Items[0].Outcome)
			require.ErrorIs(result.Items[0].Err, tt.wantErrorKind)
			assert.NotContains(result.Items[0].Err.Error(), "do-not-return-this")
			requirePendingDiscordAttachment(t, f, rawURL)
		})
	}
}

func TestMediaArchiverCancellationLeavesPendingMarker(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
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
	require.NoError(completed.err)
	result := completed.media
	require.Len(result.Items, 1)
	assert.Equal(MediaPending, result.Items[0].Outcome)
	require.ErrorIs(result.Items[0].Err, context.Canceled)
	assert.NotContains(result.Items[0].Err.Error(), "cancel-secret")
	requirePendingDiscordAttachment(t, f, rawURL)
}

func TestReadMediaFileHonorsCanceledContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bounded-download.bin")
	require.NoError(t, os.WriteFile(path, []byte("bytes that must not be published"), 0600))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	content, err := readMediaFile(ctx, path)
	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, content)
}

func TestMediaArchiverCancellationAfterCASLeavesMarkerPending(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newMediaFixture(t)
	content := []byte("CAS bytes may become an orphan")
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(content)
	}))
	defer cdn.Close()
	rawURL := cdn.URL + "/attachments/301/401/post-cas-cancel.bin?hm=cancel-after-cas"
	archiver := newTestArchiver(t, f, nil, 1<<20, cdn)
	ctx, cancel := context.WithCancel(context.Background())
	var orphanStoragePath string
	archiver.storeAttachmentFile = func(dir string, attachment *internalmime.Attachment) (string, error) {
		storagePath, err := internalexport.StoreAttachmentFile(dir, attachment)
		orphanStoragePath = storagePath
		cancel()
		return storagePath, err
	}

	result, err := archiver.PersistAttachments(
		ctx, f.messageID, []Attachment{testDiscordAttachment(rawURL, int64(len(content)))},
	)
	require.NoError(err)
	require.Len(result.Items, 1)
	assert.Equal(MediaPending, result.Items[0].Outcome)
	require.ErrorIs(result.Items[0].Err, context.Canceled)
	require.NotEmpty(orphanStoragePath, "test wrapper must exercise the real CAS")
	_, err = os.Stat(filepath.Join(f.dir, filepath.FromSlash(orphanStoragePath)))
	require.NoError(err, "an unpublished orphan CAS blob is acceptable")

	requirePendingDiscordAttachment(t, f, rawURL)
	refs, err := f.store.MessageDiscordAttachments(f.messageID)
	require.NoError(err)
	ref := refs["discord:"+mediaTestAttachmentID]
	assert.Empty(ref.ContentHash)
	assert.Equal(rawURL, ref.StoragePath)
	pending, err := f.store.ListDiscordPendingAttachmentMessages(f.sourceID)
	require.NoError(err)
	assert.Len(pending, 1)
}

func TestMediaArchiverRejectsUnapprovedOriginsAndRedirects(t *testing.T) {
	t.Run("production origin policy", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		f := newMediaFixture(t)
		archiver, err := NewMediaArchiver(f.store, nil, f.dir, 1<<20)
		require.NoError(err)
		rawURL := "https://example.invalid/attachments/301/401/private.bin?hm=origin-secret"
		result, err := archiver.PersistAttachments(
			context.Background(), f.messageID, []Attachment{testDiscordAttachment(rawURL, 0)},
		)
		require.NoError(err)
		require.Len(result.Items, 1)
		assert.Equal(MediaPending, result.Items[0].Outcome)
		require.ErrorIs(result.Items[0].Err, ErrInvalidMediaURL)
		assert.NotContains(result.Items[0].Err.Error(), "origin-secret")
		requirePendingDiscordAttachment(t, f, rawURL)
	})

	t.Run("redirect", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
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
		require.NoError(err)
		require.Len(result.Items, 1)
		assert.Equal(MediaPending, result.Items[0].Outcome)
		require.ErrorIs(result.Items[0].Err, ErrMediaRedirect)
		assert.NotContains(result.Items[0].Err.Error(), "original-secret")
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
			assert := assert.New(t)
			require := require.New(t)
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
			require.NoError(err)
			require.Len(result.Items, 1)
			assert.Equal(MediaPending, result.Items[0].Outcome)
			require.ErrorIs(result.Items[0].Err, ErrInvalidMediaURL)
			assert.Zero(requests.Load())
			requirePendingDiscordAttachment(t, f, rawURL)
		})
	}
}

func TestMediaBackfillRefreshesSignedURLThroughMessageEndpoint(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
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
	require.NoError(f.store.ReplaceMessageDiscordAttachments(f.messageID, []store.AttachmentRef{pendingDiscordRef(oldURL)}))

	var apiRequests atomic.Int32
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiRequests.Add(1)
		assert.Equal("/api/v10/channels/301/messages/501", r.URL.Path)
		assert.Equal("Bot synthetic-token", r.Header.Get("Authorization"))
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
	require.NoError(err)
	archiver := newTestArchiver(t, f, client, 1<<20, cdn)

	result, err := archiver.BackfillMessage(context.Background(), f.messageID, mediaTestChannelID, mediaTestMessageID)
	require.NoError(err)
	require.Len(result.Items, 1)
	assert.Equal(MediaDownloaded, result.Items[0].Outcome)
	assert.EqualValues(1, apiRequests.Load())
	assert.Empty(cdnAuthorization.Load())
	assert.True(refreshedMarkerSeen.Load(), "fresh signed provenance must be durable before its download")

	refs, err := f.store.MessageDiscordAttachments(f.messageID)
	require.NoError(err)
	ref := refs["discord:"+mediaTestAttachmentID]
	assert.NotEmpty(ref.ContentHash)
	assert.NotEqual(oldURL, ref.StoragePath)
	stored, err := os.ReadFile(filepath.Join(f.dir, filepath.FromSlash(ref.StoragePath)))
	require.NoError(err)
	assert.Equal(content, stored)
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
			assert := assert.New(t)
			require := require.New(t)
			f := newMediaFixture(t)
			oldURL := "https://cdn.discordapp.com/attachments/301/401/old.bin?hm=stored-secret"
			require.NoError(f.store.ReplaceMessageDiscordAttachments(f.messageID, []store.AttachmentRef{pendingDiscordRef(oldURL)}))
			apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeDiscordJSON(w, tt.status, tt.body)
			}))
			defer apiServer.Close()
			client, err := NewClient(apiServer.URL+"/api/v10", "synthetic-token")
			require.NoError(err)
			archiver, err := NewMediaArchiver(f.store, client, f.dir, 1<<20)
			require.NoError(err)

			result, err := archiver.BackfillMessage(context.Background(), f.messageID, mediaTestChannelID, mediaTestMessageID)
			require.NoError(err)
			require.Len(result.Items, 1)
			assert.Equal(MediaUnrecoverable, result.Items[0].Outcome)
			require.ErrorIs(result.Items[0].Err, ErrMediaUnrecoverable)
			assert.NotContains(result.Items[0].Err.Error(), "must-not-leak")
			assert.NotContains(result.Items[0].Err.Error(), "stored-secret")
			requirePendingDiscordAttachment(t, f, oldURL)
		})
	}
}

func TestMediaBackfillPreservesPendingMarkerOnRefreshFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newMediaFixture(t)
	oldURL := "https://cdn.discordapp.com/attachments/301/401/old.bin?hm=stored-secret"
	require.NoError(f.store.ReplaceMessageDiscordAttachments(f.messageID, []store.AttachmentRef{pendingDiscordRef(oldURL)}))
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeDiscordJSON(w, http.StatusForbidden, map[string]any{
			"code": 50013, "message": "Missing Permissions hm=api-secret",
		})
	}))
	defer apiServer.Close()
	client, err := NewClient(apiServer.URL+"/api/v10", "synthetic-token")
	require.NoError(err)
	archiver, err := NewMediaArchiver(f.store, client, f.dir, 1<<20)
	require.NoError(err)

	result, err := archiver.BackfillMessage(context.Background(), f.messageID, mediaTestChannelID, mediaTestMessageID)
	require.NoError(err)
	require.Len(result.Items, 1)
	assert.Equal(MediaPending, result.Items[0].Outcome)
	require.ErrorIs(result.Items[0].Err, ErrMediaRefresh)
	assert.NotContains(result.Items[0].Err.Error(), "api-secret")
	assert.NotContains(result.Items[0].Err.Error(), "stored-secret")
	requirePendingDiscordAttachment(t, f, oldURL)
}

func TestMediaArchiverDefaultsCapFromConfiguration(t *testing.T) {
	f := newMediaFixture(t)
	archiver, err := NewMediaArchiver(f.store, nil, f.dir, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(50<<20), archiver.maxBytes)
}
