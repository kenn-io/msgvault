package api

import (
	"cmp"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/beeper"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/docbankmedia"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

const (
	recordingsTestAPIKey      = "recordings-test-key"
	recordingsTestDestination = "reader"
)

// fakeDocbank answers the transcript route with Docbank's field names, keyed
// by the requested content version.
type fakeDocbank struct {
	mu        sync.Mutex
	responses map[string]func(http.ResponseWriter)
	requests  int
	server    *httptest.Server
}

func newFakeDocbank(t *testing.T) *fakeDocbank {
	t.Helper()
	fake := &fakeDocbank{responses: map[string]func(http.ResponseWriter){}}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		fake.requests++
		respond := fake.responses[r.URL.Query().Get("content_version_id")]
		fake.mu.Unlock()
		if !strings.HasSuffix(r.URL.Path, "/transcript") || respond == nil {
			http.NotFound(w, r)
			return
		}
		respond(w)
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

func (fake *fakeDocbank) set(contentVersionID string, respond func(http.ResponseWriter)) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.responses[contentVersionID] = respond
}

func (fake *fakeDocbank) requestCount() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.requests
}

// evidence answers with one exact version's transcript evidence.
func (fake *fakeDocbank) evidence(
	r recordingSeed, vault, evidenceState, operationState string, transcript map[string]any,
) {
	fake.set(r.contentVersionID, evidenceResponse(r, vault, evidenceState, operationState, transcript))
}

func evidenceResponse(
	r recordingSeed, vault, evidenceState, operationState string, transcript map[string]any,
) func(http.ResponseWriter) {
	if transcript != nil && transcript["origin"] == "supplied" {
		if _, ok := transcript["supplied_input_id"]; !ok {
			transcript["supplied_input_id"] = r.suppliedInputID
		}
	}
	return func(w http.ResponseWriter) {
		body := map[string]any{
			"vault_uid": vault, "source_id": r.sourceID, "source_version_id": "version",
			"content_version_id": r.contentVersionID, "evidence_state": evidenceState,
			"coverage_state": "complete", "operation_state": operationState,
		}
		if transcript != nil {
			body["transcript"] = transcript
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}
}

type recordingSeed struct {
	messageID        int64
	attachmentID     int64
	sourceID         string
	contentVersionID string
	suppliedInputID  string
}

type recordingFixture struct {
	f       *storetest.Fixture
	docbank *fakeDocbank
	client  *docbankmedia.Client
	parts   map[string][]string
	// transcripts holds each message's provider transcript text.
	transcripts map[string]string
	// finished counts finished deliveries so each one finishes after the last.
	finished int
	// sameAudio maps a message to another whose audio bytes it shares.
	sameAudio map[string]string
}

// audioHash is the content hash of a message part's audio bytes.
func (rf *recordingFixture) audioHash(sourceMessageID, part string) string {
	digest := sha256.Sum256([]byte(cmp.Or(rf.sameAudio[sourceMessageID], sourceMessageID) + part))
	return hex.EncodeToString(digest[:])
}

func newRecordingFixture(t *testing.T) *recordingFixture {
	t.Helper()
	f := storetest.New(t)
	_, err := f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE sources SET source_type = 'beeper', identifier = ? WHERE id = ?`), "signal", f.Source.ID)
	require.NoError(t, err)
	docbank := newFakeDocbank(t)
	client, err := docbankmedia.NewClient(docbank.server.URL, func() (string, error) { return "docbank-key", nil })
	require.NoError(t, err)
	return &recordingFixture{f: f, docbank: docbank, client: client, parts: map[string][]string{},
		transcripts: map[string]string{}, sameAudio: map[string]string{}}
}

func (rf *recordingFixture) server(consent bool) *Server {
	return NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIKey: recordingsTestAPIKey}},
		Store:  rf.f.Store,
		Logger: testLogger(),
		MessageRecordings: &MessageRecordingReader{
			Store: rf.f.Store, Client: rf.client, Destination: recordingsTestDestination, UploadConsent: consent,
		},
	})
}

// message writes a Beeper message with an authored body.
func (rf *recordingFixture) message(t *testing.T, sourceMessageID string) int64 {
	t.Helper()
	st := rf.f.Store
	messageID, err := st.UpsertMessage(&store.Message{ConversationID: rf.f.ConvID, SourceID: rf.f.Source.ID,
		SourceMessageID: sourceMessageID, MessageType: "beeper", SizeEstimate: 100})
	require.NoError(t, err)
	require.NoError(t, st.UpsertMessageBody(messageID,
		sql.NullString{String: "authored body " + sourceMessageID, Valid: true}, sql.NullString{}))
	rf.parts[sourceMessageID] = []string{""}
	rf.writeRaw(t, messageID, sourceMessageID)
	return messageID
}

// writeRaw saves the Beeper message JSON with one voice note per known part,
// each carrying the message's provider transcript when one is set.
func (rf *recordingFixture) writeRaw(t *testing.T, messageID int64, sourceMessageID string) []byte {
	t.Helper()
	transcript := rf.transcripts[sourceMessageID]
	attachments := make([]map[string]any, 0, len(rf.parts[sourceMessageID]))
	for _, part := range rf.parts[sourceMessageID] {
		attachment := map[string]any{"id": "mxc://audio/" + sourceMessageID + part, "isVoiceNote": true}
		if transcript != "" {
			attachment["transcription"] = map[string]any{"transcription": transcript}
		}
		attachments = append(attachments, attachment)
	}
	raw, err := json.Marshal(map[string]any{"id": sourceMessageID, "attachments": attachments})
	require.NoError(t, err)
	require.NoError(t, rf.f.Store.UpsertMessageRawWithFormat(messageID, raw, "beeper_json"))
	return raw
}

// storedAudio adds a stored voice note to the message without recording an
// occurrence, as before the media worker's discovery reaches it. It returns
// the attachment ID and the message's rewritten raw JSON.
func (rf *recordingFixture) storedAudio(
	t *testing.T, messageID int64, sourceMessageID, part string,
) (int64, []byte) {
	t.Helper()
	st := rf.f.Store
	hash := rf.audioHash(sourceMessageID, part)
	partKey := "beeper:mxc://audio/" + sourceMessageID + part
	require.NoError(t, st.UpsertAttachmentRecord(t.Context(), messageID, store.AttachmentWrite{
		Filename: "voice" + part + ".wav", MIMEType: "audio/wav", StoragePath: hash[:2] + "/" + hash,
		ContentHash: hash, Size: 44, SourceAttachmentID: partKey, SourcePartKey: partKey,
		MediaType: "voice_note", State: attachmentpolicy.StateStored, Role: store.AttachmentRoleStandalone,
		RoleSource: store.AttachmentRoleSourceImporterSemantics,
	}))
	var attachmentID int64
	require.NoError(t, st.DB().QueryRow(st.Rebind(
		`SELECT id FROM attachments WHERE message_id = ? AND content_hash = ?`), messageID, hash).Scan(&attachmentID))
	if !slices.Contains(rf.parts[sourceMessageID], part) {
		rf.parts[sourceMessageID] = append(rf.parts[sourceMessageID], part)
	}
	return attachmentID, rf.writeRaw(t, messageID, sourceMessageID)
}

// audio adds a stored voice note to the message and records its occurrence.
// A nil result leaves the occurrence pending; otherwise it finishes the
// retain operation with result.
func (rf *recordingFixture) audio(
	t *testing.T, messageID int64, sourceMessageID, part, processingKey string, result *store.BeeperMediaResult,
) recordingSeed {
	t.Helper()
	st := rf.f.Store
	attachmentID, raw := rf.storedAudio(t, messageID, sourceMessageID, part)
	hash := rf.audioHash(sourceMessageID, part)
	partKey := "beeper:mxc://audio/" + sourceMessageID + part
	rawDigest := sha256.Sum256(raw)
	revision, err := beeper.MediaRevision(t.Context(), st, attachmentID)
	require.NoError(t, err)
	transcriptHash := ""
	if processingKey != "" {
		transcriptHash = strings.Repeat("b", 64)
	}
	ref := "msgvault:" + sourceMessageID + part
	mapping := store.BeeperMediaMapping{
		DestinationKey: recordingsTestDestination, OccurrenceRef: ref, Revision: revision,
		SourceType: "beeper", SourceIdentifier: "signal", SourceConversationID: "default-thread",
		SourceMessageID: sourceMessageID, SourceAttachmentID: partKey, SourcePartKey: partKey,
		MessageID: messageID, AttachmentID: attachmentID, SourceSHA256: hash, ByteLength: 44,
		RawHash: hex.EncodeToString(rawDigest[:]), TranscriptSHA256: transcriptHash,
		OccurrenceJSON: `{"ref":"` + ref + `","revision":"` + revision + `"}`, Filename: "voice.wav", MIMEType: "audio/wav",
		ProcessingKey: processingKey, ProcessingProvider: "beeper", ProcessingProfile: "supplied-transcript",
	}
	require.NoError(t, st.ReconcileBeeperMediaMapping(t.Context(), mapping))
	seed := recordingSeed{messageID: messageID, attachmentID: attachmentID}
	if processingKey != "" {
		seed.suppliedInputID = "input-" + processingKey
		_, err := st.DB().Exec(st.Rebind(`UPDATE beeper_media_deliveries SET supplied_input_id = ?
			WHERE destination_key = ? AND processing_key = ?`), seed.suppliedInputID, recordingsTestDestination, processingKey)
		require.NoError(t, err)
	}
	if result == nil {
		return seed
	}
	prepared, err := st.PrepareBeeperMediaOperation(t.Context(), store.BeeperMediaOperation{
		Kind: store.BeeperMediaOperationRetain, DestinationKey: recordingsTestDestination,
		OccurrenceRef: ref, Revision: revision,
	})
	require.NoError(t, err)
	// Preset Docbank IDs stand for Docbank reusing them for the same audio bytes.
	finished := *result
	if finished.ErrorCode == "" {
		finished.DocbankSourceID = cmp.Or(finished.DocbankSourceID, "source-"+hash[:8])
		finished.SourceVersionID = "version"
		finished.ContentVersionID = cmp.Or(finished.ContentVersionID, uuid.NewString())
		finished.DocbankOccurrenceID = "occurrence-" + hash[:8]
		finished.CoverageState = "unprocessed"
		seed.sourceID, seed.contentVersionID = finished.DocbankSourceID, finished.ContentVersionID
	}
	applied, err := st.FinishBeeperMediaOperation(t.Context(), prepared, finished)
	require.NoError(t, err)
	require.True(t, applied)
	return seed
}

// retained adds a retained voice note whose provider transcript delivery succeeded.
func (rf *recordingFixture) retained(t *testing.T, sourceMessageID string) recordingSeed {
	t.Helper()
	messageID := rf.message(t, sourceMessageID)
	processingKey := "delivery-" + sourceMessageID
	seed := rf.audio(t, messageID, sourceMessageID, "", processingKey, &store.BeeperMediaResult{VaultUID: "vault"})
	rf.deliver(t, processingKey, "succeeded")
	return seed
}

// deliver finishes the delivery for processingKey with operationState, later
// than every delivery finished before it.
func (rf *recordingFixture) deliver(t *testing.T, processingKey, operationState string) {
	t.Helper()
	st := rf.f.Store
	rf.finished++
	finishedAt := fmt.Sprintf("2026-01-01 00:%02d:%02d", rf.finished/60, rf.finished%60)
	result, err := st.DB().Exec(st.Rebind(`UPDATE beeper_media_deliveries
		SET phase = 'done', operation_state = ?, updated_at = ?
		WHERE destination_key = ? AND processing_key = ?`),
		operationState, finishedAt, recordingsTestDestination, processingKey)
	require.NoError(t, err)
	updated, err := result.RowsAffected()
	require.NoError(t, err)
	require.Equal(t, int64(1), updated, processingKey)
}

func getRecordings(t *testing.T, srv *Server, messageID string) (int, MessageRecordingsResponse, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/messages/"+messageID+"/recordings", nil)
	req.Header.Set("X-Api-Key", recordingsTestAPIKey)
	req.RemoteAddr = "127.0.0.1:1"
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, req)
	var body MessageRecordingsResponse
	if response.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	}
	return response.Code, body, response.Body.String()
}

func recordingsFor(t *testing.T, srv *Server, messageID int64) []MessageRecording {
	t.Helper()
	status, body, raw := getRecordings(t, srv, strconv.FormatInt(messageID, 10))
	require.Equal(t, http.StatusOK, status, raw)
	require.Equal(t, messageID, body.MessageID)
	return body.Recordings
}

func readyEvidence(origin, completeness string, units ...map[string]any) map[string]any {
	list := make([]any, 0, len(units))
	for _, unit := range units {
		list = append(list, unit)
	}
	return map[string]any{
		"origin": origin, "completeness": completeness, "truncated": false, "has_omissions": false, "units": list,
	}
}

func TestMessageRecordingsStates(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	rf := newRecordingFixture(t)
	srv := rf.server(true)
	noConsent := rf.server(false)

	supplied := rf.retained(t, "supplied")
	rf.docbank.evidence(supplied, "vault", "ready", "succeeded", readyEvidence("supplied", "complete",
		map[string]any{"text": "synthetic transcript", "time_span": map[string]any{"start_ms": 0, "end_ms": 1500}, "speaker": "alice"},
		map[string]any{"text": "no timing"}))
	recordings := recordingsFor(t, srv, supplied.messageID)
	require.Len(recordings, 1)
	assert.Equal(MessageRecording{
		AttachmentID: supplied.attachmentID, Filename: "voice.wav", SizeBytes: 44, State: "ready",
		Transcript: &MessageTranscript{Origin: "supplied", Units: []MessageTranscriptUnit{
			{Text: "synthetic transcript", StartMS: new(int64(0)), EndMS: new(int64(1500)), Speaker: "alice"},
			{Text: "no timing"},
		}},
	}, recordings[0])
	_, _, raw := getRecordings(t, srv, strconv.FormatInt(supplied.messageID, 10))
	assert.Contains(raw, `{"text":"no timing"}`, "absent timing and speaker are omitted")

	generated := rf.retained(t, "generated")
	rf.docbank.evidence(generated, "vault", "ready", "succeeded", readyEvidence("generated", "partial",
		map[string]any{"text": "synthetic transcript"}))
	recordings = recordingsFor(t, srv, generated.messageID)
	require.Len(recordings, 1)
	require.NotNil(recordings[0].Transcript)
	assert.Equal("generated", recordings[0].Transcript.Origin)
	assert.True(recordings[0].Transcript.Partial)

	empty := rf.retained(t, "empty")
	rf.docbank.evidence(empty, "vault", "ready", "succeeded", readyEvidence("supplied", "complete"))
	_, _, raw = getRecordings(t, srv, strconv.FormatInt(empty.messageID, 10))
	assert.Contains(raw, `"units":[]`)

	cases := []struct {
		name          string
		seed          func() recordingSeed
		evidenceState string
		operation     string
		vault         string
		want          string
		wantNoConsent string
		deliveryPhase string
		deliveryState string
	}{
		{name: "docbank pending", evidenceState: "pending", operation: "running", want: "processing", wantNoConsent: "processing"},
		{name: "missing", evidenceState: "unavailable", operation: "succeeded", want: "missing", wantNoConsent: "missing"},
		{name: "failed", evidenceState: "unavailable", operation: "failed", want: "failed", wantNoConsent: "failed"},
		{name: "cancelled", evidenceState: "unavailable", operation: "cancelled", want: "failed", wantNoConsent: "failed"},
		{name: "queued after older failure", evidenceState: "unavailable", operation: "failed", deliveryPhase: "pending-process",
			want: "processing", wantNoConsent: "unavailable"},
		{name: "refused before admission", evidenceState: "unavailable", operation: "succeeded", deliveryPhase: "blocked",
			want: "unavailable", wantNoConsent: "unavailable"},
		{name: "refused after older failure", evidenceState: "unavailable", operation: "failed", deliveryPhase: "blocked",
			want: "unavailable", wantNoConsent: "unavailable"},
		{name: "blocked admitted failure", evidenceState: "unavailable", operation: "failed", deliveryPhase: "blocked", deliveryState: "running",
			want: "failed", wantNoConsent: "failed"},
		{name: "stale", evidenceState: "stale", operation: "succeeded", want: "unavailable", wantNoConsent: "unavailable"},
		{name: "unknown", evidenceState: "archived", operation: "succeeded", want: "unavailable", wantNoConsent: "unavailable"},
		{name: "vault mismatch", evidenceState: "unavailable", operation: "succeeded", vault: "other-vault",
			want: "unavailable", wantNoConsent: "unavailable"},
		{name: "queued delivery", evidenceState: "unavailable", operation: "succeeded",
			want: "processing", wantNoConsent: "unavailable", seed: func() recordingSeed {
				messageID := rf.message(t, "queued")
				return rf.audio(t, messageID, "queued", "", "delivery-key", &store.BeeperMediaResult{VaultUID: "vault"})
			}},
	}
	for i, tc := range cases {
		var seed recordingSeed
		if tc.seed != nil {
			seed = tc.seed()
		} else {
			seed = rf.retained(t, fmt.Sprintf("case-%d", i))
		}
		if tc.deliveryPhase != "" {
			_, err := rf.f.Store.DB().Exec(rf.f.Store.Rebind(`UPDATE beeper_media_deliveries SET phase = ?, operation_state = ? WHERE processing_key = ?`),
				tc.deliveryPhase, tc.deliveryState, "delivery-"+fmt.Sprintf("case-%d", i))
			require.NoError(err)
		}
		vault := tc.vault
		if vault == "" {
			vault = "vault"
		}
		rf.docbank.evidence(seed, vault, tc.evidenceState, tc.operation, nil)
		recordings = recordingsFor(t, srv, seed.messageID)
		require.Len(recordings, 1, tc.name)
		assert.Equal(tc.want, recordings[0].State, tc.name)
		assert.Nil(recordings[0].Transcript, tc.name)
		recordings = recordingsFor(t, noConsent, seed.messageID)
		require.Len(recordings, 1, tc.name)
		assert.Equal(tc.wantNoConsent, recordings[0].State, tc.name+" without consent")
	}

	// Local states never call Docbank.
	before := rf.docbank.requestCount()

	// Unmapped captured audio has no established processing work.
	awaitingMessage := rf.message(t, "awaiting")
	rf.storedAudio(t, awaitingMessage, "awaiting", "")
	assert.Equal("unavailable", recordingsFor(t, srv, awaitingMessage)[0].State)
	assert.Equal("unavailable", recordingsFor(t, noConsent, awaitingMessage)[0].State)
	rf.audio(t, awaitingMessage, "awaiting", "", "", nil)
	assert.Equal("unavailable", recordingsFor(t, srv, awaitingMessage)[0].State, "retention alone does not establish transcription work")

	unsupportedMessage := rf.message(t, "unsupported")
	rf.audio(t, unsupportedMessage, "unsupported", "", "", &store.BeeperMediaResult{ErrorCode: "unsupported_media"})
	assert.Equal("unsupported", recordingsFor(t, srv, unsupportedMessage)[0].State)

	for _, code := range []string{"", "source_unavailable", "source_changed"} {
		for _, profile := range []string{"", "supplied-transcript", "custom-asr"} {
			name := "source-missing-" + code + "-" + profile
			messageID := rf.message(t, name)
			key, want := "", "unavailable"
			if profile != "" {
				key, want = "delivery-"+name, "processing"
			}
			var result *store.BeeperMediaResult
			if code != "" {
				result = &store.BeeperMediaResult{SourceUnavailable: true, ErrorCode: code}
			}
			rf.audio(t, messageID, name, "", key, result)
			if profile != "" {
				_, err := rf.f.Store.DB().Exec(rf.f.Store.Rebind(`UPDATE beeper_media_deliveries SET profile = ? WHERE processing_key = ?`), profile, key)
				require.NoError(err)
			}
			assert.Equal(want, recordingsFor(t, srv, messageID)[0].State, name)
			assert.Equal("unavailable", recordingsFor(t, noConsent, messageID)[0].State, name)
		}
	}

	for _, source := range []string{"invalid raw", "missing part"} {
		name := "invalid-source-" + source
		messageID := rf.message(t, name)
		rf.audio(t, messageID, name, "", "delivery-"+name, nil)
		if source == "invalid raw" {
			require.NoError(rf.f.Store.UpsertMessageRawWithFormat(messageID, []byte("invalid"), "beeper_json"))
		} else {
			rf.parts[name] = nil
			rf.writeRaw(t, messageID, name)
		}
		assert.Equal("unavailable", recordingsFor(t, srv, messageID)[0].State, source)
		assert.Equal("unavailable", recordingsFor(t, noConsent, messageID)[0].State, source)
	}

	uncapturedMessage := rf.message(t, "uncaptured")
	require.NoError(rf.f.Store.UpsertAttachmentRecord(t.Context(), uncapturedMessage, store.AttachmentWrite{
		Filename: "late.ogg", MIMEType: "audio/ogg", Size: 12, SourceAttachmentID: "beeper:late",
		SourcePartKey: "beeper:late", MediaType: "voice_note", State: attachmentpolicy.StateFailed,
		SkipReason: attachmentpolicy.SkipFetchFailure, Role: store.AttachmentRoleStandalone,
		RoleSource: store.AttachmentRoleSourceImporterSemantics,
	}))
	recordings = recordingsFor(t, srv, uncapturedMessage)
	require.Len(recordings, 1)
	assert.Equal("late.ogg", recordings[0].Filename)
	assert.Equal("media_missing", recordings[0].State)
	assert.Equal(before, rf.docbank.requestCount())
}

func TestMessageRecordingsDocbankBudget(t *testing.T) {
	previous := messageRecordingDocbankBudget
	t.Cleanup(func() { messageRecordingDocbankBudget = previous })
	rf := newRecordingFixture(t)
	messageID := rf.message(t, "blocked")
	blocked := rf.audio(t, messageID, "blocked", "-a", "", &store.BeeperMediaResult{VaultUID: "vault"})
	rf.audio(t, messageID, "blocked", "-b", "", &store.BeeperMediaResult{ErrorCode: "unsupported_media"})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	rf.docbank.set(blocked.contentVersionID, func(http.ResponseWriter) { <-release })
	deadlineServer := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIKey: recordingsTestAPIKey}},
		Store:  rf.f.Store, Logger: testLogger(), RequestTimeout: 2 * time.Second,
		MessageRecordings: &MessageRecordingReader{
			Store: rf.f.Store, Client: rf.client, Destination: recordingsTestDestination, UploadConsent: true,
		},
	})
	for _, tc := range []struct {
		name   string
		budget time.Duration
		server *Server
	}{
		{"Docbank budget", time.Second, rf.server(true)},
		{"request deadline", previous, deadlineServer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			messageRecordingDocbankBudget = tc.budget
			recordings := recordingsFor(t, tc.server, messageID)
			require.Len(t, recordings, 2)
			assert.Equal(t, "unavailable", recordings[0].State)
			assert.Equal(t, "unsupported", recordings[1].State)
		})
	}
	t.Run("concurrent reads", func(t *testing.T) {
		require, assert := require.New(t), assert.New(t)
		messageRecordingDocbankBudget = 10 * time.Second
		messageID := rf.message(t, "pair")
		seeds := []recordingSeed{
			rf.audio(t, messageID, "pair", "-a", "", &store.BeeperMediaResult{VaultUID: "vault"}),
			rf.audio(t, messageID, "pair", "-b", "", &store.BeeperMediaResult{VaultUID: "vault"}),
		}
		// Cleanup releases a held read so the fake server can close if reads run in turn.
		both := make(chan struct{})
		release := sync.OnceFunc(func() { close(both) })
		t.Cleanup(release)
		var arrivals atomic.Int32
		for _, seed := range seeds {
			ready := evidenceResponse(seed, "vault", "ready", "succeeded", readyEvidence("generated", "complete",
				map[string]any{"text": "synthetic transcript"}))
			rf.docbank.set(seed.contentVersionID, func(w http.ResponseWriter) {
				if arrivals.Add(1) == int32(len(seeds)) {
					release()
				}
				<-both
				ready(w)
			})
		}

		recordings := recordingsFor(t, rf.server(true), messageID)
		require.Len(recordings, 2)
		for _, recording := range recordings {
			assert.Equal("ready", recording.State)
		}
	})
	t.Run("one warning", func(t *testing.T) {
		require, assert := require.New(t), assert.New(t)
		var logs strings.Builder
		var logMu sync.Mutex
		srv := NewServerWithOptions(ServerOptions{
			Config: &config.Config{Server: config.ServerConfig{APIKey: recordingsTestAPIKey}},
			Store:  rf.f.Store,
			Logger: slog.New(slog.NewTextHandler(lockedWriter{mu: &logMu, w: &logs}, nil)),
			MessageRecordings: &MessageRecordingReader{
				Store: rf.f.Store, Client: rf.client, Destination: recordingsTestDestination, UploadConsent: true,
			},
		})
		messageID := rf.message(t, "down")
		for _, part := range []string{"-a", "-b", "-c"} {
			seed := rf.audio(t, messageID, "down", part, "", &store.BeeperMediaResult{VaultUID: "vault"})
			rf.docbank.set(seed.contentVersionID, func(w http.ResponseWriter) { w.WriteHeader(http.StatusServiceUnavailable) })
		}

		recordings := recordingsFor(t, srv, messageID)
		require.Len(recordings, 3)
		logMu.Lock()
		defer logMu.Unlock()
		warnings := strings.Count(logs.String(), "read Docbank transcripts")
		assert.Equal(1, warnings, logs.String())
		assert.Contains(logs.String(), "failed=3")
	})
	t.Run("failure isolation", func(t *testing.T) {
		require, assert := require.New(t), assert.New(t)
		// One recording's Docbank failure never hides another.
		isolatedMessage := rf.message(t, "isolated")
		failing := rf.audio(t, isolatedMessage, "isolated", "-a", "", &store.BeeperMediaResult{VaultUID: "vault"})
		working := rf.audio(t, isolatedMessage, "isolated", "-b", "delivery-isolated-b", &store.BeeperMediaResult{VaultUID: "vault"})
		rf.deliver(t, "delivery-isolated-b", "succeeded")
		rf.docbank.evidence(working, "vault", "ready", "succeeded", readyEvidence("supplied", "complete",
			map[string]any{"text": "synthetic transcript"}))
		rf.docbank.set(failing.contentVersionID, func(w http.ResponseWriter) { w.WriteHeader(http.StatusServiceUnavailable) })
		recordings := recordingsFor(t, rf.server(true), isolatedMessage)
		require.Len(recordings, 2)
		assert.Equal("unavailable", recordings[0].State)
		assert.Equal("ready", recordings[1].State)
		require.NotNil(recordings[1].Transcript)
		assert.Equal("synthetic transcript", recordings[1].Transcript.Units[0].Text)
	})
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func TestMessageRecordingsVisibility(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	rf := newRecordingFixture(t)
	srv := rf.server(true)
	const secret = "synthetic transcript hidden mid-read"

	survivor := rf.message(t, "survivor")
	hidden := rf.retained(t, "hidden")
	ready := evidenceResponse(hidden, "vault", "ready", "succeeded", readyEvidence("generated", "complete",
		map[string]any{"text": secret}))
	var mergeErr error
	rf.docbank.set(hidden.contentVersionID, func(w http.ResponseWriter) {
		// The message is hidden as a duplicate while its transcript is being read.
		_, mergeErr = rf.f.Store.MergeDuplicates(survivor, []int64{hidden.messageID}, "batch")
		ready(w)
	})
	status, body, raw := getRecordings(t, srv, strconv.FormatInt(hidden.messageID, 10))
	require.NoError(mergeErr)
	require.Equal(http.StatusOK, status)
	assert.Empty(body.Recordings)
	assert.Contains(raw, `"recordings":[]`)
	assert.NotContains(raw, secret)
}

func TestMessageRecordingsProviderTranscriptEdit(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	rf := newRecordingFixture(t)
	srv := rf.server(true)
	noConsent := rf.server(false)
	const obsolete = "synthetic transcript before the edit"
	const replacement = "synthetic transcript after the edit"

	seed := func(sourceMessageID string) recordingSeed {
		rf.transcripts[sourceMessageID] = obsolete
		return rf.retained(t, sourceMessageID)
	}
	edit := func(r recordingSeed, sourceMessageID string) {
		rf.transcripts[sourceMessageID] = "synthetic transcript after the edit"
		rf.writeRaw(t, r.messageID, sourceMessageID)
	}
	supplied := func(r recordingSeed) func(http.ResponseWriter) {
		return evidenceResponse(r, "vault", "ready", "succeeded", readyEvidence("supplied", "complete",
			map[string]any{"text": obsolete}))
	}

	before := seed("before")
	rf.docbank.set(before.contentVersionID, supplied(before))
	recordings := recordingsFor(t, srv, before.messageID)
	require.Len(recordings, 1)
	assert.Equal("ready", recordings[0].State, "an unedited provider transcript shows")

	// Editing the provider transcript keeps the audio, so only the revision tells.
	edit(before, "before")
	for server, want := range map[*Server]string{srv: "unavailable", noConsent: "unavailable"} {
		status, body, raw := getRecordings(t, server, strconv.FormatInt(before.messageID, 10))
		require.Equal(http.StatusOK, status, raw)
		require.Len(body.Recordings, 1)
		assert.Equal(want, body.Recordings[0].State)
		assert.NotContains(raw, obsolete)
	}

	// Discovery revokes the old revision and retention reuses Docbank's IDs.
	after := rf.audio(t, before.messageID, "before", "", "delivery-after", &store.BeeperMediaResult{
		VaultUID: "vault", DocbankSourceID: before.sourceID, ContentVersionID: before.contentVersionID,
	})
	require.Equal(before.contentVersionID, after.contentVersionID)
	for server, want := range map[*Server]string{srv: "processing", noConsent: "unavailable"} {
		status, body, raw := getRecordings(t, server, strconv.FormatInt(before.messageID, 10))
		require.Equal(http.StatusOK, status, raw)
		require.Len(body.Recordings, 1)
		assert.Equal(want, body.Recordings[0].State)
		assert.NotContains(raw, obsolete)
	}

	rf.deliver(t, "delivery-after", "failed")
	_, replacementBody, replacementRaw := getRecordings(t, srv, strconv.FormatInt(before.messageID, 10))
	require.Len(replacementBody.Recordings, 1)
	assert.Equal("failed", replacementBody.Recordings[0].State, "a failed replacement never falls back to the old text")
	assert.NotContains(replacementRaw, obsolete)

	rf.deliver(t, "delivery-after", "succeeded")
	rf.docbank.evidence(after, "vault", "ready", "succeeded", readyEvidence("supplied", "complete",
		map[string]any{"text": replacement}))
	recordings = recordingsFor(t, srv, before.messageID)
	require.Len(recordings, 1)
	assert.Equal("ready", recordings[0].State)
	require.NotNil(recordings[0].Transcript)
	assert.Equal(replacement, recordings[0].Transcript.Units[0].Text)

	during := seed("during")
	respond := supplied(during)
	rf.docbank.set(during.contentVersionID, func(w http.ResponseWriter) {
		edit(during, "during")
		respond(w)
	})
	_, _, raw := getRecordings(t, srv, strconv.FormatInt(during.messageID, 10))
	assert.Contains(raw, `"state":"unavailable"`)
	assert.NotContains(raw, obsolete)

	// An edit during a later recording's read still withholds the earlier one.
	pairMessage := rf.message(t, "pair")
	rf.transcripts["pair"] = obsolete
	first := rf.audio(t, pairMessage, "pair", "-a", "delivery-pair-a", &store.BeeperMediaResult{VaultUID: "vault"})
	rf.deliver(t, "delivery-pair-a", "succeeded")
	second := rf.audio(t, pairMessage, "pair", "-b", "", &store.BeeperMediaResult{VaultUID: "vault"})
	rf.docbank.set(first.contentVersionID, supplied(first))
	respondSecond := evidenceResponse(second, "vault", "pending", "running", nil)
	rf.docbank.set(second.contentVersionID, func(w http.ResponseWriter) {
		edit(first, "pair")
		respondSecond(w)
	})
	_, body, raw := getRecordings(t, srv, strconv.FormatInt(pairMessage, 10))
	require.Len(body.Recordings, 2)
	assert.Equal("unavailable", body.Recordings[0].State)
	assert.Nil(body.Recordings[0].Transcript)
	assert.NotContains(raw, obsolete)

	for _, evidence := range []string{"unavailable", "pending", "supplied mismatch"} {
		for _, timing := range []string{"before", "during"} {
			name := "erased-" + evidence + "-" + timing
			erased := seed(name)
			_, err := rf.f.Store.DB().Exec(rf.f.Store.Rebind(`UPDATE beeper_media_deliveries SET phase = 'pending-process' WHERE processing_key = ?`), "delivery-"+name)
			require.NoError(err)
			state, operation := evidence, "running"
			var transcript map[string]any
			if evidence == "supplied mismatch" {
				state = "ready"
				transcript = readyEvidence("supplied", "complete", map[string]any{"text": obsolete})
				transcript["supplied_input_id"] = "other-input"
			}
			respond := evidenceResponse(erased, "vault", state, operation, transcript)
			rf.docbank.set(erased.contentVersionID, respond)
			assert.Equal("processing", recordingsFor(t, srv, erased.messageID)[0].State, "unchanged source: "+name)
			erase := func() {
				rf.transcripts[name] = ""
				rf.writeRaw(t, erased.messageID, name)
			}
			if timing == "before" {
				erase()
			} else {
				rf.docbank.set(erased.contentVersionID, func(w http.ResponseWriter) {
					erase()
					respond(w)
				})
			}
			for _, server := range []*Server{srv, noConsent} {
				recordings = recordingsFor(t, server, erased.messageID)
				require.Len(recordings, 1)
				assert.Equal("unavailable", recordings[0].State, name)
				assert.Nil(recordings[0].Transcript, name)
			}
		}
	}

	// A generated transcript comes from the audio, which the edit left alone.
	generated := seed("generated-edit")
	rf.docbank.evidence(generated, "vault", "ready", "succeeded", readyEvidence("generated", "complete",
		map[string]any{"text": "synthetic generated transcript"}))
	edit(generated, "generated-edit")
	recordings = recordingsFor(t, srv, generated.messageID)
	require.Len(recordings, 1)
	assert.Equal("ready", recordings[0].State)
}

func TestMessageRecordingsExactProviderInput(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	rf := newRecordingFixture(t)
	srv := rf.server(true)
	first := rf.retained(t, "first")
	rf.sameAudio["second"] = "first"
	secondMessage := rf.message(t, "second")
	second := rf.audio(t, secondMessage, "second", "", "delivery-second", &store.BeeperMediaResult{
		VaultUID: "vault", DocbankSourceID: first.sourceID, ContentVersionID: first.contentVersionID,
	})
	rf.docbank.evidence(second, "vault", "ready", "succeeded", readyEvidence("supplied", "complete",
		map[string]any{"text": "second transcript"}))
	got := recordingsFor(t, srv, first.messageID)
	require.Len(got, 1)
	assert.Equal("unavailable", got[0].State)
	assert.Nil(got[0].Transcript)
	// A blocked remote job can finish after credentials recover, before polling.
	for _, phase := range []string{"blocked", "observing"} {
		_, err := rf.f.Store.DB().Exec(rf.f.Store.Rebind(`UPDATE beeper_media_deliveries
			SET phase = ? WHERE processing_key = 'delivery-second'`), phase)
		require.NoError(err)
		got = recordingsFor(t, srv, second.messageID)
		require.Len(got, 1)
		assert.Equal("ready", got[0].State)
		require.NotNil(got[0].Transcript)
		assert.Equal("second transcript", got[0].Transcript.Units[0].Text)
	}
	evidence := readyEvidence("supplied", "complete", map[string]any{"text": "returned text"})
	evidence["supplied_input_id"] = ""
	rf.docbank.evidence(first, "vault", "ready", "succeeded", evidence)
	got = recordingsFor(t, srv, first.messageID)
	require.Len(got, 1)
	assert.Equal("unavailable", got[0].State)
	assert.Nil(got[0].Transcript)
}

func TestMessageRecordingsLocalOnly(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	rf := newRecordingFixture(t)
	srv := rf.server(false)
	srv.messageRecordings = &MessageRecordingReader{Store: rf.f.Store}
	retained := rf.retained(t, "retained-local")
	got := recordingsFor(t, srv, retained.messageID)
	require.Len(got, 1)
	assert.Equal("unavailable", got[0].State)
	assert.Nil(got[0].Transcript)
	require.NoError(rf.f.Store.UpsertAttachmentRecord(t.Context(), retained.messageID, store.AttachmentWrite{
		Filename: "missing.ogg", MIMEType: "audio/ogg", SourceAttachmentID: "missing",
		SourcePartKey: "missing", State: attachmentpolicy.StateSkipped,
		SkipReason: attachmentpolicy.SkipSizeCap, Role: store.AttachmentRoleStandalone,
		RoleSource: store.AttachmentRoleSourceImporterSemantics,
	}))
	got = recordingsFor(t, srv, retained.messageID)
	require.Len(got, 2)
	assert.Equal("unavailable", got[1].State)
	assert.Zero(rf.docbank.requestCount())
}

func TestMessageRecordingsOptOut(t *testing.T) {
	assert := assert.New(t)
	f := storetest.New(t)
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIKey: recordingsTestAPIKey}},
		Store:  f.Store,
		Logger: testLogger(),
	})
	messageID := f.CreateMessage("plain")
	status, _, raw := getRecordings(t, srv, strconv.FormatInt(messageID, 10))
	assert.Equal(http.StatusOK, status)
	assert.JSONEq(fmt.Sprintf(`{"message_id":%d,"recordings":[]}`, messageID), raw)
	for _, id := range []string{"0", "abc"} {
		status, _, raw = getRecordings(t, srv, id)
		assert.Equal(http.StatusBadRequest, status, id)
		assert.Contains(raw, "invalid_id", id)
	}
}

func TestMessageRecordingsOpenAPIContract(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	doc := OpenAPIDocument()
	path := doc.Paths["/api/v1/messages/{id}/recordings"]
	assert.Equal("listMessageRecordings", path.Get.OperationID)
	require.Len(path.Get.Parameters, 1)
	assert.Equal("id", path.Get.Parameters[0].Name)

	recording := doc.Components.Schemas.Map()["MessageRecording"]
	require.NotNil(recording)
	assert.Equal([]any{"ready", "processing", "missing", "failed", "unsupported", "media_missing", "unavailable"},
		recording.Properties["state"].Enum)
	transcript := doc.Components.Schemas.Map()["MessageTranscript"]
	require.NotNil(transcript)
	assert.Equal([]any{"supplied", "generated"}, transcript.Properties["origin"].Enum)
	unit := doc.Components.Schemas.Map()["MessageTranscriptUnit"]
	require.NotNil(unit)
	assert.Equal([]string{"text"}, unit.Required)
}
