package beeper

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestBeeperMediaDailyRescan(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	world := importVoiceChat(t, voiceSpec{id: "voice1", asset: "mxc://beeper.local/voice1",
		mime: "audio/wav", fileName: "voice.wav", transcript: "original", data: syntheticWAV(800, 24)})
	worker := NewMediaSubmitter(world.st, world.blobs, nil, "daily", world.dir)
	first, err := worker.RunBatch(t.Context())
	require.NoError(err)
	require.Equal(1, first.Examined)
	before := occurrenceRows(t, world.st, "daily")

	// A restart must not start the completed full scan again.
	worker = NewMediaSubmitter(world.st, world.blobs, nil, "daily", world.dir)
	idle, err := worker.RunBatch(t.Context())
	require.NoError(err)
	assert.Zero(idle.Examined)
	assert.Zero(idle.Journaled)
	assert.Equal(before, occurrenceRows(t, world.st, "daily"))

	candidates, err := world.st.ListBeeperMediaCandidates(t.Context(), 0, 1)
	require.NoError(err)
	require.Len(candidates, 1)
	raw, err := world.st.GetMessageRawContext(t.Context(), candidates[0].MessageID)
	require.NoError(err)
	require.NoError(world.st.UpsertMessageRawWithFormat(candidates[0].MessageID,
		[]byte(strings.ReplaceAll(string(raw), "original", "arrived later")), "beeper_json"))
	checkpoint, err := world.st.LoadBeeperMediaScan(t.Context(), "daily")
	require.NoError(err)
	due := checkpoint
	due.NextFullScanAt = time.Now().Add(-time.Hour)
	swapped, err := world.st.AdvanceBeeperMediaScan(t.Context(), "daily", checkpoint, due)
	require.NoError(err)
	require.True(swapped)
	scanned, err := worker.RunBatch(t.Context())
	require.NoError(err)
	assert.Equal(1, scanned.Examined)
	rows := occurrenceRows(t, world.st, "daily")
	require.Len(rows, 2)
	states := []string{rows[0].State, rows[1].State}
	assert.ElementsMatch([]string{"revoked", "pending"}, states)
}

func TestBeeperMediaDiscoveryReadsOutsideGate(t *testing.T) {
	testutil.SkipIfPostgres(t, "SQLite authorizer observes reads under the operation gate")
	require, assert := require.New(t), assert.New(t)
	world := importVoiceChat(t, voiceSpec{id: "voice1", asset: "mxc://beeper.local/voice1",
		mime: "audio/wav", fileName: "voice.wav", transcript: "words", data: syntheticWAV(800, 25)})
	world.st.DB().SetMaxOpenConns(1)
	conn, err := world.st.DB().Conn(t.Context())
	require.NoError(err)
	held, rawReadsUnderGate := false, 0
	require.NoError(conn.Raw(func(driverConn any) error {
		sqliteConn, ok := driverConn.(*sqlite3.SQLiteConn)
		require.True(ok)
		sqliteConn.RegisterAuthorizer(func(action int, table, _, _ string) int {
			if action == sqlite3.SQLITE_READ && table == "message_raw" && held {
				rawReadsUnderGate++
			}
			return sqlite3.SQLITE_OK
		})
		return nil
	}))
	require.NoError(conn.Close())
	worker := NewMediaSubmitter(world.st, world.blobs, nil, "gate-reads", world.dir).WithOperationGate(
		func(context.Context) (func(), bool) {
			held = true
			return func() { held = false }, true
		})
	result, err := worker.RunBatch(t.Context())
	require.NoError(err)
	assert.Equal(1, result.Examined)
	assert.Zero(rawReadsUnderGate, "raw-message reads and decoding must not hold the operation gate")
}

func TestBeeperMediaRawReadFailure(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	world := importVoiceChat(t, voiceSpec{id: "voice1", asset: "mxc://beeper.local/voice1",
		mime: "audio/wav", fileName: "voice.wav", data: syntheticWAV(800, 26)})
	candidates, err := world.st.ListBeeperMediaCandidates(t.Context(), 0, 1)
	require.NoError(err)
	require.Len(candidates, 1)
	worker := NewMediaSubmitter(world.st, world.blobs, nil, "read-error", world.dir)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	mapping, err := worker.mappingForCandidate(ctx, "archive", candidates[0])
	require.ErrorIs(err, context.Canceled)
	assert.Empty(mapping.Revision, "read failures must not produce a gap revision")

	_, err = world.st.DB().Exec(world.st.Rebind(`DELETE FROM message_raw WHERE message_id = ?`), candidates[0].MessageID)
	require.NoError(err)
	mapping, err = worker.mappingForCandidate(t.Context(), "archive", candidates[0])
	require.NoError(err)
	assert.Equal("source_raw_invalid", mapping.ErrorCode)

	// Corrupt archived compression is a source gap, not a database outage that stops the page.
	require.NoError(world.st.UpsertMessageRawWithFormat(candidates[0].MessageID, []byte(`{"id":"voice1"}`), "beeper_json"))
	_, err = world.st.DB().Exec(world.st.Rebind(`UPDATE message_raw SET raw_data = ?, compression = 'zlib' WHERE message_id = ?`),
		[]byte("invalid zlib"), candidates[0].MessageID)
	require.NoError(err)
	mapping, err = worker.mappingForCandidate(t.Context(), "archive", candidates[0])
	require.NoError(err)
	assert.Equal("source_raw_invalid", mapping.ErrorCode)
}

func TestBeeperMediaOperationRawReadFailure(t *testing.T) {
	t.Run("retain-source-gap", func(t *testing.T) {
		require, assert := require.New(t), assert.New(t)
		world := importVoiceChat(t, voiceSpec{id: "voice1", asset: "mxc://beeper.local/retain-gap",
			mime: "audio/wav", fileName: "voice.wav", transcript: "retain gap", data: syntheticWAV(800, 32)})
		runPasses(t, NewMediaSubmitter(world.st, world.blobs, nil, "retain-gap", world.dir), 1)
		operation, ok, err := world.st.NextBeeperMediaOperation(t.Context(), "retain-gap", time.Now().UTC())
		require.NoError(err)
		require.True(ok)
		require.Equal(store.BeeperMediaOperationRetain, operation.Kind)
		_, err = world.st.DB().Exec(world.st.Rebind(`DELETE FROM message_raw WHERE message_id = ?`), operation.MessageID)
		require.NoError(err)
		docbank := newFakeDocbank(t)
		server := httptest.NewServer(docbank)
		defer server.Close()
		worker := world.submitter(t, server, "retain-gap")
		archiveUID, err := world.st.ArchiveUIDContext(t.Context())
		require.NoError(err)
		_, err = worker.retain(t.Context(), t.Context(), archiveUID, operation)
		require.NoError(err)
		row := occurrenceRows(t, world.st, "retain-gap")[0]
		assert.Equal("source_unavailable", row.State)
		assert.Equal("source_raw_invalid", row.ErrorCode)
		assert.Zero(docbank.requests)
	})

	t.Run("artifact-source-gap", func(t *testing.T) {
		require, assert := require.New(t), assert.New(t)
		world := importVoiceChat(t, voiceSpec{id: "voice1", asset: "mxc://beeper.local/artifact-gap",
			mime: "audio/wav", fileName: "voice.wav", transcript: "artifact gap", data: syntheticWAV(800, 33)})
		docbank := newFakeDocbank(t)
		server := httptest.NewServer(docbank)
		defer server.Close()
		worker := world.submitter(t, server, "artifact-gap")
		_, err := worker.RunBatch(t.Context())
		require.NoError(err)
		docbank.mu.Lock()
		beforeRequests := docbank.requests
		docbank.mu.Unlock()
		operation, ok, err := world.st.NextBeeperMediaOperation(t.Context(), "artifact-gap", time.Now().UTC())
		require.NoError(err)
		require.True(ok)
		require.Equal(store.BeeperMediaOperationArtifact, operation.Kind)
		mappings, err := world.st.ListLiveBeeperMediaMappings(t.Context(), "artifact-gap", operation.ProcessingKey, 100)
		require.NoError(err)
		require.Len(mappings, 1)
		_, err = world.st.DB().Exec(world.st.Rebind(`DELETE FROM message_raw WHERE message_id = ?`), mappings[0].MessageID)
		require.NoError(err)
		archiveUID, err := world.st.ArchiveUIDContext(t.Context())
		require.NoError(err)
		require.NoError(worker.artifact(t.Context(), t.Context(), archiveUID, operation))
		delivery := deliveryRows(t, world.st, "artifact-gap")
		require.Len(delivery, 1)
		assert.Equal("blocked", delivery[0].Phase)
		assert.Equal("source_raw_invalid", delivery[0].ErrorCode)
		docbank.mu.Lock()
		assert.Equal(beforeRequests, docbank.requests)
		docbank.mu.Unlock()
	})

	t.Run("database-error-does-not-create-gap", func(t *testing.T) {
		testutil.SkipIfPostgres(t, "SQLite authorizer injects a real message_raw query failure")
		require, assert := require.New(t), assert.New(t)
		world := importVoiceChat(t, voiceSpec{id: "voice1", asset: "mxc://beeper.local/query-failure",
			mime: "audio/wav", fileName: "voice.wav", transcript: "query failure", data: syntheticWAV(800, 34)})
		world.st.DB().SetMaxOpenConns(1)
		conn, err := world.st.DB().Conn(t.Context())
		require.NoError(err)
		require.NoError(conn.Raw(func(driverConn any) error {
			sqliteConn, ok := driverConn.(*sqlite3.SQLiteConn)
			require.True(ok)
			sqliteConn.RegisterAuthorizer(func(action int, table, _, _ string) int {
				if action == sqlite3.SQLITE_READ && table == "message_raw" {
					return sqlite3.SQLITE_DENY
				}
				return sqlite3.SQLITE_OK
			})
			return nil
		}))
		require.NoError(conn.Close())
		worker := NewMediaSubmitter(world.st, world.blobs, nil, "query-failure", world.dir)
		_, err = worker.RunBatch(t.Context())
		require.Error(err)
		assert.Empty(occurrenceRows(t, world.st, "query-failure"))
	})
}

func TestBeeperMediaOperatorRequired(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	world := importVoiceChat(t, voiceSpec{id: "voice1", asset: "mxc://beeper.local/voice1",
		mime: "audio/wav", fileName: "voice.wav", transcript: "words", data: syntheticWAV(800, 27)})
	docbank := newFakeDocbank(t)
	docbank.coverage = "pending"
	jobReads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/processing/jobs/") {
			jobReads++
			writeDocbankJSON(w, map[string]any{"job_id": strings.TrimPrefix(r.URL.Path, "/api/v1/processing/jobs/"),
				"state": "operator_required", "phase": "failed", "failure_code": "operator_required"})
			return
		}
		docbank.ServeHTTP(w, r)
	}))
	defer server.Close()
	worker := world.submitter(t, server, "operator")
	runPasses(t, worker, 4)
	rows := deliveryRows(t, world.st, "operator")
	require.Len(rows, 1)
	assert.Equal("blocked", rows[0].Phase)
	assert.Equal("operator_required", rows[0].OperationState)
	assert.Empty(deliveryNextActions(t, world.st, "operator"))
	_, ready, err := world.st.NextBeeperMediaOperation(t.Context(), "operator", time.Now().Add(24*time.Hour))
	require.NoError(err)
	assert.False(ready)
	runPasses(t, worker, 2)
	assert.Equal(1, jobReads)
}

func TestBeeperMediaTimestampOffset(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	candidate := store.BeeperMediaCandidate{SourceType: "beeper", SourceIdentifier: "signal",
		SourceConversationID: "chat", SourceMessageID: "message-1", SourceAttachmentID: "beeper:mxc://audio",
		SourcePartKey: "beeper:mxc://audio", ContentHash: strings.Repeat("a", 64), ByteLength: 10}
	raw := []byte(`{"id":"message-1","timestamp":"2026-09-23T10:11:12.123-05:00","attachments":[{"id":"mxc://audio","type":"audio","mimeType":"audio/wav","fileName":"voice.wav"}]}`)
	descriptor, _, err := describeMedia(raw, candidate, "archive")
	require.NoError(err)
	stamp := descriptor.Occurrence.Message
	assert.Equal("2026-09-23T15:11:12.123Z", stamp.Normalized)
	assert.Equal("-05:00", stamp.ZoneText)
	assert.Equal(-18000, *stamp.OffsetSeconds)
	assert.Empty(stamp.Timezone, "a fixed offset does not identify an IANA timezone")
	assert.Equal(3, stamp.FractionDigits)
}
