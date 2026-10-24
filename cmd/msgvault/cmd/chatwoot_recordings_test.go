package cmd

import (
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentstore"
	"go.kenn.io/msgvault/internal/chatwoot"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/chatwootapi"
)

// Exercise capture and the daemon's actual consent wiring with synthetic HTTP
// APIs and a temporary archive, without starting or connecting to a daemon.
func TestChatwootRecordingDocbankConsent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	wav := testWAV()
	message := chatwootapi.Message(901, time.Now().Unix(), nil)
	message["content_type"] = "voice_call"
	call := map[string]any{"id": 1001, "provider": "twilio", "direction": "incoming", "status": "completed"}
	message["call"] = call
	api := chatwootapi.New(25, []map[string]any{message}, time.Now)
	producer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/recording" {
			assert.Empty(r.Header.Get("Api_access_token"))
			w.Header().Set("Content-Type", "application/octet-stream")
			_, err := w.Write(wav)
			assert.NoError(err)
			return
		}
		api.ServeHTTP(w, r)
	}))
	t.Cleanup(producer.Close)
	st := testutil.NewTestStore(t)
	client, err := chatwoot.NewClient(producer.URL, 3, "synthetic-token")
	require.NoError(err)
	importer := chatwoot.NewImporter(st, client)
	_, err = importer.Register(t.Context(), []chatwoot.Inbox{{ID: 7, Name: "Example Voice Inbox", ChannelType: "Channel::Voice"}})
	require.NoError(err)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	src := config.ChatwootSource{URL: producer.URL, AccountID: 3}
	opts := chatwootImportOptions(src, 7, cfg)
	_, err = importer.Import(t.Context(), opts)
	require.NoError(err)
	candidates, err := st.ListBeeperMediaCandidates(t.Context(), 0, 100)
	require.NoError(err)
	assert.Empty(candidates, "call metadata alone is not stored audio")

	// The recording appears after the first sync, without conversation activity.
	api.Mu.Lock()
	call["recording_url"] = producer.URL + "/recording"
	api.Mu.Unlock()
	_, err = importer.Import(t.Context(), opts)
	require.NoError(err)
	candidates, err = st.ListBeeperMediaCandidates(t.Context(), 0, 100)
	require.NoError(err)
	require.Len(candidates, 1)
	assert.Equal("chatwoot", candidates[0].SourceType)
	assert.Equal("call:901", candidates[0].SourceMessageID)
	assert.Equal("audio/wav", candidates[0].MIMEType)
	assert.Equal("audio", candidates[0].MediaType)

	blobs, err := attachmentstore.New(store.NewPackCatalog(st), cfg.AttachmentsDir())
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(blobs.Close()) })
	var requests atomic.Int64
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		assert.Equal("synthetic-key", r.Header.Get("X-Api-Key"))
		reader, err := r.MultipartReader()
		if !assert.NoError(err) {
			return
		}
		part, err := reader.NextPart()
		if !assert.NoError(err) {
			return
		}
		var metadata struct {
			OperationID string `json:"operation_id"`
		}
		if !assert.NoError(json.UnmarshalRead(part, &metadata)) {
			return
		}
		part, err = reader.NextPart()
		if !assert.NoError(err) {
			return
		}
		payload, err := io.ReadAll(part)
		assert.NoError(err)
		assert.Equal(wav, payload, "Docbank receives the actual archived recording")
		response, err := json.Marshal(map[string]string{
			"vault_uid": "synthetic-vault", "source_id": "synthetic-source", "source_version_id": "synthetic-version",
			"content_version_id": "synthetic-content", "occurrence_id": "synthetic-occurrence",
			"operation_id": metadata.OperationID, "operation_state": "succeeded", "coverage_state": "unprocessed",
		})
		assert.NoError(err)
		w.Header().Set("Content-Type", "application/json")
		_, err = w.Write(response)
		assert.NoError(err)
	}))
	t.Cleanup(destination.Close)
	t.Setenv(beeperMediaTestKeyEnv, "synthetic-key")
	sched := scheduler.New(nil)
	t.Cleanup(func() { <-sched.Stop().Done() })
	docbank := config.DocbankIntegrationConfig{Enabled: true, URL: destination.URL, APIKeyEnv: beeperMediaTestKeyEnv}
	require.NoError(configureBeeperMediaJob(t.Context(), sched, nil, st, blobs, t.TempDir(), cfg.AttachmentsDir(), docbank, nil))
	require.NoError(sched.TriggerJob(beeperMediaSubmitJob))
	assert.Zero(requests.Load(), "local discovery sends nothing without all-source audio consent")
	docbank.AllSourcesUploadConsent = true
	require.NoError(configureBeeperMediaJob(t.Context(), sched, nil, st, blobs, t.TempDir(), cfg.AttachmentsDir(), docbank, nil))
	require.NoError(sched.TriggerJob(beeperMediaSubmitJob))
	assert.Equal(int64(1), requests.Load())
	require.NoError(sched.TriggerJob(beeperMediaSubmitJob))
	assert.Equal(int64(1), requests.Load(), "replaying a retained occurrence does not upload again")

	// Queue another recording so consent revocation is exercised with remote work pending.
	secondCall := chatwootapi.Message(902, time.Now().Add(time.Second).Unix(), nil)
	secondCall["content_type"] = "voice_call"
	secondCall["call"] = map[string]any{
		"id": 1002, "provider": "twilio", "direction": "incoming", "status": "completed",
		"recording_url": producer.URL + "/recording",
	}
	api.Mu.Lock()
	api.Conversations[42] = append(api.Conversations[42], secondCall)
	api.UpdatedAt[42] = float64(time.Now().Add(2 * time.Second).Unix())
	api.Mu.Unlock()
	_, err = importer.Import(t.Context(), opts)
	require.NoError(err)
	candidates, err = st.ListBeeperMediaCandidates(t.Context(), 0, 100)
	require.NoError(err)
	require.Len(candidates, 2)
	assert.Equal("call:902", candidates[1].SourceMessageID)

	docbank.AllSourcesUploadConsent = false
	require.NoError(configureBeeperMediaJob(t.Context(), sched, nil, st, blobs, t.TempDir(), cfg.AttachmentsDir(), docbank, nil))
	require.NoError(sched.TriggerJob(beeperMediaSubmitJob))
	assert.Equal(int64(1), requests.Load(), "revoking consent leaves the new recording unsubmitted")
	docbank.AllSourcesUploadConsent = true
	require.NoError(configureBeeperMediaJob(t.Context(), sched, nil, st, blobs, t.TempDir(), cfg.AttachmentsDir(), docbank, nil))
	require.NoError(sched.TriggerJob(beeperMediaSubmitJob))
	assert.Equal(int64(2), requests.Load(), "re-enabling consent submits the queued recording")
}
