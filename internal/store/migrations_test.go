package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestIsMigrationApplied_NotApplied(t *testing.T) {
	f := storetest.New(t)

	applied, err := f.Store.IsMigrationApplied("test_migration")
	require.NoError(t, err, "IsMigrationApplied")
	assert.False(t, applied, "migration should not be applied yet")
}

func TestMarkAndCheckMigrationApplied(t *testing.T) {
	f := storetest.New(t)

	require.NoError(t, f.Store.MarkMigrationApplied("test_migration"), "MarkMigrationApplied")

	applied, err := f.Store.IsMigrationApplied("test_migration")
	require.NoError(t, err, "IsMigrationApplied")
	assert.True(t, applied, "migration should be marked as applied")
}

func TestMarkMigrationApplied_Idempotent(t *testing.T) {
	f := storetest.New(t)

	for range 2 {
		require.NoError(t, f.Store.MarkMigrationApplied("test_migration"), "MarkMigrationApplied")
	}

	applied, err := f.Store.IsMigrationApplied("test_migration")
	require.NoError(t, err, "IsMigrationApplied")
	assert.True(t, applied, "migration should be marked as applied after two calls")
}
