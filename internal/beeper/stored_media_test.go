package beeper

import (
	"database/sql"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/whatsapp"
)

// addStoredMediaSource adds an importer-owned row around bytes already present
// in the test CAS. It intentionally leaves raw evidence optional.
func addStoredMediaSource(
	t *testing.T, world *mediaWorld, sourceType, identifier, messageID string,
	data []byte, filename, mimeType, mediaType string, role store.AttachmentRole,
	state attachmentpolicy.DownloadState, raw []byte, sourceAttachmentID, sourcePartKey string,
) string {
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
	stored := &mime.Attachment{Filename: filename, ContentType: mimeType, Content: data}
	storagePath, err := export.StoreAttachmentFile(world.dir, stored)
	require.NoError(t, err)
	require.NoError(t, world.st.UpsertAttachmentRecord(t.Context(), message, store.AttachmentWrite{
		Filename: filename, MIMEType: mimeType, StoragePath: storagePath,
		ContentHash: stored.ContentHash, Size: int64(len(data)), SourceAttachmentID: sourceAttachmentID,
		SourcePartKey: sourcePartKey, MediaType: mediaType,
		State: state, Role: role, RoleSource: store.AttachmentRoleSourceImporterSemantics,
	}))
	return stored.ContentHash
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
	addStoredMediaSource(t, world, "whatsapp", "+15555550101", "3EB0-message-1", wav,
		"voice.bin", "application/octet-stream", "", store.AttachmentRoleStandalone, "", nil, "", "whatsapp:media")
	addStoredMediaSource(t, world, "facebook_messenger", "test.user@facebook.messenger", "message:fb-1", wav,
		"voice.wav", "audio/wav", "", store.AttachmentRoleUnknown, "", []byte("not json"),
		"attachments/audio-1.mp3", "fbmessenger:attachment:a602fedf39561086320b2483a3dfac563bc7453ea3e224bd5aea06e7c4d3f79e")
	addStoredMediaSource(t, world, "synctech_sms", "+15555550102", "mms:1", wav,
		"voice.wav", "", "", store.AttachmentRoleStandalone, "", nil, "", "synctech:mms:1")
	addStoredMediaSource(t, world, "slack", "T01:U01", "1712345678.000001", wav,
		"voice.wav", "audio/wav", "", store.AttachmentRoleStandalone, attachmentpolicy.StateStored, nil,
		"slack:F_FILE1", "slack:F_FILE1")
	addStoredMediaSource(t, world, "discord", "200", "401", wav,
		"voice.wav", "audio/wav", "", store.AttachmentRoleStandalone, attachmentpolicy.StateStored, nil,
		"discord:401", "discord:401")
	addStoredMediaSource(t, world, "google_voice", "+15555550103", "voicemail-1", mp3,
		"voicemail.mp3", "application/octet-stream", "", store.AttachmentRoleStandalone, attachmentpolicy.StateStored, nil,
		"gvoice:voicemail:audio", "gvoice:voicemail:audio")
	addStoredMediaSource(t, world, "future_provider", "future-account", "future-message", wav,
		"recording", "", "", store.AttachmentRoleStandalone, "", nil, "future:media:1", "future:media:1")

	docbank := newFakeDocbank(t)
	server := newTestDocbankServer(t, docbank)
	defer server.Close()
	runPasses(t, world.submitter(t, server, "stored-provider-matrix").WithASRProfile("configured-asr"), 40)

	rows := occurrenceRows(t, world.st, "stored-provider-matrix")
	require.Len(rows, 9)
	providers := make(map[string]string, len(rows))
	for _, row := range rows {
		assert.Equal("retained", row.State, row.MessageID)
		providers[row.MessageID] = row.SourceType
	}
	assert.Equal(map[string]string{
		"beeper-supplied": "beeper", "beeper-asr": "beeper", "3EB0-message-1": "whatsapp",
		"message:fb-1": "facebook_messenger", "mms:1": "synctech_sms", "1712345678.000001": "slack",
		"401": "discord", "voicemail-1": "google_voice", "future-message": "future_provider",
	}, providers)
	docbank.mu.Lock()
	assert.Len(docbank.uploads, 9)
	assert.Len(docbank.artifactOps, 1)
	assert.Equal([]string{"provider words"}, docbank.transcripts)
	profiles := make([]string, 0, len(docbank.processRequests))
	for _, request := range docbank.processRequests {
		profiles = append(profiles, request.Profile)
	}
	docbank.mu.Unlock()
	assert.GreaterOrEqual(len(profiles), 4)
	assert.Contains(profiles, "supplied-transcript")
	assert.Contains(profiles, "configured-asr")
}

func TestStoredMediaEmptySlackExportDoesNotBlockLaterAudio(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	world := importVoiceChat(t)
	source, err := world.st.GetOrCreateSource("slack", "T01:EMPTY")
	require.NoError(err)
	conversation, err := world.st.EnsureConversation(source.ID, "slack-thread", "Thread")
	require.NoError(err)
	message, err := world.st.UpsertMessage(&store.Message{
		ConversationID: conversation, SourceID: source.ID, SourceMessageID: "empty-first", MessageType: "slack",
	})
	require.NoError(err)
	empty := &mime.Attachment{Filename: "empty.wav", ContentType: "audio/wav", Content: []byte{}}
	storagePath, err := export.StoreAttachmentFileIncludingEmpty(world.dir, empty)
	require.NoError(err)
	require.NotEmpty(storagePath)
	require.NoError(world.st.UpsertAttachmentRecord(t.Context(), message, store.AttachmentWrite{
		Filename: empty.Filename, MIMEType: empty.ContentType, StoragePath: storagePath,
		ContentHash: empty.ContentHash, SourceAttachmentID: "slack:empty", SourcePartKey: "slack:empty",
		MediaType: "audio", Role: store.AttachmentRoleStandalone,
		RoleSource: store.AttachmentRoleSourceProviderExplicit, State: attachmentpolicy.StateStored,
	}))
	addStoredMediaSource(t, world, "slack", "T01:EMPTY", "valid-after-empty", syntheticWAV(800, 71),
		"voice.wav", "audio/wav", "audio", store.AttachmentRoleStandalone, attachmentpolicy.StateStored, nil,
		"slack:valid", "slack:valid")
	docbank := newFakeDocbank(t)
	server := newTestDocbankServer(t, docbank)
	defer server.Close()
	worker := world.submitter(t, server, "stored-empty-slack").WithASRProfile("configured-asr")
	result, err := worker.RunBatch(t.Context())
	require.NoError(err)
	assert.Equal(1, result.Examined)
	rows := occurrenceRows(t, world.st, "stored-empty-slack")
	require.Len(rows, 1)
	assert.Equal("valid-after-empty", rows[0].MessageID)
}

func TestStoredMediaEmailFallback(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	world := importVoiceChat(t)
	wav := syntheticWAV(800, 70)
	addStoredMediaSource(t, world, "gmail", "rod@example.com", "mail-audio-1", wav,
		"meeting.wav", "application/octet-stream", "", store.AttachmentRoleStandalone, "", nil,
		"mail:attachment:audio", "mime:1.2")
	addStoredMediaSource(t, world, "gmail", "rod@example.com", "mail-document-1", []byte("%PDF-1.7\narchive"),
		"report.pdf", "application/pdf", "", store.AttachmentRoleStandalone, "", nil,
		"mail:attachment:document", "mime:1.3")
	destination := "stored-email-fallback"
	local := NewMediaSubmitter(world.st, world.blobs, nil, destination, world.dir).WithASRProfile("email-asr")
	runPasses(t, local, 1)
	rows := occurrenceRows(t, world.st, destination)
	require.Len(rows, 2)
	states := make(map[string]string, len(rows))
	for _, row := range rows {
		states[row.MessageID] = row.State + ":" + row.ErrorCode
		assert.Equal("gmail", row.SourceType)
	}
	assert.Equal(map[string]string{
		"mail-audio-1": "pending:", "mail-document-1": "blocked:unsupported_media",
	}, states)
	var documentOperationID string
	require.NoError(world.st.DB().QueryRow(world.st.Rebind(`
		SELECT retention_operation_id FROM beeper_media_occurrences
		WHERE destination_key = ? AND source_message_id = 'mail-document-1'`), destination).Scan(&documentOperationID))
	assert.Empty(documentOperationID)

	docbank := newFakeDocbank(t)
	server := newTestDocbankServer(t, docbank)
	defer server.Close()
	worker := world.submitter(t, server, destination).WithASRProfile("email-asr")
	runPasses(t, worker, 5)
	rows = occurrenceRows(t, world.st, destination)
	states = make(map[string]string, len(rows))
	for _, row := range rows {
		states[row.MessageID] = row.State + ":" + row.ErrorCode
	}
	assert.Equal(map[string]string{
		"mail-audio-1": "retained:", "mail-document-1": "blocked:unsupported_media",
	}, states)
	docbank.mu.Lock()
	assert.Equal([][]byte{wav}, docbank.uploads)
	assert.Empty(docbank.artifactOps)
	assert.Equal([]string{"email-asr"}, []string{docbank.processRequests[0].Profile})
	docbank.mu.Unlock()
}

func TestStoredMediaRealWhatsAppImportWithoutRemoteRoute(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	world := importVoiceChat(t)
	mediaDir := t.TempDir()
	wav := syntheticWAV(800, 72)
	require.NoError(os.WriteFile(filepath.Join(mediaDir, "voice.wav"), wav, 0o600))
	waDBPath := filepath.Join(t.TempDir(), "msgstore.db")
	waDB, err := sql.Open("sqlite3", waDBPath)
	require.NoError(err)
	_, err = waDB.Exec(`
		PRAGMA journal_mode=WAL;
		CREATE TABLE jid (_id INTEGER PRIMARY KEY, user TEXT, server TEXT, raw_string TEXT);
		CREATE TABLE chat (_id INTEGER PRIMARY KEY, jid_row_id INTEGER UNIQUE, hidden INTEGER,
			subject TEXT, sort_timestamp INTEGER);
		CREATE TABLE message (_id INTEGER PRIMARY KEY, chat_row_id INTEGER, from_me INTEGER,
			key_id TEXT, sender_jid_row_id INTEGER, timestamp INTEGER, message_type INTEGER,
			text_data TEXT, status INTEGER, starred INTEGER);
		CREATE TABLE message_media (message_row_id INTEGER PRIMARY KEY, mime_type TEXT,
			file_size INTEGER, file_path TEXT, width INTEGER, height INTEGER, media_duration INTEGER);
		INSERT INTO jid VALUES (1, '15555550101', 's.whatsapp.net', '15555550101@s.whatsapp.net');
		INSERT INTO chat VALUES (10, 1, 0, NULL, 2000);
		INSERT INTO message VALUES (100, 10, 1, 'voice-message', NULL, 1000, 13,
			'caption text', 0, 0);
		INSERT INTO message_media VALUES (100, 'audio/wav', 0, 'voice.wav', NULL, NULL, NULL);
	`)
	require.NoError(err)
	require.NoError(waDB.Close())
	summary, err := whatsapp.NewImporter(world.st, nil).Import(t.Context(), waDBPath, whatsapp.ImportOptions{
		Phone: "+15555550100", MediaDir: mediaDir, AttachmentsDir: world.dir,
	})
	require.NoError(err)
	assert.Equal(int64(1), summary.MediaCopied)
	candidates, err := world.st.ListBeeperMediaCandidates(t.Context(), 0, 10)
	require.NoError(err)
	require.Len(candidates, 1)
	assert.Equal("whatsapp", candidates[0].SourceType)
	assert.Empty(candidates[0].AttachmentState)
	destination := "stored-real-whatsapp"
	worker := NewMediaSubmitter(world.st, world.blobs, nil, destination, world.dir).WithASRProfile("import-asr")
	result, err := worker.RunBatch(t.Context())
	require.NoError(err)
	assert.Equal(1, result.Examined)
	assert.Equal(1, result.Pending)
	assert.Zero(result.Blocked)
	rows := occurrenceRows(t, world.st, destination)
	require.Len(rows, 1)
	assert.Equal("whatsapp", rows[0].SourceType)
	assert.Equal("pending", rows[0].State)
	var partKey string
	require.NoError(world.st.DB().QueryRow(world.st.Rebind(`SELECT source_part_key FROM attachments
		WHERE content_hash = ?`), sha256Hex(wav)).Scan(&partKey))
	assert.Equal("whatsapp:media", partKey)

	docbank := newFakeDocbank(t)
	server := newTestDocbankServer(t, docbank)
	defer server.Close()
	runPasses(t, world.submitter(t, server, destination).WithASRProfile("import-asr"), 4)
	rows = occurrenceRows(t, world.st, destination)
	require.Len(rows, 1)
	assert.Equal("retained", rows[0].State)
	docbank.mu.Lock()
	assert.Equal([][]byte{wav}, docbank.uploads)
	assert.Empty(docbank.artifactOps)
	assert.Equal("import-asr", docbank.processRequests[0].Profile)
	docbank.mu.Unlock()
}

func TestStoredMediaUnavailableCASTries(t *testing.T) {
	for _, mode := range []string{"missing", "corrupt", "corrupt-header"} {
		t.Run(mode, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			world := importVoiceChat(t)
			wav := syntheticWAV(800, 71)
			hash := addStoredMediaSource(t, world, "gmail", "rod@example.com", "mail-"+mode, wav,
				"voice.wav", "audio/wav", "", store.AttachmentRoleStandalone, "", nil,
				"mail:attachment:"+mode, "mime:1.2")
			path := filepath.Join(world.dir, hash[:2], hash)
			if mode == "missing" {
				require.NoError(os.Remove(path))
			} else {
				corrupt := append([]byte(nil), wav...)
				if mode == "corrupt-header" {
					corrupt[0] ^= 1
				} else {
					corrupt[len(corrupt)-1] ^= 1
				}
				require.NoError(os.WriteFile(path, corrupt, 0o600))
			}
			docbank := newFakeDocbank(t)
			server := newTestDocbankServer(t, docbank)
			defer server.Close()
			runPasses(t, world.submitter(t, server, "stored-email-cas-"+mode), 1)
			rows := occurrenceRows(t, world.st, "stored-email-cas-"+mode)
			require.Len(rows, 1)
			assert.Equal("source_unavailable", rows[0].State)
			assert.Equal("source_unavailable", rows[0].ErrorCode)
			assert.NotEmpty(rows[0].OperationID)
			docbank.mu.Lock()
			assert.Empty(docbank.uploads)
			docbank.mu.Unlock()
		})
	}
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
	require.NoError(world.st.DB().QueryRow(world.st.Rebind(`SELECT profile FROM beeper_media_deliveries WHERE destination_key = ?`),
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
	_, err = world.st.DB().Exec(world.st.Rebind(`UPDATE beeper_media_deliveries SET next_action_at = '2000-01-01 00:00:00.000'
		WHERE destination_key = ? AND processing_key = ?`), destination, oldKey)
	require.NoError(err)
	docbank.mu.Lock()
	docbank.coverage = "transcribed"
	docbank.mu.Unlock()
	operation, ok, err := world.st.NextBeeperMediaOperation(t.Context(), destination, time.Now().UTC())
	require.NoError(err)
	require.True(ok)
	assert.Equal(store.BeeperMediaOperationStatus, operation.Kind)
	assert.Equal(oldKey, operation.ProcessingKey)
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
		_, err := world.st.DB().Exec(world.st.Rebind("UPDATE beeper_media_deliveries SET "+mismatch.column+" = ?, next_action_at = '2000-01-01 00:00:00.000' WHERE destination_key = ?"),
			mismatch.value, destination)
		require.NoError(err)
		_, ready, err := world.st.NextBeeperMediaOperation(t.Context(), destination, time.Now().UTC())
		require.NoError(err)
		assert.False(ready, mismatch.column)
		_, err = world.st.DB().Exec(world.st.Rebind(`UPDATE beeper_media_deliveries
			SET source_id = ?, source_version_id = ?, content_version_id = ?, next_action_at = '2000-01-01 00:00:00.000'
			WHERE destination_key = ?`), started[0].SourceID, started[0].SourceVersionID,
			started[0].ContentVersionID, destination)
		require.NoError(err)
	}
	docbank.mu.Lock()
	docbank.sourceVersionID = "wrong-source-version"
	docbank.sourceContentVersionID = "wrong-content-version"
	docbank.coverage = "transcribed"
	processes := len(docbank.processOps)
	docbank.mu.Unlock()
	_, err := world.st.DB().Exec(world.st.Rebind(`UPDATE beeper_media_deliveries SET next_action_at = '2000-01-01 00:00:00.000'
		WHERE destination_key = ?`), destination)
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
	require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT processing_key FROM beeper_media_deliveries
		WHERE destination_key = ? AND profile = ?`), destination, profile).Scan(&key))
	return key
}
