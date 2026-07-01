package cmd

import (
	"context"
	"errors"
	"os"
	"strconv"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

func TestAcquireDirectSQLiteWriteLockSkipsPostgreSQL(t *testing.T) {
	dataDir := t.TempDir()
	cfg := lifecycleTestConfig(dataDir)
	cfg.Data.DatabaseURL = "postgres://user:pass@example.com:5432/msgvault"

	owner, err := tryAcquireWriteOwnerLock(dataDir)
	require.NoError(t, err, "pre-acquire sqlite lock")
	t.Cleanup(func() { _ = owner.Close() })

	release, err := acquireDirectSQLiteWriteLock(cfg)
	require.NoError(t, err, "postgres direct writer should not use sqlite flock")
	require.NotNil(t, release, "release")
	release()
}

func TestAcquireDirectSQLiteWriteLock_HoldsThenReleases(t *testing.T) {
	require := require.New(
		t)

	dataDir := t.TempDir()
	cfg := lifecycleTestConfig(dataDir)

	release, err := acquireDirectSQLiteWriteLock(cfg)
	require.NoError(
		err, "acquire on a free archive")

	require.NotNil(release, "release func")

	// While a direct writer owns the archive, no second writer can claim it.
	blocked, err := tryAcquireWriteOwnerLock(dataDir)
	assert.Nil(t, blocked, "second owner")
	require.ErrorAs(err, &writeOwnerLockHeldError{}, "second acquisition blocked")

	release()

	// After release the archive is free again.
	reacquired, err := tryAcquireWriteOwnerLock(dataDir)
	require.NoError(
		err, "reacquire after release")

	require.NoError(
		reacquired.Close(), "close reacquired lock")
}

func TestOpenWritableStoreAndInitOwnsArchiveUntilCleanup(t *testing.T) {
	require := require.New(
		t)

	dataDir := t.TempDir()
	testCfg := lifecycleTestConfig(dataDir)
	withStoreResolverConfig(t, testCfg)

	st, cleanup, err := openWritableStoreAndInit()
	require.NoError(
		err, "open writable store")

	require.NotNil(st, "store")
	require.NotNil(cleanup, "cleanup")
	defer func() {
		if cleanup != nil {
			cleanup()
		}
	}()

	blocked, err := tryAcquireWriteOwnerLock(dataDir)
	assert.Nil(t, blocked, "second owner while store is open")
	require.ErrorAs(err, &writeOwnerLockHeldError{}, "store helper holds write-owner lock")

	cleanup()
	cleanup = nil

	reacquired, err := tryAcquireWriteOwnerLock(dataDir)
	require.NoError(
		err, "cleanup releases write-owner lock")

	require.NoError(
		reacquired.Close(), "close reacquired lock")
}

func TestAcquireDirectSQLiteWriteLock_ActionableErrorWhenOwned(t *testing.T) {
	assert := assert.New(t)
	require := require.New(
		t)

	dataDir := t.TempDir()
	cfg := lifecycleTestConfig(dataDir)

	owner, err := tryAcquireWriteOwnerLock(dataDir)
	require.NoError(
		err, "pre-acquire owner")

	t.Cleanup(func() { _ = owner.Close() })

	release, err := acquireDirectSQLiteWriteLock(cfg)
	assert.Nil(release, "no release when blocked")
	require.Error(err, "acquire on an owned archive")
	assert.Contains(err.Error(), "owned", "names the ownership condition")
	assert.Contains(err.Error(), "serve stop", "points at the remedy")
}

func TestDirectWriterOwnsArchive_TrueWhenHeldWithoutDaemon(t *testing.T) {
	dataDir := t.TempDir()
	cfg := lifecycleTestConfig(dataDir)

	owner, err := tryAcquireWriteOwnerLock(dataDir)
	require.NoError(t, err, "acquire owner lock")
	t.Cleanup(func() { _ = owner.Close() })

	owned, err := directSQLiteWriterOwnsArchive(cfg)
	require.NoError(t, err, "inspect direct writer ownership")
	assert.True(t, owned,
		"lock held with no daemon record => a direct writer owns the archive")
}

func TestDirectWriterOwnsArchive_FalseWhenArchiveFree(t *testing.T) {
	require := require.New(
		t)

	dataDir := t.TempDir()
	cfg := lifecycleTestConfig(dataDir)

	owned, err := directSQLiteWriterOwnsArchive(cfg)
	require.NoError(
		err, "inspect direct writer ownership")

	assert.False(t, owned, "free archive")

	// And the archive is still claimable afterwards (the probe released it).
	owner, err := tryAcquireWriteOwnerLock(dataDir)
	require.NoError(
		err, "probe did not leak the lock")

	require.NoError(
		owner.Close(), "close")
}

func TestDirectWriterOwnsArchive_FalseWhenDaemonRecordPresent(t *testing.T) {
	require := require.New(
		t)

	dataDir := t.TempDir()
	cfg := lifecycleTestConfig(dataDir)

	// A live daemon advertises a runtime record; simulate one for this
	// (alive) test process so liveDaemonRecords observes it.
	_, _, err := writeDaemonRuntime(dataDir, "127.0.0.1", 0, "test", "")
	require.NoError(
		err, "write daemon runtime record")

	owner, err := tryAcquireWriteOwnerLock(dataDir)
	require.NoError(
		err, "acquire owner lock")

	t.Cleanup(func() { _ = owner.Close() })

	owned, err := directSQLiteWriterOwnsArchive(cfg)
	require.NoError(
		err, "inspect direct writer ownership")

	assert.False(t, owned,
		"lock held by a live daemon is not a direct writer")
}

func TestDaemonAutostartPreflight_BlocksWhenDirectWriterOwnsArchive(t *testing.T) {
	dataDir := t.TempDir()
	cfg := lifecycleTestConfig(dataDir)

	owner, err := tryAcquireWriteOwnerLock(dataDir)
	require.NoError(t, err, "acquire owner lock")
	t.Cleanup(func() { _ = owner.Close() })

	err = daemonAutostartPreflight(cfg)
	require.Error(t, err, "preflight blocks autostart while a writer owns the archive")
	assert.Contains(t, err.Error(), "write operation", "explains the contention")
}

func TestDaemonAutostartPreflight_AllowsWhenArchiveFree(t *testing.T) {
	dataDir := t.TempDir()
	cfg := lifecycleTestConfig(dataDir)

	require.NoError(t, daemonAutostartPreflight(cfg),
		"preflight permits autostart on a free archive")
}

// TestDeduplicateFailsFastWhenArchiveOwned exercises the daemon autostart
// preflight through a real write command: with the archive owned by a direct
// writer, the foreground command must refuse rather than start a daemon that
// cannot claim archive ownership.
func TestDeduplicateFailsFastWhenArchiveOwned(t *testing.T) {
	assert := assert.New(t)
	require := require.New(
		t)

	dataDir := t.TempDir()
	withStoreResolverConfig(t, lifecycleTestConfig(dataDir))

	owner, err := tryAcquireWriteOwnerLock(dataDir)
	require.NoError(
		err, "acquire owner lock")

	t.Cleanup(func() { _ = owner.Close() })

	err = runDeduplicate(&cobra.Command{}, nil)
	require.Error(err, "deduplicate must fail while the archive is owned")
	assert.Contains(err.Error(), "write operation", "actionable ownership error")
	assert.Contains(err.Error(), "cannot start", "explains daemon autostart is blocked")
}

func TestEmbeddingsRetireFailsFastWhenArchiveOwned(t *testing.T) {
	assert := assert.New(t)
	require := require.New(
		t)

	dataDir := t.TempDir()
	withStoreResolverConfig(t, lifecycleTestConfig(dataDir))

	owner, err := tryAcquireWriteOwnerLock(dataDir)
	require.NoError(
		err, "acquire owner lock")

	t.Cleanup(func() { _ = owner.Close() })

	cmd := &cobra.Command{Use: "retire"}
	cmd.SetContext(context.Background())
	err = runEmbeddingsRetire(cmd, []string{"1"})
	require.Error(err, "embeddings retire must fail while the archive is owned")
	assert.Contains(err.Error(), "owned", "actionable ownership error")
	assert.Contains(err.Error(), "serve stop", "points at the remedy")
}

func TestEmbeddingsListFailsFastWhenArchiveOwned(t *testing.T) {
	assert := assert.New(t)
	require := require.New(
		t)

	dataDir := t.TempDir()
	testCfg := lifecycleTestConfig(dataDir)
	withStoreResolverConfig(t, testCfg)

	st, err := store.Open(testCfg.DatabaseDSN())
	require.NoError(
		err, "open test archive")

	require.NoError(
		st.InitSchema(), "init test archive")

	require.NoError(
		st.Close(), "close test archive")

	owner, err := tryAcquireWriteOwnerLock(dataDir)
	require.NoError(
		err, "acquire owner lock")

	t.Cleanup(func() { _ = owner.Close() })

	cmd := &cobra.Command{Use: "list"}
	cmd.SetContext(context.Background())
	err = runEmbeddingsList(cmd, nil)
	require.Error(err, "embeddings list must fail while the archive is owned")
	assert.Contains(err.Error(), "owned", "actionable ownership error")
	assert.Contains(err.Error(), "serve stop", "points at the remedy")
}

func TestInitDBFailsFastWhenArchiveOwned(t *testing.T) {
	assert := assert.New(t)
	require := require.New(
		t)

	dataDir := t.TempDir()
	withStoreResolverConfig(t, lifecycleTestConfig(dataDir))

	owner, err := tryAcquireWriteOwnerLock(dataDir)
	require.NoError(
		err, "acquire owner lock")

	t.Cleanup(func() { _ = owner.Close() })

	err = initDBCmd.RunE(&cobra.Command{Use: "init-db"}, nil)
	require.Error(err, "init-db must fail while the archive is owned")
	assert.Contains(err.Error(), "write operation", "explains the active writer")
	assert.Contains(err.Error(), "wait", "points at the remedy")
}

func TestVerifyDaemonAutostartFailsFastWhenArchiveOwned(t *testing.T) {
	assert := assert.New(t)
	require := require.New(
		t)

	dataDir := t.TempDir()
	withStoreResolverConfig(t, lifecycleTestConfig(dataDir))

	owner, err := tryAcquireWriteOwnerLock(dataDir)
	require.NoError(
		err, "acquire owner lock")

	t.Cleanup(func() { _ = owner.Close() })

	err = verifyCmd.RunE(&cobra.Command{Use: "verify"}, []string{"alice@example.com"})
	require.Error(err, "verify must not autostart a daemon while the archive is owned")
	assert.Contains(err.Error(), "write operation is in progress", "actionable ownership error")
	assert.Contains(err.Error(), "cannot start", "daemon start is refused")
}

func TestBuildCacheFailsFastWhenArchiveOwned(t *testing.T) {
	assert := assert.New(t)
	require := require.New(
		t)

	dataDir := t.TempDir()
	testCfg := lifecycleTestConfig(dataDir)
	withStoreResolverConfig(t, testCfg)

	st, err := store.Open(testCfg.DatabaseDSN())
	require.NoError(
		err, "open test archive")

	require.NoError(
		st.InitSchema(), "init test archive")

	require.NoError(
		st.Close(), "close test archive")

	owner, err := tryAcquireWriteOwnerLock(dataDir)
	require.NoError(
		err, "acquire owner lock")

	t.Cleanup(func() { _ = owner.Close() })

	err = buildCacheCmd.RunE(&cobra.Command{Use: "build-cache"}, nil)
	require.Error(err, "build-cache must fail while a local writer owns the archive")
	assert.Contains(err.Error(), "write operation is in progress", "actionable ownership error")
	assert.Contains(err.Error(), "cannot start", "daemon start is refused")
}

func TestBuildCacheDaemonChildBypassesArchiveOwnershipLock(t *testing.T) {
	dataDir := t.TempDir()
	testCfg := lifecycleTestConfig(dataDir)

	owner, err := tryAcquireWriteOwnerLock(dataDir)
	require.NoError(t, err, "acquire daemon owner lock")
	t.Cleanup(func() { _ = owner.Close() })

	t.Setenv(buildCacheDaemonSubprocessEnv, strconv.Itoa(os.Getppid()))
	release, err := acquireBuildCacheWriteLock(testCfg)
	require.NoError(t, err, "daemon-owned build-cache child should not reacquire the daemon lock")
	release()
}

func TestDaemonCLIChildBypassesArchiveOwnershipLock(t *testing.T) {
	dataDir := t.TempDir()
	testCfg := lifecycleTestConfig(dataDir)

	owner, err := tryAcquireWriteOwnerLock(dataDir)
	require.NoError(t, err, "acquire daemon owner lock")
	t.Cleanup(func() { _ = owner.Close() })

	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	release, err := acquireDirectSQLiteWriteLock(testCfg)
	require.NoError(t, err, "daemon-owned CLI child should not reacquire the daemon lock")
	release()
}

func TestCreateSubsetFailsFastWhenArchiveOwned(t *testing.T) {
	assert := assert.New(t)
	require := require.New(
		t)

	dataDir := t.TempDir()
	testCfg := lifecycleTestConfig(dataDir)
	withStoreResolverConfig(t, testCfg)

	st, err := store.Open(testCfg.DatabaseDSN())
	require.NoError(
		err, "open test archive")

	require.NoError(
		st.InitSchema(), "init test archive")

	require.NoError(
		st.Close(), "close test archive")

	oldRows := subsetRows
	oldOutput := subsetOutput
	t.Cleanup(func() {
		subsetRows = oldRows
		subsetOutput = oldOutput
	})
	subsetRows = 1
	subsetOutput = t.TempDir()

	owner, err := tryAcquireWriteOwnerLock(dataDir)
	require.NoError(
		err, "acquire owner lock")

	t.Cleanup(func() { _ = owner.Close() })

	err = runCreateSubset(&cobra.Command{Use: "create-subset"}, nil)
	require.Error(err, "create-subset must not autostart a daemon while the archive is owned")
	assert.Contains(err.Error(), "write operation", "actionable ownership error")
	assert.Contains(err.Error(), "cannot start", "daemon start is refused")
}

// TestOpenHTTPStoreFailsWhenDirectWriterOwnsArchive exercises the
// writer-running-daemon-autostart behavior: a read that would autostart the
// daemon must fail fast (and must not spawn) while a direct writer owns the
// archive.
func TestOpenHTTPStoreFailsWhenDirectWriterOwnsArchive(t *testing.T) {
	dataDir := t.TempDir()
	withStoreResolverConfig(t, lifecycleTestConfig(dataDir))
	stubStartServeBackgroundProcess(t, func(*config.Config) (*backgroundServeProcess, error) {
		require.FailNow(t, "must not spawn a daemon while a writer owns the archive")
		return nil, errors.New("unreachable")
	})

	owner, err := tryAcquireWriteOwnerLock(dataDir)
	require.NoError(t, err, "acquire owner lock")
	t.Cleanup(func() { _ = owner.Close() })

	_, _, err = OpenHTTPStore(context.Background())
	require.Error(t, err, "OpenHTTPStore must not autostart over a direct writer")
	assert.Contains(t, err.Error(), "write operation", "explains the contention")
}
