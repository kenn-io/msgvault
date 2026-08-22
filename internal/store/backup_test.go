package store_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestBackupDatabaseContext_AtomicallyPublishesValidBackup(t *testing.T) {
	testutil.SkipIfPostgres(t, "VACUUM INTO backup publication is SQLite-only")
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	_, err := f.Store.DB().Exec(`
		CREATE TABLE backup_payload (value TEXT);
		INSERT INTO backup_payload VALUES ('complete');
	`)
	require.NoError(err, "create backup payload")

	backupDir := t.TempDir()
	dst := filepath.Join(backupDir, "msgvault.db.backup")
	require.NoError(f.Store.BackupDatabaseContext(context.Background(), dst))
	require.FileExists(dst)

	backupStore, err := store.OpenReadOnly(dst)
	require.NoError(err, "open published backup")
	t.Cleanup(func() { require.NoError(backupStore.Close()) })

	var value string
	err = backupStore.DB().QueryRow("SELECT value FROM backup_payload").Scan(&value)
	require.NoError(err, "query published backup")
	assert.Equal("complete", value)

	tempMatches, globErr := filepath.Glob(
		filepath.Join(backupDir, ".msgvault.db.backup.tmp-*"),
	)
	require.NoError(globErr, "glob temporary backup directories")
	assert.Empty(tempMatches, "successful backup must remove its staging directory")
}

func TestBackupDatabaseContext_CancellationRemovesUnpublishedBackup(t *testing.T) {
	testutil.SkipIfPostgres(t, "VACUUM INTO backup cancellation is SQLite-only")
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	_, err := f.Store.DB().Exec(`
		CREATE TABLE backup_cancellation_payload (data BLOB);
		INSERT INTO backup_cancellation_payload VALUES (zeroblob(33554432));
	`)
	require.NoError(err, "create backup payload")

	backupDir := t.TempDir()
	dst := filepath.Join(backupDir, "msgvault.db.backup")
	tempPattern := filepath.Join(backupDir, ".msgvault.db.backup.tmp-*", "backup.db")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		done <- f.Store.BackupDatabaseContext(ctx, dst)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		_, finalErr := os.Stat(dst)
		tempMatches, globErr := filepath.Glob(tempPattern)
		require.NoError(globErr, "glob temporary backups")
		if finalErr == nil || len(tempMatches) > 0 {
			break
		}
		require.ErrorIs(finalErr, os.ErrNotExist)
		if time.Now().After(deadline) {
			require.FailNow("backup output did not appear")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()

	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		require.FailNow("backup did not stop after cancellation")
	}
	require.Error(err, "canceled backup should fail")
	assert.NoFileExists(dst, "an incomplete backup must not use the final filename")

	tempMatches, globErr := filepath.Glob(tempPattern)
	require.NoError(globErr, "glob temporary backups after cancellation")
	assert.Empty(tempMatches, "temporary backup files must be cleaned up")
}

func TestBackupDatabaseContext_PreservesReversiblePersonMerge(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	testutil.SkipIfPostgres(t, "VACUUM INTO backup publication is SQLite-only")
	ctx := context.Background()
	f := storetest.New(t)
	survivorParticipant := f.EnsureParticipant(
		"backup-merge-survivor@example.com", "Survivor", "example.com")
	absorbedParticipant := f.EnsureParticipant(
		"backup-merge-absorbed@example.com", "Absorbed", "example.com")
	survivor, _, err := f.Store.CreatePersonFromParticipant(survivorParticipant)
	require.NoError(err)
	absorbed, _, err := f.Store.CreatePersonFromParticipant(absorbedParticipant)
	require.NoError(err)
	_, err = f.Store.AddPersonNameContext(ctx, absorbed.ID, store.PersonNameInput{
		NameKind: store.PersonNameFormatted, Formatted: new("Backup Absorbed"),
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	absorbed, err = f.Store.GetPersonContext(ctx, absorbed.ID)
	require.NoError(err)
	merged, err := f.Store.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "backup-person-merge", Actor: "test",
	})
	require.NoError(err)

	destination := filepath.Join(t.TempDir(), "msgvault.db.backup")
	require.NoError(f.Store.BackupDatabaseContext(ctx, destination))
	restored, err := store.Open(destination)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(restored.Close()) })
	detail, err := restored.GetPersonMergeContext(ctx, merged.Merge.ID)
	require.NoError(err)
	assert.Len(detail.Participants, 2)
	_, err = restored.GetPersonMergeSnapshotContext(ctx, merged.Merge.ID)
	require.NoError(err)
	split, err := restored.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
		SourcePersonID: merged.Person.ID, MergeID: merged.Merge.ID,
		ParticipantIDs:         []int64{absorbedParticipant},
		ExpectedSourceRevision: merged.Person.Revision,
		IdempotencyKey:         "backup-person-split", Actor: "test",
	})
	require.NoError(err)
	assert.True(split.ExactReversal)
	assert.Equal([]int64{absorbedParticipant}, split.NewPerson.ParticipantIDs)
}
