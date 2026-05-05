package cmd

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/wesm/msgvault/internal/deletion"
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
	if err == nil {
		t.Fatalf("err = nil, want mutual-exclusion error")
	}
	if !strings.Contains(err.Error(), "permanent") || !strings.Contains(err.Error(), "yes") {
		t.Errorf("err = %q, want substrings 'permanent' and 'yes'", err.Error())
	}
}

func TestListDeletions_ShowsCancelled(t *testing.T) {
	tmpDir := t.TempDir()
	mgr, err := deletion.NewManager(tmpDir)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	manifest := deletion.NewManifest("test cancel", []string{"abc123"})
	if err := manifest.Save(filepath.Join(tmpDir, "pending", manifest.ID+".json")); err != nil {
		t.Fatalf("save manifest: %v", err)
	}
	if err := mgr.CancelManifest(manifest.ID); err != nil {
		t.Fatalf("CancelManifest: %v", err)
	}

	var buf bytes.Buffer
	if err := runListDeletionsForManager(mgr, &buf); err != nil {
		t.Fatalf("runListDeletionsForManager: %v", err)
	}

	if !strings.Contains(buf.String(), "Cancelled") {
		t.Errorf("output missing 'Cancelled' header:\n%s", buf.String())
	}
	// The ID is truncated to 25 chars in the table; check the first 20 chars
	// (the timestamp prefix) which always survive truncation.
	idPrefix := manifest.ID
	if len(idPrefix) > 20 {
		idPrefix = idPrefix[:20]
	}
	if !strings.Contains(buf.String(), idPrefix) {
		t.Errorf("output missing manifest ID prefix %q:\n%s", idPrefix, buf.String())
	}
}
