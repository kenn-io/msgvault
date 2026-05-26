package cmd

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/deletion"
)

func TestDeleteStaged_PermanentAndYesMutuallyExclusive(t *testing.T) {
	cmd := &cobra.Command{
		Use:  "delete-staged",
		RunE: func(cmd *cobra.Command, args []string) error { return nil },
	}
	var permanent, yes bool
	cmd.Flags().BoolVar(&permanent, "permanent", false, "")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "")
	cmd.MarkFlagsMutuallyExclusive("permanent", "yes")
	cmd.SetArgs([]string{"--permanent", "--yes"})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	err := cmd.Execute()
	requirepkg.Error(t, err, "want mutual-exclusion error")
	assertpkg.Contains(t, err.Error(), "permanent")
	assertpkg.Contains(t, err.Error(), "yes")
}

func TestListDeletions_ShowsCancelled(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	tmpDir := t.TempDir()
	mgr, err := deletion.NewManager(tmpDir)
	require.NoError(err, "NewManager")

	manifest := deletion.NewManifest("test cancel", []string{"abc123"})
	require.NoError(manifest.Save(filepath.Join(tmpDir, "pending", manifest.ID+".json")), "save manifest")
	require.NoError(mgr.CancelManifest(manifest.ID), "CancelManifest")

	var buf bytes.Buffer
	require.NoError(runListDeletionsForManager(mgr, &buf), "runListDeletionsForManager")

	assert.Contains(buf.String(), "Cancelled", "output missing 'Cancelled' header")
	// The ID is truncated to 25 chars in the table; check the first 20 chars
	// (the timestamp prefix) which always survive truncation.
	idPrefix := manifest.ID
	if len(idPrefix) > 20 {
		idPrefix = idPrefix[:20]
	}
	assert.Contains(buf.String(), idPrefix, "output missing manifest ID prefix %q", idPrefix)
}
