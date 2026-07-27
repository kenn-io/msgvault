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
