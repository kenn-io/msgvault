package cmd

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/attachmentstore"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/docbankmedia"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/twilio"
)

// Exercise the real Twilio HTTP client, importer, CAS, and daemon media job.
// The external services use synthetic responses and a valid stereo PCM WAV.
func TestTwilioRecordingDocbankConsent(t *testing.T) {
	const accountSID = "AC00000000000000000000000000000001"
	const callSID = "CA00000000000000000000000000000001"
	const recordingSID = "RE00000000000000000000000000000001"
	const accountPath = "/2010-04-01/Accounts/" + accountSID
	wav := testWAV()
	binary.LittleEndian.PutUint16(wav[22:24], 2)
	binary.LittleEndian.PutUint32(wav[28:32], 32000)
	binary.LittleEndian.PutUint16(wav[32:34], 4)
	hash := sha256.Sum256(wav)
	wantHash := hex.EncodeToString(hash[:])
	recording := twilio.Recording{SID: recordingSID, AccountSID: accountSID, CallSID: callSID,
		Status: "completed", Channels: 2, Duration: "60", DateCreated: "2026-10-12T10:00:00Z"}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case accountPath + "/Recordings.json", accountPath + "/Calls/" + callSID + "/Recordings.json":
			assert.NoError(t, json.MarshalWrite(w, map[string]any{"recordings": []twilio.Recording{recording}, "next_page_uri": nil}))
		case accountPath + "/Calls/" + callSID + ".json":
			assert.NoError(t, json.MarshalWrite(w, twilio.Call{SID: callSID, AccountSID: accountSID,
				Status: "completed", StartTime: "2026-10-12T10:00:00Z", Duration: "60"}))
		case accountPath + "/Recordings/" + recordingSID + ".wav":
			assert.Equal(t, "2", r.URL.Query().Get("RequestedChannels"))
			_, err := w.Write(wav)
			assert.NoError(t, err)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(provider.Close)
	client, err := twilio.NewClient(twilio.Options{AccountSID: accountSID, AuthToken: "synthetic-token",
		Endpoints: map[string]string{"voice": provider.URL, "intelligence": provider.URL}})
	require.NoError(t, err)
	st := testutil.NewTestStore(t)
	_, err = st.GetOrCreateSource("twilio", "work")
	require.NoError(t, err)
	dir := t.TempDir()
	opts := twilio.ImportOptions{Identifier: "work", AccountEmail: "owner@example.com",
		AttachmentsDir: dir, MediaPolicy: (config.TwilioSource{}).MediaPolicy()}
	sum, err := twilio.NewImporter(st, client).Import(t.Context(), opts)
	require.NoError(t, err)
	assert.EqualValues(t, 1, sum.MeetingsAdded)
	assert.EqualValues(t, 1, sum.AttachmentsStored)
	candidates, err := st.ListBeeperMediaCandidates(t.Context(), 0, 100)
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	candidate := candidates[0]
	assert.Equal(t, "twilio", candidate.SourceType)
	assert.Equal(t, callSID, candidate.SourceConversationID)
	assert.Equal(t, callSID, candidate.SourceMessageID)
	assert.Equal(t, "twilio:"+recordingSID, candidate.SourceAttachmentID)
	assert.Equal(t, "twilio:"+recordingSID, candidate.SourcePartKey)
	assert.Equal(t, "audio", candidate.MediaType)
	assert.Equal(t, string(store.AttachmentRoleStandalone), candidate.Role)
	assert.Equal(t, "stored", candidate.AttachmentState)
	assert.Equal(t, wantHash, candidate.ContentHash)
	assert.Equal(t, int64(len(wav)), candidate.ByteLength)
	data, err := os.ReadFile(filepath.Join(dir, wantHash[:2], wantHash))
	require.NoError(t, err)
	assert.Equal(t, wav, data)

	blobs, err := attachmentstore.New(store.NewPackCatalog(st), dir)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, blobs.Close()) })
	var uploads atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uploads.Add(1)
		assert.Equal(t, "/api/v1/media/sources", r.URL.Path)
		assert.Equal(t, "synthetic-key", r.Header.Get("X-Api-Key"))
		reader, err := r.MultipartReader()
		if !assert.NoError(t, err) {
			return
		}
		part, err := reader.NextPart()
		if !assert.NoError(t, err) {
			return
		}
		var metadata docbankmedia.SuppliedMetadata
		if !assert.NoError(t, json.UnmarshalRead(part, &metadata)) {
			return
		}
		assert.Equal(t, wantHash, metadata.SHA256)
		assert.Equal(t, int64(len(wav)), metadata.ByteLength)
		assert.Equal(t, "audio/wav", metadata.MediaType)
		part, err = reader.NextPart()
		if !assert.NoError(t, err) {
			return
		}
		data, err := io.ReadAll(part)
		assert.NoError(t, err)
		assert.Equal(t, wav, data)
		assert.NoError(t, json.MarshalWrite(w, docbankmedia.Receipt{VaultUID: "vault", SourceID: "source",
			SourceVersionID: "version", ContentVersionID: "content", OccurrenceID: "occurrence",
			OperationID: metadata.OperationID, OperationState: "succeeded", CoverageState: "unprocessed"}))
	}))
	t.Cleanup(peer.Close)
	t.Setenv(beeperMediaTestKeyEnv, "synthetic-key")
	sched := scheduler.New(nil)
	t.Cleanup(func() { <-sched.Stop().Done() })
	cfg := config.DocbankIntegrationConfig{Enabled: true, URL: peer.URL, APIKeyEnv: beeperMediaTestKeyEnv}
	spool := t.TempDir()
	require.NoError(t, configureBeeperMediaJob(t.Context(), sched, nil, st, blobs, spool, cfg, nil))
	require.NoError(t, sched.TriggerJob(beeperMediaSubmitJob))
	assert.Zero(t, uploads.Load(), "without consent discovery stays local")
	archiveUID, err := st.ArchiveUIDContext(t.Context())
	require.NoError(t, err)
	destination := docbankmedia.DestinationKey(peer.URL, archiveUID)
	assert.Equal(t, map[string]string{destination: "pending::"}, retentionRows(t, st))

	cfg.AllSourcesUploadConsent = true
	require.NoError(t, configureBeeperMediaJob(t.Context(), sched, nil, st, blobs, spool, cfg, nil))
	require.NoError(t, sched.TriggerJob(beeperMediaSubmitJob))
	assert.Equal(t, int32(1), uploads.Load())
	assert.Equal(t, map[string]string{destination: "retained::source"}, retentionRows(t, st))
	// Re-sync and restart the route: the stored occurrence must not upload twice.
	opts.Full = true
	_, err = twilio.NewImporter(st, client).Import(t.Context(), opts)
	require.NoError(t, err)
	require.NoError(t, configureBeeperMediaJob(t.Context(), sched, nil, st, blobs, spool, cfg, nil))
	require.NoError(t, sched.TriggerJob(beeperMediaSubmitJob))
	assert.Equal(t, int32(1), uploads.Load())
}
