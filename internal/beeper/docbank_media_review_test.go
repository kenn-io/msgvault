package beeper

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/docbankmedia"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type processDeliveryIdentity struct {
	key, phase, operationID, suppliedInputID, frozenRequest, errorCode string
}

func processDeliveryIdentities(t *testing.T, st *store.Store, destination string) []processDeliveryIdentity {
	t.Helper()
	rows, err := st.DB().Query(st.Rebind(`
		SELECT processing_key, phase, COALESCE(pending_operation_id, ''),
		       COALESCE(supplied_input_id, ''), COALESCE(frozen_request_json, ''),
		       COALESCE(error_code, '')
		FROM beeper_media_deliveries WHERE destination_key = ? ORDER BY processing_key`), destination)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var result []processDeliveryIdentity
	for rows.Next() {
		var identity processDeliveryIdentity
		require.NoError(t, rows.Scan(&identity.key, &identity.phase, &identity.operationID,
			&identity.suppliedInputID, &identity.frozenRequest, &identity.errorCode))
		result = append(result, identity)
	}
	require.NoError(t, rows.Err())
	return result
}

func preparePendingProcess(t *testing.T, worker *MediaSubmitter, destination string) store.BeeperMediaOperation {
	t.Helper()
	runPasses(t, worker, 2)
	operation, ok, err := worker.store.NextBeeperMediaOperation(t.Context(), destination, time.Now().UTC())
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, store.BeeperMediaOperationProcess, operation.Kind)
	operation.FrozenRequestJSON = mustJSON(docbankmedia.Processing{
		Profile: "supplied-transcript", SuppliedInputID: operation.SuppliedInputID,
	})
	prepared, err := worker.prepareOperation(t.Context(), operation)
	require.NoError(t, err)
	return prepared
}

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
	assert.Empty(mapping.Revision, "read failures must leave the gap revision absent")

	_, err = world.st.DB().Exec(world.st.Rebind(`DELETE FROM message_raw WHERE message_id = ?`), candidates[0].MessageID)
	require.NoError(err)
	mapping, err = worker.mappingForCandidate(t.Context(), "archive", candidates[0])
	require.NoError(err)
	assert.Equal("source_raw_invalid", mapping.ErrorCode)

	// Malformed provider JSON is a confirmed source gap, not a query failure.
	require.NoError(world.st.UpsertMessageRawWithFormat(candidates[0].MessageID, []byte("{"), "beeper_json"))
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

	for _, kind := range []string{"retain", "artifact", "process"} {
		for _, failure := range []string{"cancel", "database"} {
			t.Run(kind+"-final-read-"+failure, func(t *testing.T) {
				if failure == "database" {
					testutil.SkipIfPostgres(t, "SQLite authorizer injects a final message_raw query failure")
				}
				require, assert := require.New(t), assert.New(t)
				destination := kind + "-final-" + failure
				world := importVoiceChat(t, voiceSpec{id: "voice1", asset: "mxc://beeper.local/" + destination,
					mime: "audio/wav", fileName: "voice.wav", transcript: "final read", data: syntheticWAV(800, 35)})
				docbank := newFakeDocbank(t)
				server := httptest.NewServer(docbank)
				defer server.Close()
				worker := world.submitter(t, server, destination)
				var operation store.BeeperMediaOperation
				var ok bool
				var err error
				switch kind {
				case "retain":
					runPasses(t, NewMediaSubmitter(world.st, world.blobs, nil, destination, world.dir), 1)
				case "artifact":
					_, err = worker.RunBatch(t.Context())
					require.NoError(err)
				case "process":
					operation = preparePendingProcess(t, worker, destination)
					ok = true
				}
				if kind != "process" {
					operation, ok, err = world.st.NextBeeperMediaOperation(t.Context(), destination, time.Now().UTC())
					require.NoError(err)
					require.True(ok)
				}
				require.NoError(err)
				require.True(ok)
				switch kind {
				case "retain":
					require.Equal(store.BeeperMediaOperationRetain, operation.Kind)
				case "artifact":
					require.Equal(store.BeeperMediaOperationArtifact, operation.Kind)
				case "process":
					require.Equal(store.BeeperMediaOperationProcess, operation.Kind)
					require.NotEmpty(operation.FrozenRequestJSON)
				}
				beforeOccurrences := occurrenceRows(t, world.st, destination)
				beforeDeliveries := deliveryRows(t, world.st, destination)
				docbank.mu.Lock()
				beforeRequests := docbank.requests
				docbank.mu.Unlock()

				denyRawRead := false
				if failure == "database" {
					installMessageRawReadAuthorizer(t, world.st, &denyRawRead)
				}
				calls, finalGate := 0, 1
				if kind != "retain" {
					finalGate = 2
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				worker.WithOperationGate(func(context.Context) (func(), bool) {
					calls++
					if calls == finalGate {
						if failure == "database" {
							denyRawRead = true
						} else {
							cancel()
						}
					}
					return func() {
						if failure == "database" {
							denyRawRead = false
						}
					}, true
				})
				archiveUID, err := world.st.ArchiveUIDContext(t.Context())
				require.NoError(err)
				switch kind {
				case "retain":
					_, err = worker.retain(ctx, ctx, archiveUID, operation)
				case "artifact":
					err = worker.artifact(ctx, ctx, archiveUID, operation)
				case "process":
					err = worker.process(ctx, ctx, archiveUID, operation)
				}
				if failure == "cancel" {
					require.ErrorIs(err, context.Canceled)
				} else {
					require.Error(err)
				}
				assert.Equal(finalGate, calls)
				assert.Equal(beforeOccurrences, occurrenceRows(t, world.st, destination))
				assert.Equal(beforeDeliveries, deliveryRows(t, world.st, destination))
				after, ok, nextErr := world.st.NextBeeperMediaOperation(t.Context(), destination, time.Now().UTC())
				require.NoError(nextErr)
				require.True(ok)
				assert.Equal(operation.OperationID, after.OperationID)
				assert.Equal(operation.Kind, after.Kind)
				docbank.mu.Lock()
				assert.Equal(beforeRequests, docbank.requests)
				docbank.mu.Unlock()
			})
		}
	}

	t.Run("artifact-final-read-error-preserves-prior-stale-revocation", func(t *testing.T) {
		testutil.SkipIfPostgres(t, "SQLite authorizer injects the final message_raw query failure")
		require, assert := require.New(t), assert.New(t)
		world := importVoiceChat(t,
			voiceSpec{id: "first", asset: "mxc://beeper.local/stale-first", mime: "audio/wav",
				fileName: "first.wav", transcript: "stale shared transcript", data: syntheticWAV(800, 38)},
			voiceSpec{id: "second", asset: "mxc://beeper.local/stale-second", mime: "audio/wav",
				fileName: "second.wav", transcript: "stale shared transcript", data: syntheticWAV(800, 38)})
		docbank := newFakeDocbank(t)
		server := httptest.NewServer(docbank)
		defer server.Close()
		worker := world.submitter(t, server, "stale-final-read")
		_, err := worker.RunBatch(t.Context())
		require.NoError(err)
		_, err = world.st.DB().Exec(`UPDATE beeper_media_deliveries SET next_action_at = '2999-01-01 00:00:00.000'`)
		require.NoError(err)
		_, err = worker.RunBatch(t.Context())
		require.NoError(err)
		_, err = world.st.DB().Exec(`UPDATE beeper_media_deliveries SET next_action_at = '2000-01-01 00:00:00.000'`)
		require.NoError(err)
		operation, ok, err := world.st.NextBeeperMediaOperation(t.Context(), "stale-final-read", time.Now().UTC())
		require.NoError(err)
		require.True(ok)
		require.Equal(store.BeeperMediaOperationArtifact, operation.Kind)
		mappings, err := world.st.ListLiveBeeperMediaMappings(t.Context(), "stale-final-read", operation.ProcessingKey, 100)
		require.NoError(err)
		require.Len(mappings, 2)
		stale, sibling := mappings[0], mappings[1]
		beforeOccurrences := occurrenceRows(t, world.st, "stale-final-read")
		beforeDeliveries := deliveryRows(t, world.st, "stale-final-read")
		_, err = world.st.DB().Exec(world.st.Rebind(
			`UPDATE attachments SET content_hash = ? WHERE id = ?`), strings.Repeat("d", 64), stale.AttachmentID)
		require.NoError(err)
		denyRawRead := false
		installMessageRawReadAuthorizer(t, world.st, &denyRawRead)
		calls := 0
		worker.WithOperationGate(func(context.Context) (func(), bool) {
			calls++
			if calls == 2 {
				denyRawRead = true
			}
			return func() { denyRawRead = false }, true
		})
		archiveUID, err := world.st.ArchiveUIDContext(t.Context())
		require.NoError(err)
		err = worker.artifact(t.Context(), t.Context(), archiveUID, operation)
		require.Error(err)
		assert.Equal(2, calls)
		rows := occurrenceRows(t, world.st, "stale-final-read")
		require.Len(rows, len(beforeOccurrences))
		var staleRow, siblingRow *occurrenceRow
		for i := range rows {
			if rows[i].Ref == stale.OccurrenceRef && rows[i].Revision == stale.Revision {
				staleRow = &rows[i]
			}
			if rows[i].Ref == sibling.OccurrenceRef && rows[i].Revision == sibling.Revision {
				siblingRow = &rows[i]
			}
		}
		require.NotNil(staleRow)
		require.NotNil(siblingRow)
		assert.Equal("revoked", staleRow.State, "the earlier authoritative stale check remains committed")
		assert.Equal("retained", siblingRow.State)
		assert.Equal(beforeDeliveries, deliveryRows(t, world.st, "stale-final-read"))
		after, ok, err := world.st.NextBeeperMediaOperation(t.Context(), "stale-final-read", time.Now().UTC())
		require.NoError(err)
		require.True(ok)
		assert.Equal(operation.OperationID, after.OperationID)
		docbank.mu.Lock()
		assert.Empty(docbank.artifactOps)
		docbank.mu.Unlock()
	})

	for _, kind := range []string{"retain", "artifact"} {
		for _, rawFailure := range []string{"malformed-envelope", "corrupt-compression"} {
			t.Run(kind+"-"+rawFailure, func(t *testing.T) {
				require, assert := require.New(t), assert.New(t)
				destination := kind + "-" + rawFailure
				world := importVoiceChat(t, voiceSpec{id: "voice1", asset: "mxc://beeper.local/" + destination,
					mime: "audio/wav", fileName: "voice.wav", transcript: "bad raw", data: syntheticWAV(800, 36)})
				docbank := newFakeDocbank(t)
				server := httptest.NewServer(docbank)
				defer server.Close()
				worker := world.submitter(t, server, destination)
				if kind == "retain" {
					runPasses(t, NewMediaSubmitter(world.st, world.blobs, nil, destination, world.dir), 1)
				} else {
					_, err := worker.RunBatch(t.Context())
					require.NoError(err)
				}
				operation, ok, err := world.st.NextBeeperMediaOperation(t.Context(), destination, time.Now().UTC())
				require.NoError(err)
				require.True(ok)
				messageID := operation.MessageID
				var artifactMapping store.BeeperMediaMapping
				if kind == "retain" {
					require.Equal(store.BeeperMediaOperationRetain, operation.Kind)
				} else {
					require.Equal(store.BeeperMediaOperationArtifact, operation.Kind)
					mappings, err := world.st.ListLiveBeeperMediaMappings(t.Context(), destination, operation.ProcessingKey, 100)
					require.NoError(err)
					require.Len(mappings, 1)
					artifactMapping = mappings[0]
					messageID = artifactMapping.MessageID
				}
				if rawFailure == "malformed-envelope" {
					require.NoError(world.st.UpsertMessageRawWithFormat(messageID, []byte("{"), "beeper_json"))
				} else {
					_, err := world.st.DB().Exec(world.st.Rebind(
						`UPDATE message_raw SET raw_data = ?, compression = 'zlib' WHERE message_id = ?`),
						[]byte("corrupt zlib"), messageID)
					require.NoError(err)
				}
				docbank.mu.Lock()
				beforeRequests := docbank.requests
				docbank.mu.Unlock()
				archiveUID, err := world.st.ArchiveUIDContext(t.Context())
				require.NoError(err)
				if kind == "retain" {
					_, err = worker.retain(t.Context(), t.Context(), archiveUID, operation)
					require.NoError(err)
					var retained *occurrenceRow
					rows := occurrenceRows(t, world.st, destination)
					for i := range rows {
						if rows[i].OperationID == operation.OperationID {
							retained = &rows[i]
							break
						}
					}
					require.NotNil(retained)
					assert.Equal("source_unavailable", retained.State)
					assert.Equal("source_raw_invalid", retained.ErrorCode)
				} else {
					evidence, err := worker.mappingEvidence(t.Context(), archiveUID, artifactMapping)
					require.NoError(err)
					assert.Equal("source_raw_invalid", evidence.gapCode)
					require.NoError(worker.artifact(t.Context(), t.Context(), archiveUID, operation))
					deliveries := deliveryRows(t, world.st, destination)
					require.Len(deliveries, 1)
					assert.Equal("blocked", deliveries[0].Phase)
					assert.Equal("source_raw_invalid", deliveries[0].ErrorCode)
				}
				docbank.mu.Lock()
				assert.Equal(beforeRequests, docbank.requests)
				assert.Empty(docbank.artifactOps)
				docbank.mu.Unlock()
			})
		}
	}

	for _, rawFailure := range []string{"missing", "malformed-envelope", "corrupt-compression"} {
		t.Run("process-"+rawFailure, func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
			destination := "process-raw-" + rawFailure
			world := importVoiceChat(t, voiceSpec{id: "voice1", asset: "mxc://beeper.example/" + destination,
				mime: "audio/wav", fileName: "voice.wav", transcript: "process raw gap", data: syntheticWAV(800, 46)})
			docbank := newFakeDocbank(t)
			server := httptest.NewServer(docbank)
			defer server.Close()
			worker := world.submitter(t, server, destination)
			operation := preparePendingProcess(t, worker, destination)
			beforeOperationID, beforeInput, beforeFrozen := operation.OperationID, operation.SuppliedInputID, operation.FrozenRequestJSON
			mappings, err := world.st.ListLiveBeeperMediaMappings(t.Context(), destination, operation.ProcessingKey, 1)
			require.NoError(err)
			require.Len(mappings, 1)
			messageID := mappings[0].MessageID
			switch rawFailure {
			case "missing":
				_, err := world.st.DB().Exec(world.st.Rebind(`DELETE FROM message_raw WHERE message_id = ?`), messageID)
				require.NoError(err)
			case "malformed-envelope":
				require.NoError(world.st.UpsertMessageRawWithFormat(messageID, []byte("{"), "beeper_json"))
			case "corrupt-compression":
				_, err := world.st.DB().Exec(world.st.Rebind(
					`UPDATE message_raw SET raw_data = ?, compression = 'zlib' WHERE message_id = ?`),
					[]byte("corrupt zlib"), messageID)
				require.NoError(err)
			}
			archiveUID, err := world.st.ArchiveUIDContext(t.Context())
			require.NoError(err)
			require.NoError(worker.process(t.Context(), t.Context(), archiveUID, operation))
			docbank.mu.Lock()
			assert.Empty(docbank.processOps)
			docbank.mu.Unlock()
			identity := findProcessDelivery(t, processDeliveryIdentities(t, world.st, destination), operation.ProcessingKey)
			assert.Equal("blocked", identity.phase)
			assert.Equal("source_raw_invalid", identity.errorCode)
			assert.Equal(beforeOperationID, identity.operationID)
			assert.Equal(beforeInput, identity.suppliedInputID)
			assert.Equal(beforeFrozen, identity.frozenRequest)
			rows := occurrenceRows(t, world.st, destination)
			require.Len(rows, 2)
			var revoked, gap *occurrenceRow
			for i := range rows {
				if rows[i].ErrorCode == "source_raw_invalid" {
					gap = &rows[i]
				} else {
					revoked = &rows[i]
				}
			}
			require.NotNil(revoked)
			require.NotNil(gap)
			assert.Equal("revoked", revoked.State)
			assert.Equal("blocked", gap.State)
		})
	}

	t.Run("artifact-source-changed", func(t *testing.T) {
		require, assert := require.New(t), assert.New(t)
		world := importVoiceChat(t, voiceSpec{id: "voice1", asset: "mxc://beeper.example/source-changed",
			mime: "audio/wav", fileName: "voice.wav", transcript: "changed source", data: syntheticWAV(800, 39)})
		docbank := newFakeDocbank(t)
		server := httptest.NewServer(docbank)
		defer server.Close()
		worker := world.submitter(t, server, "source-changed")
		_, err := worker.RunBatch(t.Context())
		require.NoError(err)
		operation, ok, err := world.st.NextBeeperMediaOperation(t.Context(), "source-changed", time.Now().UTC())
		require.NoError(err)
		require.True(ok)
		require.Equal(store.BeeperMediaOperationArtifact, operation.Kind)
		mappings, err := world.st.ListLiveBeeperMediaMappings(t.Context(), "source-changed", operation.ProcessingKey, 100)
		require.NoError(err)
		require.Len(mappings, 1)
		donor := mappings[0]
		beforeRequests := func() int {
			docbank.mu.Lock()
			defer docbank.mu.Unlock()
			return docbank.requests
		}()

		require.NoError(world.st.UpsertMessageRawWithFormat(donor.MessageID,
			[]byte(`{"id":"different-message"}`), "beeper_json"))
		archiveUID, err := world.st.ArchiveUIDContext(t.Context())
		require.NoError(err)
		require.NoError(worker.artifact(t.Context(), t.Context(), archiveUID, operation))

		rows := occurrenceRows(t, world.st, "source-changed")
		var old, replacement *occurrenceRow
		for i := range rows {
			row := &rows[i]
			if row.Ref != donor.OccurrenceRef {
				continue
			}
			if row.Revision == donor.Revision {
				old = row
			} else if row.ErrorCode == "source_changed" {
				replacement = row
			}
		}
		require.NotNil(old)
		assert.Equal("revoked", old.State)
		require.NotNil(replacement)
		assert.NotEqual(donor.Revision, replacement.Revision)
		assert.Equal("blocked", replacement.State)
		assert.Equal("source_changed", replacement.ErrorCode)

		deliveries := deliveryRows(t, world.st, "source-changed")
		require.Len(deliveries, 1)
		assert.Equal("blocked", deliveries[0].Phase)
		assert.Equal("source_changed", deliveries[0].ErrorCode)
		docbank.mu.Lock()
		assert.Equal(beforeRequests, docbank.requests)
		assert.Empty(docbank.artifactOps)
		docbank.mu.Unlock()
		_, ready, err := world.st.NextBeeperMediaOperation(t.Context(), "source-changed", time.Now().UTC())
		require.NoError(err)
		assert.False(ready)
	})

	t.Run("artifact-corrupt-shared-donor-uses-sibling", func(t *testing.T) {
		require, assert := require.New(t), assert.New(t)
		world := importVoiceChat(t,
			voiceSpec{id: "first", asset: "mxc://beeper.local/shared-first", mime: "audio/wav",
				fileName: "first.wav", transcript: "shared transcript", data: syntheticWAV(800, 37)},
			voiceSpec{id: "second", asset: "mxc://beeper.local/shared-second", mime: "audio/wav",
				fileName: "second.wav", transcript: "shared transcript", data: syntheticWAV(800, 37)})
		docbank := newFakeDocbank(t)
		server := httptest.NewServer(docbank)
		defer server.Close()
		worker := world.submitter(t, server, "shared-gap")
		_, err := worker.RunBatch(t.Context())
		require.NoError(err)
		_, err = world.st.DB().Exec(`UPDATE beeper_media_deliveries SET next_action_at = '2999-01-01 00:00:00.000'`)
		require.NoError(err)
		_, err = worker.RunBatch(t.Context())
		require.NoError(err)
		_, err = world.st.DB().Exec(`UPDATE beeper_media_deliveries SET next_action_at = '2000-01-01 00:00:00.000'`)
		require.NoError(err)
		var operation store.BeeperMediaOperation
		for range 4 {
			operation, _, err = world.st.NextBeeperMediaOperation(t.Context(), "shared-gap", time.Now().UTC())
			require.NoError(err)
			if operation.Kind == store.BeeperMediaOperationArtifact {
				break
			}
			_, err = worker.RunBatch(t.Context())
			require.NoError(err)
		}
		require.Equal(store.BeeperMediaOperationArtifact, operation.Kind)
		mappings, err := world.st.ListLiveBeeperMediaMappings(t.Context(), "shared-gap", operation.ProcessingKey, 100)
		require.NoError(err)
		require.Len(mappings, 2)
		withdrawn, surviving := mappings[0], mappings[1]
		_, err = world.st.DB().Exec(world.st.Rebind(
			`UPDATE message_raw SET raw_data = ?, compression = 'zlib' WHERE message_id = ?`),
			[]byte("corrupt zlib"), withdrawn.MessageID)
		require.NoError(err)
		docbank.mu.Lock()
		beforeRequests := docbank.requests
		docbank.mu.Unlock()
		archiveUID, err := world.st.ArchiveUIDContext(t.Context())
		require.NoError(err)
		require.NoError(worker.artifact(t.Context(), t.Context(), archiveUID, operation))
		deliveries := deliveryRows(t, world.st, "shared-gap")
		require.Len(deliveries, 1)
		assert.Equal("pending-process", deliveries[0].Phase)
		assert.Equal(surviving.DocbankOccurrenceID, deliveries[0].Donor)
		assert.NotEmpty(deliveries[0].SuppliedInput)
		assert.NotEmpty(deliveries[0].PendingOperationID)
		docbank.mu.Lock()
		assert.Equal(beforeRequests+1, docbank.requests)
		require.Len(docbank.artifactOps, 1)
		assert.NotEmpty(docbank.artifactOps[0])
		require.Len(docbank.artifactReceipts, 1)
		assert.Equal(docbank.artifactOps[0], docbank.artifactReceipts[0].OperationID)
		assert.Equal(surviving.DocbankOccurrenceID, docbank.artifactReceipts[0].OccurrenceID)
		docbank.mu.Unlock()
		rows := occurrenceRows(t, world.st, "shared-gap")
		var gap *occurrenceRow
		for i := range rows {
			if rows[i].Ref == withdrawn.OccurrenceRef && rows[i].Revision != withdrawn.Revision {
				gap = &rows[i]
				break
			}
		}
		require.NotNil(gap)
		assert.Equal("blocked", gap.State)
		assert.Equal("source_raw_invalid", gap.ErrorCode)
	})
}

func TestBeeperMediaArtifactReactionRefresh(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	world := importVoiceChat(t, voiceSpec{id: "voice1", asset: "mxc://beeper.example/reaction-refresh",
		mime: "audio/wav", fileName: "voice.wav", transcript: "reaction transcript", data: syntheticWAV(800, 40)})
	docbank := newFakeDocbank(t)
	server := httptest.NewServer(docbank)
	defer server.Close()
	worker := world.submitter(t, server, "reaction-refresh")
	_, err := worker.RunBatch(t.Context())
	require.NoError(err)
	operation, ok, err := world.st.NextBeeperMediaOperation(t.Context(), "reaction-refresh", time.Now().UTC())
	require.NoError(err)
	require.True(ok)
	require.Equal(store.BeeperMediaOperationArtifact, operation.Kind)
	before, err := world.st.ListLiveBeeperMediaMappings(t.Context(), "reaction-refresh", operation.ProcessingKey, 100)
	require.NoError(err)
	require.Len(before, 1)

	raw, err := world.st.GetMessageRawContext(t.Context(), before[0].MessageID)
	require.NoError(err)
	updated := strings.Replace(string(raw), `"attachments":[`, `"reactions":[{"reactionKey":"reaction-1"}],"attachments":[`, 1)
	require.NotEqual(string(raw), updated)
	require.NoError(world.st.UpsertMessageRawWithFormat(before[0].MessageID, []byte(updated), "beeper_json"))
	newRawHash := hashBytes([]byte(updated))
	require.NotEqual(before[0].RawHash, newRawHash)

	_, err = worker.RunBatch(t.Context())
	require.NoError(err)
	docbank.mu.Lock()
	require.Len(docbank.artifactOps, 1)
	assert.NotEmpty(docbank.artifactOps[0])
	require.Len(docbank.artifactReceipts, 1)
	assert.Equal(docbank.artifactOps[0], docbank.artifactReceipts[0].OperationID)
	docbank.mu.Unlock()

	after, err := world.st.ListLiveBeeperMediaMappings(t.Context(), "reaction-refresh", operation.ProcessingKey, 100)
	require.NoError(err)
	require.Len(after, 1)
	assert.Equal(before[0].OccurrenceRef, after[0].OccurrenceRef)
	assert.Equal(before[0].Revision, after[0].Revision)
	assert.Equal(before[0].SourceSHA256, after[0].SourceSHA256)
	assert.Equal(before[0].ProcessingKey, after[0].ProcessingKey)
	assert.Equal(before[0].DocbankOccurrenceID, after[0].DocbankOccurrenceID)
	assert.Equal(newRawHash, after[0].RawHash)
	deliveries := deliveryRows(t, world.st, "reaction-refresh")
	require.Len(deliveries, 1)
	assert.Equal("pending-process", deliveries[0].Phase)
	assert.NotEmpty(deliveries[0].SuppliedInput)
}

func TestBeeperMediaArtifactRevisionChangeDoesNotReuseReceipt(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	world := importVoiceChat(t, voiceSpec{id: "voice1", asset: "mxc://beeper.example/revision-change",
		mime: "audio/wav", fileName: "voice.wav", transcript: "revision transcript", data: syntheticWAV(800, 43)})
	docbank := newFakeDocbank(t)
	server := httptest.NewServer(docbank)
	defer server.Close()
	destination := "revision-change"
	worker := world.submitter(t, server, destination)
	_, err := worker.RunBatch(t.Context())
	require.NoError(err)
	operation, ok, err := world.st.NextBeeperMediaOperation(t.Context(), destination, time.Now().UTC())
	require.NoError(err)
	require.True(ok)
	require.Equal(store.BeeperMediaOperationArtifact, operation.Kind)
	mappings, err := world.st.ListLiveBeeperMediaMappings(t.Context(), destination, operation.ProcessingKey, 100)
	require.NoError(err)
	require.Len(mappings, 1)
	old := mappings[0]
	raw, err := world.st.GetMessageRawContext(t.Context(), old.MessageID)
	require.NoError(err)
	updated := strings.Replace(string(raw), "voice.wav", "revision.wav", 1)
	require.NotEqual(string(raw), updated)
	require.NoError(world.st.UpsertMessageRawWithFormat(old.MessageID, []byte(updated), "beeper_json"))
	archiveUID, err := world.st.ArchiveUIDContext(t.Context())
	require.NoError(err)
	descriptor, transcript, err := describeMedia([]byte(updated), mappingCandidate(old), archiveUID)
	require.NoError(err)
	assert.Equal("revision transcript", transcript)
	assert.Equal(old.ProcessingKey, descriptor.ProcessingKey)
	assert.Equal(old.TranscriptSHA256, descriptor.TranscriptSHA256)
	assert.NotEqual(old.Revision, descriptor.Occurrence.Revision)
	docbank.mu.Lock()
	beforeRequests := docbank.requests
	docbank.mu.Unlock()
	require.NoError(worker.artifact(t.Context(), t.Context(), archiveUID, operation))
	docbank.mu.Lock()
	assert.Equal(beforeRequests, docbank.requests)
	assert.Empty(docbank.artifactOps)
	docbank.mu.Unlock()
	rows := occurrenceRows(t, world.st, destination)
	require.Len(rows, 1)
	assert.Equal(old.Revision, rows[0].Revision)
	assert.Equal(old.DocbankOccurrenceID, rows[0].OccurrenceID)
	deliveries := deliveryRows(t, world.st, destination)
	require.Len(deliveries, 1)
	assert.Equal("pending-artifact", deliveries[0].Phase)

	checkpoint, err := world.st.LoadBeeperMediaScan(t.Context(), destination)
	require.NoError(err)
	due := checkpoint
	due.NextFullScanAt = time.Now().UTC().Add(-time.Hour)
	swapped, err := world.st.AdvanceBeeperMediaScan(t.Context(), destination, checkpoint, due)
	require.NoError(err)
	require.True(swapped)
	discovery := NewMediaSubmitter(world.st, world.blobs, nil, destination, world.dir)
	result, err := discovery.RunBatch(t.Context())
	require.NoError(err)
	assert.Equal(1, result.Examined)
	rows = occurrenceRows(t, world.st, destination)
	require.Len(rows, 2)
	var oldRow, currentRow *occurrenceRow
	for i := range rows {
		if rows[i].Revision == old.Revision {
			oldRow = &rows[i]
		} else {
			currentRow = &rows[i]
		}
	}
	require.NotNil(oldRow)
	require.NotNil(currentRow)
	assert.Equal("revoked", oldRow.State)
	assert.Equal(old.DocbankOccurrenceID, oldRow.OccurrenceID)
	assert.Equal(old.OccurrenceRef, currentRow.Ref)
	assert.Equal(old.ProcessingKey, currentRow.ProcessingKey)
	assert.Equal("pending", currentRow.State)
	assert.Empty(currentRow.OccurrenceID)
	deliveries = deliveryRows(t, world.st, destination)
	require.Len(deliveries, 1)
	assert.Equal("pending-artifact", deliveries[0].Phase)
	docbank.mu.Lock()
	assert.Equal(beforeRequests, docbank.requests)
	docbank.mu.Unlock()
}

func TestBeeperMediaArtifactFinalReadSourceChangeRetries(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	world := importVoiceChat(t, voiceSpec{id: "voice1", asset: "mxc://beeper.example/final-read-change",
		mime: "audio/wav", fileName: "voice.wav", transcript: "final read transcript", data: syntheticWAV(800, 41)})
	docbank := newFakeDocbank(t)
	server := httptest.NewServer(docbank)
	defer server.Close()
	worker := world.submitter(t, server, "final-read-change")
	_, err := worker.RunBatch(t.Context())
	require.NoError(err)
	operation, ok, err := world.st.NextBeeperMediaOperation(t.Context(), "final-read-change", time.Now().UTC())
	require.NoError(err)
	require.True(ok)
	require.Equal(store.BeeperMediaOperationArtifact, operation.Kind)
	mappings, err := world.st.ListLiveBeeperMediaMappings(t.Context(), "final-read-change", operation.ProcessingKey, 100)
	require.NoError(err)
	require.Len(mappings, 1)
	raw, err := world.st.GetMessageRawContext(t.Context(), mappings[0].MessageID)
	require.NoError(err)
	updated := strings.Replace(string(raw), `"attachments":[`, `"reactions":[{"reactionKey":"reaction-2"}],"attachments":[`, 1)
	require.NotEqual(string(raw), updated)
	newRawHash := hashBytes([]byte(updated))

	calls := 0
	worker.WithOperationGate(func(context.Context) (func(), bool) {
		calls++
		if calls == 2 {
			require.NoError(world.st.UpsertMessageRawWithFormat(mappings[0].MessageID, []byte(updated), "beeper_json"))
		}
		return func() {}, true
	})
	archiveUID, err := world.st.ArchiveUIDContext(t.Context())
	require.NoError(err)
	require.NoError(worker.artifact(t.Context(), t.Context(), archiveUID, operation))
	assert.Equal(2, calls)
	docbank.mu.Lock()
	assert.Empty(docbank.artifactOps)
	docbank.mu.Unlock()
	deliveries := deliveryRows(t, world.st, "final-read-change")
	require.Len(deliveries, 1)
	savedOperationID := deliveries[0].PendingOperationID
	require.NotEmpty(savedOperationID)
	assert.Equal("pending-artifact", deliveries[0].Phase)
	assert.Equal("source_changed", deliveries[0].ErrorCode)
	var nextActionAt time.Time
	require.NoError(world.st.DB().QueryRow(world.st.Rebind(`
		SELECT next_action_at FROM beeper_media_deliveries WHERE destination_key = ?`),
		"final-read-change").Scan(&nextActionAt))
	assert.True(nextActionAt.After(time.Now().UTC()))

	_, err = world.st.DB().Exec(world.st.Rebind(`
		UPDATE beeper_media_deliveries SET next_action_at = ? WHERE destination_key = ?`),
		time.Now().UTC().Add(-time.Minute), "final-read-change")
	require.NoError(err)
	_, err = worker.RunBatch(t.Context())
	require.NoError(err)
	docbank.mu.Lock()
	require.Len(docbank.artifactOps, 1)
	assert.Equal(savedOperationID, docbank.artifactOps[0])
	docbank.mu.Unlock()
	after, err := world.st.ListLiveBeeperMediaMappings(t.Context(), "final-read-change", operation.ProcessingKey, 100)
	require.NoError(err)
	require.Len(after, 1)
	assert.Equal(newRawHash, after[0].RawHash)
	deliveries = deliveryRows(t, world.st, "final-read-change")
	require.Len(deliveries, 1)
	assert.Equal("pending-process", deliveries[0].Phase)
}

func TestBeeperMediaProcessDescriptorRefresh(t *testing.T) {
	tests := []struct {
		name         string
		mutate       func(string) string
		keyChanges   bool
		reactionOnly bool
	}{
		{name: "transcript", mutate: func(raw string) string {
			return strings.Replace(raw, "refresh transcript", "replacement transcript", 1)
		}, keyChanges: true},
		{name: "language", mutate: func(raw string) string {
			return strings.Replace(raw, `"language":"en"`, `"language":"fr"`, 1)
		}, keyChanges: true},
		{name: "filename", mutate: func(raw string) string {
			return strings.Replace(raw, `"fileName":"voice.wav"`, `"fileName":"renamed.wav"`, 1)
		}},
		{name: "timestamp", mutate: func(raw string) string {
			marker := `"timestamp":"`
			start := strings.Index(raw, marker)
			if start < 0 {
				return raw
			}
			start += len(marker)
			end := strings.IndexByte(raw[start:], '"')
			if end < 0 {
				return raw
			}
			return raw[:start] + "2026-09-23T10:11:12.123Z" + raw[start+end:]
		}},
		{name: "reaction", mutate: func(raw string) string {
			return strings.Replace(raw, `"attachments":[`, `"reactions":[{"reactionKey":"reaction-process"}],"attachments":[`, 1)
		}, reactionOnly: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
			world := importVoiceChat(t, voiceSpec{id: "voice1", asset: "mxc://beeper.example/process-refresh-" + tc.name,
				mime: "audio/wav", fileName: "voice.wav", transcript: "refresh transcript", data: syntheticWAV(800, 44)})
			docbank := newFakeDocbank(t)
			server := httptest.NewServer(docbank)
			defer server.Close()
			destination := "process-refresh-" + tc.name
			worker := world.submitter(t, server, destination)
			operation := preparePendingProcess(t, worker, destination)
			beforeOperationID, beforeInput, beforeFrozen := operation.OperationID, operation.SuppliedInputID, operation.FrozenRequestJSON
			mappings, err := world.st.ListLiveBeeperMediaMappings(t.Context(), destination, operation.ProcessingKey, 1)
			require.NoError(err)
			require.Len(mappings, 1)
			beforeMapping := mappings[0]
			raw, err := world.st.GetMessageRawContext(t.Context(), beforeMapping.MessageID)
			require.NoError(err)
			updated := tc.mutate(string(raw))
			require.NotEqual(string(raw), updated)
			require.NoError(world.st.UpsertMessageRawWithFormat(beforeMapping.MessageID, []byte(updated), "beeper_json"))
			archiveUID, err := world.st.ArchiveUIDContext(t.Context())
			require.NoError(err)

			require.NoError(worker.process(t.Context(), t.Context(), archiveUID, operation))
			docbank.mu.Lock()
			processOps := append([]string(nil), docbank.processOps...)
			processReceipt, processSeen := docbank.processReceipts[beforeOperationID]
			docbank.mu.Unlock()

			if tc.reactionOnly {
				require.Len(processOps, 1)
				assert.Equal(beforeOperationID, processOps[0])
				assert.True(processSeen)
				assert.Equal(beforeInput, processReceipt.SuppliedInputID)
				assert.Equal("observing", deliveryRows(t, world.st, destination)[0].Phase)
				after, err := world.st.ListLiveBeeperMediaMappings(t.Context(), destination, beforeMapping.ProcessingKey, 1)
				require.NoError(err)
				require.Len(after, 1)
				assert.Equal(beforeMapping.Revision, after[0].Revision)
				assert.Equal(hashBytes([]byte(updated)), after[0].RawHash)
				return
			}

			assert.Empty(processOps)
			identities := processDeliveryIdentities(t, world.st, destination)
			oldIdentity := findProcessDelivery(t, identities, beforeMapping.ProcessingKey)
			assert.Equal(beforeOperationID, oldIdentity.operationID)
			assert.Equal(beforeInput, oldIdentity.suppliedInputID)
			assert.Equal(beforeFrozen, oldIdentity.frozenRequest)
			rows := occurrenceRows(t, world.st, destination)
			if tc.keyChanges {
				assert.Equal("blocked", oldIdentity.phase)
				assert.Equal("no_live_occurrence", oldIdentity.errorCode)
				var current *occurrenceRow
				for i := range rows {
					if rows[i].Revision != beforeMapping.Revision {
						current = &rows[i]
					}
				}
				require.NotNil(current)
				assert.Equal("pending", current.State)
				assert.NotEqual(beforeMapping.ProcessingKey, current.ProcessingKey)
				for _, identity := range identities {
					if identity.key != beforeMapping.ProcessingKey {
						assert.NotEqual(beforeOperationID, identity.operationID)
					}
				}
				return
			}

			assert.Equal("pending-process", oldIdentity.phase)
			assert.Empty(oldIdentity.errorCode)
			var current *occurrenceRow
			for i := range rows {
				if rows[i].Revision != beforeMapping.Revision {
					current = &rows[i]
				}
			}
			require.NotNil(current)
			assert.Equal("pending", current.State)
			assert.Equal(beforeMapping.ProcessingKey, current.ProcessingKey)

			_, err = worker.RunBatch(t.Context())
			require.NoError(err)
			rows = occurrenceRows(t, world.st, destination)
			for _, row := range rows {
				if row.Revision == current.Revision {
					assert.Equal("retained", row.State)
				}
			}
			identities = processDeliveryIdentities(t, world.st, destination)
			oldIdentity = findProcessDelivery(t, identities, beforeMapping.ProcessingKey)
			assert.Equal(beforeOperationID, oldIdentity.operationID)
			assert.Equal(beforeInput, oldIdentity.suppliedInputID)
			assert.Equal(beforeFrozen, oldIdentity.frozenRequest)
			operation, ok, err := world.st.NextBeeperMediaOperation(t.Context(), destination, time.Now().UTC())
			require.NoError(err)
			require.True(ok)
			require.Equal(store.BeeperMediaOperationProcess, operation.Kind)
			assert.Equal(beforeOperationID, operation.OperationID)
			require.NoError(worker.process(t.Context(), t.Context(), archiveUID, operation))
			docbank.mu.Lock()
			assert.Len(docbank.processOps, 1)
			assert.Equal(beforeOperationID, docbank.processOps[0])
			docbank.mu.Unlock()
		})
	}
}

func findProcessDelivery(t *testing.T, identities []processDeliveryIdentity, key string) processDeliveryIdentity {
	t.Helper()
	for _, identity := range identities {
		if identity.key == key {
			return identity
		}
	}
	require.FailNow(t, "processing delivery not found", key)
	return processDeliveryIdentity{}
}

func TestBeeperMediaProcessActionFence(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	world := importVoiceChat(t, voiceSpec{id: "voice1", asset: "mxc://beeper.example/process-action-fence",
		mime: "audio/wav", fileName: "voice.wav", transcript: "fence transcript", data: syntheticWAV(800, 45)})
	docbank := newFakeDocbank(t)
	server := httptest.NewServer(docbank)
	defer server.Close()
	destination := "process-action-fence"
	worker := world.submitter(t, server, destination)
	operation := preparePendingProcess(t, worker, destination)
	beforeOperationID, beforeInput, beforeFrozen := operation.OperationID, operation.SuppliedInputID, operation.FrozenRequestJSON
	mappings, err := world.st.ListLiveBeeperMediaMappings(t.Context(), destination, operation.ProcessingKey, 1)
	require.NoError(err)
	require.Len(mappings, 1)
	raw, err := world.st.GetMessageRawContext(t.Context(), mappings[0].MessageID)
	require.NoError(err)
	updated := strings.Replace(string(raw), "fence transcript", "changed fence transcript", 1)
	require.NotEqual(string(raw), updated)
	calls := 0
	worker.WithOperationGate(func(context.Context) (func(), bool) {
		calls++
		if calls == 2 {
			require.NoError(world.st.UpsertMessageRawWithFormat(mappings[0].MessageID, []byte(updated), "beeper_json"))
		}
		return func() {}, true
	})
	archiveUID, err := world.st.ArchiveUIDContext(t.Context())
	require.NoError(err)
	require.NoError(worker.process(t.Context(), t.Context(), archiveUID, operation))
	assert.Equal(2, calls)
	docbank.mu.Lock()
	assert.Empty(docbank.processOps)
	docbank.mu.Unlock()
	identity := findProcessDelivery(t, processDeliveryIdentities(t, world.st, destination), operation.ProcessingKey)
	assert.Equal("pending-process", identity.phase)
	assert.Equal("source_changed", identity.errorCode)
	assert.Equal(beforeOperationID, identity.operationID)
	assert.Equal(beforeInput, identity.suppliedInputID)
	assert.Equal(beforeFrozen, identity.frozenRequest)
	var nextActionAt time.Time
	require.NoError(world.st.DB().QueryRow(world.st.Rebind(
		`SELECT next_action_at FROM beeper_media_deliveries WHERE destination_key = ? AND processing_key = ?`),
		destination, operation.ProcessingKey).Scan(&nextActionAt))
	assert.True(nextActionAt.After(time.Now().UTC()))
	_, err = world.st.DB().Exec(world.st.Rebind(`UPDATE beeper_media_deliveries SET next_action_at = ? WHERE destination_key = ?`),
		time.Now().UTC().Add(-time.Minute), destination)
	require.NoError(err)
	worker.WithOperationGate(nil)
	require.NoError(worker.process(t.Context(), t.Context(), archiveUID, operation))
	docbank.mu.Lock()
	assert.Empty(docbank.processOps)
	docbank.mu.Unlock()
	rows := occurrenceRows(t, world.st, destination)
	var replacement *occurrenceRow
	for i := range rows {
		if rows[i].Revision != mappings[0].Revision {
			replacement = &rows[i]
		}
	}
	require.NotNil(replacement)
	assert.NotEqual(operation.ProcessingKey, replacement.ProcessingKey)
	assert.Equal("pending", replacement.State)
}

func TestBeeperMediaFullRescanRevokesAfterReregistration(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	world := importVoiceChat(t,
		voiceSpec{id: "voice1", asset: "mxc://beeper.example/reregister-one", mime: "audio/wav",
			fileName: "voice.wav", transcript: "shared transcript", data: syntheticWAV(800, 42)},
		voiceSpec{id: "voice2", asset: "mxc://beeper.example/reregister-two", mime: "audio/wav",
			fileName: "voice.wav", transcript: "shared transcript", data: syntheticWAV(800, 42)})
	docbank := newFakeDocbank(t)
	server := httptest.NewServer(docbank)
	defer server.Close()
	destination := "reregister"
	runPasses(t, world.submitter(t, server, destination), 3)
	rows := occurrenceRows(t, world.st, destination)
	require.Len(rows, 2)
	assert.Equal(rows[0].ProcessingKey, rows[1].ProcessingKey)
	for _, row := range rows {
		assert.Equal("retained", row.State)
	}
	deliveries := deliveryRows(t, world.st, destination)
	require.Len(deliveries, 1)
	_, err := world.st.DB().Exec(world.st.Rebind(`
		UPDATE beeper_media_deliveries
		SET phase = 'pending-artifact', pending_operation_id = NULL,
		    frozen_request_json = NULL, supplied_input_id = '', next_action_at = ?, error_code = ''
		WHERE destination_key = ?`), time.Now().UTC().Add(-time.Minute), destination)
	require.NoError(err)
	deliveries = deliveryRows(t, world.st, destination)
	require.Len(deliveries, 1)
	assert.Equal("pending-artifact", deliveries[0].Phase)

	source, err := world.st.GetOrCreateSource("beeper", "signal")
	require.NoError(err)
	require.NoError(world.st.UnregisterAttachmentChangeConsumer(t.Context(), store.BeeperMediaAttachmentConsumerKey))
	require.NoError(world.st.MarkMessageDeleted(source.ID, "voice1"))
	worker := NewMediaSubmitter(world.st, world.blobs, nil, destination, world.dir)
	result, err := worker.RunBatch(t.Context())
	require.NoError(err)
	assert.Equal(1, result.Examined)
	rows = occurrenceRows(t, world.st, destination)
	require.Len(rows, 2)
	for _, row := range rows {
		if row.MessageID == "voice1" {
			assert.Equal("revoked", row.State)
		} else {
			assert.Equal("voice2", row.MessageID)
			assert.Equal("retained", row.State)
		}
	}
	deliveries = deliveryRows(t, world.st, destination)
	require.Len(deliveries, 1)
	assert.Equal("pending-artifact", deliveries[0].Phase)
	changes, err := world.st.ListAttachmentChanges(t.Context(), store.BeeperMediaAttachmentConsumerKey, 100)
	require.NoError(err)
	assert.Empty(changes)

	require.NoError(world.st.UnregisterAttachmentChangeConsumer(t.Context(), store.BeeperMediaAttachmentConsumerKey))
	require.NoError(world.st.MarkMessageDeleted(source.ID, "voice2"))
	worker = NewMediaSubmitter(world.st, world.blobs, nil, destination, world.dir)
	result, err = worker.RunBatch(t.Context())
	require.NoError(err)
	assert.Zero(result.Examined)
	rows = occurrenceRows(t, world.st, destination)
	require.Len(rows, 2)
	for _, row := range rows {
		assert.Equal("revoked", row.State)
	}
	deliveries = deliveryRows(t, world.st, destination)
	require.Len(deliveries, 1)
	assert.Equal("blocked", deliveries[0].Phase)
	assert.Equal("no_live_occurrence", deliveries[0].ErrorCode)
	changes, err = world.st.ListAttachmentChanges(t.Context(), store.BeeperMediaAttachmentConsumerKey, 100)
	require.NoError(err)
	assert.Empty(changes)
}

func TestBeeperMediaFullRescanRevokesAllNonRevokedStates(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	world := importVoiceChat(t,
		voiceSpec{id: "pending", asset: "mxc://beeper.example/full-scan-pending", mime: "audio/wav",
			fileName: "voice.wav", transcript: "shared transcript", data: syntheticWAV(800, 43)},
		voiceSpec{id: "source-gap", asset: "mxc://beeper.example/full-scan-source-gap", mime: "audio/wav",
			fileName: "voice.wav", transcript: "shared transcript", data: syntheticWAV(800, 43)},
		voiceSpec{id: "blocked", asset: "mxc://beeper.example/full-scan-blocked", mime: "audio/wav",
			fileName: "voice.wav", transcript: "shared transcript", data: syntheticWAV(800, 43)},
		voiceSpec{id: "live", asset: "mxc://beeper.example/full-scan-live", mime: "audio/wav",
			fileName: "voice.wav", transcript: "shared transcript", data: syntheticWAV(800, 43)})
	destination := "full-scan-states"
	worker := NewMediaSubmitter(world.st, world.blobs, nil, destination, world.dir)
	result, err := worker.RunBatch(t.Context())
	require.NoError(err)
	assert.Equal(4, result.Examined)
	for _, row := range occurrenceRows(t, world.st, destination) {
		assert.Equal("pending", row.State)
	}

	_, err = world.st.DB().Exec(world.st.Rebind(`
		UPDATE beeper_media_occurrences
		SET retention_state = ?, error_code = ?, next_action_at = ?
		WHERE destination_key = ? AND source_message_id = ?`),
		store.BeeperMediaRetentionSourceUnavailable, "source_unavailable", time.Now().UTC().Add(-time.Minute),
		destination, "source-gap")
	require.NoError(err)
	_, err = world.st.DB().Exec(world.st.Rebind(`
		UPDATE beeper_media_occurrences
		SET retention_state = ?, error_code = ?, next_action_at = NULL
		WHERE destination_key = ? AND source_message_id = ?`),
		store.BeeperMediaRetentionBlocked, "credential_unavailable", destination, "blocked")
	require.NoError(err)

	source, err := world.st.GetOrCreateSource("beeper", "signal")
	require.NoError(err)
	require.NoError(world.st.UnregisterAttachmentChangeConsumer(t.Context(), store.BeeperMediaAttachmentConsumerKey))
	for _, messageID := range []string{"pending", "source-gap", "blocked"} {
		require.NoError(world.st.MarkMessageDeleted(source.ID, messageID))
	}
	result, err = NewMediaSubmitter(world.st, world.blobs, nil, destination, world.dir).RunBatch(t.Context())
	require.NoError(err)
	assert.Equal(1, result.Examined)
	rows := occurrenceRows(t, world.st, destination)
	for _, row := range rows {
		if row.MessageID == "live" {
			assert.Equal("pending", row.State)
		} else {
			assert.Equal("revoked", row.State)
		}
	}
	deliveries := deliveryRows(t, world.st, destination)
	require.Len(deliveries, 1)
	assert.Equal("pending-artifact", deliveries[0].Phase)

	require.NoError(world.st.UnregisterAttachmentChangeConsumer(t.Context(), store.BeeperMediaAttachmentConsumerKey))
	require.NoError(world.st.MarkMessageDeleted(source.ID, "live"))
	result, err = NewMediaSubmitter(world.st, world.blobs, nil, destination, world.dir).RunBatch(t.Context())
	require.NoError(err)
	assert.Zero(result.Examined)
	for _, row := range occurrenceRows(t, world.st, destination) {
		assert.Equal("revoked", row.State)
	}
	deliveries = deliveryRows(t, world.st, destination)
	require.Len(deliveries, 1)
	assert.Equal("blocked", deliveries[0].Phase)
	assert.Equal("no_live_occurrence", deliveries[0].ErrorCode)
}

func installMessageRawReadAuthorizer(t *testing.T, st *store.Store, denied *bool) {
	t.Helper()
	testutil.SkipIfPostgres(t, "SQLite authorizer injects a message_raw query failure")
	st.DB().SetMaxOpenConns(1)
	conn, err := st.DB().Conn(t.Context())
	require.NoError(t, err)
	require.NoError(t, conn.Raw(func(driverConn any) error {
		sqliteConn, ok := driverConn.(*sqlite3.SQLiteConn)
		if !ok {
			return fmt.Errorf("expected SQLite connection, got %T", driverConn)
		}
		sqliteConn.RegisterAuthorizer(func(action int, table, _, _ string) int {
			if *denied && action == sqlite3.SQLITE_READ && table == "message_raw" {
				return sqlite3.SQLITE_DENY
			}
			return sqlite3.SQLITE_OK
		})
		return nil
	}))
	require.NoError(t, conn.Close())
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
