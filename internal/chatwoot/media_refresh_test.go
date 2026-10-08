package chatwoot

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type mediaRefreshServer struct {
	mu        sync.Mutex
	server    *httptest.Server
	failures  map[string]bool
	requests  map[string]int
	declared  bool
	onRequest func(string)
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
		if media.onRequest != nil {
			media.onRequest(r.URL.Path)
		}
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
		if media.declared {
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		}
		flusher, ok := w.(http.Flusher)
		if !assert.True(t, ok) {
			return
		}
		flusher.Flush()
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
	return ImportOptions{InboxID: 7, IncludePrivate: true, Policy: attachmentpolicy.Policy{MaxBytes: 4096}, AttachmentsDir: t.TempDir()}
}

func readMediaRefreshBytes(t *testing.T, st *store.Store, messageID int64, dir string) (map[string]store.AttachmentRef, []string) {
	t.Helper()
	refs, err := st.MessageProviderAttachments(messageID, "chatwoot:")
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
		name                               string
		recordingPath                      string
		want                               []string
		wantRequests                       int
		initialFailure, replacementFailure bool
	}{
		{"distinct_recordings", "/recording-b.ogg", []string{"synthetic recording A bytes", "synthetic replacement recording B bytes"}, 2, false, false},
		{"same_recording", "/recording-a.ogg", []string{"synthetic recording A bytes"}, 1, false, false},
		{"new_failed_attachment", "/recording-a.ogg", []string{"synthetic recording A bytes"}, 1, true, false},
		{"retained_failed_replacement", "/recording-a.ogg", []string{"synthetic replacement recording B bytes"}, 1, false, true},
		{"successful_same_key_replacement", "/recording-a.ogg", []string{"synthetic replacement recording B bytes"}, 2, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			media := newMediaRefreshServer(t)
			router := newChatwootMediaRouter(t, media.server)
			message := mediaRefreshCall(router.url(t, media.server, tc.recordingPath), router.url(t, media.server, "/recording-a.ogg"))
			call, ok := message["call"].(map[string]any)
			require.True(ok)
			attachments, ok := message["attachments"].([]any)
			require.True(ok)
			require.Len(attachments, 1)
			attachment, ok := attachments[0].(map[string]any)
			require.True(ok)
			api := newContractAPI(t, 2, []map[string]any{message})
			api.mediaRouter = router
			st := testutil.NewTestStore(t)
			importer, _ := contractRegister(t, st, api)
			opts := mediaRefreshOptions(t)
			media.failures["/recording-a.ogg"] = tc.initialFailure
			summary, err := importer.Import(t.Context(), opts)
			require.NoError(err)
			path := "/recording-a.ogg"
			if tc.replacementFailure || tc.name == "successful_same_key_replacement" {
				path = "/recording-b.ogg"
				call["recording_url"] = router.url(t, media.server, path)
				attachment["data_url"] = call["recording_url"]
				media.failures[path] = tc.replacementFailure
				summary, err = NewImporter(st, api.client(t)).Import(t.Context(), opts)
				require.NoError(err)
			}
			if tc.initialFailure || tc.replacementFailure {
				assert.Equal(1, media.requestCount(path), "one failed resource is attempted once per import")
				assert.Equal(1, summary.MediaFailures, "one failed resource is counted once per import")
				for _, id := range []string{"901", "call:901"} {
					archivedID := contractArchivedMessageID(t, st, id)
					refs, err := st.MessageProviderAttachments(archivedID, "chatwoot:")
					require.NoError(err)
					require.Len(refs, 1)
					assert.Equal(attachmentpolicy.StateFailed, refs["chatwoot:attachment:2001"].State)
					if tc.replacementFailure {
						_, payloads := readMediaRefreshBytes(t, st, archivedID, opts.AttachmentsDir)
						assert.Equal([]string{"synthetic recording A bytes"}, payloads)
					}
				}
				attachment["data_url"] = router.url(t, media.server, path) + "?signature=next"
				if tc.initialFailure {
					attachment["id"] = int64(2002)
				}
				summary, err = importer.Import(t.Context(), opts)
				require.NoError(err)
				assert.Equal(2, media.requestCount(path), "a later import gets one fresh attempt")
				assert.Equal(1, summary.MediaFailures)
				if tc.replacementFailure {
					message["attachments"] = nil
					for _, full := range []bool{false, true} {
						opts.Full = full
						attempts := media.requestCount(path)
						summary, err = importer.Import(t.Context(), opts)
						require.NoError(err)
						assert.Equal(attempts+1, media.requestCount(path), "unlisted retained failure cannot suppress the call-only retry")
						assert.Equal(1, summary.MediaFailures)
					}
				}
				media.failures[path] = false
				summary, err = importer.Import(t.Context(), opts)
				require.NoError(err)
				assert.Zero(summary.MediaFailures)
				_, payloads := readMediaRefreshBytes(t, st, contractArchivedMessageID(t, st, "call:901"), opts.AttachmentsDir)
				assert.Contains(payloads, tc.want[0])
				return
			}
			if tc.name == "successful_same_key_replacement" {
				assert.Zero(summary.MediaFailures)
				assert.Equal(1, media.requestCount(path))
				_, payloads := readMediaRefreshBytes(t, st, contractArchivedMessageID(t, st, "901"), opts.AttachmentsDir)
				assert.Equal(tc.want, payloads)
			}
			assert.Equal(tc.wantRequests, media.requestCount("/recording-a.ogg")+media.requestCount("/recording-b.ogg"), "each unique audio URL should be fetched once across the chat and linked meeting")
			meetingID := contractArchivedMessageID(t, st, "call:901")
			refs, payloads := readMediaRefreshBytes(t, st, meetingID, opts.AttachmentsDir)
			assert.Len(refs, len(tc.want), "the message audio does not suppress a different live call recording")
			assert.ElementsMatch(tc.want, payloads)
		})
	}
}

func TestMediaRefreshFailedReplacementRetainsBytesAndRetriesNextSync(t *testing.T) {
	for _, name := range []string{"replacement_fails", "recording_dropped", "replacement_canceled", "first_import_canceled"} {
		dropped := name == "recording_dropped"
		t.Run(name, func(t *testing.T) {
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
			ctx := t.Context()
			if name == "first_import_canceled" {
				canceled, cancel := context.WithCancel(ctx)
				defer cancel()
				ctx = canceled
				media.onRequest = func(string) { cancel() }
			}
			_, err := importer.Import(ctx, opts)
			if name == "first_import_canceled" {
				require.ErrorIs(err, context.Canceled)
				chatID := contractArchivedMessageID(t, st, "901")
				meetingID := contractArchivedMessageID(t, st, "call:901")
				for _, pair := range [][2]int64{{chatID, meetingID}, {meetingID, chatID}} {
					detail, err := st.GetMessageContext(t.Context(), pair[0])
					require.NoError(err)
					assert.Equal(new(pair[1]), detail.RelatedMessageID)
				}
				source, err := st.GetSourceByTypeAndIdentifier(SourceType, SourceIdentifier(api.server.URL, 3, 7))
				require.NoError(err)
				require.NotNil(source)
				state, err := importer.resumeState(source.ID, source.Identifier)
				require.NoError(err)
				assert.NotEmpty(state.Conversations["42"].Pending, "cancellation retains unfinished saved work")
				media.mu.Lock()
				media.onRequest = nil
				media.mu.Unlock()
				_, err = NewImporter(st, api.client(t)).Import(t.Context(), opts)
				require.NoError(err)
				_, payloads := readMediaRefreshBytes(t, st, meetingID, opts.AttachmentsDir)
				assert.Equal([]string{"synthetic recording A bytes"}, payloads)
				return
			}
			require.NoError(err)
			meetingID := contractArchivedMessageID(t, st, "call:901")
			before, payloads := readMediaRefreshBytes(t, st, meetingID, opts.AttachmentsDir)
			require.Len(before, 1)
			assert.Equal([]string{"synthetic recording A bytes"}, payloads)

			if name == "replacement_canceled" {
				call["recording_url"] = router.url(t, media.server, "/recording-b.ogg")
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				media.onRequest = func(string) { cancel() }
				opts.Full = true
				_, err = NewImporter(st, api.client(t)).Import(ctx, opts)
				require.ErrorIs(err, context.Canceled)
				chatID := contractArchivedMessageID(t, st, "901")
				for _, pair := range [][2]int64{{chatID, meetingID}, {meetingID, chatID}} {
					detail, err := st.GetMessageContext(t.Context(), pair[0])
					require.NoError(err)
					assert.Equal(new(pair[1]), detail.RelatedMessageID, "a canceled reread keeps its existing meeting link")
				}
				_, payloads := readMediaRefreshBytes(t, st, meetingID, opts.AttachmentsDir)
				assert.Equal([]string{"synthetic recording A bytes"}, payloads)
				return
			}
			if dropped {
				api.Mu.Lock()
				delete(call, "recording_url")
				api.Mu.Unlock()
				_, err = NewImporter(st, api.client(t)).Import(t.Context(), opts)
				require.NoError(err)
				refs, payloads := readMediaRefreshBytes(t, st, meetingID, opts.AttachmentsDir)
				assert.Len(refs, 1)
				assert.Equal([]string{"synthetic recording A bytes"}, payloads, "the archive keeps a recording the provider stops listing")
				return
			}
			media.mu.Lock()
			media.failures["/recording-b.ogg"] = true
			media.mu.Unlock()
			api.Mu.Lock()
			call["recording_url"] = router.url(t, media.server, "/recording-b.ogg")
			api.Mu.Unlock()
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
		})
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
			api.Mu.Lock()
			message["attachments"] = []any{map[string]any{
				"id": 2001, "message_id": 901, "file_type": "audio", "content_type": "audio/ogg", "extension": "ogg",
				"data_url": attachmentURL, "transcribed_text": "Synthetic newly exposed transcript",
			}}
			api.Mu.Unlock()
			media.mu.Lock()
			media.failures["/recording-a.ogg"] = true
			media.mu.Unlock()
			opts.NoMedia = tc.noMedia
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

func TestDeferredMediaRetriesAndLocalWritesFail(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(strconv.FormatBool(disabled), func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			media := newMediaRefreshServer(t)
			router := newChatwootMediaRouter(t, media.server)
			message := contractMessage(901, now().Unix(), nil)
			message["attachments"] = []any{map[string]any{"id": int64(2001), "file_type": "image", "data_url": router.url(t, media.server, "/recording-a.ogg")}}
			api := newContractAPI(t, 1000, []map[string]any{message})
			api.mediaRouter = router
			st := testutil.NewTestStore(t)
			imp, source := contractRegister(t, st, api)
			opts := mediaRefreshOptions(t)
			opts.NoMedia = true
			if disabled {
				opts.Policy.DisabledReason = attachmentpolicy.SkipAccountPolicy
			}
			_, err := imp.Import(t.Context(), opts)
			require.NoError(err)
			refs, err := st.MessageProviderAttachments(contractArchivedMessageID(t, st, "901"), "chatwoot:")
			require.NoError(err)
			for _, ref := range refs {
				if disabled {
					assert.Equal(attachmentpolicy.SkipAccountPolicy, ref.SkipReason)
				} else {
					assert.Equal(attachmentpolicy.StatePending, ref.State)
				}
			}
			assert.Zero(media.requestCount("/recording-a.ogg"))
			opts.NoMedia = false
			if disabled {
				assert.Empty(savedState(t, st, source).Conversations)
				return
			}
			require.NoError(os.WriteFile(filepath.Join(opts.AttachmentsDir, "blocked"), []byte("file"), 0600))
			dir := opts.AttachmentsDir
			opts.AttachmentsDir = filepath.Join(dir, "blocked")
			_, err = NewImporter(st, api.client(t)).Import(t.Context(), opts)
			require.Error(err, "local storage failures stop the sync")
			opts.AttachmentsDir = dir
			_, err = NewImporter(st, api.client(t)).Import(t.Context(), opts)
			require.NoError(err)
			_, payloads := readMediaRefreshBytes(t, st, contractArchivedMessageID(t, st, "901"), dir)
			assert.Equal([]string{"synthetic recording A bytes"}, payloads)
			assert.Empty(savedState(t, st, source).Conversations)
		})
	}
}

func TestStreamOversizeRetainsMimeAndSizeEvidence(t *testing.T) {
	for _, tc := range []struct {
		name                                   string
		retained, declared, call, declaredFile bool
	}{
		{"initial_chunked", false, false, false, false}, {"replacement_chunked", true, false, false, false}, {"replacement_declared", true, true, false, false}, {"replacement_declared_file", true, false, false, true}, {"replacement_shared_call", true, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			media := newMediaRefreshServer(t)
			media.declared = tc.declared
			router := newChatwootMediaRouter(t, media.server)
			attachment := map[string]any{"id": int64(2001), "file_type": "image", "file_size": int64(1), "data_url": router.url(t, media.server, "/recording-a.ogg")}
			message := contractMessage(901, now().Unix(), nil)
			if tc.call {
				message = mediaRefreshCall("", "")
				attachment["file_type"] = "audio"
			}
			message["attachments"] = []any{attachment}
			api := newContractAPI(t, 1000, []map[string]any{message})
			api.mediaRouter = router
			st := testutil.NewTestStore(t)
			imp, _ := contractRegister(t, st, api)
			opts := mediaRefreshOptions(t)
			path, payload := "/recording-a.ogg", "synthetic recording A bytes"
			var original store.AttachmentRef
			if tc.retained {
				_, err := imp.Import(t.Context(), opts)
				require.NoError(err)
				refs, _ := readMediaRefreshBytes(t, st, contractArchivedMessageID(t, st, "901"), opts.AttachmentsDir)
				original = refs["chatwoot:attachment:2001"]
				path, payload = "/recording-b.ogg", "synthetic replacement recording B bytes"
				attachment["data_url"] = router.url(t, media.server, path)
				opts.Policy.MaxBytes = 28
				if tc.declaredFile {
					attachment["file_size"] = int64(38)
				}
				opts.Full = true
			} else {
				opts.Policy.MaxBytes = 8
			}
			for range 2 {
				_, err := NewImporter(st, api.client(t)).Import(t.Context(), opts)
				require.NoError(err)
				refs, err := st.MessageProviderAttachments(contractArchivedMessageID(t, st, "901"), "chatwoot:")
				require.NoError(err)
				ref := refs["chatwoot:attachment:2001"]
				assert.Equal(attachmentpolicy.SkipSizeCap, ref.SkipReason)
				assert.Equal("audio/ogg", ref.MimeType)
				if tc.retained {
					assert.Equal(original.Size, ref.Size)
					assert.Equal(original.ContentHash, ref.ContentHash)
					assert.Equal(original.StoragePath, ref.StoragePath)
				} else {
					assert.Greater(int64(ref.Size), opts.Policy.MaxBytes)
				}
				opts.Full = true
				attachment["data_url"] = router.url(t, media.server, path) + "?signature=rotated"
			}
			attempts := 1
			if tc.declaredFile {
				attempts = 0
			}
			assert.Equal(attempts, media.requestCount(path))
			if tc.retained {
				if tc.declared || tc.declaredFile {
					opts.Policy.MaxBytes = 29
					_, err := NewImporter(st, api.client(t)).Import(t.Context(), opts)
					require.NoError(err)
					assert.Equal(attempts, media.requestCount(path), "a cap below observed size still excludes the replacement")
					opts.Policy.MaxBytes = 28
				}
				opts.NoMedia = true
				_, err := NewImporter(st, api.client(t)).Import(t.Context(), opts)
				require.NoError(err)
				delete(attachment, "data_url")
				_, err = NewImporter(st, api.client(t)).Import(t.Context(), opts)
				require.NoError(err)
				attachment["data_url"] = router.url(t, media.server, path) + "?signature=restored"
				opts.NoMedia = false
				_, err = NewImporter(st, api.client(t)).Import(t.Context(), opts)
				require.NoError(err)
				assert.Equal(attempts, media.requestCount(path), "deferred reads and missing URLs retain rejection evidence")
				attachment["data_url"] = router.url(t, media.server, path) + "?resource=next"
				attachment["file_size"] = int64(1)
				_, err = NewImporter(st, api.client(t)).Import(t.Context(), opts)
				require.NoError(err)
				assert.Equal(attempts+1, media.requestCount(path), "a different resource can be attempted")
			}
			opts.Policy.MaxBytes = 4096
			_, err := NewImporter(st, api.client(t)).Import(t.Context(), opts)
			require.NoError(err)
			refs, payloads := readMediaRefreshBytes(t, st, contractArchivedMessageID(t, st, "901"), opts.AttachmentsDir)
			assert.Equal([]string{payload}, payloads)
			var rejected mediaMetadata
			require.NoError(json.Unmarshal([]byte(refs["chatwoot:attachment:2001"].Metadata), &rejected))
			assert.Zero(rejected.RejectedSize)
			assert.Empty(rejected.RejectedURL)
		})
	}
}

func TestReconcileDoesNotRenewFailedDownload(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	fixed := now
	clock := fixed()
	now = func() time.Time { return clock }
	t.Cleanup(func() { now = fixed })
	media := newMediaRefreshServer(t)
	router := newChatwootMediaRouter(t, media.server)
	media.failures["/recording-a.ogg"] = true
	old := contractMessage(901, clock.Add(-30*24*time.Hour).Unix(), nil)
	attachment := map[string]any{"id": int64(2001), "file_type": "image", "data_url": router.url(t, media.server, "/recording-a.ogg")}
	old["attachments"] = []any{attachment}
	api := newContractAPI(t, 1000, []map[string]any{old})
	api.mediaRouter = router
	st := testutil.NewTestStore(t)
	imp, source := contractRegister(t, st, api)
	opts := mediaRefreshOptions(t)
	start := clock.Unix()
	attempts := 0
	for day := range 10 {
		clock = fixed().Add(time.Duration(day) * 24 * time.Hour)
		api.AddMessage(42, int64(1000+day), clock)
		attachment["data_url"] = router.url(t, media.server, "/recording-a.ogg") + "?signature=" + strconv.Itoa(day)
		if day == 3 {
			attachment["id"] = int64(2003)
		}
		if day == 2 {
			opts.Policy.DisabledReason = attachmentpolicy.SkipAccountPolicy
		} else {
			opts.Policy.DisabledReason = ""
		}
		_, err := NewImporter(st, api.client(t)).Import(t.Context(), opts)
		require.NoError(err)
		state := savedState(t, st, source)
		if day == 2 {
			assert.NotContains(state.Conversations, "42")
			continue
		}
		if day < 7 {
			attempts = media.requestCount("/recording-a.ogg")
			require.Contains(state.Conversations, "42")
			assert.Equal(start, state.Conversations["42"].Artifacts["901"])
		} else {
			assert.NotContains(state.Conversations, "42")
		}
	}
	assert.Equal(attempts, media.requestCount("/recording-a.ogg"))
	opts.Full = true
	_, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.Equal(attempts+1, media.requestCount("/recording-a.ogg"))
	opts.Full = false
	clock = clock.Add(24 * time.Hour)
	api.AddMessage(42, 1100, clock)
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.Equal(attempts+1, media.requestCount("/recording-a.ogg"), "explicit full retry does not renew automatic expiry")
	attachment["data_url"] = router.url(t, media.server, "/recording-b.ogg")
	attachment["id"] = int64(2002)
	opts.Full = true
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	_, payloads := readMediaRefreshBytes(t, st, contractArchivedMessageID(t, st, "901"), opts.AttachmentsDir)
	assert.Equal([]string{"synthetic replacement recording B bytes"}, payloads)
}
