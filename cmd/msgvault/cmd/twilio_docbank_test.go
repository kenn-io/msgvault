package cmd

import (
	"encoding/binary"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/attachmentstore"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/twilio"
)

// TestTwilioRecordingReachesDocbank checks that an imported Twilio recording keeps
// its Twilio identity and reaches Docbank byte-for-byte with upload consent.
func TestTwilioRecordingReachesDocbank(t *testing.T) {
	const accountSID = "AC00000000000000000000000000000001"
	const callSID = "CA00000000000000000000000000000001"
	const recordingSID = "RE00000000000000000000000000000001"
	const accountPath = "/2010-04-01/Accounts/" + accountSID
	wav := testWAV()
	binary.LittleEndian.PutUint16(wav[22:24], 2)
	binary.LittleEndian.PutUint32(wav[28:32], 32000)
	binary.LittleEndian.PutUint16(wav[32:34], 4)
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
	_, err = twilio.NewImporter(st, client).Import(t.Context(), opts)
	require.NoError(t, err)
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
	wantHash := candidate.ContentHash
	data, err := os.ReadFile(filepath.Join(dir, wantHash[:2], wantHash))
	require.NoError(t, err)
	assert.Equal(t, wav, data)

	blobs, err := attachmentstore.New(store.NewPackCatalog(st), dir)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, blobs.Close()) })
	server, peer := newRetentionServer(t)
	t.Setenv(beeperMediaTestKeyEnv, "synthetic-key")
	sched := scheduler.New(nil)
	t.Cleanup(func() { <-sched.Stop().Done() })
	cfg := config.DocbankIntegrationConfig{Enabled: true, URL: peer.URL,
		APIKeyEnv: beeperMediaTestKeyEnv, AllSourcesUploadConsent: true}
	spool := t.TempDir()
	require.NoError(t, configureBeeperMediaJob(t.Context(), sched, nil, st, blobs, spool, dir, cfg, nil))
	require.NoError(t, sched.TriggerJob(beeperMediaSubmitJob))
	assert.Equal(t, 1, server.requestCount())
	server.mu.Lock()
	assert.Equal(t, data, server.uploaded)
	server.mu.Unlock()
}
