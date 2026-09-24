package beeper

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/attachmentstore"
	"go.kenn.io/msgvault/internal/docbankmedia"
	"go.kenn.io/msgvault/internal/store"
)

const docbankTestKey = "synthetic-docbank-key"

// voiceSpec is one synthetic Beeper audio message served by fakeBeeper.
type voiceSpec struct {
	id, asset, mime, fileName, transcript string
	data                                  []byte
	ordinary                              bool
}

type mediaWorld struct {
	st     *store.Store
	blobs  *attachmentstore.Store
	dir    string
	imp    *Importer
	beeper *fakeBeeper
}

// importVoiceChat runs the Beeper importer against a fake Beeper
// API so every test starts from rows, raw JSON and CAS written by capture.
func importVoiceChat(t *testing.T, specs ...voiceSpec) *mediaWorld {
	t.Helper()
	f := newFakeBeeper(t)
	base := time.Now().Add(-30 * 24 * time.Hour).UTC().Truncate(time.Second)
	ch := &fakeChat{
		ID: "!audio:beeper.local", AccountID: "signal", Network: "Signal", Title: "Audio", Type: "single",
		Participants: []map[string]any{
			{"id": "@me:beeper.local", "fullName": "Test User", "isSelf": true},
			{"id": "@signal_ann:beeper.local", "fullName": "Ann"},
		},
	}
	for i, spec := range specs {
		attachment := map[string]any{
			"id": spec.asset, "type": "audio", "isVoiceNote": !spec.ordinary,
			"mimeType": spec.mime, "fileName": spec.fileName, "fileSize": len(spec.data),
		}
		if spec.transcript != "" {
			attachment["transcription"] = map[string]any{
				"transcription": spec.transcript, "engine": "synthetic", "language": "en",
			}
		}
		ch.Msgs = append(ch.Msgs, fakeMsg{
			ID: spec.id, SortKey: i, Timestamp: base.Add(time.Duration(i)*time.Minute + 123*time.Millisecond),
			Type: "VOICE", SenderID: "@signal_ann:beeper.local", SenderName: "Ann",
			Attachments: []map[string]any{attachment},
		})
		f.setAsset(spec.asset, spec.data)
	}
	ch.LastActivity = ch.Msgs[len(ch.Msgs)-1].Timestamp
	f.addChat(ch)
	imp, st, done := newTestImporter(t, f)
	t.Cleanup(done)
	dir := t.TempDir()
	_, err := imp.Import(t.Context(), ImportOptions{AccountID: "signal", AttachmentsDir: dir})
	require.NoError(t, err)
	blobs, err := attachmentstore.New(store.NewPackCatalog(st), dir)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, blobs.Close()) })
	return &mediaWorld{st: st, blobs: blobs, dir: dir, imp: imp, beeper: f}
}

func (w *mediaWorld) submitter(t *testing.T, server *httptest.Server, destination string) *MediaSubmitter {
	t.Helper()
	client, err := docbankmedia.NewClient(server.URL, func() (string, error) { return docbankTestKey, nil })
	require.NoError(t, err)
	return NewMediaSubmitter(w.st, w.blobs, client, destination, w.dir)
}

func runPasses(t *testing.T, submitter *MediaSubmitter, passes int) {
	t.Helper()
	for range passes {
		_, err := submitter.RunBatch(t.Context())
		require.NoError(t, err)
	}
}

type occurrenceRow struct {
	Ref, Revision, State, OperationID, ErrorCode, SourceID, SourceVersionID string
	ContentVersionID, OccurrenceID, Coverage, ProcessingKey, MessageID      string
}

func occurrenceRows(t *testing.T, st *store.Store, destination string) []occurrenceRow {
	t.Helper()
	rows, err := st.DB().Query(st.Rebind(`
		SELECT occurrence_ref, revision, retention_state, retention_operation_id, error_code,
		       source_id, source_version_id, content_version_id, occurrence_id, coverage_state,
		       processing_key, source_message_id
		FROM beeper_media_occurrences WHERE destination_key = ?
		ORDER BY source_message_id, revision`), destination)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var result []occurrenceRow
	for rows.Next() {
		var row occurrenceRow
		require.NoError(t, rows.Scan(&row.Ref, &row.Revision, &row.State, &row.OperationID, &row.ErrorCode,
			&row.SourceID, &row.SourceVersionID, &row.ContentVersionID, &row.OccurrenceID, &row.Coverage,
			&row.ProcessingKey, &row.MessageID))
		result = append(result, row)
	}
	require.NoError(t, rows.Err())
	return result
}

type deliveryRow struct {
	Phase, SourceID, SourceVersionID, ContentVersionID, Donor, SuppliedInput string
	PendingOperationID, ProcessingOperationID                                string
	JobID, OperationState, Coverage, ErrorCode                               string
}

func deliveryRows(t *testing.T, st *store.Store, destination string) []deliveryRow {
	t.Helper()
	rows, err := st.DB().Query(st.Rebind(`
		SELECT phase, source_id, source_version_id, content_version_id, donor_occurrence_id,
		       supplied_input_id, COALESCE(pending_operation_id, ''),
		       COALESCE(processing_operation_id, ''), job_id, operation_state, coverage_state, error_code
		FROM beeper_media_deliveries WHERE destination_key = ? ORDER BY processing_key`), destination)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var result []deliveryRow
	for rows.Next() {
		var row deliveryRow
		require.NoError(t, rows.Scan(&row.Phase, &row.SourceID, &row.SourceVersionID, &row.ContentVersionID,
			&row.Donor, &row.SuppliedInput, &row.PendingOperationID, &row.ProcessingOperationID,
			&row.JobID, &row.OperationState, &row.Coverage, &row.ErrorCode))
		result = append(result, row)
	}
	require.NoError(t, rows.Err())
	return result
}

func TestBeeperMediaSubmission(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	wav := syntheticWAV(1600, 1)
	world := importVoiceChat(t, voiceSpec{id: "voice1", asset: "mxc://beeper.local/voice1",
		mime: "audio/wav", fileName: "voice.wav", transcript: "a complete provider transcript", data: wav})
	docbank := newFakeDocbank(t)
	server := httptest.NewServer(docbank)
	defer server.Close()
	submitter := world.submitter(t, server, "destination-a")

	runPasses(t, submitter, 5)

	docbank.mu.Lock()
	require.Len(docbank.uploads, 1)
	assert.Equal(wav, docbank.uploads[0])
	assert.Equal([]string{"a complete provider transcript"}, docbank.transcripts)
	require.Len(docbank.retentionOps, 1)
	require.Len(docbank.artifactOps, 1)
	require.Len(docbank.processOps, 1)
	for _, id := range []string{docbank.retentionOps[0], docbank.artifactOps[0], docbank.processOps[0]} {
		parsed, err := uuid.Parse(id)
		require.NoError(err)
		assert.Equal(uuid.Version(4), parsed.Version())
	}
	occurrence := docbank.occurrences[0]
	docbank.mu.Unlock()
	assert.True(strings.HasPrefix(occurrence.Ref, "msgvault:"))
	assert.NotEmpty(occurrence.Revision)
	assert.Equal("voice.wav", occurrence.Filename)
	assert.Equal("instant", occurrence.Message.Precision)
	assert.True(strings.HasSuffix(occurrence.Message.Raw, ".123Z"))

	rows := occurrenceRows(t, world.st, "destination-a")
	require.Len(rows, 1)
	assert.Equal("retained", rows[0].State)
	assert.Equal("source-1", rows[0].SourceID)
	assert.Equal("audio-version-1", rows[0].SourceVersionID)
	assert.Equal("audio-content-1", rows[0].ContentVersionID)
	assert.Equal("occurrence-1", rows[0].OccurrenceID)
	deliveries := deliveryRows(t, world.st, "destination-a")
	require.Len(deliveries, 1)
	assert.Equal("done", deliveries[0].Phase)
	assert.Equal("succeeded", deliveries[0].OperationState)
	assert.Equal("transcribed", deliveries[0].Coverage)

	mappings, err := world.st.ListLiveBeeperMediaMappings(t.Context(), "destination-a", "", 10)
	require.NoError(err)
	require.Len(mappings, 1)
	assert.Equal("occurrence-1", mappings[0].DocbankOccurrenceID)
	assert.Equal("voice1", mappings[0].SourceMessageID)
}

// TestBeeperMediaGatedWrites runs every operation kind behind a recording
// operation gate. The archive changes only while the gate is held, and no
// Docbank request runs under it.
func TestBeeperMediaGatedWrites(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	world := importVoiceChat(t, voiceSpec{id: "voice1", asset: "mxc://beeper.local/voice1",
		mime: "audio/wav", fileName: "voice.wav", transcript: "a gated transcript", data: syntheticWAV(1600, 3)})
	docbank := newFakeDocbank(t)
	var held atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.False(held.Load(), "Docbank request %s ran under the operation gate", r.URL.Path)
		docbank.ServeHTTP(w, r)
	}))
	defer server.Close()
	var released []string
	acquired := 0
	submitter := world.submitter(t, server, "destination-gated").WithOperationGate(
		func(context.Context) (func(), bool) {
			assert.False(held.Load(), "operation gate was acquired recursively")
			assert.Equal(released, mediaArchiveState(t, world.st), "the archive changed while the gate was free")
			held.Store(true)
			acquired++
			return func() {
				held.Store(false)
				released = mediaArchiveState(t, world.st)
			}, true
		})

	for range 5 {
		released = mediaArchiveState(t, world.st)
		_, err := submitter.RunBatch(t.Context())
		require.NoError(err)
		assert.Equal(released, mediaArchiveState(t, world.st), "the archive changed after the last release")
	}
	rows := occurrenceRows(t, world.st, "destination-gated")
	require.Len(rows, 1)
	assert.Equal("retained", rows[0].State)
	deliveries := deliveryRows(t, world.st, "destination-gated")
	require.Len(deliveries, 1)
	assert.Equal("done", deliveries[0].Phase)
	assert.Equal("transcribed", deliveries[0].Coverage)
	assert.GreaterOrEqual(acquired, 9, "discovery, prepare and finish each take the gate")
	docbank.mu.Lock()
	requests := docbank.requests
	docbank.mu.Unlock()
	assert.Equal(5, requests)

	// A busy gate ends the pass before any write or request.
	before := mediaArchiveState(t, world.st)
	busy := world.submitter(t, server, "destination-busy").WithOperationGate(
		func(context.Context) (func(), bool) { return func() {}, false })
	result, err := busy.RunBatch(t.Context())
	require.NoError(err)
	assert.Zero(result.Examined)
	assert.Equal(before, mediaArchiveState(t, world.st))
	docbank.mu.Lock()
	assert.Equal(requests, docbank.requests)
	docbank.mu.Unlock()
}

// mediaArchiveState covers every table the media worker writes.
func mediaArchiveState(t *testing.T, st *store.Store) []string {
	t.Helper()
	state := tableSnapshot(t, st)
	rows, err := st.DB().Query(`
		SELECT key || '=' || value FROM archive_metadata
		UNION ALL
		SELECT consumer_key || '|' || CAST(last_sequence AS TEXT) || '|' || CAST(reconciliation_complete AS TEXT)
		FROM attachment_change_consumers
		ORDER BY 1`)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	for rows.Next() {
		var value string
		require.NoError(t, rows.Scan(&value))
		state = append(state, value)
	}
	require.NoError(t, rows.Err())
	return state
}

func TestBeeperMediaSyncIsolation(t *testing.T) {
	for fault, wantCode := range map[string]string{
		"http-503": "server_error", "malformed-json": "invalid_receipt", "wait-for-cancel": "",
	} {
		t.Run(fault, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			wav := syntheticWAV(800, 2)
			world := importVoiceChat(t, voiceSpec{id: "voice1", asset: "mxc://beeper.local/voice1",
				mime: "audio/wav", fileName: "voice.wav", transcript: "provider words", data: wav})
			docbank := newFakeDocbank(t)
			switch fault {
			case "http-503":
				docbank.status = http.StatusServiceUnavailable
			case "malformed-json":
				docbank.malformed = true
			default:
				docbank.hang = true
			}
			server := httptest.NewServer(docbank)
			defer server.Close()
			submitter := world.submitter(t, server, "destination-fault")
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			_, err := submitter.RunBatch(ctx)
			cancel()
			if fault == "wait-for-cancel" {
				require.ErrorIs(err, context.DeadlineExceeded)
			} else {
				require.NoError(err)
			}
			rows := occurrenceRows(t, world.st, "destination-fault")
			require.Len(rows, 1)
			assert.Equal("pending", rows[0].State)
			assert.Equal(wantCode, rows[0].ErrorCode)

			// Capture keeps its own completion and checkpoint after the fault.
			world.beeper.appendMsg("!audio:beeper.local", fakeMsg{
				ID: "text-after-fault", SortKey: 99, Timestamp: time.Now().Add(-29 * 24 * time.Hour).UTC(),
				Text: "later text", SenderID: "@signal_ann:beeper.local", SenderName: "Ann",
			})
			summary, err := world.imp.Import(t.Context(), ImportOptions{AccountID: "signal", AttachmentsDir: world.dir})
			require.NoError(err)
			assert.EqualValues(0, summary.Errors)
			var added int
			require.NoError(world.st.DB().QueryRow(
				`SELECT COUNT(*) FROM messages WHERE source_message_id = 'text-after-fault'`).Scan(&added))
			assert.Equal(1, added)
			src, err := world.st.GetOrCreateSource("beeper", "signal")
			require.NoError(err)
			run, err := world.st.GetLastSuccessfulSync(src.ID)
			require.NoError(err)
			assert.True(run.CursorAfter.Valid)
			var state, hash string
			require.NoError(world.st.DB().QueryRow(`
				SELECT attachment_state, content_hash FROM attachments
				WHERE source_attachment_id = 'beeper:mxc://beeper.local/voice1'`).Scan(&state, &hash))
			assert.Equal("stored", state)
			assert.Equal(sha256Hex(wav), hash)
		})
	}
}

func TestBeeperMediaRestart(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	world := importVoiceChat(t, voiceSpec{id: "voice1", asset: "mxc://beeper.local/voice1",
		mime: "audio/wav", fileName: "voice.wav", data: syntheticWAV(800, 3)})
	docbank := newFakeDocbank(t)
	docbank.dropRetention = true
	server := httptest.NewServer(docbank)
	defer server.Close()

	runPasses(t, world.submitter(t, server, "destination-restart"), 1)
	rows := occurrenceRows(t, world.st, "destination-restart")
	require.Len(rows, 1)
	assert.Equal("pending", rows[0].State)
	assert.Equal("transport", rows[0].ErrorCode)

	// A new worker stands in for a restarted daemon once the retry delay passes.
	_, err := world.st.DB().Exec(`UPDATE beeper_media_occurrences SET next_action_at = '2000-01-01 00:00:00.000'`)
	require.NoError(err)
	runPasses(t, world.submitter(t, server, "destination-restart"), 1)

	docbank.mu.Lock()
	defer docbank.mu.Unlock()
	require.Len(docbank.retentionOps, 2)
	assert.Equal(docbank.retentionOps[0], docbank.retentionOps[1])
	require.Len(docbank.retentionMetadata, 2)
	assert.Equal(docbank.retentionMetadata[0], docbank.retentionMetadata[1])
	assert.Equal(rows[0].OperationID, docbank.retentionOps[0])
	rows = occurrenceRows(t, world.st, "destination-restart")
	require.Len(rows, 1)
	assert.Equal("retained", rows[0].State)
	assert.Equal("source-1", rows[0].SourceID)
	assert.Equal("occurrence-1", rows[0].OccurrenceID)
}

// TestBeeperMediaStepTimeout keeps a step that outlives its own deadline from
// holding the queue: it backs off like a transport fault and the next item runs.
func TestBeeperMediaStepTimeout(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	world := importVoiceChat(t,
		voiceSpec{id: "voice1", asset: "mxc://beeper.local/voice1", mime: "audio/wav",
			fileName: "voice.wav", data: syntheticWAV(800, 11)},
		voiceSpec{id: "voice2", asset: "mxc://beeper.local/voice2", mime: "audio/wav",
			fileName: "voice.wav", data: syntheticWAV(800, 12)})
	docbank := newFakeDocbank(t)
	docbank.hang = true
	server := httptest.NewServer(docbank)
	defer server.Close()
	submitter := world.submitter(t, server, "destination-timeout")
	submitter.actionTimeout = 100 * time.Millisecond

	runPasses(t, submitter, 1)
	rows := occurrenceRows(t, world.st, "destination-timeout")
	require.Len(rows, 2)
	var timedOut string
	for _, row := range rows {
		assert.Equal("pending", row.State)
		if row.ErrorCode != "" {
			assert.Equal("timeout", row.ErrorCode)
			timedOut = row.MessageID
		}
	}
	require.NotEmpty(timedOut)

	docbank.mu.Lock()
	docbank.hang = false
	docbank.mu.Unlock()
	runPasses(t, submitter, 3)

	docbank.mu.Lock()
	assert.Len(docbank.uploads, 1, "the timed-out item waits for its backoff")
	docbank.mu.Unlock()
	for _, row := range occurrenceRows(t, world.st, "destination-timeout") {
		if row.MessageID == timedOut {
			assert.Equal("pending", row.State)
			assert.Equal("timeout", row.ErrorCode)
		} else {
			assert.Equal("retained", row.State)
		}
	}
}

// TestBeeperMediaStatusRejection keeps a queued job after a rejected poll and
// resumes observing it when the daemon restarts.
func TestBeeperMediaStatusRejection(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	world := importVoiceChat(t, voiceSpec{id: "voice1", asset: "mxc://beeper.local/voice1",
		mime: "audio/wav", fileName: "voice.wav", transcript: "status words", data: syntheticWAV(800, 13)})
	docbank := newFakeDocbank(t)
	docbank.jobHTTPStatus = http.StatusUnauthorized
	server := httptest.NewServer(docbank)
	defer server.Close()
	submitter := world.submitter(t, server, "destination-status")

	runPasses(t, submitter, 4)
	deliveries := deliveryRows(t, world.st, "destination-status")
	require.Len(deliveries, 1)
	assert.Equal("blocked", deliveries[0].Phase)
	assert.Equal("unauthorized", deliveries[0].ErrorCode)
	assert.NotEmpty(deliveries[0].JobID)
	assert.Equal("pending", deliveries[0].Coverage)
	jobID := deliveries[0].JobID

	docbank.mu.Lock()
	docbank.jobHTTPStatus = 0
	requests := docbank.requests
	docbank.mu.Unlock()
	runPasses(t, submitter, 2)
	docbank.mu.Lock()
	assert.Equal(requests, docbank.requests, "a blocked poll waits for restart")
	docbank.mu.Unlock()

	require.NoError(world.st.ReconsiderBlockedBeeperMediaOperations(t.Context(), "destination-status"))
	runPasses(t, submitter, 1)
	deliveries = deliveryRows(t, world.st, "destination-status")
	require.Len(deliveries, 1)
	assert.Equal("done", deliveries[0].Phase)
	assert.Equal(jobID, deliveries[0].JobID)
	assert.Equal("succeeded", deliveries[0].OperationState)
	assert.Equal("transcribed", deliveries[0].Coverage)
	assert.Empty(deliveries[0].ErrorCode)
}

// TestBeeperMediaCoverageAfterJob keeps observing a completed job until
// Docbank publishes its coverage.
func TestBeeperMediaCoverageAfterJob(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	world := importVoiceChat(t, voiceSpec{id: "voice1", asset: "mxc://beeper.local/voice1",
		mime: "audio/wav", fileName: "voice.wav", transcript: "coverage words", data: syntheticWAV(800, 14)})
	docbank := newFakeDocbank(t)
	docbank.coverage = "pending"
	server := httptest.NewServer(docbank)
	defer server.Close()
	submitter := world.submitter(t, server, "destination-coverage")

	runPasses(t, submitter, 4)
	deliveries := deliveryRows(t, world.st, "destination-coverage")
	require.Len(deliveries, 1)
	assert.Equal("observing", deliveries[0].Phase)
	assert.Equal("queued", deliveries[0].OperationState)
	assert.Equal("pending", deliveries[0].Coverage)

	docbank.mu.Lock()
	docbank.coverage = "transcribed"
	docbank.mu.Unlock()
	_, err := world.st.DB().Exec(`UPDATE beeper_media_deliveries SET next_action_at = '2000-01-01 00:00:00.000'`)
	require.NoError(err)
	runPasses(t, submitter, 1)
	deliveries = deliveryRows(t, world.st, "destination-coverage")
	require.Len(deliveries, 1)
	assert.Equal("done", deliveries[0].Phase)
	assert.Equal("succeeded", deliveries[0].OperationState)
	assert.Equal("transcribed", deliveries[0].Coverage)
}

// TestBeeperMediaProcessingFailure records Docbank's failed processing
// receipt, which has no job, as a terminal result and sends no more retries.
func TestBeeperMediaProcessingFailure(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	world := importVoiceChat(t, voiceSpec{id: "voice1", asset: "mxc://beeper.local/voice1",
		mime: "audio/wav", fileName: "voice.wav", transcript: "failed words", data: syntheticWAV(800, 15)})
	docbank := newFakeDocbank(t)
	docbank.failProcessing = true
	server := httptest.NewServer(docbank)
	defer server.Close()
	submitter := world.submitter(t, server, "destination-failed")

	runPasses(t, submitter, 3)
	deliveries := deliveryRows(t, world.st, "destination-failed")
	require.Len(deliveries, 1)
	assert.Equal("done", deliveries[0].Phase)
	assert.Equal("failed", deliveries[0].OperationState)
	assert.Equal("unavailable", deliveries[0].Coverage)
	assert.Equal("processing_failed", deliveries[0].ErrorCode)
	assert.Empty(deliveries[0].JobID)

	_, err := world.st.DB().Exec(`UPDATE beeper_media_deliveries SET next_action_at = '2000-01-01 00:00:00.000'`)
	require.NoError(err)
	runPasses(t, submitter, 3)
	docbank.mu.Lock()
	defer docbank.mu.Unlock()
	assert.Len(docbank.processOps, 1)
}

// TestBeeperMediaCoverageOwnership settles each processing operation from its
// own receipt. Docbank's source status names the newest operation but takes
// coverage from the newest succeeded one on the same audio.
func TestBeeperMediaCoverageOwnership(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	wav := syntheticWAV(800, 16)
	world := importVoiceChat(t,
		voiceSpec{id: "voice1", asset: "mxc://beeper.local/voice1", mime: "audio/wav",
			fileName: "voice.wav", transcript: "first transcript", data: wav},
		voiceSpec{id: "voice2", asset: "mxc://beeper.local/voice2", mime: "audio/wav",
			fileName: "voice.wav", transcript: "second transcript", data: wav})
	docbank := newFakeDocbank(t)
	docbank.coverage = "pending"
	server := httptest.NewServer(docbank)
	defer server.Close()
	submitter := world.submitter(t, server, "destination-owner")

	runPasses(t, submitter, 8)
	docbank.mu.Lock()
	require.Len(docbank.processOps, 2)
	olderJob, newerJob := sha256Hex([]byte(docbank.processOps[0])), sha256Hex([]byte(docbank.processOps[1]))
	// The older operation succeeds; the newer one's job completes but its receipt stays queued.
	docbank.coverage = "transcribed"
	docbank.holdIndex[1] = true
	replays := docbank.replays
	docbank.mu.Unlock()
	byJob := func() map[string]deliveryRow {
		rows := map[string]deliveryRow{}
		for _, row := range deliveryRows(t, world.st, "destination-owner") {
			rows[row.JobID] = row
		}
		return rows
	}
	for _, row := range byJob() {
		assert.Equal("observing", row.Phase)
	}
	due := func() {
		_, err := world.st.DB().Exec(`UPDATE beeper_media_deliveries SET next_action_at = '2000-01-01 00:00:00.000'`)
		require.NoError(err)
	}

	due()
	runPasses(t, submitter, 2)
	rows := byJob()
	assert.Equal("done", rows[olderJob].Phase)
	assert.Equal("succeeded", rows[olderJob].OperationState)
	assert.Equal("transcribed", rows[olderJob].Coverage, "the older operation reads its own receipt")
	assert.Equal("observing", rows[newerJob].Phase, "another operation's coverage cannot settle this one")
	assert.Equal("queued", rows[newerJob].OperationState)
	assert.Equal("pending", rows[newerJob].Coverage)
	docbank.mu.Lock()
	assert.Equal(replays+1, docbank.replays)
	delete(docbank.holdIndex, 1)
	docbank.mu.Unlock()

	due()
	runPasses(t, submitter, 1)
	rows = byJob()
	assert.Equal("done", rows[newerJob].Phase)
	assert.Equal("succeeded", rows[newerJob].OperationState)
	assert.Equal("transcribed", rows[newerJob].Coverage)
	docbank.mu.Lock()
	defer docbank.mu.Unlock()
	assert.Len(docbank.processOps, 2, "reading a saved receipt starts no new processing")
}

// TestBeeperMediaLargeUpload gives a large upload time proportional to its
// size, so a slow but working link retains it instead of retrying forever.
func TestBeeperMediaLargeUpload(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	wav := syntheticWAV(800, 17)
	world := importVoiceChat(t, voiceSpec{id: "voice1", asset: "mxc://beeper.local/voice1",
		mime: "audio/wav", fileName: "voice.wav", data: wav})
	docbank := newFakeDocbank(t)
	docbank.submitDelay = 300 * time.Millisecond
	server := httptest.NewServer(docbank)
	defer server.Close()
	submitter := world.submitter(t, server, "destination-large")
	submitter.actionTimeout = 50 * time.Millisecond
	submitter.uploadRate = 0

	// Without a size allowance the transfer outlives the fixed deadline.
	runPasses(t, submitter, 1)
	rows := occurrenceRows(t, world.st, "destination-large")
	require.Len(rows, 1)
	assert.Equal("pending", rows[0].State)
	assert.Equal("timeout", rows[0].ErrorCode)

	// Scale the rate so this file's allowance is at least ten seconds.
	submitter.uploadRate = max(1, int64(len(wav))/10)
	_, err := world.st.DB().Exec(`UPDATE beeper_media_occurrences SET next_action_at = '2000-01-01 00:00:00.000'`)
	require.NoError(err)
	runPasses(t, submitter, 1)
	rows = occurrenceRows(t, world.st, "destination-large")
	require.Len(rows, 1)
	assert.Equal("retained", rows[0].State)
	assert.Empty(rows[0].ErrorCode)
	docbank.mu.Lock()
	defer docbank.mu.Unlock()
	require.Len(docbank.retentionOps, 2)
	assert.Equal(docbank.retentionOps[0], docbank.retentionOps[1])
	assert.Len(docbank.uploads, 1)
}

// TestBeeperMediaHiddenPending retires a pending row whose message is hidden
// before upload, sends nothing for it, and reopens it if the message returns.
func TestBeeperMediaHiddenPending(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	hidden, live := syntheticWAV(800, 18), syntheticWAV(800, 19)
	world := importVoiceChat(t,
		voiceSpec{id: "hidden", asset: "mxc://beeper.local/hidden", mime: "audio/wav", fileName: "voice.wav", data: hidden},
		voiceSpec{id: "live", asset: "mxc://beeper.local/live", mime: "audio/wav", fileName: "voice.wav", data: live})
	// Discovery without upload consent records both rows as pending.
	runPasses(t, NewMediaSubmitter(world.st, world.blobs, nil, "destination-hidden", world.dir), 1)
	for _, row := range occurrenceRows(t, world.st, "destination-hidden") {
		assert.Equal("pending", row.State)
	}
	var hiddenID, liveID int64
	require.NoError(world.st.DB().QueryRow(`SELECT id FROM messages WHERE source_message_id = 'hidden'`).Scan(&hiddenID))
	require.NoError(world.st.DB().QueryRow(`SELECT id FROM messages WHERE source_message_id = 'live'`).Scan(&liveID))
	_, err := world.st.MergeDuplicates(liveID, []int64{hiddenID}, "batch-hidden")
	require.NoError(err)

	docbank := newFakeDocbank(t)
	server := httptest.NewServer(docbank)
	defer server.Close()
	submitter := world.submitter(t, server, "destination-hidden")
	runPasses(t, submitter, 2)
	states := map[string]string{}
	for _, row := range occurrenceRows(t, world.st, "destination-hidden") {
		states[row.MessageID] = row.State + ":" + row.ErrorCode
	}
	assert.Equal(map[string]string{"hidden": "revoked:no_live_occurrence", "live": "retained:"}, states)

	_, err = world.st.DB().Exec(`UPDATE beeper_media_occurrences SET next_action_at = '2000-01-01 00:00:00.000'`)
	require.NoError(err)
	docbank.mu.Lock()
	requests := docbank.requests
	docbank.mu.Unlock()
	runPasses(t, submitter, 3)
	docbank.mu.Lock()
	assert.Equal(requests, docbank.requests, "a revoked row is not retried")
	assert.Equal([][]byte{live}, docbank.uploads)
	docbank.mu.Unlock()

	_, err = world.st.UndoDedup("batch-hidden")
	require.NoError(err)
	runPasses(t, submitter, 2)
	for _, row := range occurrenceRows(t, world.st, "destination-hidden") {
		assert.Equal("retained", row.State, row.MessageID)
	}
}

func TestBeeperMediaRetainActionFence(t *testing.T) {
	for _, mutation := range []struct {
		name  string
		apply func(*testing.T, *mediaWorld, store.BeeperMediaOperation)
		code  string
	}{
		{name: "dedup-hide", code: "no_live_occurrence", apply: func(t *testing.T, world *mediaWorld, operation store.BeeperMediaOperation) {
			t.Helper()
			var survivor int64
			require.NoError(t, world.st.DB().QueryRow(world.st.Rebind(
				`SELECT id FROM messages WHERE id <> ? ORDER BY id LIMIT 1`), operation.MessageID).Scan(&survivor))
			_, err := world.st.MergeDuplicates(survivor, []int64{operation.MessageID}, "retain-hide")
			require.NoError(t, err)
		}},
		{name: "source-delete", code: "no_live_occurrence", apply: func(t *testing.T, world *mediaWorld, operation store.BeeperMediaOperation) {
			t.Helper()
			var sourceID int64
			var sourceMessageID string
			require.NoError(t, world.st.DB().QueryRow(world.st.Rebind(`SELECT source_id, source_message_id FROM messages WHERE id = ?`), operation.MessageID).
				Scan(&sourceID, &sourceMessageID))
			require.NoError(t, world.st.MarkMessageDeleted(sourceID, sourceMessageID))
		}},
		{name: "attachment-replacement", code: "source_changed", apply: func(t *testing.T, world *mediaWorld, operation store.BeeperMediaOperation) {
			t.Helper()
			_, err := world.st.DB().Exec(world.st.Rebind(
				`UPDATE attachments SET content_hash = ? WHERE id = ?`), strings.Repeat("f", 64), operation.AttachmentID)
			require.NoError(t, err)
		}},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
			world := importVoiceChat(t,
				voiceSpec{id: "selected", asset: "mxc://beeper.local/selected", mime: "audio/wav",
					fileName: "selected.wav", transcript: "selected words", data: syntheticWAV(800, 28)},
				voiceSpec{id: "survivor", asset: "mxc://beeper.local/survivor", mime: "audio/wav",
					fileName: "survivor.wav", transcript: "survivor words", data: syntheticWAV(800, 29)})
			runPasses(t, NewMediaSubmitter(world.st, world.blobs, nil, "retain-fence", world.dir), 1)
			operation, ok, err := world.st.NextBeeperMediaOperation(t.Context(), "retain-fence", time.Now().UTC())
			require.NoError(err)
			require.True(ok)
			require.Equal(store.BeeperMediaOperationRetain, operation.Kind)
			docbank := newFakeDocbank(t)
			server := httptest.NewServer(docbank)
			defer server.Close()
			worker := world.submitter(t, server, "retain-fence")
			worker.WithOperationGate(func(context.Context) (func(), bool) {
				mutation.apply(t, world, operation)
				return func() {}, true
			})
			archiveUID, err := world.st.ArchiveUIDContext(t.Context())
			require.NoError(err)
			_, err = worker.retain(t.Context(), t.Context(), archiveUID, operation)
			require.NoError(err)
			assert.Equal(0, docbank.requests)
			rows := occurrenceRows(t, world.st, "retain-fence")
			var selected occurrenceRow
			for _, row := range rows {
				if row.OperationID == operation.OperationID {
					selected = row
					break
				}
			}
			assert.Equal(mutation.code, selected.ErrorCode)
			if mutation.name == "attachment-replacement" {
				assert.Equal("source_unavailable", selected.State)
			} else {
				assert.Equal("revoked", selected.State)
			}
			if mutation.name == "dedup-hide" {
				_, err := world.st.UndoDedup("retain-hide")
				require.NoError(err)
				runPasses(t, NewMediaSubmitter(world.st, world.blobs, nil, "retain-fence", world.dir), 1)
				restored := world.submitter(t, server, "retain-fence")
				archiveUID, err := world.st.ArchiveUIDContext(t.Context())
				require.NoError(err)
				_, err = restored.retain(t.Context(), t.Context(), archiveUID, operation)
				require.NoError(err)
				rows = occurrenceRows(t, world.st, "retain-fence")
				found := false
				for _, row := range rows {
					if row.OperationID == operation.OperationID {
						found = true
						assert.Equal("retained", row.State)
						break
					}
				}
				require.True(found, "the restored occurrence keeps its saved retention operation")
				docbank.mu.Lock()
				assert.Equal([]string{operation.OperationID}, docbank.retentionOps)
				docbank.mu.Unlock()
			}
		})
	}
}

func TestBeeperMediaArtifactActionFence(t *testing.T) {
	for _, shared := range []bool{false, true} {
		for _, mutation := range []string{"dedup-hide", "source-delete", "attachment-replacement"} {
			name := "single-" + mutation
			if shared {
				name = "shared-" + mutation
			}
			t.Run(name, func(t *testing.T) {
				require, assert := require.New(t), assert.New(t)
				transcript := "selected words"
				if shared {
					transcript = "shared words"
				}
				selectedData := syntheticWAV(800, 30)
				otherData := syntheticWAV(800, 31)
				if shared {
					otherData = selectedData
				}
				world := importVoiceChat(t,
					voiceSpec{id: "selected", asset: "mxc://beeper.local/artifact-selected", mime: "audio/wav",
						fileName: "selected.wav", transcript: transcript, data: selectedData},
					voiceSpec{id: "other", asset: "mxc://beeper.local/artifact-other", mime: "audio/wav",
						fileName: "other.wav", transcript: func() string {
							if shared {
								return transcript
							}
							return "other words"
						}(), data: otherData})
				docbank := newFakeDocbank(t)
				server := httptest.NewServer(docbank)
				defer server.Close()
				worker := world.submitter(t, server, "artifact-fence")
				_, err := worker.RunBatch(t.Context())
				require.NoError(err)
				if shared {
					_, err = world.st.DB().Exec(`UPDATE beeper_media_deliveries SET next_action_at = '2999-01-01 00:00:00.000'`)
					require.NoError(err)
					_, err = worker.RunBatch(t.Context())
					require.NoError(err)
					_, err = world.st.DB().Exec(`UPDATE beeper_media_deliveries SET next_action_at = '2000-01-01 00:00:00.000'`)
					require.NoError(err)
				}
				for range 4 {
					op, ok, err := world.st.NextBeeperMediaOperation(t.Context(), "artifact-fence", time.Now().UTC())
					require.NoError(err)
					if ok && op.Kind == store.BeeperMediaOperationArtifact {
						break
					}
					_, err = worker.RunBatch(t.Context())
					require.NoError(err)
				}
				operation, ok, err := world.st.NextBeeperMediaOperation(t.Context(), "artifact-fence", time.Now().UTC())
				require.NoError(err)
				require.True(ok)
				require.Equal(store.BeeperMediaOperationArtifact, operation.Kind)
				mappings, err := world.st.ListLiveBeeperMediaMappings(t.Context(), "artifact-fence", operation.ProcessingKey, 100)
				require.NoError(err)
				require.NotEmpty(mappings)
				donor := mappings[0]
				var surviving store.BeeperMediaMapping
				if shared {
					require.Len(mappings, 2)
					surviving = mappings[1]
				}
				var otherMessage int64
				require.NoError(world.st.DB().QueryRow(world.st.Rebind(`SELECT id FROM messages WHERE id <> ? ORDER BY id LIMIT 1`), donor.MessageID).Scan(&otherMessage))
				apply := func() {
					switch mutation {
					case "dedup-hide":
						_, err := world.st.MergeDuplicates(otherMessage, []int64{donor.MessageID}, "artifact-hide")
						require.NoError(err)
					case "source-delete":
						var sourceID int64
						var sourceMessageID string
						require.NoError(world.st.DB().QueryRow(world.st.Rebind(`SELECT source_id, source_message_id FROM messages WHERE id = ?`), donor.MessageID).
							Scan(&sourceID, &sourceMessageID))
						require.NoError(world.st.MarkMessageDeleted(sourceID, sourceMessageID))
					case "attachment-replacement":
						_, err := world.st.DB().Exec(world.st.Rebind(
							`UPDATE attachments SET content_hash = ? WHERE id = ?`), strings.Repeat("e", 64), donor.AttachmentID)
						require.NoError(err)
					}
				}
				calls := 0
				worker.WithOperationGate(func(context.Context) (func(), bool) {
					calls++
					if calls == 2 {
						apply()
					}
					return func() {}, true
				})
				archiveUID, err := world.st.ArchiveUIDContext(t.Context())
				require.NoError(err)
				require.NoError(worker.artifact(t.Context(), t.Context(), archiveUID, operation))
				docbank.mu.Lock()
				artifactRequests := len(docbank.artifactOps)
				docbank.mu.Unlock()
				if shared {
					assert.Equal(1, artifactRequests)
					deliveries := deliveryRows(t, world.st, "artifact-fence")
					require.Len(deliveries, 1)
					assert.Equal("pending-process", deliveries[0].Phase)
					assert.Equal(surviving.DocbankOccurrenceID, deliveries[0].Donor)
					assert.NotEmpty(deliveries[0].SuppliedInput)
					docbank.mu.Lock()
					require.Len(docbank.artifactOps, 1)
					artifactOperationID := docbank.artifactOps[0]
					assert.NotEmpty(artifactOperationID)
					require.Len(docbank.artifactReceipts, 1)
					assert.Equal(artifactOperationID, docbank.artifactReceipts[0].OperationID)
					assert.Equal(surviving.DocbankOccurrenceID, docbank.artifactReceipts[0].OccurrenceID)
					docbank.mu.Unlock()
				} else {
					assert.Zero(artifactRequests)
				}
			})
		}
	}
}

func TestBeeperMediaProcessSupplierRace(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	world := importVoiceChat(t, voiceSpec{id: "voice1", asset: "mxc://beeper.local/race",
		mime: "audio/wav", fileName: "voice.wav", transcript: "race words", data: syntheticWAV(800, 24)})
	docbank := newFakeDocbank(t)
	server := httptest.NewServer(docbank)
	defer server.Close()
	submitter := world.submitter(t, server, "destination-race")
	runPasses(t, submitter, 2)
	operation, ok, err := world.st.NextBeeperMediaOperation(t.Context(), "destination-race", time.Now().UTC())
	require.NoError(err)
	require.True(ok)
	assert.Equal(store.BeeperMediaOperationProcess, operation.Kind)
	var sourceID int64
	require.NoError(world.st.DB().QueryRow(`SELECT source_id FROM messages WHERE source_message_id = 'voice1'`).Scan(&sourceID))
	archiveUID, err := world.st.ArchiveUIDContext(t.Context())
	require.NoError(err)

	var gateCalls int
	submitter.WithOperationGate(func(context.Context) (func(), bool) {
		gateCalls++
		if gateCalls == 1 {
			return func() {
				require.NoError(world.st.MarkMessageDeleted(sourceID, "voice1"))
			}, true
		}
		return func() {}, true
	})
	require.NoError(submitter.process(t.Context(), t.Context(), archiveUID, operation))
	deliveries := deliveryRows(t, world.st, "destination-race")
	require.Len(deliveries, 1)
	assert.Equal("blocked", deliveries[0].Phase)
	assert.Equal("no_live_occurrence", deliveries[0].ErrorCode)
	assert.Equal("input-1", deliveries[0].SuppliedInput)
	docbank.mu.Lock()
	assert.Empty(docbank.processOps)
	docbank.mu.Unlock()

	require.NoError(world.st.ClearMessageDeletedFromSource(sourceID, "voice1"))
	runPasses(t, NewMediaSubmitter(world.st, world.blobs, nil, "destination-race", world.dir), 1)
	deliveries = deliveryRows(t, world.st, "destination-race")
	require.Len(deliveries, 1)
	assert.Equal("pending-process", deliveries[0].Phase)
	assert.Equal("input-1", deliveries[0].SuppliedInput)
}

func TestBeeperMediaProcessRaceKeepsPendingSupplier(t *testing.T) {
	for _, state := range []string{"pending", "source_unavailable"} {
		t.Run(state, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			wav := syntheticWAV(800, 28)
			world := importVoiceChat(t,
				voiceSpec{id: "voice1", asset: "mxc://beeper.local/race-shared-1", mime: "audio/wav",
					fileName: "voice.wav", transcript: "shared race words", data: wav},
				voiceSpec{id: "voice2", asset: "mxc://beeper.local/race-shared-2", mime: "audio/wav",
					fileName: "voice.wav", transcript: "shared race words", data: wav})
			docbank := newFakeDocbank(t)
			server := httptest.NewServer(docbank)
			defer server.Close()
			submitter := world.submitter(t, server, "destination-race-shared")

			runPasses(t, submitter, 1)
			rows := occurrenceRows(t, world.st, "destination-race-shared")
			require.Len(rows, 2)
			var retained, sibling occurrenceRow
			for _, row := range rows {
				if row.State == "retained" {
					retained = row
				} else {
					sibling = row
				}
			}
			require.Equal("retained", retained.State)
			require.Equal("pending", sibling.State)
			assert.Equal(retained.ProcessingKey, sibling.ProcessingKey)
			archiveUID, err := world.st.ArchiveUIDContext(t.Context())
			require.NoError(err)

			// Keep the second source pending while the first reaches process.
			future := time.Now().UTC().Add(24 * time.Hour)
			_, err = world.st.DB().Exec(world.st.Rebind(`
				UPDATE beeper_media_occurrences SET next_action_at = ?
				WHERE destination_key = ? AND occurrence_ref = ? AND revision = ?`),
				future, "destination-race-shared", sibling.Ref, sibling.Revision)
			require.NoError(err)
			runPasses(t, submitter, 1)
			deliveries := deliveryRows(t, world.st, "destination-race-shared")
			require.Len(deliveries, 1)
			require.Equal("pending-process", deliveries[0].Phase)

			operation, ok, err := world.st.NextBeeperMediaOperation(t.Context(), "destination-race-shared", time.Now().UTC())
			require.NoError(err)
			require.True(ok)
			require.Equal(store.BeeperMediaOperationProcess, operation.Kind)
			var sourceID int64
			require.NoError(world.st.DB().QueryRow(world.st.Rebind(`
				SELECT source_id FROM messages WHERE source_message_id = ?`), retained.MessageID).Scan(&sourceID))
			errorCode := ""
			if state == "source_unavailable" {
				errorCode = state
			}
			_, err = world.st.DB().Exec(world.st.Rebind(`
				UPDATE beeper_media_occurrences
				SET retention_state = ?, next_action_at = ?, error_code = ?
				WHERE destination_key = ? AND occurrence_ref = ? AND revision = ?`),
				state, future, errorCode,
				"destination-race-shared", sibling.Ref, sibling.Revision)
			require.NoError(err)
			require.NoError(world.st.MarkMessageDeleted(sourceID, retained.MessageID))

			require.NoError(submitter.process(t.Context(), t.Context(), archiveUID, operation))
			deliveries = deliveryRows(t, world.st, "destination-race-shared")
			require.Len(deliveries, 1)
			assert.Equal("pending-process", deliveries[0].Phase)
			assert.Empty(deliveries[0].ErrorCode)
			assert.Equal("input-1", deliveries[0].SuppliedInput)
			docbank.mu.Lock()
			assert.Empty(docbank.processOps)
			docbank.mu.Unlock()

			// Once the other source can retain the audio, the saved operation runs.
			_, err = world.st.DB().Exec(world.st.Rebind(`
				UPDATE beeper_media_occurrences SET next_action_at = ?
				WHERE destination_key = ? AND occurrence_ref = ? AND revision = ?`),
				time.Now().UTC().Add(-time.Minute), "destination-race-shared", sibling.Ref, sibling.Revision)
			require.NoError(err)
			runPasses(t, submitter, 1)
			rows = occurrenceRows(t, world.st, "destination-race-shared")
			for _, row := range rows {
				if row.Ref == sibling.Ref {
					assert.Equal("retained", row.State)
				}
			}
			resumed, ok, err := world.st.NextBeeperMediaOperation(t.Context(), "destination-race-shared", time.Now().UTC())
			require.NoError(err)
			require.True(ok)
			assert.Equal(store.BeeperMediaOperationProcess, resumed.Kind)
			assert.Equal(operation.OperationID, resumed.OperationID)
			require.NoError(submitter.process(t.Context(), t.Context(), archiveUID, resumed))
			deliveries = deliveryRows(t, world.st, "destination-race-shared")
			require.Len(deliveries, 1)
			assert.Equal("observing", deliveries[0].Phase)
			docbank.mu.Lock()
			assert.Len(docbank.processOps, 1)
			docbank.mu.Unlock()
		})
	}
}

func TestBeeperMediaStartedJobAfterRevocation(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	world := importVoiceChat(t, voiceSpec{id: "voice1", asset: "mxc://beeper.local/revoked-job",
		mime: "audio/wav", fileName: "voice.wav", transcript: "revoked job words", data: syntheticWAV(800, 25)})
	docbank := newFakeDocbank(t)
	docbank.coverage = "pending"
	server := httptest.NewServer(docbank)
	defer server.Close()
	submitter := world.submitter(t, server, "destination-revoked-job")
	runPasses(t, submitter, 4)
	deliveries := deliveryRows(t, world.st, "destination-revoked-job")
	require.Len(deliveries, 1)
	require.Equal("observing", deliveries[0].Phase)
	started := deliveries[0]
	jobID := started.JobID
	docbank.mu.Lock()
	requests := docbank.requests
	docbank.mu.Unlock()
	var sourceID int64
	require.NoError(world.st.DB().QueryRow(`SELECT source_id FROM messages WHERE source_message_id = 'voice1'`).Scan(&sourceID))
	require.NoError(world.st.MarkMessageDeleted(sourceID, "voice1"))
	require.NoError(world.st.UnregisterAttachmentChangeConsumer(t.Context(), store.BeeperMediaAttachmentConsumerKey))
	result, err := NewMediaSubmitter(world.st, world.blobs, nil, "destination-revoked-job", world.dir).RunBatch(t.Context())
	require.NoError(err)
	assert.Zero(result.Examined)
	rows := occurrenceRows(t, world.st, "destination-revoked-job")
	require.Len(rows, 1)
	assert.Equal("revoked", rows[0].State)
	deliveries = deliveryRows(t, world.st, "destination-revoked-job")
	require.Len(deliveries, 1)
	assert.Equal("observing", deliveries[0].Phase)
	assert.Equal(started.SourceID, deliveries[0].SourceID)
	assert.Equal(started.SourceVersionID, deliveries[0].SourceVersionID)
	assert.Equal(started.ContentVersionID, deliveries[0].ContentVersionID)
	assert.Equal(started.Donor, deliveries[0].Donor)
	assert.Equal(started.ProcessingOperationID, deliveries[0].ProcessingOperationID)
	assert.Equal(jobID, deliveries[0].JobID)
	docbank.mu.Lock()
	assert.Equal(requests, docbank.requests)
	docbank.mu.Unlock()
	_, err = world.st.DB().Exec(`UPDATE beeper_media_deliveries SET next_action_at = '2000-01-01 00:00:00.000'`)
	require.NoError(err)
	docbank.mu.Lock()
	docbank.coverage = "transcribed"
	docbank.mu.Unlock()
	runPasses(t, submitter, 1)
	deliveries = deliveryRows(t, world.st, "destination-revoked-job")
	require.Len(deliveries, 1)
	assert.Equal("done", deliveries[0].Phase)
	assert.Equal(jobID, deliveries[0].JobID)
	assert.Equal("succeeded", deliveries[0].OperationState)
	assert.Equal("transcribed", deliveries[0].Coverage)
}

func TestBeeperMediaRevokedObservingVaultMismatch(t *testing.T) {
	for _, tc := range []struct {
		name         string
		sourceVault  string
		processVault string
		sourceOp     string
	}{
		{name: "source status", sourceVault: "foreign-vault"},
		{name: "replayed processing receipt", processVault: "foreign-vault", sourceOp: "other-operation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			world := importVoiceChat(t, voiceSpec{id: "voice1", asset: "mxc://beeper.local/mismatch",
				mime: "audio/wav", fileName: "voice.wav", transcript: "mismatch words", data: syntheticWAV(800, 26)})
			docbank := newFakeDocbank(t)
			docbank.coverage = "pending"
			server := httptest.NewServer(docbank)
			defer server.Close()
			submitter := world.submitter(t, server, "destination-mismatch")
			runPasses(t, submitter, 4)
			deliveries := deliveryRows(t, world.st, "destination-mismatch")
			require.Len(deliveries, 1)
			require.Equal("observing", deliveries[0].Phase)
			before := len(docbank.processOps)
			var sourceID int64
			require.NoError(world.st.DB().QueryRow(`SELECT source_id FROM messages WHERE source_message_id = 'voice1'`).Scan(&sourceID))
			require.NoError(world.st.MarkMessageDeleted(sourceID, "voice1"))
			_, err := world.st.DB().Exec(`UPDATE beeper_media_deliveries SET next_action_at = '2000-01-01 00:00:00.000'`)
			require.NoError(err)
			docbank.mu.Lock()
			docbank.sourceVaultUID = tc.sourceVault
			docbank.processVaultUID = tc.processVault
			docbank.sourceOperationID = tc.sourceOp
			docbank.coverage = "transcribed"
			docbank.mu.Unlock()
			runPasses(t, submitter, 1)
			deliveries = deliveryRows(t, world.st, "destination-mismatch")
			require.Len(deliveries, 1)
			assert.Equal("blocked", deliveries[0].Phase)
			assert.Equal("destination_mismatch", deliveries[0].ErrorCode)
			docbank.mu.Lock()
			assert.Len(docbank.processOps, before)
			docbank.mu.Unlock()
		})
	}
}

func TestBeeperMediaTerminalJobBeforeSourceStatus(t *testing.T) {
	for _, state := range []string{"failed", "abandoned"} {
		t.Run(state, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			world := importVoiceChat(t, voiceSpec{id: "voice1", asset: "mxc://beeper.local/terminal-" + state,
				mime: "audio/wav", fileName: "voice.wav", transcript: "terminal words", data: syntheticWAV(800, 27)})
			docbank := newFakeDocbank(t)
			docbank.jobState = state
			docbank.jobFailureCode = "provider_" + state
			docbank.sourceHTTPStatus = http.StatusBadGateway
			server := httptest.NewServer(docbank)
			defer server.Close()
			runPasses(t, world.submitter(t, server, "destination-terminal-"+state), 4)
			deliveries := deliveryRows(t, world.st, "destination-terminal-"+state)
			require.Len(deliveries, 1)
			assert.Equal("done", deliveries[0].Phase)
			assert.Equal("failed", deliveries[0].OperationState)
			assert.Equal("unavailable", deliveries[0].Coverage)
			assert.Equal("provider_"+state, deliveries[0].ErrorCode)
			docbank.mu.Lock()
			assert.Equal(1, docbank.jobRequests)
			assert.Zero(docbank.sourceRequests)
			docbank.mu.Unlock()
		})
	}
}

func TestBeeperMediaCASBoundary(t *testing.T) {
	wav := syntheticWAV(800, 4)
	digest := sha256Hex(wav)
	descriptor := MediaDescriptor{SourceSHA256: digest, ByteLength: int64(len(wav)),
		Filename: "voice.wav", MIMEType: "audio/wav"}

	for _, storage := range []string{"loose", "packed"} {
		t.Run(storage, func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
			blobs := casStore(t, wav, storage)
			spoolDir := t.TempDir()
			file, format, err := prepareMediaUpload(t.Context(), blobs, descriptor, spoolDir)
			require.NoError(err)
			assert.Equal("wav", format)
			assert.Equal(spoolDir, filepath.Dir(file.Name()))
			got, err := io.ReadAll(file)
			require.NoError(err)
			closeAndRemove(file)
			_, err = os.Stat(file.Name())
			require.ErrorIs(err, os.ErrNotExist)
			assert.Equal(wav, got)
		})
	}

	for name, stored := range map[string][]byte{
		"corrupt": append([]byte("RIFX"), wav[4:]...), "truncated": wav[:len(wav)/2], "zero-bytes": {},
	} {
		t.Run(name, func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
			world := importVoiceChat(t, voiceSpec{id: "voice1", asset: "mxc://beeper.local/voice1",
				mime: "audio/wav", fileName: "voice.wav", transcript: "words", data: wav})
			require.NoError(os.WriteFile(filepath.Join(world.dir, digest[:2], digest), stored, 0o600))
			docbank := newFakeDocbank(t)
			server := httptest.NewServer(docbank)
			defer server.Close()
			runPasses(t, world.submitter(t, server, "destination-cas"), 1)
			docbank.mu.Lock()
			assert.Zero(docbank.requests)
			docbank.mu.Unlock()
			rows := occurrenceRows(t, world.st, "destination-cas")
			require.Len(rows, 1)
			assert.Equal("source_unavailable", rows[0].State)
			assert.Equal("source_unavailable", rows[0].ErrorCode)
			data, err := os.ReadFile(filepath.Join(world.dir, digest[:2], digest))
			require.NoError(err)
			assert.Equal(stored, data)
		})
	}
}

func TestBeeperMediaTranscriptBoundary(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	long := strings.TrimSpace(strings.Repeat("long provider words ", 2000))
	world := importVoiceChat(t, voiceSpec{id: "voice1", asset: "mxc://beeper.local/voice1",
		mime: "audio/wav", fileName: "voice.wav", transcript: long, data: syntheticWAV(800, 5)})

	var body, metadata string
	require.NoError(world.st.DB().QueryRow(`
		SELECT b.body_text, COALESCE(CAST(a.attachment_metadata AS TEXT), '')
		FROM messages m JOIN message_bodies b ON b.message_id = m.id
		JOIN attachments a ON a.message_id = m.id
		WHERE m.source_message_id = 'voice1'`).Scan(&body, &metadata))
	assert.Contains(body, long)
	assert.Less(len(metadata), 32768+1024)

	docbank := newFakeDocbank(t)
	server := httptest.NewServer(docbank)
	defer server.Close()
	runPasses(t, world.submitter(t, server, "destination-transcript"), 3)
	docbank.mu.Lock()
	require.Len(docbank.transcripts, 1)
	assert.Equal(long, docbank.transcripts[0])
	assert.Greater(len(docbank.transcripts[0]), 32768)
	docbank.mu.Unlock()

	candidate := store.BeeperMediaCandidate{SourceType: "beeper", SourceIdentifier: "signal",
		SourceConversationID: "chat", SourceMessageID: "message-1", SourceAttachmentID: "beeper:mxc://audio",
		SourcePartKey: "beeper:mxc://audio", ContentHash: strings.Repeat("a", 64), ByteLength: 10}
	_, _, err := describeMedia(syntheticRaw(t, "message-1", "mxc://audio", strings.Repeat("x", 16777216)), candidate, "archive")
	require.NoError(err)
	_, _, err = describeMedia(syntheticRaw(t, "message-1", "mxc://audio", strings.Repeat("x", 16777217)), candidate, "archive")
	require.ErrorIs(err, errBeeperMediaTranscriptTooLarge)
	_, _, err = describeMedia(append(syntheticRaw(t, "message-1", "mxc://audio", "ok"), 0xff), candidate, "archive")
	require.ErrorIs(err, errBeeperMediaRawInvalid)
}

func TestBeeperMediaReceiptIdentity(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	world := importVoiceChat(t, voiceSpec{id: "voice1", asset: "mxc://beeper.local/voice1",
		mime: "audio/wav", fileName: "voice.wav", transcript: "identity words", data: syntheticWAV(800, 6)})
	docbank := newFakeDocbank(t)
	server := httptest.NewServer(docbank)
	defer server.Close()
	runPasses(t, world.submitter(t, server, "destination-receipt"), 3)

	rows := occurrenceRows(t, world.st, "destination-receipt")
	require.Len(rows, 1)
	assert.Equal("audio-version-1", rows[0].SourceVersionID)
	assert.Equal("audio-content-1", rows[0].ContentVersionID)
	assert.Equal("occurrence-1", rows[0].OccurrenceID)
	deliveries := deliveryRows(t, world.st, "destination-receipt")
	require.Len(deliveries, 1)
	assert.Equal("input-1", deliveries[0].SuppliedInput)
	assert.Equal("audio-content-1", deliveries[0].ContentVersionID)
	assert.Equal("occurrence-1", deliveries[0].Donor)
	docbank.mu.Lock()
	defer docbank.mu.Unlock()
	require.Len(docbank.artifactReceipts, 1)
	assert.Equal("transcript-content-1", docbank.artifactReceipts[0].ContentVersionID)
	assert.NotEqual(rows[0].ContentVersionID, docbank.artifactReceipts[0].ContentVersionID)
}

func TestBeeperMediaSharedContent(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	wav := syntheticWAV(800, 7)
	world := importVoiceChat(t,
		voiceSpec{id: "voice1", asset: "mxc://beeper.local/voice1", mime: "audio/wav",
			fileName: "voice.wav", transcript: "shared words", data: wav},
		voiceSpec{id: "voice2", asset: "mxc://beeper.local/voice2", mime: "audio/wav",
			fileName: "voice.wav", transcript: "shared words", data: wav})
	docbank := newFakeDocbank(t)
	server := httptest.NewServer(docbank)
	defer server.Close()
	submitter := world.submitter(t, server, "destination-shared")
	runPasses(t, submitter, 6)

	rows := occurrenceRows(t, world.st, "destination-shared")
	require.Len(rows, 2)
	assert.NotEqual(rows[0].Ref, rows[1].Ref)
	assert.NotEqual(rows[0].OccurrenceID, rows[1].OccurrenceID)
	assert.Equal(rows[0].SourceID, rows[1].SourceID)
	assert.Equal(rows[0].ContentVersionID, rows[1].ContentVersionID)
	assert.Equal(rows[0].ProcessingKey, rows[1].ProcessingKey)
	require.Len(deliveryRows(t, world.st, "destination-shared"), 1)
	docbank.mu.Lock()
	assert.Len(docbank.uploads, 2)
	assert.Len(docbank.artifactOps, 1)
	assert.Len(docbank.processOps, 1)
	requests := docbank.requests
	docbank.mu.Unlock()

	// A repeated complete backfill finds nothing to change or send.
	_, err := world.st.DB().Exec(`UPDATE beeper_media_occurrences SET updated_at = '2001-02-03 04:05:06'`)
	require.NoError(err)
	_, err = world.st.DB().Exec(`UPDATE beeper_media_deliveries SET updated_at = '2001-02-03 04:05:06'`)
	require.NoError(err)
	before := tableSnapshot(t, world.st)
	runPasses(t, submitter, 3)
	assert.Equal(before, tableSnapshot(t, world.st))
	docbank.mu.Lock()
	assert.Equal(requests, docbank.requests)
	docbank.mu.Unlock()
}

func TestBeeperMediaEligibility(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	voice, ordinary := syntheticWAV(800, 8), syntheticWAV(900, 9)
	fakeWAV := []byte("this is not audio at all, only text bytes")
	world := importVoiceChat(t,
		voiceSpec{id: "voice1", asset: "mxc://beeper.local/voice1", mime: "audio/wav", fileName: "voice.wav", data: voice},
		voiceSpec{id: "audio1", asset: "mxc://beeper.local/audio1", mime: "audio/wav", fileName: "song.wav", data: ordinary, ordinary: true},
		voiceSpec{id: "fake1", asset: "mxc://beeper.local/fake1", mime: "audio/wav", fileName: "claim.wav", data: fakeWAV})

	other, err := world.st.GetOrCreateSource("whatsapp", "beeper-lookalike")
	require.NoError(err)
	conversation, err := world.st.EnsureConversation(other.ID, "whatsapp-thread", "Thread")
	require.NoError(err)
	otherMessage, err := world.st.UpsertMessage(&store.Message{ConversationID: conversation, SourceID: other.ID,
		SourceMessageID: "beeper-message", MessageType: "whatsapp", SizeEstimate: 10})
	require.NoError(err)
	require.NoError(world.st.UpsertAttachmentRecord(t.Context(), otherMessage, store.AttachmentWrite{
		Filename: "beeper.wav", MIMEType: "audio/wav", StoragePath: sha256Hex(voice)[:2] + "/" + sha256Hex(voice),
		ContentHash: sha256Hex(voice), Size: int64(len(voice)), SourceAttachmentID: "beeper:mxc://beeper.local/voice1",
		MediaType: "voice_note", State: attachmentpolicy.StateStored, Role: store.AttachmentRoleStandalone,
		RoleSource: store.AttachmentRoleSourceImporterSemantics,
	}))
	var beeperMessage int64
	require.NoError(world.st.DB().QueryRow(`SELECT id FROM messages WHERE source_message_id = 'voice1'`).Scan(&beeperMessage))
	require.NoError(world.st.UpsertAttachmentRecord(t.Context(), beeperMessage, store.AttachmentWrite{
		Filename: "preview.wav", MIMEType: "audio/wav", StoragePath: sha256Hex(ordinary)[:2] + "/" + sha256Hex(ordinary),
		ContentHash: sha256Hex(ordinary), Size: int64(len(ordinary)), SourceAttachmentID: "beeper:preview",
		MediaType: "audio", State: attachmentpolicy.StateStored, Role: store.AttachmentRolePreview,
		RoleSource: store.AttachmentRoleSourceImporterSemantics,
	}))

	docbank := newFakeDocbank(t)
	server := httptest.NewServer(docbank)
	defer server.Close()
	runPasses(t, world.submitter(t, server, "destination-eligible"), 4)

	docbank.mu.Lock()
	assert.ElementsMatch([][]byte{voice, ordinary}, docbank.uploads)
	docbank.mu.Unlock()
	states := map[string]string{}
	for _, row := range occurrenceRows(t, world.st, "destination-eligible") {
		states[row.MessageID] = row.State + ":" + row.ErrorCode
	}
	assert.Equal(map[string]string{
		"voice1": "retained:", "audio1": "retained:", "fake1": "blocked:unsupported_media",
	}, states)
	var messageType string
	require.NoError(world.st.DB().QueryRow(`SELECT message_type FROM messages WHERE source_message_id = 'audio1'`).Scan(&messageType))
	assert.Equal("beeper", messageType)
}

func TestBeeperMediaGaps(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	world := importVoiceChat(t,
		voiceSpec{id: "plain", asset: "mxc://beeper.local/plain", mime: "audio/wav", fileName: "plain.wav", data: syntheticWAV(800, 10)},
		voiceSpec{id: "opus", asset: "mxc://beeper.local/opus", mime: "audio/ogg", fileName: "voice.ogg",
			transcript: "ogg words", data: append([]byte("OggS"), make([]byte, 60)...)},
		voiceSpec{id: "m4a", asset: "mxc://beeper.local/m4a", mime: "audio/mp4", fileName: "voice.m4a",
			transcript: "m4a words", data: append([]byte("\x00\x00\x00\x18ftypM4A "), make([]byte, 60)...)},
		voiceSpec{id: "gap", asset: "mxc://beeper.local/gap", mime: "audio/wav", fileName: "gap.wav",
			transcript: "gap words", data: syntheticWAV(800, 11)})
	var gapMessage int64
	require.NoError(world.st.DB().QueryRow(`SELECT id FROM messages WHERE source_message_id = 'gap'`).Scan(&gapMessage))
	require.NoError(world.st.UpsertMessageRawWithFormat(gapMessage,
		[]byte(`{"id":"gap","timestamp":"2026-01-01T00:00:00Z","attachments":[]}`), "beeper_json"))

	docbank := newFakeDocbank(t)
	server := httptest.NewServer(docbank)
	defer server.Close()
	runPasses(t, world.submitter(t, server, "destination-gaps"), 5)

	states := map[string]string{}
	for _, row := range occurrenceRows(t, world.st, "destination-gaps") {
		states[row.MessageID] = row.State + ":" + row.ErrorCode + ":" + row.Coverage
	}
	assert.Equal(map[string]string{
		"plain": "retained::unprocessed", "opus": "blocked:unsupported_media:",
		"m4a": "blocked:unsupported_media:", "gap": "blocked:source_part_missing:",
	}, states)
	// Transcript deliveries of unsupported audio are retired with the same code.
	deliveries := deliveryRows(t, world.st, "destination-gaps")
	require.Len(deliveries, 2)
	for _, delivery := range deliveries {
		assert.Equal("blocked:unsupported_media", delivery.Phase+":"+delivery.ErrorCode)
		assert.Empty(delivery.SuppliedInput)
	}
	assert.Empty(deliveryNextActions(t, world.st, "destination-gaps"))
	docbank.mu.Lock()
	assert.Len(docbank.uploads, 1)
	assert.Empty(docbank.transcripts)
	assert.Empty(docbank.processOps)
	requests := docbank.requests
	docbank.mu.Unlock()

	// A daemon restart retries remote blocks only; local source gaps stay blocked.
	require.NoError(world.st.ReconsiderBlockedBeeperMediaOperations(t.Context(), "destination-gaps"))
	_, ready, err := world.st.NextBeeperMediaOperation(t.Context(), "destination-gaps", time.Now().UTC())
	require.NoError(err)
	assert.False(ready, "unsupported audio is not queued again")
	runPasses(t, world.submitter(t, server, "destination-gaps"), 3)
	for _, row := range occurrenceRows(t, world.st, "destination-gaps") {
		assert.Equal(states[row.MessageID], row.State+":"+row.ErrorCode+":"+row.Coverage)
	}
	assert.Equal(deliveries, deliveryRows(t, world.st, "destination-gaps"))
	docbank.mu.Lock()
	assert.Equal(requests, docbank.requests)
	docbank.mu.Unlock()

	// A new source revision of the same audio and transcript reopens its delivery.
	var opusMessage int64
	require.NoError(world.st.DB().QueryRow(`SELECT id FROM messages WHERE source_message_id = 'opus'`).Scan(&opusMessage))
	raw, err := world.st.GetMessageRawContext(t.Context(), opusMessage)
	require.NoError(err)
	var envelope map[string]any
	require.NoError(json.Unmarshal(raw, &envelope))
	envelope["timestamp"] = "2026-02-02T03:04:05.678Z"
	raw, err = json.Marshal(envelope)
	require.NoError(err)
	require.NoError(world.st.UpsertMessageRawWithFormat(opusMessage, raw, "beeper_json"))
	checkpoint, err := world.st.LoadBeeperMediaScan(t.Context(), "destination-gaps")
	require.NoError(err)
	due := checkpoint
	due.NextFullScanAt = time.Now().Add(-time.Hour)
	swapped, err := world.st.AdvanceBeeperMediaScan(t.Context(), "destination-gaps", checkpoint, due)
	require.NoError(err)
	require.True(swapped)
	runPasses(t, NewMediaSubmitter(world.st, world.blobs, nil, "destination-gaps", world.dir), 1)
	phases := map[string]int{}
	for _, delivery := range deliveryRows(t, world.st, "destination-gaps") {
		phases[delivery.Phase+":"+delivery.ErrorCode]++
	}
	assert.Equal(map[string]int{"pending-artifact:": 1, "blocked:unsupported_media": 1}, phases)

	// Retention finds the codec unsupported again and retires the delivery again.
	runPasses(t, world.submitter(t, server, "destination-gaps"), 2)
	for _, delivery := range deliveryRows(t, world.st, "destination-gaps") {
		assert.Equal("blocked:unsupported_media", delivery.Phase+":"+delivery.ErrorCode)
	}
	assert.Empty(deliveryNextActions(t, world.st, "destination-gaps"))
}

// TestBeeperMediaAliasIdentity sends verified WAV and MP3 under the labels
// Docbank accepts, whatever MIME alias or extension the provider reported.
func TestBeeperMediaAliasIdentity(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	world := importVoiceChat(t,
		voiceSpec{id: "mp3", asset: "mxc://beeper.local/mp3", mime: "audio/mp3", fileName: "voice.mp3", data: syntheticMP3(10)},
		voiceSpec{id: "wave", asset: "mxc://beeper.local/wave", mime: "audio/wave", fileName: "voice.wav", data: syntheticWAV(800, 21)},
		voiceSpec{id: "vnd", asset: "mxc://beeper.local/vnd", mime: "audio/vnd.wave", fileName: "memo.wave", data: syntheticWAV(800, 22)},
		voiceSpec{id: "opus", asset: "mxc://beeper.local/opus", mime: "audio/ogg", fileName: "voice.ogg",
			data: append([]byte("OggS"), make([]byte, 60)...)})
	docbank := newFakeDocbank(t)
	server := httptest.NewServer(docbank)
	defer server.Close()
	runPasses(t, world.submitter(t, server, "destination-alias"), 6)

	states := map[string]string{}
	for _, row := range occurrenceRows(t, world.st, "destination-alias") {
		states[row.MessageID] = row.State + ":" + row.ErrorCode
	}
	assert.Equal(map[string]string{"mp3": "retained:", "wave": "retained:", "vnd": "retained:",
		"opus": "blocked:unsupported_media"}, states)
	docbank.mu.Lock()
	metadata := append([]string(nil), docbank.retentionMetadata...)
	docbank.mu.Unlock()
	var sent []string
	for _, raw := range metadata {
		var fields docbankmedia.SuppliedMetadata
		require.NoError(json.Unmarshal([]byte(raw), &fields))
		sent = append(sent, fields.Filename+" "+fields.MediaType)
	}
	assert.ElementsMatch([]string{"voice.mp3 audio/mpeg", "voice.wav audio/wav", "memo.wav audio/wav"}, sent)
}

// TestBeeperMediaSpoolUnavailable waits out a local spool failure as a source
// gap with a five-minute retry instead of a permanent transport block.
func TestBeeperMediaSpoolUnavailable(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	world := importVoiceChat(t, voiceSpec{id: "voice1", asset: "mxc://beeper.local/voice1",
		mime: "audio/wav", fileName: "voice.wav", transcript: "spool words", data: syntheticWAV(800, 23)})
	docbank := newFakeDocbank(t)
	server := httptest.NewServer(docbank)
	defer server.Close()
	submitter := world.submitter(t, server, "destination-spool")
	// A file in place of the spool directory makes the filesystem unavailable.
	spoolDir := submitter.spoolDir
	obstacle := filepath.Join(t.TempDir(), "file")
	require.NoError(os.WriteFile(obstacle, []byte("obstacle"), 0o600))
	submitter.spoolDir = filepath.Join(obstacle, "spool")

	// A cancelled pass writes nothing.
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := submitter.RunBatch(cancelled)
	require.ErrorIs(err, context.Canceled)
	assert.Empty(occurrenceRows(t, world.st, "destination-spool"))

	before := time.Now().UTC()
	runPasses(t, submitter, 1)
	rows := occurrenceRows(t, world.st, "destination-spool")
	require.Len(rows, 1)
	assert.Equal("source_unavailable:source_unavailable", rows[0].State+":"+rows[0].ErrorCode)
	deliveries := deliveryRows(t, world.st, "destination-spool")
	require.Len(deliveries, 1)
	assert.Equal("pending-artifact:", deliveries[0].Phase+":"+deliveries[0].ErrorCode)
	docbank.mu.Lock()
	assert.Zero(docbank.requests)
	docbank.mu.Unlock()

	// A restart leaves the gap waiting for its retry, which succeeds once the spool recovers.
	require.NoError(world.st.ReconsiderBlockedBeeperMediaOperations(t.Context(), "destination-spool"))
	_, ready, err := world.st.NextBeeperMediaOperation(t.Context(), "destination-spool", before.Add(4*time.Minute))
	require.NoError(err)
	assert.False(ready, "the gap waits five minutes")
	operation, ready, err := world.st.NextBeeperMediaOperation(t.Context(), "destination-spool", before.Add(6*time.Minute))
	require.NoError(err)
	require.True(ready)
	assert.Equal(rows[0].OperationID, operation.OperationID)
	submitter.spoolDir = spoolDir
	_, err = world.st.DB().Exec(world.st.Rebind(`
		UPDATE beeper_media_occurrences SET next_action_at = updated_at WHERE destination_key = ?`),
		"destination-spool")
	require.NoError(err)
	runPasses(t, submitter, 4)
	rows = occurrenceRows(t, world.st, "destination-spool")
	require.Len(rows, 1)
	assert.Equal("retained:", rows[0].State+":"+rows[0].ErrorCode)
	assert.Equal("done", deliveryRows(t, world.st, "destination-spool")[0].Phase)
}

func deliveryNextActions(t *testing.T, st *store.Store, destination string) []string {
	t.Helper()
	rows, err := st.DB().Query(st.Rebind(`
		SELECT processing_key FROM beeper_media_deliveries
		WHERE destination_key = ? AND next_action_at IS NOT NULL`), destination)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var keys []string
	for rows.Next() {
		var key string
		require.NoError(t, rows.Scan(&key))
		keys = append(keys, key)
	}
	require.NoError(t, rows.Err())
	return keys
}

// fakeDocbank decodes the Docbank media wire contract at e33d77e4: strict
// metadata-then-file multipart, X-Api-Key, UUIDv4 replay and top-level
// HTTP 200 receipts. It never returns an inline transcript field.
type fakeDocbank struct {
	t                 *testing.T
	mu                sync.Mutex
	status            int
	malformed         bool
	hang              bool
	jobHTTPStatus     int
	sourceHTTPStatus  int
	jobState          string
	jobFailureCode    string
	sourceVaultUID    string
	processVaultUID   string
	sourceOperationID string
	coverage          string
	failProcessing    bool
	dropRetention     bool
	submitDelay       time.Duration
	holdIndex         map[int]bool
	requests          int
	jobRequests       int
	sourceRequests    int
	replays           int
	next              int
	sources           map[string]int
	occurrenceIDs     map[string]string
	replies           map[string]docbankmedia.Receipt
	firstMetadata     map[string]string
	uploads           [][]byte
	occurrences       []docbankmedia.Occurrence
	retentionOps      []string
	retentionMetadata []string
	artifactOps       []string
	artifactReceipts  []docbankmedia.Receipt
	transcripts       []string
	processOps        []string
	sourceOps         map[string][]string
	processReceipts   map[string]docbankmedia.Receipt
	rejected          []string
}

func newFakeDocbank(t *testing.T) *fakeDocbank {
	t.Helper()
	f := &fakeDocbank{t: t, sources: map[string]int{}, occurrenceIDs: map[string]string{},
		replies: map[string]docbankmedia.Receipt{}, firstMetadata: map[string]string{},
		sourceOps: map[string][]string{}, processReceipts: map[string]docbankmedia.Receipt{},
		holdIndex: map[int]bool{}, coverage: "transcribed"}
	t.Cleanup(func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		assert.Empty(t, f.rejected, "fake Docbank rejected a request")
	})
	return f
}

// reject records a contract violation and answers as Docbank's validator does.
func (f *fakeDocbank) reject(w http.ResponseWriter, err error) {
	f.mu.Lock()
	f.rejected = append(f.rejected, err.Error())
	f.mu.Unlock()
	http.Error(w, "validation", http.StatusUnprocessableEntity)
}

func (f *fakeDocbank) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	f.mu.Lock()
	f.requests++
	if strings.HasPrefix(path, "/api/v1/processing/jobs/") {
		f.jobRequests++
	}
	if strings.HasPrefix(path, "/api/v1/media/sources/") && r.Method == http.MethodGet {
		f.sourceRequests++
	}
	status, malformed, hang, jobHTTPStatus, sourceHTTPStatus := f.status, f.malformed, f.hang, f.jobHTTPStatus, f.sourceHTTPStatus
	jobState, jobFailureCode := f.jobState, f.jobFailureCode
	f.mu.Unlock()
	switch {
	case hang:
		// Draining lets the server notice when the client abandons the request.
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
		return
	case status != 0:
		http.Error(w, "private response body", status)
		return
	case r.Header.Get("X-Api-Key") != docbankTestKey:
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	case malformed:
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"vault_uid":`))
		return
	}
	switch {
	case r.Method == http.MethodPost && path == "/api/v1/media/sources":
		f.submit(w, r)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/artifacts"):
		f.artifact(w, r, strings.TrimSuffix(strings.TrimPrefix(path, "/api/v1/media/sources/"), "/artifacts"))
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/retry"):
		f.retry(w, r, strings.TrimSuffix(strings.TrimPrefix(path, "/api/v1/media/sources/"), "/retry"))
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/api/v1/processing/jobs/") && jobHTTPStatus != 0:
		http.Error(w, "private response body", jobHTTPStatus)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/api/v1/processing/jobs/"):
		id := strings.TrimPrefix(path, "/api/v1/processing/jobs/")
		if jobState == "" {
			jobState = "completed"
		}
		writeDocbankJSON(w, map[string]any{"job_id": id, "state": jobState, "phase": "done",
			"failure_code": jobFailureCode, "embedding_job_ids": []string{}, "completed_bindings": 1})
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/api/v1/media/sources/") && sourceHTTPStatus != 0:
		http.Error(w, "private response body", sourceHTTPStatus)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/api/v1/media/sources/"):
		f.sourceStatus(w, strings.TrimPrefix(path, "/api/v1/media/sources/"))
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeDocbank) submit(w http.ResponseWriter, r *http.Request) {
	var metadata docbankmedia.SuppliedMetadata
	raw, content, err := readContractMultipart(r, &metadata)
	if err == nil {
		err = validContractUpload(metadata.OperationID, metadata.SHA256, metadata.ByteLength, content)
	}
	if err == nil {
		err = validMediaIdentity(metadata.Filename, metadata.MediaType)
	}
	if err != nil {
		f.reject(w, err)
		return
	}
	f.mu.Lock()
	f.retentionOps = append(f.retentionOps, metadata.OperationID)
	f.retentionMetadata = append(f.retentionMetadata, string(raw))
	receipt, replay := f.replies[metadata.OperationID]
	if replay {
		assert.Equal(f.t, f.firstMetadata[metadata.OperationID], string(raw), "replayed metadata changed")
	} else {
		source, known := f.sources[metadata.SHA256]
		if !known {
			f.next++
			source = f.next
			f.sources[metadata.SHA256] = source
		}
		key := metadata.Occurrence.Ref + "|" + metadata.Occurrence.Revision
		if f.occurrenceIDs[key] == "" {
			f.occurrenceIDs[key] = "occurrence-" + strconv.Itoa(len(f.occurrenceIDs)+1)
		}
		receipt = docbankmedia.Receipt{VaultUID: "vault-1", SourceID: "source-" + strconv.Itoa(source),
			SourceVersionID: "audio-version-" + strconv.Itoa(source), ContentVersionID: "audio-content-" + strconv.Itoa(source),
			OccurrenceID: f.occurrenceIDs[key], OperationID: metadata.OperationID, Outcome: "content_available",
			OperationState: "succeeded", CoverageState: "unprocessed"}
		f.replies[metadata.OperationID] = receipt
		f.firstMetadata[metadata.OperationID] = string(raw)
		f.uploads = append(f.uploads, content)
		f.occurrences = append(f.occurrences, metadata.Occurrence)
	}
	drop, delay := f.dropRetention, f.submitDelay
	f.dropRetention = false
	f.mu.Unlock()
	if delay > 0 {
		// A slow link: the response arrives only after the upload's transfer time.
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
	if drop {
		// The request was accepted, but its response is lost in transit.
		if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
			_ = conn.Close()
		}
		return
	}
	writeDocbankJSON(w, receipt)
}

func (f *fakeDocbank) artifact(w http.ResponseWriter, r *http.Request, source string) {
	var metadata docbankmedia.ArtifactMetadata
	_, content, err := readContractMultipart(r, &metadata)
	if err == nil {
		err = validContractUpload(metadata.OperationID, metadata.SHA256, metadata.ByteLength, content)
	}
	if err != nil {
		f.reject(w, err)
		return
	}
	assert.Equal(f.t, "transcript", metadata.Kind)
	assert.Equal(f.t, "provider", metadata.Origin)
	assert.Equal(f.t, "beeper", metadata.Provider)
	assert.Equal(f.t, "text/plain", metadata.MediaType)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.artifactOps = append(f.artifactOps, metadata.OperationID)
	receipt, replay := f.replies[metadata.OperationID]
	if !replay {
		n := strconv.Itoa(len(f.artifactReceipts) + 1)
		receipt = docbankmedia.Receipt{VaultUID: "vault-1", SourceID: source,
			SourceVersionID: "transcript-version-" + n, ContentVersionID: "transcript-content-" + n,
			OccurrenceID: metadata.OccurrenceID, OperationID: metadata.OperationID,
			OperationState: "succeeded", CoverageState: "unprocessed", SuppliedInputID: "input-" + n}
		f.replies[metadata.OperationID] = receipt
		f.artifactReceipts = append(f.artifactReceipts, receipt)
		f.transcripts = append(f.transcripts, string(content))
	}
	writeDocbankJSON(w, receipt)
}

func (f *fakeDocbank) retry(w http.ResponseWriter, r *http.Request, source string) {
	var body struct {
		OperationID string                  `json:"operation_id"`
		Processing  docbankmedia.Processing `json:"processing"`
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err == nil {
		err = json.Unmarshal(data, &body, json.RejectUnknownMembers(true))
	}
	if err != nil {
		f.reject(w, err)
		return
	}
	assert.Equal(f.t, "supplied-transcript", body.Processing.Profile)
	assert.NotEmpty(f.t, body.Processing.SuppliedInputID)
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, replay := f.processReceipts[body.OperationID]; replay {
		// Docbank replays a known operation ID with its saved receipt.
		f.replays++
		receipt := f.processingReceiptLocked(body.OperationID)
		if f.processVaultUID != "" {
			receipt.VaultUID = f.processVaultUID
		}
		writeDocbankJSON(w, receipt)
		return
	}
	receipt := docbankmedia.Receipt{VaultUID: "vault-1", SourceID: source,
		OperationID: body.OperationID, JobID: sha256Hex([]byte(body.OperationID)),
		OperationState: "queued", CoverageState: "pending", SuppliedInputID: body.Processing.SuppliedInputID}
	if f.failProcessing {
		// Docbank's saved receipt after a non-retryable enqueue failure has no job.
		receipt.JobID, receipt.OperationState, receipt.CoverageState = "", "failed", "unavailable"
	}
	f.processOps = append(f.processOps, body.OperationID)
	f.sourceOps[source] = append(f.sourceOps[source], body.OperationID)
	f.processReceipts[body.OperationID] = receipt
	writeDocbankJSON(w, receipt)
}

// processingReceiptLocked applies the continuation worker: a queued operation
// succeeds with the configured coverage unless the test holds it.
func (f *fakeDocbank) processingReceiptLocked(operationID string) docbankmedia.Receipt {
	receipt := f.processReceipts[operationID]
	held := f.coverage == "pending" || f.holdIndex[slices.Index(f.processOps, operationID)]
	if receipt.OperationState == "queued" && !held {
		receipt.OperationState, receipt.CoverageState = "succeeded", f.coverage
	}
	return receipt
}

// sourceStatus follows Docbank e33d77e4: the newest processing operation
// supplies the operation fields, while coverage comes from the newest
// succeeded operation on the same source.
func (f *fakeDocbank) sourceStatus(w http.ResponseWriter, source string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	vaultUID := f.sourceVaultUID
	if vaultUID == "" {
		vaultUID = "vault-1"
	}
	receipt := docbankmedia.Receipt{VaultUID: vaultUID, SourceID: source,
		OperationState: "succeeded", CoverageState: "unprocessed"}
	operations := f.sourceOps[source]
	if len(operations) > 0 {
		newest := f.processingReceiptLocked(operations[len(operations)-1])
		receipt.OperationID, receipt.JobID = newest.OperationID, newest.JobID
		receipt.OperationState, receipt.CoverageState = newest.OperationState, newest.CoverageState
		for _, operation := range slices.Backward(operations) {
			if older := f.processingReceiptLocked(operation); older.OperationState == "succeeded" {
				receipt.CoverageState = older.CoverageState
				break
			}
		}
	}
	if f.sourceOperationID != "" {
		receipt.OperationID = f.sourceOperationID
	}
	writeDocbankJSON(w, receipt)
}

// readContractMultipart enforces Docbank's metadata-then-file envelope.
func readContractMultipart(r *http.Request, metadata any) ([]byte, []byte, error) {
	reader, err := r.MultipartReader()
	if err != nil {
		return nil, nil, fmt.Errorf("read multipart: %w", err)
	}
	first, err := reader.NextPart()
	if err != nil {
		return nil, nil, fmt.Errorf("read multipart: %w", err)
	}
	if first.FormName() != "metadata" || first.FileName() != "" {
		return nil, nil, errors.New("first multipart part must be metadata")
	}
	raw, err := io.ReadAll(io.LimitReader(first, 64<<10))
	if err != nil {
		return nil, nil, fmt.Errorf("read multipart: %w", err)
	}
	if err := json.Unmarshal(raw, metadata, json.RejectUnknownMembers(true)); err != nil {
		return nil, nil, fmt.Errorf("read multipart: %w", err)
	}
	second, err := reader.NextPart()
	if err != nil {
		return nil, nil, fmt.Errorf("read multipart: %w", err)
	}
	if second.FormName() != "file" || second.FileName() == "" {
		return nil, nil, errors.New("second multipart part must be a file")
	}
	content, err := io.ReadAll(second)
	if err != nil {
		return nil, nil, fmt.Errorf("read multipart: %w", err)
	}
	if _, err := reader.NextPart(); !errors.Is(err, io.EOF) {
		return nil, nil, errors.New("media upload requires exactly metadata and file parts")
	}
	return raw, content, nil
}

func validContractUpload(operationID, digest string, length int64, content []byte) error {
	id, err := uuid.Parse(operationID)
	switch {
	case err != nil || id.Version() != 4:
		return errors.New("operation_id must be UUIDv4")
	case sha256Hex(content) != digest || int64(len(content)) != length:
		return errors.New("upload digest or length mismatch")
	}
	return nil
}

// validMediaIdentity applies Docbank's exact supplied-media filename and type pairs.
func validMediaIdentity(filename, mediaType string) error {
	ext := strings.ToLower(filepath.Ext(filename))
	if ext == ".wav" && (mediaType == "audio/wav" || mediaType == "audio/x-wav") ||
		ext == ".mp3" && mediaType == "audio/mpeg" {
		return nil
	}
	return fmt.Errorf("supplied media requires a WAV or MP3 filename and media type, got %q %q", filename, mediaType)
}

func writeDocbankJSON(w http.ResponseWriter, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		http.Error(w, "encode", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

// casStore places content in loose or packed CAS behind the shared media reader.
func casStore(t *testing.T, content []byte, storage string) *attachmentstore.Store {
	t.Helper()
	root := t.TempDir()
	layout, err := packstore.NewLayout(root, packstore.LayoutOptions{Staging: packstore.StagingSameDirectory})
	require.NoError(t, err)
	hash, err := packstore.ParseHash(pack.ComputeBlobID(content).String())
	require.NoError(t, err)
	require.Equal(t, sha256Hex(content), hash.String())
	location := packstore.Location{Member: true}
	if storage == "loose" {
		path := layout.LoosePath(hash)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, content, 0o600))
	} else {
		require.NoError(t, pack.MkdirAllSynced(layout.PacksDir()))
		writer, err := pack.NewWriter(layout.PacksDir(), pack.WriterOptions{})
		require.NoError(t, err)
		entry, err := writer.Append(content)
		require.NoError(t, err)
		packID := writer.ID()
		_, err = writer.Seal(layout.PackPath(packID))
		require.NoError(t, err)
		location.Pack = &packstore.IndexEntry{Hash: hash, PackID: packID, Offset: int64(entry.Offset),
			StoredLen: int64(entry.StoredLen), RawLen: int64(entry.RawLen), Flags: uint8(entry.Flags), CRC32C: entry.CRC32C}
	}
	blobs, err := attachmentstore.New(casResolver{hash: location}, root)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, blobs.Close()) })
	return blobs
}

type casResolver map[packstore.Hash]packstore.Location

func (r casResolver) Resolve(_ context.Context, hash packstore.Hash) (packstore.Location, error) {
	location, ok := r[hash]
	if !ok {
		return packstore.Location{}, errors.New("unknown hash")
	}
	return location, nil
}

func tableSnapshot(t *testing.T, st *store.Store) []string {
	t.Helper()
	var snapshot []string
	for _, query := range []string{
		`SELECT occurrence_ref || '|' || revision || '|' || retention_state || '|' || retention_operation_id || '|' ||
		        error_code || '|' || COALESCE(CAST(next_action_at AS TEXT), '') || '|' || raw_hash || '|' ||
		        CAST(attachment_id AS TEXT) || '|' || CAST(updated_at AS TEXT)
		 FROM beeper_media_occurrences ORDER BY occurrence_ref, revision`,
		`SELECT processing_key || '|' || phase || '|' || COALESCE(pending_operation_id, '') || '|' ||
		        COALESCE(CAST(next_action_at AS TEXT), '') || '|' || CAST(updated_at AS TEXT)
		 FROM beeper_media_deliveries ORDER BY processing_key`,
	} {
		func() {
			rows, err := st.DB().Query(query)
			require.NoError(t, err)
			defer func() { require.NoError(t, rows.Close()) }()
			for rows.Next() {
				var value string
				require.NoError(t, rows.Scan(&value))
				snapshot = append(snapshot, value)
			}
			require.NoError(t, rows.Err())
		}()
	}
	return snapshot
}

func syntheticRaw(t *testing.T, messageID, asset, transcript string) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"id": messageID, "timestamp": "2026-09-16T10:11:12.123Z",
		"attachments": []map[string]any{{"id": asset, "type": "audio", "isVoiceNote": true,
			"mimeType": "audio/wav", "fileName": "voice.wav",
			"transcription": map[string]any{"transcription": transcript, "engine": "synthetic"}}},
	})
	require.NoError(t, err)
	return data
}

// syntheticWAV returns a valid 8 kHz mono PCM WAV whose samples vary by seed.
func syntheticWAV(samples int, seed byte) []byte {
	audio := make([]byte, samples*2)
	for i := range audio {
		audio[i] = seed + byte(i%7)
	}
	data := make([]byte, 44+len(audio))
	copy(data[0:4], "RIFF")
	binary.LittleEndian.PutUint32(data[4:8], uint32(len(data)-8))
	copy(data[8:16], "WAVEfmt ")
	binary.LittleEndian.PutUint32(data[16:20], 16)
	binary.LittleEndian.PutUint16(data[20:22], 1)
	binary.LittleEndian.PutUint16(data[22:24], 1)
	binary.LittleEndian.PutUint32(data[24:28], 8000)
	binary.LittleEndian.PutUint32(data[28:32], 16000)
	binary.LittleEndian.PutUint16(data[32:34], 2)
	binary.LittleEndian.PutUint16(data[34:36], 16)
	copy(data[36:40], "data")
	binary.LittleEndian.PutUint32(data[40:44], uint32(len(audio)))
	copy(data[44:], audio)
	return data
}

// syntheticMP3 returns MPEG-1 Layer III frames at 128 kbps and 44.1 kHz.
func syntheticMP3(frames int) []byte {
	const frameBytes = 417
	data := make([]byte, frames*frameBytes)
	for offset := 0; offset < len(data); offset += frameBytes {
		data[offset], data[offset+1], data[offset+2] = 0xff, 0xfb, 0x90
	}
	return data
}

func sha256Hex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
