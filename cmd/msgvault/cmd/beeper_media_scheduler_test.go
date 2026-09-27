package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json/v2"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/attachmentstore"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

const beeperMediaTestKeyEnv = "MSGVAULT_TEST_BEEPER_MEDIA_KEY"

// storedBeeperVoiceNote writes the message, raw JSON, attachment row and
// loose CAS blob for one captured WAV voice note.
func storedBeeperVoiceNote(t *testing.T) (*store.Store, *attachmentstore.Store) {
	t.Helper()
	f := storetest.New(t)
	_, err := f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE sources SET source_type = 'beeper', identifier = ? WHERE id = ?`), "signal", f.Source.ID)
	require.NoError(t, err)
	messageID := f.CreateMessage("voice1")
	raw, err := json.Marshal(map[string]any{
		"id": "voice1", "timestamp": "2026-09-16T10:11:12.123Z",
		"attachments": []map[string]any{{"id": "mxc://beeper.local/voice1", "type": "audio",
			"isVoiceNote": true, "mimeType": "audio/wav", "fileName": "voice.wav"}},
	})
	require.NoError(t, err)
	require.NoError(t, f.Store.UpsertMessageRawWithFormat(messageID, raw, "beeper_json"))
	wav := testWAV()
	digest := sha256.Sum256(wav)
	hash := hex.EncodeToString(digest[:])
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, hash[:2]), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, hash[:2], hash), wav, 0o600))
	require.NoError(t, f.Store.UpsertAttachmentRecord(t.Context(), messageID, store.AttachmentWrite{
		Filename: "voice.wav", MIMEType: "audio/wav", StoragePath: hash[:2] + "/" + hash, ContentHash: hash,
		Size: int64(len(wav)), SourceAttachmentID: "beeper:mxc://beeper.local/voice1",
		SourcePartKey: "beeper:mxc://beeper.local/voice1", MediaType: "voice_note",
		State: attachmentpolicy.StateStored, Role: store.AttachmentRoleStandalone,
		RoleSource: store.AttachmentRoleSourceImporterSemantics,
	}))
	blobs, err := attachmentstore.New(store.NewPackCatalog(f.Store), dir)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, blobs.Close()) })
	return f.Store, blobs
}

// retentionServer accepts Docbank supplied-media retention requests.
type retentionServer struct {
	mu         sync.Mutex
	requests   int
	operations []string
	hang       atomic.Bool
	failStatus atomic.Int32
	arrived    chan struct{}
	release    chan struct{}
}

func newRetentionServer(t *testing.T) (*retentionServer, *httptest.Server) {
	t.Helper()
	server := &retentionServer{arrived: make(chan struct{}, 10), release: make(chan struct{})}
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		server.mu.Lock()
		server.requests++
		server.mu.Unlock()
		if status := server.failStatus.Load(); status != 0 {
			http.Error(w, "secret response body", int(status))
			server.arrived <- struct{}{}
			return
		}
		if !assert.Equal(t, "synthetic-key", r.Header.Get("X-Api-Key")) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		reader, err := r.MultipartReader()
		if !assert.NoError(t, err) {
			return
		}
		part, err := reader.NextPart()
		if !assert.NoError(t, err) {
			return
		}
		var metadata struct {
			OperationID string `json:"operation_id"`
		}
		assert.NoError(t, json.UnmarshalRead(part, &metadata))
		server.arrived <- struct{}{}
		_, _ = io.Copy(io.Discard, r.Body)
		if server.hang.Load() {
			select {
			case <-r.Context().Done():
				server.mu.Lock()
				server.operations = append(server.operations, metadata.OperationID)
				server.mu.Unlock()
				return
			case <-server.release:
			}
		}
		server.mu.Lock()
		server.operations = append(server.operations, metadata.OperationID)
		server.mu.Unlock()
		data, _ := json.Marshal(map[string]string{"vault_uid": "vault", "source_id": "source",
			"source_version_id": "version", "content_version_id": "content", "occurrence_id": "occurrence",
			"operation_id": metadata.OperationID, "operation_state": "succeeded", "coverage_state": "unprocessed"})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	}))
	t.Cleanup(httpServer.Close)
	return server, httpServer
}

func (s *retentionServer) requestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

func retentionRows(t *testing.T, st *store.Store) map[string]string {
	t.Helper()
	rows, err := st.DB().Query(`SELECT destination_key, retention_state || ':' || error_code || ':' || source_id
		FROM beeper_media_occurrences`)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	result := map[string]string{}
	for rows.Next() {
		var destination, value string
		require.NoError(t, rows.Scan(&destination, &value))
		result[destination] = value
	}
	require.NoError(t, rows.Err())
	return result
}

func consumerRegistered(t *testing.T, st *store.Store) bool {
	t.Helper()
	_, err := st.GetAttachmentChangeConsumer(t.Context(), store.BeeperMediaAttachmentConsumerKey)
	return err == nil
}

func TestBeeperMediaConfig(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, blobs := storedBeeperVoiceNote(t)
	server, httpServer := newRetentionServer(t)
	archiveUID, err := st.ArchiveUIDContext(t.Context())
	require.NoError(err)
	destination := beeperMediaDestinationKey(httpServer.URL, archiveUID)
	sched := scheduler.New(nil)
	defer func() { <-sched.Stop().Done() }()

	// Absent configuration registers nothing.
	require.NoError(configureBeeperMediaJob(t.Context(), sched, nil, st, blobs, t.TempDir(), config.DocbankIntegrationConfig{}, nil))
	assert.False(sched.IsJobScheduled(beeperMediaSubmitJob))
	assert.False(consumerRegistered(t, st))

	// Remote plaintext is refused before any job exists.
	require.Error(configureBeeperMediaJob(t.Context(), sched, nil, st, blobs, t.TempDir(), config.DocbankIntegrationConfig{
		Enabled: true, URL: "http://docbank.example.com", APIKeyEnv: beeperMediaTestKeyEnv, UploadConsent: true}, nil))
	assert.False(sched.IsJobScheduled(beeperMediaSubmitJob))

	// Without upload consent the job records local discovery only.
	t.Setenv(beeperMediaTestKeyEnv, "synthetic-key")
	require.NoError(configureBeeperMediaJob(t.Context(), sched, nil, st, blobs, t.TempDir(), config.DocbankIntegrationConfig{
		Enabled: true, URL: httpServer.URL, APIKeyEnv: beeperMediaTestKeyEnv}, nil))
	require.NoError(sched.TriggerJob(beeperMediaSubmitJob))
	assert.Equal(map[string]string{destination: "pending::"}, retentionRows(t, st))
	assert.Zero(server.requestCount())
	assert.True(consumerRegistered(t, st))

	// A missing credential blocks the operation without scheduling a retry.
	t.Setenv(beeperMediaTestKeyEnv, "")
	require.NoError(configureBeeperMediaJob(t.Context(), sched, nil, st, blobs, t.TempDir(), config.DocbankIntegrationConfig{
		Enabled: true, URL: httpServer.URL, APIKeyEnv: beeperMediaTestKeyEnv, UploadConsent: true}, nil))
	require.NoError(sched.TriggerJob(beeperMediaSubmitJob))
	var state, code, operationID string
	var scheduled bool
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT retention_state, error_code, retention_operation_id, next_action_at IS NOT NULL
		FROM beeper_media_occurrences WHERE destination_key = ?`), destination).
		Scan(&state, &code, &operationID, &scheduled))
	assert.Equal("blocked", state)
	assert.Equal("credential_unavailable", code)
	assert.False(scheduled)
	assert.Zero(server.requestCount())
	blockedOperationID := operationID

	// Startup reconsideration reopens the same operation. A failing peer still retries.
	t.Setenv(beeperMediaTestKeyEnv, "synthetic-key")
	require.NoError(configureBeeperMediaJob(t.Context(), sched, nil, st, blobs, t.TempDir(), config.DocbankIntegrationConfig{
		Enabled: true, URL: httpServer.URL, APIKeyEnv: beeperMediaTestKeyEnv, UploadConsent: true}, nil))
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT retention_state, error_code, retention_operation_id, next_action_at IS NOT NULL
		FROM beeper_media_occurrences WHERE destination_key = ?`), destination).
		Scan(&state, &code, &operationID, &scheduled))
	assert.Equal("pending", state)
	assert.Equal("credential_unavailable", code)
	assert.Equal(blockedOperationID, operationID)
	assert.True(scheduled)

	server.failStatus.Store(http.StatusServiceUnavailable)
	_, err = st.DB().Exec(`UPDATE beeper_media_occurrences SET next_action_at = '2000-01-01 00:00:00.000'`)
	require.NoError(err)
	require.NoError(sched.TriggerJob(beeperMediaSubmitJob))
	assert.Equal(map[string]string{destination: "pending:server_error:"}, retentionRows(t, st))
	var retryScheduled bool
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT next_action_at IS NOT NULL FROM beeper_media_occurrences WHERE destination_key = ?`), destination).
		Scan(&retryScheduled))
	assert.True(retryScheduled)
	assert.Equal(1, server.requestCount())

	// Disabling removes the job and its journal consumer but keeps receipts.
	require.NoError(configureBeeperMediaJob(t.Context(), sched, nil, st, blobs, t.TempDir(), config.DocbankIntegrationConfig{}, nil))
	assert.False(sched.IsJobScheduled(beeperMediaSubmitJob))
	assert.False(consumerRegistered(t, st))
	assert.Len(retentionRows(t, st), 1)
}

// TestBeeperMediaInvalidConfigUnregisters keeps a failed reconfiguration from
// leaving an idle journal consumer that holds change log cleanup.
func TestBeeperMediaInvalidConfigUnregisters(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, blobs := storedBeeperVoiceNote(t)
	t.Setenv(beeperMediaTestKeyEnv, "synthetic-key")
	_, httpServer := newRetentionServer(t)
	sched := scheduler.New(nil)
	defer func() { <-sched.Stop().Done() }()
	cfg := config.DocbankIntegrationConfig{Enabled: true, URL: httpServer.URL,
		APIKeyEnv: beeperMediaTestKeyEnv, UploadConsent: true}
	require.NoError(configureBeeperMediaJob(t.Context(), sched, nil, st, blobs, t.TempDir(), cfg, nil))
	require.NoError(sched.TriggerJob(beeperMediaSubmitJob))
	require.True(consumerRegistered(t, st))
	require.Len(retentionRows(t, st), 1)

	// An invalid endpoint removes the job and consumer but keeps receipts.
	invalid := cfg
	invalid.URL = "http://docbank.example.com"
	require.Error(configureBeeperMediaJob(t.Context(), sched, nil, st, blobs, t.TempDir(), invalid, nil))
	assert.False(sched.IsJobScheduled(beeperMediaSubmitJob))
	assert.False(consumerRegistered(t, st))
	assert.Len(retentionRows(t, st), 1)

	// A valid restart registers the consumer again.
	require.NoError(configureBeeperMediaJob(t.Context(), sched, nil, st, blobs, t.TempDir(), cfg, nil))
	require.NoError(sched.TriggerJob(beeperMediaSubmitJob))
	assert.True(consumerRegistered(t, st))
}

func TestBeeperMediaScheduledRoute(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, blobs := storedBeeperVoiceNote(t)
	t.Setenv(beeperMediaTestKeyEnv, "synthetic-key")
	server, httpServer := newRetentionServer(t)
	gate := api.NewSerialOperationGate()
	sched := scheduler.New(nil).WithWorkTracker(labelWorkTracker(gate, "media"))
	defer func() { <-sched.Stop().Done() }()
	cfg := config.DocbankIntegrationConfig{Enabled: true, URL: httpServer.URL,
		APIKeyEnv: beeperMediaTestKeyEnv, UploadConsent: true}
	require.NoError(configureBeeperMediaJob(t.Context(), sched, nil, st, blobs, t.TempDir(), cfg, nil))
	archiveUID, err := st.ArchiveUIDContext(t.Context())
	require.NoError(err)
	destination := beeperMediaDestinationKey(httpServer.URL, archiveUID)

	// A waiting operation interrupts the upload; the saved request survives.
	server.hang.Store(true)
	done := make(chan error, 1)
	go func() { done <- sched.TriggerJob(beeperMediaSubmitJob) }()
	<-server.arrived
	requestAcquired := make(chan func(), 1)
	go func() {
		release, ok := gate.BeginRequestWorkContext(t.Context(), "request")
		if !ok {
			release = nil
		}
		requestAcquired <- release
	}()
	select {
	case err := <-done:
		require.NoError(err)
	case <-time.After(time.Minute):
		require.FailNow("scheduled job did not yield")
	}
	releaseRequest := <-requestAcquired
	require.NotNil(releaseRequest)
	defer releaseRequest()
	assert.Equal(map[string]string{destination: "pending::"}, retentionRows(t, st))

	// The queued follow-up resumes with the same operation ID after the request.
	server.hang.Store(false)
	releaseRequest()
	require.Eventually(func() bool { return !sched.JobStatus()[0].Running }, time.Minute, 10*time.Millisecond)
	assert.Equal(map[string]string{destination: "retained::source"}, retentionRows(t, st))
	server.mu.Lock()
	require.Len(server.operations, 2)
	assert.Equal(server.operations[0], server.operations[1])
	server.mu.Unlock()

	// A new destination starts its own delivery scope.
	otherServer, otherHTTP := newRetentionServer(t)
	cfg.URL = otherHTTP.URL
	require.NoError(configureBeeperMediaJob(t.Context(), sched, nil, st, blobs, t.TempDir(), cfg, nil))
	otherDestination := beeperMediaDestinationKey(otherHTTP.URL, archiveUID)
	otherServer.hang.Store(true)
	stopped := make(chan error, 1)
	go func() { stopped <- sched.TriggerJob(beeperMediaSubmitJob) }()
	<-otherServer.arrived
	select {
	case <-sched.Stop().Done():
	case <-time.After(time.Minute):
		require.FailNow("scheduler did not drain the running job")
	}
	<-stopped
	assert.Equal(map[string]string{
		destination: "retained::source", otherDestination: "pending::",
	}, retentionRows(t, st))
	require.Error(sched.TriggerJob(beeperMediaSubmitJob))
}

// TestBeeperMediaGatedStoreWrites composes the daemon schedulers. A long
// upload holds no operation gate, while every media Store write waits for it,
// so a backup freeze sees no writer.
func TestBeeperMediaGatedStoreWrites(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, blobs := storedBeeperVoiceNote(t)
	t.Setenv(beeperMediaTestKeyEnv, "synthetic-key")
	wait := beeperMediaGateWait
	beeperMediaGateWait = 100 * time.Millisecond
	t.Cleanup(func() { beeperMediaGateWait = wait })
	server, httpServer := newRetentionServer(t)
	gate := api.NewSerialOperationGate()
	logger := slog.New(slog.DiscardHandler)
	sched, media := newServeSchedulers(nil, logger, nil, gate)
	require.NoError(sched.AddJob(scheduler.Job{Name: "test-gated-job", Schedule: "0 0 1 1 *",
		Run: func(context.Context) error { return nil }}))
	cfg := config.DocbankIntegrationConfig{Enabled: true, URL: httpServer.URL,
		APIKeyEnv: beeperMediaTestKeyEnv, UploadConsent: true}
	require.NoError(configureBeeperMediaJob(t.Context(), media, gate, st, blobs, t.TempDir(), cfg, logger))
	archiveUID, err := st.ArchiveUIDContext(t.Context())
	require.NoError(err)
	destination := beeperMediaDestinationKey(httpServer.URL, archiveUID)
	waitCtx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	// While a backup freeze holds the gate, the pass ends before any write.
	freeze, ok := gate.BeginLabeledWorkContext(waitCtx, "backup freeze")
	require.True(ok)
	require.NoError(media.TriggerJob(beeperMediaSubmitJob))
	assert.Empty(retentionRows(t, st))
	assert.False(consumerRegistered(t, st))
	assert.Zero(server.requestCount())
	freeze()

	// An upload in flight holds no gate, so the freeze starts at once.
	server.hang.Store(true)
	done := make(chan error, 1)
	go func() { done <- media.TriggerJob(beeperMediaSubmitJob) }()
	<-server.arrived
	_, _, held := gate.Holder()
	assert.False(held, "an upload in flight holds no operation gate")
	assert.Equal(map[string]string{destination: "pending::"}, retentionRows(t, st))
	freeze, ok = gate.BeginLabeledWorkContext(waitCtx, "backup freeze")
	require.True(ok)

	// The finished upload can't record its receipt under the freeze.
	close(server.release)
	select {
	case err := <-done:
		require.NoError(err)
	case <-waitCtx.Done():
		require.FailNow("upload pass did not end while the gate was held")
	}
	assert.Equal(map[string]string{destination: "pending::"}, retentionRows(t, st))
	freeze()

	// The next pass replays the saved operation ID and records the receipt.
	require.NoError(media.TriggerJob(beeperMediaSubmitJob))
	assert.Equal(map[string]string{destination: "retained::source"}, retentionRows(t, st))
	server.mu.Lock()
	require.Len(server.operations, 2)
	assert.Equal(server.operations[0], server.operations[1])
	server.mu.Unlock()

	// Daemon shutdown cancels an upload in flight and drains both schedulers.
	otherServer, otherHTTP := newRetentionServer(t)
	cfg.URL = otherHTTP.URL
	require.NoError(configureBeeperMediaJob(t.Context(), media, gate, st, blobs, t.TempDir(), cfg, logger))
	otherServer.hang.Store(true)
	stopped := make(chan error, 1)
	go func() { stopped <- media.TriggerJob(beeperMediaSubmitJob) }()
	<-otherServer.arrived
	require.NoError(shutdownServeRuntime(waitCtx, io.Discard, nil, serveSchedulers{sched, media}, gate))
	shutdownWait, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	select {
	case err := <-stopped:
		require.ErrorIs(err, context.Canceled)
	case <-shutdownWait.Done():
		require.FailNow("shutdown returned before the upload stopped")
	}
	assert.Equal(map[string]string{
		destination: "retained::source", beeperMediaDestinationKey(otherHTTP.URL, archiveUID): "pending::",
	}, retentionRows(t, st))
	assert.True(gate.Draining())
	require.Error(media.TriggerJob(beeperMediaSubmitJob))
	require.Error(sched.TriggerJob("test-gated-job"))
}

// TestBeeperMediaJobStatus shows the API's scheduler adapter lists and runs
// the media job alongside the gated daemon jobs.
func TestBeeperMediaJobStatus(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, blobs := storedBeeperVoiceNote(t)
	t.Setenv(beeperMediaTestKeyEnv, "synthetic-key")
	_, httpServer := newRetentionServer(t)
	gate := api.NewSerialOperationGate()
	logger := slog.New(slog.DiscardHandler)
	sched, media := newServeSchedulers(nil, logger, nil, gate)
	defer func() { <-serveSchedulers{sched, media}.Stop().Done() }()
	require.NoError(sched.AddJob(scheduler.Job{Name: "test-gated-job", Schedule: "0 0 1 1 *",
		Run: func(context.Context) error { return nil }}))
	require.NoError(configureBeeperMediaJob(t.Context(), media, gate, st, blobs, t.TempDir(), config.DocbankIntegrationConfig{
		Enabled: true, URL: httpServer.URL, APIKeyEnv: beeperMediaTestKeyEnv}, logger))
	var adapter api.SyncScheduler = &schedulerAdapter{scheduler: sched, media: media}

	assert.True(adapter.IsJobScheduled(beeperMediaSubmitJob))
	assert.True(adapter.IsJobScheduled("test-gated-job"))
	require.NoError(adapter.TriggerJob(beeperMediaSubmitJob))
	jobs := map[string]api.JobStatus{}
	for _, job := range adapter.JobStatus() {
		jobs[job.Name] = job
	}
	require.Contains(jobs, beeperMediaSubmitJob)
	require.Contains(jobs, "test-gated-job")
	assert.Equal(beeperMediaSubmitCron, jobs[beeperMediaSubmitJob].Schedule)
	assert.False(jobs[beeperMediaSubmitJob].LastRun.IsZero())
	assert.Empty(jobs[beeperMediaSubmitJob].LastError)
	archiveUID, err := st.ArchiveUIDContext(t.Context())
	require.NoError(err)
	assert.Equal(map[string]string{beeperMediaDestinationKey(httpServer.URL, archiveUID): "pending::"},
		retentionRows(t, st))
}

func TestBeeperMediaDoesNotPreventIdleShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st, blobs := storedBeeperVoiceNote(t)
		idleShutdown := make(chan struct{})
		idle := api.NewIdleTracker(3*time.Minute, func() { close(idleShutdown) })
		logger := slog.New(slog.DiscardHandler)
		gate := api.NewSerialOperationGate()
		sched, media := newServeSchedulers(nil, logger, idle, gate)
		defer func() { <-serveSchedulers{sched, media}.Stop().Done() }()
		require.NoError(t, configureBeeperMediaJob(t.Context(), media, gate, st, blobs, t.TempDir(),
			config.DocbankIntegrationConfig{Enabled: true, URL: "http://127.0.0.1"}, logger))
		go idle.Run(t.Context())
		media.Start()

		// Repeated discovery passes must leave an unused daemon free to stop.
		synctest.Sleep(3*time.Minute + time.Second)
		require.Len(t, retentionRows(t, st), 1, "scheduled discovery must have run")
		select {
		case <-idleShutdown:
		default:
			assert.Fail(t, "background media passes prevented idle shutdown")
		}
	})
}

func testWAV() []byte {
	audio := make([]byte, 1600)
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
	return data
}
