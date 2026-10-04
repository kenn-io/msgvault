package chatwoot

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type mediaRefreshServer struct {
	mu       sync.Mutex
	server   *httptest.Server
	failures map[string]bool
	requests map[string]int
}

func newMediaRefreshServer(t *testing.T) *mediaRefreshServer {
	t.Helper()
	media := &mediaRefreshServer{failures: map[string]bool{}, requests: map[string]int{}}
	media.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		media.mu.Lock()
		defer media.mu.Unlock()
		assert.Empty(t, r.Header.Get("Api_access_token"))
		assert.Empty(t, r.Header.Get("Authorization"))
		media.requests[r.URL.Path]++
		if media.failures[r.URL.Path] {
			http.Error(w, "synthetic temporary media failure", http.StatusServiceUnavailable)
			return
		}
		payloads := map[string]string{
			"/recording-a.ogg": "synthetic recording A bytes",
			"/recording-b.ogg": "synthetic replacement recording B bytes",
		}
		payload, ok := payloads[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "audio/ogg")
		_, err := w.Write([]byte(payload))
		assert.NoError(t, err)
	}))
	t.Cleanup(media.server.Close)
	return media
}

func (media *mediaRefreshServer) requestCount(path string) int {
	media.mu.Lock()
	defer media.mu.Unlock()
	return media.requests[path]
}

func mediaRefreshCall(recordingURL, audioURL string) map[string]any {
	message := contractMessage(901, 1767225600, map[string]any{"id": int64(7), "type": "contact", "name": "Example Contact", "phone_number": "+12025550101"})
	message["content_type"] = "voice_call"
	message["content"] = "Synthetic voice call"
	message["call"] = map[string]any{
		"id": 1001, "provider_call_id": "CA_synthetic_refresh", "provider": "twilio", "direction": "incoming", "status": "completed",
		"duration_seconds": 30, "accepted_by_agent_id": 7, "accepted_by_agent_name": "Example Agent", "recording_url": recordingURL,
	}
	if audioURL != "" {
		message["attachments"] = []any{map[string]any{
			"id": 2001, "message_id": 901, "file_type": "audio", "content_type": "audio/ogg", "extension": "ogg", "data_url": audioURL,
		}}
	}
	return message
}

func mediaRefreshOptions(t *testing.T) ImportOptions {
	t.Helper()
	return ImportOptions{InboxID: 7, IncludePrivate: true, Media: true, MaxMediaBytes: 4096, AttachmentsDir: t.TempDir()}
}

func readMediaRefreshBytes(t *testing.T, st *store.Store, messageID int64, dir string) (map[string]store.AttachmentRef, []string) {
	t.Helper()
	refs, err := st.MessageChatwootAttachments(messageID)
	require.NoError(t, err)
	var payloads []string
	for _, ref := range refs {
		require.NotEmpty(t, ref.ContentHash, "a successful recording remains readable even while replacement retries")
		payload, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(ref.StoragePath)))
		require.NoError(t, err)
		payloads = append(payloads, string(payload))
	}
	return refs, payloads
}

func TestMediaRefreshCallKeepsDistinctRecordingsAndDeduplicatesSameURL(t *testing.T) {
	for _, tc := range []struct {
		name          string
		recordingPath string
		want          []string
		wantRequests  int
	}{
		{"distinct_recordings", "/recording-b.ogg", []string{"synthetic recording A bytes", "synthetic replacement recording B bytes"}, 2},
		{"same_recording", "/recording-a.ogg", []string{"synthetic recording A bytes"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			media := newMediaRefreshServer(t)
			router := newChatwootMediaRouter(t, media.server)
			message := mediaRefreshCall(router.url(t, media.server, tc.recordingPath), router.url(t, media.server, "/recording-a.ogg"))
			api := newContractAPI(t, 2, []map[string]any{message})
			api.mediaRouter = router
			st := testutil.NewTestStore(t)
			importer, _ := contractRegister(t, st, api)
			opts := mediaRefreshOptions(t)
			_, err := importer.Import(t.Context(), opts)
			require.NoError(err)
			assert.Equal(tc.wantRequests, media.requestCount("/recording-a.ogg")+media.requestCount("/recording-b.ogg"), "each unique audio URL should be fetched once across the chat and linked meeting")
			meetingID := contractArchivedMessageID(t, st, "call:901")
			refs, payloads := readMediaRefreshBytes(t, st, meetingID, opts.AttachmentsDir)
			assert.Len(refs, len(tc.want), "the message audio does not suppress a different live call recording")
			assert.ElementsMatch(tc.want, payloads)
		})
	}
}

func TestMediaRefreshChangedRecordingURLReplacesStableOccurrenceBytes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	media := newMediaRefreshServer(t)
	router := newChatwootMediaRouter(t, media.server)
	message := mediaRefreshCall(router.url(t, media.server, "/recording-a.ogg"), "")
	call, ok := message["call"].(map[string]any)
	require.True(ok)
	api := newContractAPI(t, 2, []map[string]any{message})
	api.mediaRouter = router
	st := testutil.NewTestStore(t)
	importer, _ := contractRegister(t, st, api)
	opts := mediaRefreshOptions(t)
	_, err := importer.Import(t.Context(), opts)
	require.NoError(err)
	meetingID := contractArchivedMessageID(t, st, "call:901")
	before, payloads := readMediaRefreshBytes(t, st, meetingID, opts.AttachmentsDir)
	require.Len(before, 1)
	assert.Equal([]string{"synthetic recording A bytes"}, payloads)

	api.mu.Lock()
	call["recording_url"] = router.url(t, media.server, "/recording-b.ogg")
	api.mu.Unlock()
	_, err = NewImporter(st, api.client(t)).Import(t.Context(), opts)
	require.NoError(err)
	after, payloads := readMediaRefreshBytes(t, st, meetingID, opts.AttachmentsDir)
	require.Len(after, 1)
	assert.Equal([]string{"synthetic replacement recording B bytes"}, payloads)
	for key, old := range before {
		ref, exists := after[key]
		require.True(exists, "a replacement keeps the provider occurrence identity")
		assert.NotEqual(old.ContentHash, ref.ContentHash)
	}
}

func TestMediaRefreshFailedReplacementRetainsBytesAndRetriesNextSync(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	media := newMediaRefreshServer(t)
	router := newChatwootMediaRouter(t, media.server)
	message := mediaRefreshCall(router.url(t, media.server, "/recording-a.ogg"), "")
	call, ok := message["call"].(map[string]any)
	require.True(ok)
	api := newContractAPI(t, 2, []map[string]any{message})
	api.mediaRouter = router
	st := testutil.NewTestStore(t)
	importer, _ := contractRegister(t, st, api)
	opts := mediaRefreshOptions(t)
	_, err := importer.Import(t.Context(), opts)
	require.NoError(err)
	meetingID := contractArchivedMessageID(t, st, "call:901")
	before, payloads := readMediaRefreshBytes(t, st, meetingID, opts.AttachmentsDir)
	require.Len(before, 1)
	assert.Equal([]string{"synthetic recording A bytes"}, payloads)

	media.mu.Lock()
	media.failures["/recording-b.ogg"] = true
	media.mu.Unlock()
	api.mu.Lock()
	call["recording_url"] = router.url(t, media.server, "/recording-b.ogg")
	api.mu.Unlock()
	summary, err := NewImporter(st, api.client(t)).Import(t.Context(), opts)
	require.NoError(err, "a media failure must not discard the archived call")
	assert.Positive(summary.MediaFailures)
	failedAttempts := media.requestCount("/recording-b.ogg")
	assert.Positive(failedAttempts, "the changed recording must actually be attempted")
	preserved, payloads := readMediaRefreshBytes(t, st, meetingID, opts.AttachmentsDir)
	require.Len(preserved, 1)
	assert.Equal([]string{"synthetic recording A bytes"}, payloads)
	for key, old := range before {
		assert.Equal(old.ContentHash, preserved[key].ContentHash)
		assert.Equal(old.StoragePath, preserved[key].StoragePath)
	}

	media.mu.Lock()
	media.failures["/recording-b.ogg"] = false
	media.mu.Unlock()
	// Keep the same provider URL and conversation timestamp. The failed refresh
	// must not relabel the old blob as already downloaded from this new URL.
	_, err = NewImporter(st, api.client(t)).Import(t.Context(), opts)
	require.NoError(err)
	assert.Greater(media.requestCount("/recording-b.ogg"), failedAttempts)
	after, payloads := readMediaRefreshBytes(t, st, meetingID, opts.AttachmentsDir)
	require.Len(after, 1)
	assert.Equal([]string{"synthetic replacement recording B bytes"}, payloads)
	for key, old := range before {
		assert.NotEqual(old.ContentHash, after[key].ContentHash)
	}
}

func TestMediaRefreshSignedQueryRotationKeepsStoredRecording(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	media := newMediaRefreshServer(t)
	router := newChatwootMediaRouter(t, media.server)
	message := mediaRefreshCall(router.url(t, media.server, "/recording-a.ogg?signature=synthetic-old"), "")
	call, ok := message["call"].(map[string]any)
	require.True(ok)
	api := newContractAPI(t, 2, []map[string]any{message})
	api.mediaRouter = router
	st := testutil.NewTestStore(t)
	importer, _ := contractRegister(t, st, api)
	opts := mediaRefreshOptions(t)
	_, err := importer.Import(t.Context(), opts)
	require.NoError(err)
	meetingID := contractArchivedMessageID(t, st, "call:901")
	before, payloads := readMediaRefreshBytes(t, st, meetingID, opts.AttachmentsDir)
	require.Len(before, 1)
	assert.Equal([]string{"synthetic recording A bytes"}, payloads)
	initialRequests := media.requestCount("/recording-a.ogg")
	require.Positive(initialRequests)
	api.mu.Lock()
	call["recording_url"] = router.url(t, media.server, "/recording-a.ogg?signature=synthetic-new")
	api.mu.Unlock()
	_, err = NewImporter(st, api.client(t)).Import(t.Context(), opts)
	require.NoError(err)
	assert.Equal(initialRequests, media.requestCount("/recording-a.ogg"), "rotated signatures do not identify new recording bytes")
	after, payloads := readMediaRefreshBytes(t, st, meetingID, opts.AttachmentsDir)
	require.Len(after, 1)
	assert.Equal([]string{"synthetic recording A bytes"}, payloads)
	for key, old := range before {
		assert.Equal(old.ContentHash, after[key].ContentHash)
	}
}

func TestMediaRefreshRecordingRepresentationMigration(t *testing.T) {
	for _, tc := range []struct {
		name           string
		noMedia        bool
		rotateQuery    bool
		wantNewFetches int
	}{
		{name: "media_disabled", noMedia: true},
		{name: "origin_unavailable_with_rotated_signature", rotateQuery: true, wantNewFetches: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			media := newMediaRefreshServer(t)
			router := newChatwootMediaRouter(t, media.server)
			originalURL := router.url(t, media.server, "/recording-a.ogg")
			message := mediaRefreshCall(originalURL, "")
			api := newContractAPI(t, 2, []map[string]any{message})
			api.mediaRouter = router
			st := testutil.NewTestStore(t)
			importer, _ := contractRegister(t, st, api)
			opts := mediaRefreshOptions(t)
			_, err := importer.Import(t.Context(), opts)
			require.NoError(err)
			meetingID := contractArchivedMessageID(t, st, "call:901")
			before, payloads := readMediaRefreshBytes(t, st, meetingID, opts.AttachmentsDir)
			require.Len(before, 1)
			previous, exists := before["chatwoot:recording:901"]
			require.True(exists, "the first snapshot exposes only the live call recording")
			require.Equal([]string{"synthetic recording A bytes"}, payloads)
			initialRequests := media.requestCount("/recording-a.ogg")

			attachmentURL := originalURL
			if tc.rotateQuery {
				attachmentURL += "?signature=synthetic-new"
			}
			api.mu.Lock()
			message["attachments"] = []any{map[string]any{
				"id": 2001, "message_id": 901, "file_type": "audio", "content_type": "audio/ogg", "extension": "ogg",
				"data_url": attachmentURL, "transcribed_text": "Synthetic newly exposed transcript",
			}}
			api.mu.Unlock()
			media.mu.Lock()
			media.failures["/recording-a.ogg"] = true
			media.mu.Unlock()
			opts.Media = opts.Media && !tc.noMedia
			summary, err := NewImporter(st, api.client(t)).Import(t.Context(), opts)
			require.NoError(err)
			// The chat message gains its first attachment and may need one fetch.
			// The linked meeting already has these bytes and must reuse them.
			assert.Equal(tc.wantNewFetches, media.requestCount("/recording-a.ogg")-initialRequests)
			assert.Equal(tc.wantNewFetches, summary.MediaFailures)
			after, payloads := readMediaRefreshBytes(t, st, meetingID, opts.AttachmentsDir)
			require.Len(after, 1)
			ref, exists := after["chatwoot:attachment:2001"]
			require.True(exists, "provider attachment identity replaces the synthetic recording identity")
			assert.Equal([]string{"synthetic recording A bytes"}, payloads)
			assert.Equal(previous.ContentHash, ref.ContentHash)
			assert.Equal(previous.StoragePath, ref.StoragePath)
			assert.Equal(previous.Size, ref.Size)
			assert.Equal(attachmentpolicy.StateStored, ref.State)
			assert.Empty(ref.SkipReason)
			var metadata struct {
				URL       string `json:"url"`
				StoredURL string `json:"stored_url"`
				Source    struct {
					ID int64 `json:"id"`
				} `json:"source"`
				SourceTranscript struct {
					Text string `json:"text"`
				} `json:"source_transcript"`
			}
			require.NoError(json.Unmarshal([]byte(ref.Metadata), &metadata))
			assert.Equal(attachmentURL, metadata.URL)
			assert.Equal(originalURL, metadata.StoredURL)
			assert.Equal(int64(2001), metadata.Source.ID)
			assert.Equal("Synthetic newly exposed transcript", metadata.SourceTranscript.Text)
		})
	}
}
