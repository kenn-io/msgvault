package backupapp_test

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/msgvault/internal/attachmentstore"
	"go.kenn.io/msgvault/internal/backupapp"
	"go.kenn.io/msgvault/internal/store"
)

const (
	benchmarkRestoreBlobs     = 2000
	benchmarkRestoreBlobBytes = 4096
)

func BenchmarkBackupRestoreLayouts(b *testing.B) {
	ctx := context.Background()
	f := newVaultFixture(b)
	expected := make(map[string][]byte, benchmarkRestoreBlobs)
	var totalBytes int64
	for i := range benchmarkRestoreBlobs {
		content := deterministicRestoreBlob(i, benchmarkRestoreBlobBytes)
		hash := f.addBlob(content, canonicalPath(hashOf(content)))
		expected[hash] = content
		totalBytes += int64(len(content))
	}
	repo, err := backup.Init(filepath.Join(b.TempDir(), "repo"))
	require.NoError(b, err)
	app := backupapp.New("benchmark")
	_, err = backup.Create(ctx, repo, app, backup.CreateOptions{
		DBPath: f.dbPath, ContentDir: f.attDir, DataDir: f.dataDir,
		ContentSource: f.contentSource(),
	})
	require.NoError(b, err)
	verified, err := backup.Verify(ctx, repo, app, backup.VerifyOptions{All: true})
	require.NoError(b, err)
	require.Empty(b, verified.Problems)

	for _, layout := range []struct {
		name   string
		packed bool
	}{
		{name: "loose"},
		{name: "packed", packed: true},
	} {
		b.Run(layout.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(totalBytes)
			var attachmentDuration, databaseCheckDuration, totalDuration time.Duration
			var looseFiles, packFiles, packedBlobs, looseBlobs int64
			b.ResetTimer()
			for range b.N {
				b.StopTimer()
				target := filepath.Join(b.TempDir(), "restore")
				timer := &restoreStageTimer{}
				opts := backup.RestoreOptions{TargetDir: target, Progress: timer.handle}
				if layout.packed {
					opts.PackedContent = backupapp.NewPackedRestoreTarget(packstore.DefaultLimits())
				}
				b.StartTimer()
				started := time.Now()
				res, restoreErr := backup.Restore(ctx, repo, app, opts)
				elapsed := time.Since(started)
				if !layout.packed && restoreErr == nil {
					var restored *store.Store
					restored, restoreErr = store.OpenForTest(res.DBPath)
					if restoreErr == nil {
						restoreErr = restored.ClearAttachmentPackMetadata()
					}
					if restored != nil {
						restoreErr = errors.Join(restoreErr, restored.Close())
					}
				}
				b.StopTimer()
				require.NoError(b, restoreErr)
				require.Equal(b, int64(benchmarkRestoreBlobs), res.AttachmentBlobs)
				require.Equal(b, totalBytes, res.AttachmentBytes)
				filesLoose, filesPacked := countRestoreLayoutFiles(b, filepath.Join(target, "attachments"))
				if layout.packed {
					require.Zero(b, filesLoose)
					require.Equal(b, int64(res.AttachmentPacks), filesPacked)
					require.Equal(b, int64(benchmarkRestoreBlobs), res.PackedAttachmentBlobs)
					require.Zero(b, res.LooseAttachmentBlobs)
				} else {
					require.Equal(b, int64(benchmarkRestoreBlobs), filesLoose)
					require.Zero(b, filesPacked)
					require.Zero(b, res.PackedAttachmentBlobs)
					require.Equal(b, int64(benchmarkRestoreBlobs), res.LooseAttachmentBlobs)
				}
				assertRestoredBenchmarkBytes(b, res.DBPath, filepath.Join(target, "attachments"), expected)
				attach, databaseChecks := timer.durations()
				attachmentDuration += attach
				databaseCheckDuration += databaseChecks
				totalDuration += elapsed
				looseFiles += filesLoose
				packFiles += filesPacked
				packedBlobs += res.PackedAttachmentBlobs
				looseBlobs += res.LooseAttachmentBlobs
			}
			iterations := float64(b.N)
			b.ReportMetric(float64(looseFiles)/iterations, "attachment_files/op")
			b.ReportMetric(float64(packFiles)/iterations, "pack_files/op")
			b.ReportMetric(float64(packedBlobs)/iterations, "packed_blobs/op")
			b.ReportMetric(float64(looseBlobs)/iterations, "loose_blobs/op")
			b.ReportMetric(float64(attachmentDuration.Microseconds())/1000/iterations, "attachment_ms/op")
			b.ReportMetric(float64(databaseCheckDuration.Microseconds())/1000/iterations, "database_checks_ms/op")
			b.ReportMetric(float64(totalDuration.Microseconds())/1000/iterations, "total_ms/op")
			b.ReportMetric(float64(totalBytes)/(1<<20), "attachment_MiB/op")
			b.ReportMetric(
				float64(totalBytes)*iterations/(1<<20)/attachmentDuration.Seconds(),
				"attachment_MiB/s",
			)
		})
	}
}

type restoreStageTimer struct {
	mu                 sync.Mutex
	attachmentStart    time.Time
	attachment         time.Duration
	databaseCheckStart time.Time
	databaseChecks     time.Duration
}

func (t *restoreStageTimer) handle(event backup.ProgressEvent) {
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	switch event.Stage {
	case backup.ProgressStageAttachments:
		if t.attachmentStart.IsZero() {
			t.attachmentStart = now
		}
		if event.Final {
			t.attachment = now.Sub(t.attachmentStart)
		}
	case backup.ProgressStageIntegrityCheck:
		if t.databaseCheckStart.IsZero() {
			t.databaseCheckStart = now
		}
	case backup.ProgressStageRestoreStats:
		if t.databaseCheckStart.IsZero() {
			t.databaseCheckStart = now
		}
		if event.Final {
			t.databaseChecks = now.Sub(t.databaseCheckStart)
		}
	default:
		return
	}
}

func (t *restoreStageTimer) durations() (time.Duration, time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.attachment, t.databaseChecks
}

func deterministicRestoreBlob(index, size int) []byte {
	result := make([]byte, size)
	var input [16]byte
	binary.LittleEndian.PutUint64(input[:8], uint64(index))
	for offset, block := 0, uint64(0); offset < len(result); block++ {
		binary.LittleEndian.PutUint64(input[8:], block)
		digest := sha256.Sum256(input[:])
		offset += copy(result[offset:], digest[:])
	}
	return result
}

func countRestoreLayoutFiles(tb testing.TB, root string) (int64, int64) {
	tb.Helper()
	var loose, packed int64
	require.NoError(tb, filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if first, _, _ := strings.Cut(filepath.ToSlash(rel), "/"); first == "packs" {
			if filepath.Ext(path) == packstore.PackExt {
				packed++
			}
			return nil
		}
		loose++
		return nil
	}))
	return loose, packed
}

func assertRestoredBenchmarkBytes(
	tb testing.TB,
	dbPath, attachmentsDir string,
	expected map[string][]byte,
) {
	tb.Helper()
	st, err := store.OpenForTest(dbPath)
	require.NoError(tb, err)
	blobs, err := attachmentstore.New(store.NewPackCatalog(st), attachmentsDir)
	require.NoError(tb, err)
	for hash, want := range expected {
		reader, size, err := blobs.Open(hash)
		require.NoError(tb, err)
		got, err := io.ReadAll(reader)
		require.NoError(tb, err)
		require.NoError(tb, reader.Close())
		assert.Equal(tb, int64(len(want)), size)
		assert.Equal(tb, want, got)
	}
	require.NoError(tb, blobs.Close())
	require.NoError(tb, st.Close())
}
