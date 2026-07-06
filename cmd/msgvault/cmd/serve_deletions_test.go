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
	require.NoError(t, adapter.SaveCLIDeletionManifest(ctx, m), "save")

	// List all and by status.
	all, err := adapter.ListDeletionManifests(ctx, "")
	require.NoError(t, err, "list all")
	require.Len(t, all, 1)
	assert.Equal(t, m.ID, all[0].ID)

	pending, err := adapter.ListDeletionManifests(ctx, deletion.StatusPending)
	require.NoError(t, err, "list pending")
	require.Len(t, pending, 1)

	// Get with status, cancel, verify.
	_, status, err := adapter.GetDeletionManifest(ctx, m.ID)
	require.NoError(t, err, "get")
	assert.Equal(t, deletion.StatusPending, status)

	require.NoError(t, adapter.CancelDeletionManifest(ctx, m.ID), "cancel")
	_, status, err = adapter.GetDeletionManifest(ctx, m.ID)
	require.NoError(t, err, "get after cancel")
	assert.Equal(t, deletion.StatusCancelled, status)

	cancelled, err := adapter.ListDeletionManifests(ctx, deletion.StatusCancelled)
	require.NoError(t, err, "list cancelled")
	assert.Len(t, cancelled, 1)
}
