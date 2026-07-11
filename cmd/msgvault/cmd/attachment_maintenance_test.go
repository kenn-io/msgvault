package cmd

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/msgvault/internal/attachmentstore"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

type attachmentMaintenanceFixture struct {
	t           *testing.T
	store       *store.Store
	dir         string
	blob        *attachmentstore.Store
	maintenance *attachmentMaintenance
	logs        *bytes.Buffer
	messageID   int64
	sequence    int
}

func newAttachmentMaintenanceFixture(t *testing.T) *attachmentMaintenanceFixture {
	t.Helper()
	storeFixture := storetest.New(t)
	dir := t.TempDir()
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	maintenance, err := newAttachmentMaintenance(storeFixture.Store, dir, logger)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, maintenance.close(), "close attachment maintenance") })
	return &attachmentMaintenanceFixture{
		t:           t,
		store:       storeFixture.Store,
		dir:         dir,
		blob:        maintenance.blob,
		logs:        logs,
		messageID:   storeFixture.CreateMessage("attachment-maintenance"),
		maintenance: maintenance,
	}
}

func newFailingAttachmentMaintenance(t *testing.T) (*attachmentMaintenance, *bytes.Buffer) {
	t.Helper()
	storeFixture := storetest.New(t)
	attachmentsPath := filepath.Join(t.TempDir(), "attachments")
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	maintenance, err := newAttachmentMaintenance(storeFixture.Store, attachmentsPath, logger)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, maintenance.close(), "close attachment maintenance") })
	require.NoError(t, storeFixture.Store.Close(), "close catalog to inject maintenance failure")
	return maintenance, logs
}

func (f *attachmentMaintenanceFixture) addLoose(content []byte) string {
	f.t.Helper()
	f.sequence++
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])
	rel := hash[:2] + "/" + hash
	full := filepath.Join(f.dir, filepath.FromSlash(rel))
	require.NoError(f.t, os.MkdirAll(filepath.Dir(full), 0o700), "create loose blob directory")
	require.NoError(f.t, os.WriteFile(full, content, 0o600), "write loose blob")
	require.NoError(f.t, f.store.UpsertAttachment(
		f.messageID,
		fmt.Sprintf("attachment-%d.bin", f.sequence),
		"application/octet-stream",
		rel,
		hash,
		len(content),
	), "record loose attachment")
	return hash
}

func (f *attachmentMaintenanceFixture) packedEntry(hash string) *store.PackIndexEntry {
	f.t.Helper()
	entry, err := f.store.GetAttachmentPackEntry(hash)
	require.NoError(f.t, err, "GetAttachmentPackEntry(%s)", hash)
	return entry
}

func (f *attachmentMaintenanceFixture) readBlob(hash string) []byte {
	f.t.Helper()
	r, _, err := f.blob.Open(hash)
	require.NoError(f.t, err, "open blob %s", hash)
	defer func() { require.NoError(f.t, r.Close(), "close blob reader") }()
	data, err := io.ReadAll(r)
	require.NoError(f.t, err, "read blob %s", hash)
	return data
}

func (f *attachmentMaintenanceFixture) makeZeroLivePack(content []byte) string {
	f.t.Helper()
	hash := f.addLoose(content)
	_, err := f.maintenance.pack(context.Background(), 0)
	require.NoError(f.t, err, "pack zero-live fixture")
	entry := f.packedEntry(hash)
	require.NotNil(f.t, entry)
	_, err = f.store.DB().Exec(f.store.Rebind(
		`DELETE FROM attachments WHERE content_hash = ? OR thumbnail_hash = ?`), hash, hash)
	require.NoError(f.t, err, "logically delete packed fixture")
	return entry.PackID
}

func (f *attachmentMaintenanceFixture) makeSparsePack(live []byte, createdAt time.Time) (string, string) {
	f.t.Helper()
	liveHash := f.addLoose(live)
	dead := make([]byte, (8<<20)+(256<<10))
	_, err := crand.Read(dead)
	require.NoError(f.t, err, "fill incompressible dead attachment")
	deadHash := f.addLoose(dead)
	deadSmallHash := f.addLoose(fmt.Appendf(nil,
		"second dead entry %d makes the source pack sparse", f.sequence))
	_, err = f.maintenance.pack(context.Background(), 0)
	require.NoError(f.t, err, "pack sparse fixture")
	entry := f.packedEntry(liveHash)
	require.NotNil(f.t, entry)
	_, err = f.store.DB().Exec(f.store.Rebind(
		`DELETE FROM attachments WHERE content_hash IN (?, ?)`), deadHash, deadSmallHash)
	require.NoError(f.t, err, "delete sparse fixture entries")
	_, err = f.store.DB().Exec(f.store.Rebind(
		`UPDATE attachment_packs SET created_at = ? WHERE pack_id = ?`),
		createdAt.UTC().Format(time.RFC3339), entry.PackID)
	require.NoError(f.t, err, "age sparse fixture pack")
	return liveHash, entry.PackID
}

func TestAutomaticAttachmentMaintenancePacksBoundedAndLogsCompleteStats(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	const wantAutomaticAttachmentBytes int64 = 256 << 20
	f := newAttachmentMaintenanceFixture(t)
	content := []byte("automatic attachment maintenance payload")
	hash := f.addLoose(content)
	warnings := 0

	err := f.maintenance.runAutomaticPack(context.Background(), func(string) error {
		warnings++
		return nil
	})

	require.NoError(err, "runAutomaticPack")
	gotAutomaticAttachmentBytes := automaticAttachmentBytes
	assert.Equal(wantAutomaticAttachmentBytes, gotAutomaticAttachmentBytes)
	assert.Equal("attachment-maintenance", attachmentMaintenanceJob)
	assert.Equal("17 3 * * *", attachmentMaintenanceCron)
	assert.Zero(warnings, "successful automatic packing must not emit CLI output")
	require.NotNil(f.packedEntry(hash), "loose attachment must be packed")
	assert.Equal(content, f.readBlob(hash), "packed attachment stays readable")

	logOutput := f.logs.String()
	assert.Contains(logOutput, "level=INFO msg=\"automatic attachment maintenance complete\"")
	assert.Equal(1, strings.Count(logOutput, "automatic attachment maintenance complete"),
		"one trigger makes exactly one maintenance attempt")
	for _, field := range []string{
		"max_bytes=268435456",
		"packs_sealed=1",
		"blobs_packed=1",
		"bytes_packed=40",
		"packs_adopted=0",
		"packs_removed=0",
		"packs_quarantined=0",
		"packs_unreadable=0",
		"blobs_deferred_oversized=0",
		"packs_deferred_oversized=0",
		"records_dropped=0",
		"mappings_pruned=0",
		"blobs_missing=0",
		"blobs_corrupt=0",
		"loose_swept=0",
		"loose_orphans_removed=0",
		"budget_exhausted=false",
	} {
		assert.Contains(logOutput, field, "complete stats field %q", field)
	}
}

func TestAutomaticAttachmentMaintenanceCancellationIsInformational(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newAttachmentMaintenanceFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	warnings := 0

	err := f.maintenance.runAutomaticPack(ctx, func(string) error {
		warnings++
		return nil
	})

	require.ErrorIs(err, context.Canceled)
	assert.Zero(warnings)
	assert.Contains(f.logs.String(), "level=INFO msg=\"automatic attachment maintenance canceled\"")
	assert.NotContains(f.logs.String(), "level=WARN")
}

func TestAutomaticAttachmentMaintenanceFailureWarnsWithoutReplacingIngestSuccess(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	maintenance, logs := newFailingAttachmentMaintenance(t)
	var warning string

	err := runAfterSuccessfulAttachmentIngest(
		context.Background(),
		maintenance,
		func(context.Context) error { return nil },
		func(message string) error {
			warning = message
			return nil
		},
	)

	require.NoError(err, "maintenance failure must preserve successful ingest")
	assert.Contains(warning, "pack-attachments")
	assert.Contains(warning, "retry")
	logOutput := logs.String()
	progressAt := strings.Index(logOutput, "level=INFO msg=\"automatic attachment maintenance progress\"")
	warnAt := strings.Index(logOutput, "level=WARN msg=\"automatic attachment maintenance failed\"")
	assert.GreaterOrEqual(progressAt, 0, "partial stats are logged on error")
	assert.Greater(warnAt, progressAt, "progress must be observable before the warning")
	assert.NotContains(logOutput, "automatic attachment maintenance complete")
	assert.Contains(logOutput, "blobs_deferred_oversized=0")
	assert.Contains(logOutput, "packs_deferred_oversized=0")
	assert.Contains(logOutput, "pack-attachments", "log names the explicit retry command")
}

func TestAutomaticAttachmentMaintenanceWarningFailurePreservesIngestSuccess(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	maintenance, logs := newFailingAttachmentMaintenance(t)
	warningErr := errors.New("warning stream closed")

	err := runAfterSuccessfulAttachmentIngest(
		context.Background(),
		maintenance,
		func(context.Context) error { return nil },
		func(string) error { return warningErr },
	)

	require.NoError(err, "warning failure must preserve successful ingest")
	assert.Contains(logs.String(), "failed to emit automatic attachment maintenance warning")
	assert.Contains(logs.String(), warningErr.Error())
}

func TestRunScheduledSourcePacksOnlySuccessfulAttachmentSources(t *testing.T) {
	tests := []struct {
		name                string
		attachmentProducing bool
		predecessorErr      error
		wantPacked          bool
		wantMaintenanceRuns int
	}{
		{
			name:                "successful Gmail IMAP or Teams provider",
			attachmentProducing: true,
			wantPacked:          true,
			wantMaintenanceRuns: 1,
		},
		{
			name:                "failed attachment provider",
			attachmentProducing: true,
			predecessorErr:      errors.New("scheduled ingest failed"),
		},
		{
			name:                "successful GCal source",
			attachmentProducing: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := newAttachmentMaintenanceFixture(t)
			hash := f.addLoose([]byte("scheduled source payload"))
			predecessorCalls := 0

			err := runScheduledSource(
				context.Background(),
				f.maintenance,
				tt.attachmentProducing,
				func(context.Context) error {
					predecessorCalls++
					assert.Nil(f.packedEntry(hash), "packing must happen only after predecessor success")
					return tt.predecessorErr
				},
			)

			if tt.predecessorErr != nil {
				require.ErrorIs(err, tt.predecessorErr)
			} else {
				require.NoError(err)
			}
			assert.Equal(1, predecessorCalls)
			assert.Equal(tt.wantPacked, f.packedEntry(hash) != nil)
			assert.Equal(tt.wantMaintenanceRuns,
				strings.Count(f.logs.String(), "automatic attachment maintenance complete"),
				"automatic attempts")
		})
	}
}

func TestRegisterAttachmentMaintenanceJobAndTrigger(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newAttachmentMaintenanceFixture(t)
	hash := f.addLoose([]byte("daily maintenance payload"))
	sched := scheduler.New(func(context.Context, string) error { return nil }).WithLogger(f.maintenance.logger)
	t.Cleanup(func() {
		ctx := sched.Stop()
		<-ctx.Done()
	})

	require.NoError(registerAttachmentMaintenanceJob(sched, f.maintenance), "register maintenance job")
	require.True(sched.IsJobScheduled(attachmentMaintenanceJob), "maintenance job must be registered")
	status := sched.JobStatus()
	require.Len(status, 1)
	assert.Equal(attachmentMaintenanceJob, status[0].Name)
	assert.Equal(attachmentMaintenanceCron, status[0].Schedule)

	require.NoError(sched.TriggerJob(attachmentMaintenanceJob), "TriggerJob")
	require.NotNil(f.packedEntry(hash), "triggered daily job must perform real packing")
	assert.Equal(1, strings.Count(f.logs.String(), "automatic attachment maintenance complete"),
		"daily trigger makes exactly one bounded attempt")
	assert.Contains(f.logs.String(), "max_bytes=268435456")
}

func TestAttachmentMaintenanceDailyPacksThenRepacks(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newAttachmentMaintenanceFixture(t)
	deadPackID := f.makeZeroLivePack([]byte("daily repack old dead bytes"))
	live := []byte("daily packing new loose bytes")
	liveHash := f.addLoose(live)

	require.NoError(f.maintenance.daily(context.Background()))
	require.NotNil(f.packedEntry(liveHash))
	assert.Equal(live, f.readBlob(liveHash))
	has, err := f.store.HasPackRecord(deadPackID)
	require.NoError(err)
	assert.False(has)
	logs := f.logs.String()
	packAt := strings.Index(logs, "automatic attachment maintenance complete")
	repackAt := strings.Index(logs, "automatic attachment repack complete")
	assert.GreaterOrEqual(packAt, 0)
	assert.Greater(repackAt, packAt, "daily maintenance packs before repacking")
}

func TestPostRemovalRepackFailureWarnsWithoutReplacingSuccess(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	maintenance, logs := newFailingAttachmentMaintenance(t)
	var warning string

	err := runAfterSuccessfulAttachmentRemoval(
		context.Background(), maintenance,
		func(context.Context) error { return nil },
		func(message string) error {
			warning = message
			return nil
		},
	)

	require.NoError(err)
	assert.Contains(warning, "repack-attachments")
	assert.Contains(warning, "retry")
	logOutput := logs.String()
	progressAt := strings.Index(logOutput, "level=INFO msg=\"automatic attachment repack progress\"")
	warnAt := strings.Index(logOutput, "level=WARN msg=\"automatic attachment repack failed\"")
	assert.GreaterOrEqual(progressAt, 0, "partial stats are logged on error")
	assert.Greater(warnAt, progressAt, "progress must be observable before the warning")
	assert.NotContains(logOutput, "automatic attachment repack complete")
	for _, field := range []string{
		"max_bytes=268435456",
		"mappings_pruned=0",
		"packs_selected=0",
		"packs_rewritten=0",
		"packs_sealed=0",
		"packs_removed=0",
		"packs_deferred_oversized=0",
		"blobs_repacked=0",
		"bytes_repacked=0",
		"budget_exhausted=false",
	} {
		assert.Contains(logOutput, field, "progress stats field %q", field)
	}
}

func TestAutomaticRepackLogsCommittedSiblingProgressBeforeAggregateWarning(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newAttachmentMaintenanceFixture(t)
	now := time.Now().UTC()
	_, corruptPackID := f.makeSparsePack([]byte("live blob in corrupt oldest source"), now.Add(-72*time.Hour))
	healthyLive := []byte("healthy sibling remains eligible after the corrupt source")
	healthyHash, healthyPackID := f.makeSparsePack(healthyLive, now.Add(-48*time.Hour))
	corruptPath := filepath.Join(f.dir, "packs", corruptPackID[:2], corruptPackID+packstore.PackExt)
	require.NoError(os.WriteFile(corruptPath, []byte("truncated corrupt pack"), 0o600), "corrupt oldest source")
	var warning string

	err := f.maintenance.runAutomaticRepack(context.Background(), func(message string) error {
		warning = message
		return nil
	})

	require.Error(err, "corrupt source remains an aggregate maintenance error")
	assert.Contains(err.Error(), corruptPackID)
	assert.Contains(warning, "repack-attachments")
	newEntry := f.packedEntry(healthyHash)
	require.NotNil(newEntry)
	assert.NotEqual(healthyPackID, newEntry.PackID, "healthy sibling mapping commits")
	assert.Equal(healthyLive, f.readBlob(healthyHash))
	logs := f.logs.String()
	progressAt := strings.Index(logs, "level=INFO msg=\"automatic attachment repack progress\"")
	warnAt := strings.Index(logs, "level=WARN msg=\"automatic attachment repack failed\"")
	assert.GreaterOrEqual(progressAt, 0)
	assert.Greater(warnAt, progressAt, "committed progress is logged before the aggregate warning")
	assert.NotContains(logs, "automatic attachment repack complete")
	for _, field := range []string{
		"max_bytes=268435456",
		"mappings_pruned=2",
		"packs_selected=2",
		"packs_rewritten=1",
		"packs_sealed=1",
		"packs_removed=1",
		"packs_deferred_oversized=0",
		"blobs_repacked=1",
		fmt.Sprintf("bytes_repacked=%d", len(healthyLive)),
		"budget_exhausted=false",
	} {
		assert.Contains(logs, field, "progress stats field %q", field)
	}
}

func TestAttachmentProducingCommandExactAllowlist(t *testing.T) {
	allowlisted := []string{
		"backfill-teams-media",
		"import",
		"import-emlx",
		"import-gvoice",
		"import-imessage",
		importMboxCommand,
		"import-messenger",
		"import-pst",
		"import-synctech-sms",
		"import-whatsapp",
		"sync-synctech-sms",
		"sync-teams",
	}
	for _, command := range allowlisted {
		t.Run("allows "+command, func(t *testing.T) {
			assert.True(t, attachmentProducingCommand([]string{command, "--example"}))
		})
	}

	for _, command := range []string{
		"pack-attachments",
		"sync-calendar",
		"add-account",
		"remove-account",
		"build-cache",
		"unrelated-command",
	} {
		t.Run("rejects "+command, func(t *testing.T) {
			assert.False(t, attachmentProducingCommand([]string{command}))
		})
	}
	assert.False(t, attachmentProducingCommand(nil))
}

func TestAttachmentIngestMutationLeaseWaitsForMaintenance(t *testing.T) {
	require := require.New(t)
	f := newAttachmentMaintenanceFixture(t)
	maintenanceLease, err := f.maintenance.coordinator.AcquireMaintenance(context.Background())
	require.NoError(err)
	ingestStarted := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- runAfterSuccessfulAttachmentIngest(
			context.Background(), f.maintenance,
			func(context.Context) error {
				close(ingestStarted)
				return nil
			}, nil,
		)
	}()

	select {
	case <-ingestStarted:
		assert.Fail(t, "ingest started while maintenance held the exclusive lease")
	case <-time.After(25 * time.Millisecond):
	}
	require.NoError(maintenanceLease.Release())
	select {
	case <-ingestStarted:
	case <-time.After(time.Second):
		require.Fail("ingest did not start after maintenance released its lease")
	}
	require.NoError(<-done)
}
