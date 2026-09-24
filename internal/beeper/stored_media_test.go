package beeper

import (
	"encoding/json/v2"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/store"
)

// addStoredMediaSource adds an importer-owned row around bytes already present
// in the test CAS. It intentionally leaves raw evidence optional.
func addStoredMediaSource(
	t *testing.T, world *mediaWorld, sourceType, identifier, messageID string,
	data []byte, filename, mimeType, mediaType string, role store.AttachmentRole,
	raw []byte, sourceTranscript string,
) {
	t.Helper()
	source, err := world.st.GetOrCreateSource(sourceType, identifier)
	require.NoError(t, err)
	conversation, err := world.st.EnsureConversation(source.ID, sourceType+"-thread", "Thread")
	require.NoError(t, err)
	message, err := world.st.UpsertMessage(&store.Message{ConversationID: conversation, SourceID: source.ID,
		SourceMessageID: messageID, MessageType: sourceType, SizeEstimate: int64(len(data))})
	require.NoError(t, err)
	if raw != nil {
		require.NoError(t, world.st.UpsertMessageRawWithFormat(message, raw, sourceType+"_raw"))
	}
	metadata := ""
	if sourceTranscript != "" {
		encoded, err := json.Marshal(map[string]any{"source_transcript": map[string]any{
			"provider": sourceType, "text": sourceTranscript, "language": "en",
		}})
		require.NoError(t, err)
		metadata = string(encoded)
	}
	hash := sha256Hex(data)
	require.NoError(t, world.st.UpsertAttachmentRecord(t.Context(), message, store.AttachmentWrite{
		Filename: filename, MIMEType: mimeType, StoragePath: hash[:2] + "/" + hash,
		ContentHash: hash, Size: int64(len(data)), SourceAttachmentID: sourceType + ":" + messageID,
		SourcePartKey: sourceType + ":" + messageID, MediaType: mediaType, Metadata: metadata,
		State: attachmentpolicy.StateStored, Role: role, RoleSource: store.AttachmentRoleSourceImporterSemantics,
	}))
}

func TestStoredMediaProviderMatrix(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	wav := syntheticWAV(800, 61)
	mp3 := syntheticMP3(4)
	world := importVoiceChat(t,
		voiceSpec{id: "beeper-supplied", asset: "mxc://beeper.local/supplied", mime: "audio/wav",
			fileName: "supplied.wav", transcript: "provider words", data: wav},
		voiceSpec{id: "beeper-asr", asset: "mxc://beeper.local/asr", mime: "audio/mp3",
			fileName: "asr.mp3", data: mp3})
	addStoredMediaSource(t, world, "whatsapp", "wa-account", "whatsapp-message", wav,
		"voice.bin", "application/octet-stream", "", store.AttachmentRoleStandalone, nil, "")
	addStoredMediaSource(t, world, "messenger", "messenger-account", "messenger-message", wav,
		"voice.wav", "audio/wav", "", store.AttachmentRoleUnknown, []byte("not json"), "")
	addStoredMediaSource(t, world, "synctechsms", "mms-account", "mms-message", wav,
		"voice.wav", "", "", store.AttachmentRoleStandalone, nil, "")
	addStoredMediaSource(t, world, "slack", "slack-team", "slack-message", wav,
		"voice.wav", "audio/wav", "", store.AttachmentRoleStandalone, nil, "slack words")
	addStoredMediaSource(t, world, "discord", "discord-account", "discord-message", wav,
		"voice.wav", "audio/wav", "", store.AttachmentRoleStandalone, nil, "")
	addStoredMediaSource(t, world, "gvoice", "gvoice-account", "gvoice-message", mp3,
		"voicemail", "application/octet-stream", "", store.AttachmentRoleStandalone, nil, "")
	addStoredMediaSource(t, world, "future-provider", "future-account", "future-message", wav,
		"recording", "", "", store.AttachmentRoleStandalone, nil, "")

	docbank := newFakeDocbank(t)
	server := newTestDocbankServer(t, docbank)
	defer server.Close()
	runPasses(t, world.submitter(t, server, "stored-provider-matrix").WithASRProfile("configured-asr"), 40)

	rows := occurrenceRows(t, world.st, "stored-provider-matrix")
	require.Len(rows, 9)
	for _, row := range rows {
		assert.Equal("retained", row.State, row.MessageID)
	}
	docbank.mu.Lock()
	assert.Len(docbank.uploads, 9)
	assert.Len(docbank.artifactOps, 2)
	profiles := make([]string, 0, len(docbank.processRequests))
	for _, request := range docbank.processRequests {
		profiles = append(profiles, request.Profile)
	}
	docbank.mu.Unlock()
	assert.GreaterOrEqual(len(profiles), 4)
	assert.Contains(profiles, "supplied-transcript")
	assert.Contains(profiles, "configured-asr")
}

func newTestDocbankServer(t *testing.T, docbank *fakeDocbank) *httptest.Server {
	t.Helper()
	return httptest.NewServer(docbank)
}

func TestStoredMediaProcessingChoice(t *testing.T) {
	assert := assert.New(t)
	descriptor := MediaDescriptor{SourceType: "whatsapp", SourceSHA256: strings.Repeat("a", 64),
		TranscriptSHA256: hashBytes([]byte("source words")), Language: "en"}
	supplied := configureMediaProcessing(descriptor, "source words", "configured-asr")
	assert.Equal("supplied-transcript", supplied.ProcessingProfile)
	assert.Equal("whatsapp", supplied.ProcessingProvider)
	assert.NotEmpty(supplied.ProcessingKey)
	asr := configureMediaProcessing(MediaDescriptor{SourceType: "whatsapp", SourceSHA256: descriptor.SourceSHA256}, "", "configured-asr")
	assert.Equal("configured-asr", asr.ProcessingProfile)
	assert.Equal("whatsapp", asr.ProcessingProvider)
	assert.Equal(mediaASRProcessingKey(asr, "configured-asr"), asr.ProcessingKey)
	empty := configureMediaProcessing(asr, "", "")
	assert.Empty(empty.ProcessingProfile)
	assert.Empty(empty.ProcessingKey)
	assert.NotEqual(supplied.ProcessingKey, asr.ProcessingKey)
}

func TestStoredMediaProfileReplay(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	world := importVoiceChat(t, voiceSpec{id: "asr-replay", asset: "mxc://beeper.local/asr-replay",
		mime: "audio/wav", fileName: "voice.wav", data: syntheticWAV(800, 62)})
	docbank := newFakeDocbank(t)
	docbank.coverage = "pending"
	server := newTestDocbankServer(t, docbank)
	defer server.Close()
	worker := world.submitter(t, server, "stored-profile-replay").WithASRProfile("configured-asr")
	runPasses(t, worker, 6)
	deliveries := deliveryRows(t, world.st, "stored-profile-replay")
	require.Len(deliveries, 1)
	assert.Equal("observing", deliveries[0].Phase)
	assert.NotEmpty(deliveries[0].Donor)
	rows := occurrenceRows(t, world.st, "stored-profile-replay")
	require.Len(rows, 1)
	assert.NotEqual(rows[0].Ref, deliveries[0].Donor)
	var profile string
	require.NoError(world.st.DB().QueryRow(`SELECT profile FROM beeper_media_deliveries WHERE destination_key = ?`,
		"stored-profile-replay").Scan(&profile))
	assert.Equal("configured-asr", profile)

	docbank.mu.Lock()
	docbank.sourceOperationID = "other-operation"
	docbank.coverage = "transcribed"
	firstRequests := len(docbank.processRequests)
	docbank.mu.Unlock()
	_, err := world.st.DB().Exec(`UPDATE beeper_media_deliveries SET next_action_at = '2000-01-01 00:00:00.000'`)
	require.NoError(err)
	runPasses(t, worker, 1)
	deliveries = deliveryRows(t, world.st, "stored-profile-replay")
	require.Len(deliveries, 1)
	assert.Equal("done", deliveries[0].Phase)
	docbank.mu.Lock()
	assert.Equal(firstRequests+1, len(docbank.processRequests))
	assert.Equal("configured-asr", docbank.processRequests[len(docbank.processRequests)-1].Profile)
	docbank.mu.Unlock()
}

func TestStoredMediaProfileChangeStartedJob(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	world := importVoiceChat(t, voiceSpec{id: "profile-change", asset: "mxc://beeper.local/profile-change",
		mime: "audio/wav", fileName: "voice.wav", data: syntheticWAV(800, 63)})
	docbank := newFakeDocbank(t)
	docbank.coverage = "pending"
	server := newTestDocbankServer(t, docbank)
	defer server.Close()
	destination := "stored-profile-change"
	worker := world.submitter(t, server, destination).WithASRProfile("old-asr")
	runPasses(t, worker, 6)
	oldDeliveries := deliveryRows(t, world.st, destination)
	require.Len(oldDeliveries, 1)
	require.Equal("observing", oldDeliveries[0].Phase)
	oldKey := processingKeyForDestination(t, world.st, destination, "old-asr")
	archiveUID, err := world.st.ArchiveUIDContext(t.Context())
	require.NoError(err)
	candidates, err := world.st.ListBeeperMediaCandidates(t.Context(), 0, 10)
	require.NoError(err)
	var current store.BeeperMediaCandidate
	for _, candidate := range candidates {
		if candidate.SourceMessageID == "profile-change" {
			current = candidate
			break
		}
	}
	require.NotZero(current.AttachmentID)
	updated := world.submitter(t, server, destination).WithASRProfile("new-asr")
	_, err = updated.reconcileCandidate(t.Context(), archiveUID, current)
	require.NoError(err)
	newKey := processingKeyForDestination(t, world.st, destination, "new-asr")
	assert.NotEqual(oldKey, newKey)
	_, err = world.st.DB().Exec(`UPDATE beeper_media_deliveries SET next_action_at = '2000-01-01 00:00:00.000'
		WHERE destination_key = ? AND processing_key = ?`, destination, oldKey)
	require.NoError(err)
	docbank.mu.Lock()
	docbank.coverage = "transcribed"
	docbank.mu.Unlock()
	operation, ok, err := world.st.NextBeeperMediaOperation(t.Context(), destination, time.Now().UTC())
	require.NoError(err)
	require.True(ok)
	assert.Equal(store.BeeperMediaOperationStatus, operation.Kind)
	assert.Equal(oldKey, operation.ProcessingKey)
	assert.NotEmpty(operation.OccurrenceRef)
	assert.NotEmpty(operation.Revision)
	require.NoError(updated.status(t.Context(), t.Context(), operation))
	deliveries := processDeliveryIdentities(t, world.st, destination)
	assert.Equal("done", findProcessDelivery(t, deliveries, oldKey).phase)
	assert.Equal("pending-process", findProcessDelivery(t, deliveries, newKey).phase)
	oldMappings, err := world.st.ListLiveBeeperMediaMappings(t.Context(), destination, oldKey, 10)
	require.NoError(err)
	assert.Empty(oldMappings)
	newMappings, err := world.st.ListLiveBeeperMediaMappings(t.Context(), destination, newKey, 10)
	require.NoError(err)
	require.Len(newMappings, 1)
	assert.Equal(newKey, newMappings[0].ProcessingKey)
	assert.Equal("pending-process", newMappings[0].ProcessingPhase)
	assert.Equal("new-asr", newMappings[0].ProcessingProfile)
	assert.Empty(newMappings[0].ProcessingCoverage)
}

func TestStoredMediaStartedReceiptIdentityMismatch(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	world := importVoiceChat(t, voiceSpec{id: "identity-mismatch", asset: "mxc://beeper.local/identity-mismatch",
		mime: "audio/wav", fileName: "voice.wav", data: syntheticWAV(800, 64)})
	docbank := newFakeDocbank(t)
	docbank.coverage = "pending"
	server := newTestDocbankServer(t, docbank)
	defer server.Close()
	destination := "stored-identity-mismatch"
	worker := world.submitter(t, server, destination).WithASRProfile("configured-asr")
	runPasses(t, worker, 6)
	started := deliveryRows(t, world.st, destination)
	require.Len(started, 1)
	require.Equal("observing", started[0].Phase)
	for _, mismatch := range []struct {
		column string
		value  string
	}{
		{column: "source_id", value: "wrong-source"},
		{column: "source_version_id", value: "wrong-source-version"},
		{column: "content_version_id", value: "wrong-content-version"},
	} {
		_, err := world.st.DB().Exec("UPDATE beeper_media_deliveries SET "+mismatch.column+" = ?, next_action_at = '2000-01-01 00:00:00.000' WHERE destination_key = ?",
			mismatch.value, destination)
		require.NoError(err)
		_, ready, err := world.st.NextBeeperMediaOperation(t.Context(), destination, time.Now().UTC())
		require.NoError(err)
		assert.False(ready, mismatch.column)
		_, err = world.st.DB().Exec(`UPDATE beeper_media_deliveries
			SET source_id = ?, source_version_id = ?, content_version_id = ?, next_action_at = '2000-01-01 00:00:00.000'
			WHERE destination_key = ?`, started[0].SourceID, started[0].SourceVersionID,
			started[0].ContentVersionID, destination)
		require.NoError(err)
	}
	docbank.mu.Lock()
	docbank.sourceVersionID = "wrong-source-version"
	docbank.sourceContentVersionID = "wrong-content-version"
	docbank.coverage = "transcribed"
	processes := len(docbank.processOps)
	docbank.mu.Unlock()
	_, err := world.st.DB().Exec(`UPDATE beeper_media_deliveries SET next_action_at = '2000-01-01 00:00:00.000'
		WHERE destination_key = ?`, destination)
	require.NoError(err)
	runPasses(t, worker, 1)
	ended := deliveryRows(t, world.st, destination)
	require.Len(ended, 1)
	assert.Equal("blocked", ended[0].Phase)
	assert.Equal("destination_mismatch", ended[0].ErrorCode)
	docbank.mu.Lock()
	assert.Equal(processes, len(docbank.processOps))
	docbank.mu.Unlock()
}

func processingKeyForDestination(t *testing.T, st *store.Store, destination, profile string) string {
	t.Helper()
	var key string
	require.NoError(t, st.DB().QueryRow(`SELECT processing_key FROM beeper_media_deliveries
		WHERE destination_key = ? AND profile = ?`, destination, profile).Scan(&key))
	return key
}
