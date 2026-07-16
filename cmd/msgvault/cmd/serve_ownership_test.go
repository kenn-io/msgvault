package cmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

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
