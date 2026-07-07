package cmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/deletion"
)

func TestStoreAPIAdapterDeletionManifests(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	savedCfg := cfg
	t.Cleanup(func() { cfg = savedCfg })
	cfg = &config.Config{Data: config.DataConfig{DataDir: t.TempDir()}}

	adapter := &storeAPIAdapter{}
	var _ api.DeletionManifestLister = adapter
	var _ api.DeletionManifestCanceller = adapter

	ctx := context.Background()

	// Save through the existing saver path.
	m := deletion.NewManifest("adapter test", []string{"gm-1"})
	m.CreatedBy = "api"
	require.NoError(adapter.SaveCLIDeletionManifest(ctx, m), "save")

	// List all and by status.
	all, err := adapter.ListDeletionManifests(ctx, "")
	require.NoError(err, "list all")
	require.Len(all, 1)
	assert.Equal(m.ID, all[0].ID)

	pending, err := adapter.ListDeletionManifests(ctx, deletion.StatusPending)
	require.NoError(err, "list pending")
	require.Len(pending, 1)

	// Get with status, cancel, verify.
	_, status, err := adapter.GetDeletionManifest(ctx, m.ID)
	require.NoError(err, "get")
	assert.Equal(deletion.StatusPending, status)

	require.NoError(adapter.CancelDeletionManifest(ctx, m.ID), "cancel")
	_, status, err = adapter.GetDeletionManifest(ctx, m.ID)
	require.NoError(err, "get after cancel")
	assert.Equal(deletion.StatusCancelled, status)

	cancelled, err := adapter.ListDeletionManifests(ctx, deletion.StatusCancelled)
	require.NoError(err, "list cancelled")
	assert.Len(cancelled, 1)
}
