package cmd

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/config"
)

func TestServeOwnershipEnsureRuntimeRecordRepublishesMissingRecord(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dataDir := t.TempDir()
	cfg := &config.Config{Data: config.DataConfig{DataDir: dataDir}}
	owner, err := claimServeOwnership(context.Background(), cfg, "127.0.0.1", 8123, "v-test")
	require.NoError(err, "claimServeOwnership")
	t.Cleanup(func() { require.NoError(owner.Close(), "close ownership") })

	readRecord := func() daemon.RuntimeRecord {
		records, err := daemonRuntimeStore(dataDir).List()
		require.NoError(err, "list runtime records")
		require.Len(records, 1, "runtime records")
		return records[0]
	}
	initial := readRecord()

	// No-op while the record is present.
	require.NoError(owner.EnsureRuntimeRecord(), "ensure with record present")
	assert.Equal(initial.Metadata, readRecord().Metadata, "record untouched while present")

	// Simulate a wrongful prune by another process.
	path, err := daemonRuntimeStore(dataDir).Path(initial.PID)
	require.NoError(err, "runtime record path")
	require.NoError(os.Remove(path), "remove runtime record")

	require.NoError(owner.EnsureRuntimeRecord(), "ensure after prune")
	restored := readRecord()
	assert.Equal(initial.PID, restored.PID, "pid restored")
	assert.Equal(initial.Address, restored.Address, "address restored")
	assert.Equal(initial.Metadata[runtimeShutdownToken], restored.Metadata[runtimeShutdownToken], "shutdown token preserved")
	// Compare time.Time with Equal — the JSON round-trip drops the
	// monotonic reading, so assert.Equal's ==/DeepEqual is unreliable.
	assert.True(initial.StartedAt.Equal(restored.StartedAt), "started_at preserved")
}

func TestRuntimeRecordHeartbeatRepublishesUntilCancelled(t *testing.T) {
	require := require.New(t)

	dataDir := t.TempDir()
	cfg := &config.Config{Data: config.DataConfig{DataDir: dataDir}}
	owner, err := claimServeOwnership(context.Background(), cfg, "127.0.0.1", 8123, "v-test")
	require.NoError(err, "claimServeOwnership")
	t.Cleanup(func() { require.NoError(owner.Close(), "close ownership") })

	path, err := daemonRuntimeStore(dataDir).Path(owner.record.PID)
	require.NoError(err, "runtime record path")
	require.NoError(os.Remove(path), "remove runtime record")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runtimeRecordHeartbeat(ctx, owner, 10*time.Millisecond)
	}()

	require.Eventually(func() bool {
		_, statErr := os.Stat(path)
		return statErr == nil
	}, 5*time.Second, 10*time.Millisecond, "heartbeat republishes the pruned record")

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		require.FailNow("heartbeat did not stop after context cancellation")
	}
}

func TestRuntimeRecordHeartbeatDoesNotRepublishAfterOwnershipClose(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dataDir := t.TempDir()
	cfg := &config.Config{Data: config.DataConfig{DataDir: dataDir}}
	owner, err := claimServeOwnership(context.Background(), cfg, "127.0.0.1", 8123, "v-test")
	require.NoError(err, "claimServeOwnership")

	path, err := daemonRuntimeStore(dataDir).Path(owner.record.PID)
	require.NoError(err, "runtime record path")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runtimeRecordHeartbeat(ctx, owner, time.Millisecond)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	require.NoError(owner.Close(), "close ownership")
	require.NoError(owner.SetStartupPhase("still starting"), "startup phase update after close")
	assert.Never(func() bool {
		_, statErr := os.Stat(path)
		return statErr == nil
	}, 100*time.Millisecond, time.Millisecond, "closed ownership must stay unpublished")
}

func TestRuntimeRecordHeartbeatSerializesStartupPhaseUpdates(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dataDir := t.TempDir()
	cfg := &config.Config{Data: config.DataConfig{DataDir: dataDir}}
	owner, err := claimServeOwnership(context.Background(), cfg, "127.0.0.1", 8123, "v-test")
	require.NoError(err, "claimServeOwnership")
	t.Cleanup(func() { require.NoError(owner.Close(), "close ownership") })

	path, err := daemonRuntimeStore(dataDir).Path(owner.record.PID)
	require.NoError(err, "runtime record path")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runtimeRecordHeartbeat(ctx, owner, time.Microsecond)
	}()

	for range 100 {
		_ = os.Remove(path)
		require.NoError(owner.SetStartupPhase("building analytics cache"), "set startup phase")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		require.FailNow("heartbeat did not stop after context cancellation")
	}

	records, err := daemonRuntimeStore(dataDir).List()
	require.NoError(err, "list runtime records")
	require.Len(records, 1, "runtime records")
	assert.Equal("building analytics cache", records[0].Metadata[runtimeStartupPhase], "latest startup phase")
}

func TestClaimServeOwnershipLocksAndPublishesRuntime(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dataDir := t.TempDir()
	cfg := &config.Config{Data: config.DataConfig{DataDir: dataDir}}

	owner, err := claimServeOwnership(context.Background(), cfg, "127.0.0.1", 8123, "v-test")
	require.NoError(
		err, "claimServeOwnership")

	second, err := tryAcquireWriteOwnerLock(dataDir)
	assert.Nil(second, "second write lock")
	require.ErrorAs(err, &writeOwnerLockHeldError{}, "second owner error")

	records, err := daemonRuntimeStore(dataDir).List()
	require.NoError(
		err, "list runtime records")

	require.Len(records, 1, "runtime records while serve owns archive")
	assert.Equal(daemonService, records[0].Service, "service")
	require.NoError(
		owner.Close(), "close ownership")

	records, err = daemonRuntimeStore(dataDir).List()
	require.NoError(
		err, "list runtime records after close")

	assert.Empty(records, "runtime records after close")

	reacquired, err := tryAcquireWriteOwnerLock(dataDir)
	require.NoError(
		err, "lock after ownership close")

	require.NoError(
		reacquired.Close(), "close reacquired lock")
}

func TestServeOwnershipStartupPhaseUpdatesRuntimeRecord(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dataDir := t.TempDir()
	cfg := &config.Config{Data: config.DataConfig{DataDir: dataDir}}
	owner, err := claimServeOwnership(context.Background(), cfg, "127.0.0.1", 8123, "v-test")
	require.NoError(err, "claimServeOwnership")
	t.Cleanup(func() { require.NoError(owner.Close(), "close ownership") })

	readRecord := func() daemon.RuntimeRecord {
		records, err := daemonRuntimeStore(dataDir).List()
		require.NoError(err, "list runtime records")
		require.Len(records, 1, "runtime records")
		return records[0]
	}

	initial := readRecord()
	assert.Equal(daemonStartupPhaseInitial, initial.Metadata[runtimeStartupPhase], "initial phase")

	require.NoError(owner.SetStartupPhase("building analytics cache"), "set startup phase")
	updated := readRecord()
	assert.Equal("building analytics cache", updated.Metadata[runtimeStartupPhase], "updated phase")
	assert.Equal(initial.Metadata[runtimeShutdownToken], updated.Metadata[runtimeShutdownToken], "shutdown token preserved")
	assert.Equal(initial.StartedAt, updated.StartedAt, "started_at preserved")

	require.NoError(owner.SetStartupPhase(""), "clear startup phase")
	cleared := readRecord()
	assert.NotContains(cleared.Metadata, runtimeStartupPhase, "phase cleared")
}

func TestClaimServeOwnershipRejectsSecondOwner(t *testing.T) {
	dataDir := t.TempDir()
	cfg := &config.Config{Data: config.DataConfig{DataDir: dataDir}}

	first, err := tryAcquireWriteOwnerLock(dataDir)
	require.NoError(t, err, "pre-held lock")
	t.Cleanup(func() { require.NoError(t, first.Close(), "close pre-held lock") })

	owner, err := claimServeOwnership(context.Background(), cfg, "127.0.0.1", 8123, "v-test")
	assert.Nil(t, owner, "ownership")
	require.ErrorAs(t, err, &writeOwnerLockHeldError{}, "error type")
}

func TestClaimServeOwnershipSkipsSQLiteLockForPostgreSQL(t *testing.T) {
	require := require.New(t)

	dataDir := t.TempDir()
	cfg := &config.Config{Data: config.DataConfig{
		DataDir:     dataDir,
		DatabaseURL: "postgres://user:pass@example.com:5432/msgvault",
	}}

	owner, err := claimServeOwnership(context.Background(), cfg, "127.0.0.1", 8123, "v-test")
	require.NoError(
		err, "claimServeOwnership")

	t.Cleanup(func() { require.NoError(owner.Close(), "close ownership") })

	sqliteLock, err := tryAcquireWriteOwnerLock(dataDir)
	require.NoError(
		err, "postgres daemon should not hold sqlite write lock")

	require.NoError(
		sqliteLock.Close(), "close sqlite lock")

	records, err := daemonRuntimeStore(dataDir).List()
	require.NoError(
		err, "list runtime records")

	require.Len(records, 1, "runtime record still published")
}

func TestClaimServeOwnershipRejectsSecondPostgreSQLDaemon(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dataDir := t.TempDir()
	cfg := &config.Config{Data: config.DataConfig{
		DataDir:     dataDir,
		DatabaseURL: "postgres://user:pass@example.com:5432/msgvault",
	}}

	owner, err := claimServeOwnership(context.Background(), cfg, "127.0.0.1", 8123, "v-test")
	require.NoError(
		err, "claimServeOwnership")

	t.Cleanup(func() { require.NoError(owner.Close(), "close ownership") })

	second, err := claimServeOwnership(context.Background(), cfg, "127.0.0.1", 8124, "v-test")
	assert.Nil(second, "second owner")
	require.Error(err, "second PostgreSQL daemon should be rejected")
	assert.Contains(err.Error(), "daemon", "error names daemon ownership")
	assert.Contains(err.Error(), "msgvault daemon stop", "error recommends canonical stop command")
	assert.Contains(err.Error(), "msgvault daemon status", "error recommends canonical status command")
}
