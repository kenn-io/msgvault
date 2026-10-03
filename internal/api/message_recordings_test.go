package api

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
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
}

type recordingFixture struct {
	f       *storetest.Fixture
	docbank *fakeDocbank
	client  *docbankmedia.Client
	parts   map[string][]string
	// transcripts holds each message's provider transcript text.
	transcripts map[string]string
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
	return &recordingFixture{f: f, docbank: docbank, client: client, parts: map[string][]string{}, transcripts: map[string]string{}}
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

// audio adds a stored voice note to the message and records its occurrence.
// A nil result leaves the occurrence pending; otherwise it finishes the
// retain operation with result.
func (rf *recordingFixture) audio(
	t *testing.T, messageID int64, sourceMessageID, part, processingKey string, result *store.BeeperMediaResult,
) recordingSeed {
	t.Helper()
	st := rf.f.Store
	digest := sha256.Sum256([]byte(sourceMessageID + part))
	hash := hex.EncodeToString(digest[:])
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
	rawDigest := sha256.Sum256(rf.writeRaw(t, messageID, sourceMessageID))
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
	if result == nil {
		return seed
	}
	prepared, err := st.PrepareBeeperMediaOperation(t.Context(), store.BeeperMediaOperation{
		Kind: store.BeeperMediaOperationRetain, DestinationKey: recordingsTestDestination,
		OccurrenceRef: ref, Revision: revision,
	})
	require.NoError(t, err)
	finished := *result
	if finished.ErrorCode == "" {
		finished.DocbankSourceID = "source-" + hash[:8]
		finished.SourceVersionID = "version"
		finished.ContentVersionID = uuid.NewString()
		finished.DocbankOccurrenceID = "occurrence-" + hash[:8]
		finished.CoverageState = "unprocessed"
		seed.sourceID, seed.contentVersionID = finished.DocbankSourceID, finished.ContentVersionID
	}
	applied, err := st.FinishBeeperMediaOperation(t.Context(), prepared, finished)
	require.NoError(t, err)
	require.True(t, applied)
	return seed
}

func (rf *recordingFixture) retained(t *testing.T, sourceMessageID string) recordingSeed {
	t.Helper()
	messageID := rf.message(t, sourceMessageID)
	return rf.audio(t, messageID, sourceMessageID, "", "", &store.BeeperMediaResult{VaultUID: "vault"})
}

func getRecordings(t *testing.T, srv *Server, messageID string) (int, MessageRecordingsResponse, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/messages/"+messageID+"/recordings", nil)
	req.Header.Set("X-Api-Key", recordingsTestAPIKey)
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
	}{
		{name: "docbank pending", evidenceState: "pending", operation: "running", want: "processing", wantNoConsent: "processing"},
		{name: "missing", evidenceState: "unavailable", operation: "succeeded", want: "missing", wantNoConsent: "missing"},
		{name: "failed", evidenceState: "unavailable", operation: "failed", want: "failed", wantNoConsent: "failed"},
		{name: "cancelled", evidenceState: "unavailable", operation: "cancelled", want: "failed", wantNoConsent: "failed"},
		{name: "stale", evidenceState: "stale", operation: "succeeded", want: "unavailable", wantNoConsent: "unavailable"},
		{name: "unknown", evidenceState: "archived", operation: "succeeded", want: "unavailable", wantNoConsent: "unavailable"},
		{name: "vault mismatch", evidenceState: "unavailable", operation: "succeeded", vault: "other-vault",
			want: "unavailable", wantNoConsent: "unavailable"},
		{name: "queued delivery", evidenceState: "unavailable", operation: "succeeded",
			want: "processing", wantNoConsent: "missing", seed: func() recordingSeed {
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
	pendingMessage := rf.message(t, "pending")
	rf.audio(t, pendingMessage, "pending", "", "", nil)
	assert.Equal("processing", recordingsFor(t, srv, pendingMessage)[0].State)
	assert.Equal("unavailable", recordingsFor(t, noConsent, pendingMessage)[0].State)

	unsupportedMessage := rf.message(t, "unsupported")
	rf.audio(t, unsupportedMessage, "unsupported", "", "", &store.BeeperMediaResult{ErrorCode: "unsupported_media"})
	assert.Equal("unsupported", recordingsFor(t, srv, unsupportedMessage)[0].State)

	// The worker retries a source it could not read, whatever the reason code.
	for _, code := range []string{"source_unavailable", "source_changed"} {
		sourceMissingMessage := rf.message(t, "source-missing-"+code)
		rf.audio(t, sourceMissingMessage, "source-missing-"+code, "", "",
			&store.BeeperMediaResult{SourceUnavailable: true, ErrorCode: code})
		assert.Equal("processing", recordingsFor(t, srv, sourceMissingMessage)[0].State, code)
		assert.Equal("unavailable", recordingsFor(t, noConsent, sourceMissingMessage)[0].State, code)
	}

	uncapturedMessage := rf.message(t, "uncaptured")
	for _, write := range []store.AttachmentWrite{
		{Filename: "late.ogg", MIMEType: "audio/ogg", Size: 12, SourceAttachmentID: "beeper:late",
			SourcePartKey: "beeper:late", MediaType: "voice_note", State: attachmentpolicy.StateSkipped,
			SkipReason: attachmentpolicy.SkipSizeCap, Role: store.AttachmentRoleStandalone,
			RoleSource: store.AttachmentRoleSourceImporterSemantics},
		{Filename: "photo.jpg", MIMEType: "image/jpeg", Size: 30, SourceAttachmentID: "beeper:photo",
			SourcePartKey: "beeper:photo", MediaType: "image", State: attachmentpolicy.StateSkipped,
			SkipReason: attachmentpolicy.SkipSizeCap, Role: store.AttachmentRoleStandalone,
			RoleSource: store.AttachmentRoleSourceImporterSemantics},
		// Slack leaves media_type empty on files it never downloaded.
		{Filename: "clip.mp3", MIMEType: "audio/mpeg", Size: 20, SourceAttachmentID: "slack:clip",
			SourcePartKey: "slack:clip", State: attachmentpolicy.StateFailed,
			SkipReason: attachmentpolicy.SkipFetchFailure, Role: store.AttachmentRoleStandalone,
			RoleSource: store.AttachmentRoleSourceImporterSemantics},
		// Importers can leave voice notes with a generic type and only a name.
		{Filename: "note.OGG", MIMEType: "application/octet-stream", Size: 9, SourceAttachmentID: "beeper:note-ogg",
			SourcePartKey: "beeper:note-ogg", State: attachmentpolicy.StateFailed,
			SkipReason: attachmentpolicy.SkipFetchFailure, Role: store.AttachmentRoleStandalone,
			RoleSource: store.AttachmentRoleSourceImporterSemantics},
		{Filename: "memo.m4a", Size: 8, SourceAttachmentID: "beeper:memo-m4a",
			SourcePartKey: "beeper:memo-m4a", State: attachmentpolicy.StateFailed,
			SkipReason: attachmentpolicy.SkipFetchFailure, Role: store.AttachmentRoleStandalone,
			RoleSource: store.AttachmentRoleSourceImporterSemantics},
	} {
		require.NoError(rf.f.Store.UpsertAttachmentRecord(t.Context(), uncapturedMessage, write))
	}
	recordings = recordingsFor(t, srv, uncapturedMessage)
	require.Len(recordings, 4)
	for i, filename := range []string{"late.ogg", "clip.mp3", "note.OGG", "memo.m4a"} {
		assert.Equal(filename, recordings[i].Filename)
		assert.Equal("media_missing", recordings[i].State, filename)
	}
	assert.Equal(before, rf.docbank.requestCount())

	// One recording's Docbank failure never hides another.
	isolatedMessage := rf.message(t, "isolated")
	failing := rf.audio(t, isolatedMessage, "isolated", "-a", "", &store.BeeperMediaResult{VaultUID: "vault"})
	working := rf.audio(t, isolatedMessage, "isolated", "-b", "", &store.BeeperMediaResult{VaultUID: "vault"})
	rf.docbank.evidence(working, "vault", "ready", "succeeded", readyEvidence("supplied", "complete",
		map[string]any{"text": "synthetic transcript"}))
	for name, respond := range map[string]func(){
		"503": func() {
			rf.docbank.set(failing.contentVersionID, func(w http.ResponseWriter) { w.WriteHeader(http.StatusServiceUnavailable) })
		},
		"archived": func() { rf.docbank.evidence(failing, "vault", "archived", "succeeded", nil) },
	} {
		respond()
		recordings = recordingsFor(t, srv, isolatedMessage)
		require.Len(recordings, 2, name)
		assert.Equal("unavailable", recordings[0].State, name)
		assert.Equal("ready", recordings[1].State, name)
		require.NotNil(recordings[1].Transcript, name)
		assert.Equal("synthetic transcript", recordings[1].Transcript.Units[0].Text, name)
	}
}

func TestMessageRecordingsDocbankBudget(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	previous := messageRecordingDocbankBudget
	messageRecordingDocbankBudget = time.Second
	t.Cleanup(func() { messageRecordingDocbankBudget = previous })
	rf := newRecordingFixture(t)
	srv := rf.server(true)

	messageID := rf.message(t, "blocked")
	blocked := rf.audio(t, messageID, "blocked", "-a", "", &store.BeeperMediaResult{VaultUID: "vault"})
	rf.audio(t, messageID, "blocked", "-b", "", &store.BeeperMediaResult{ErrorCode: "unsupported_media"})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	rf.docbank.set(blocked.contentVersionID, func(http.ResponseWriter) { <-release })

	recordings := recordingsFor(t, srv, messageID)
	require.Len(recordings, 2)
	assert.Equal("unavailable", recordings[0].State)
	assert.Equal("unsupported", recordings[1].State)
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

	deleted := rf.retained(t, "deleted")
	rf.docbank.evidence(deleted, "vault", "ready", "succeeded", readyEvidence("generated", "complete",
		map[string]any{"text": secret}))
	require.NoError(rf.f.Store.MarkMessageDeleted(rf.f.Source.ID, "deleted"))
	before := rf.docbank.requestCount()
	assert.Empty(recordingsFor(t, srv, deleted.messageID))
	assert.Equal(before, rf.docbank.requestCount())

	// Served transcript text never becomes the authored body.
	served := rf.retained(t, "served")
	rf.docbank.evidence(served, "vault", "ready", "succeeded", readyEvidence("generated", "complete",
		map[string]any{"text": secret}))
	recordings := recordingsFor(t, srv, served.messageID)
	require.Len(recordings, 1)
	assert.Equal("ready", recordings[0].State)
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/messages/%d", served.messageID), nil)
	req.Header.Set("X-Api-Key", recordingsTestAPIKey)
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, req)
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	var detail MessageDetail
	require.NoError(json.Unmarshal(response.Body.Bytes(), &detail))
	assert.Equal("authored body served", detail.Body)
}

func TestMessageRecordingsProviderTranscriptEdit(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	rf := newRecordingFixture(t)
	srv := rf.server(true)
	noConsent := rf.server(false)
	const obsolete = "synthetic transcript before the edit"

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
	for server, want := range map[*Server]string{srv: "processing", noConsent: "unavailable"} {
		status, body, raw := getRecordings(t, server, strconv.FormatInt(before.messageID, 10))
		require.Equal(http.StatusOK, status, raw)
		require.Len(body.Recordings, 1)
		assert.Equal(want, body.Recordings[0].State)
		assert.NotContains(raw, obsolete)
	}

	during := seed("during")
	respond := supplied(during)
	rf.docbank.set(during.contentVersionID, func(w http.ResponseWriter) {
		edit(during, "during")
		respond(w)
	})
	_, _, raw := getRecordings(t, srv, strconv.FormatInt(during.messageID, 10))
	assert.Contains(raw, `"state":"processing"`)
	assert.NotContains(raw, obsolete)

	// An edit during a later recording's read still withholds the earlier one.
	pairMessage := rf.message(t, "pair")
	rf.transcripts["pair"] = obsolete
	first := rf.audio(t, pairMessage, "pair", "-a", "", &store.BeeperMediaResult{VaultUID: "vault"})
	second := rf.audio(t, pairMessage, "pair", "-b", "", &store.BeeperMediaResult{VaultUID: "vault"})
	rf.docbank.set(first.contentVersionID, supplied(first))
	respondSecond := evidenceResponse(second, "vault", "pending", "running", nil)
	rf.docbank.set(second.contentVersionID, func(w http.ResponseWriter) {
		edit(first, "pair")
		respondSecond(w)
	})
	_, body, raw := getRecordings(t, srv, strconv.FormatInt(pairMessage, 10))
	require.Len(body.Recordings, 2)
	assert.Equal("processing", body.Recordings[0].State)
	assert.Nil(body.Recordings[0].Transcript)
	assert.NotContains(raw, obsolete)

	// A generated transcript comes from the audio, which the edit left alone.
	generated := seed("generated-edit")
	rf.docbank.evidence(generated, "vault", "ready", "succeeded", readyEvidence("generated", "complete",
		map[string]any{"text": "synthetic generated transcript"}))
	edit(generated, "generated-edit")
	recordings = recordingsFor(t, srv, generated.messageID)
	require.Len(recordings, 1)
	assert.Equal("ready", recordings[0].State)
}

func TestMessageRecordingsDisabled(t *testing.T) {
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
	assert.Equal("3.1.0", doc.Info.Version)
	path := doc.Paths["/api/v1/messages/{id}/recordings"]
	require.NotNil(path)
	require.NotNil(path.Get)
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
