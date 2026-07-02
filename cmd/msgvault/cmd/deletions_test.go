package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/deletion"
	"go.kenn.io/msgvault/internal/testutil"
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
	require.Error(t, err, "want mutual-exclusion error")
	assert.Contains(t, err.Error(), "permanent")
	assert.Contains(t, err.Error(), "yes")
}

func TestListDeletions_ShowsCancelled(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tmpDir := t.TempDir()
	mgr, err := deletion.NewManager(tmpDir)
	require.NoError(err, "NewManager")

	manifest := deletion.NewManifest("test cancel", []string{"abc123"})
	require.NoError(manifest.Save(filepath.Join(tmpDir, "pending", manifest.ID+".json")), "save manifest")
	require.NoError(mgr.CancelManifest(manifest.ID), "CancelManifest")

	var buf bytes.Buffer
	require.NoError(runListDeletionsForManager(mgr, &buf), "runListDeletionsForManager")

	assert.Contains(buf.String(), "Cancelled", "output missing 'Cancelled' header")
	// The full batch ID must appear untruncated so it can be fed to
	// show-deletion / delete-staged (F6).
	assert.Contains(buf.String(), manifest.ID, "output missing full manifest ID %q", manifest.ID)
}

func TestListDeletions_JSONEmitsFullIDs(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tmpDir := t.TempDir()
	mgr, err := deletion.NewManager(tmpDir)
	require.NoError(err, "NewManager")

	manifest := deletion.NewManifest("a very long description that would otherwise be truncated in the table", []string{"abc123", "def456"})
	require.NoError(manifest.Save(filepath.Join(tmpDir, "pending", manifest.ID+".json")), "save manifest")

	oldJSON := listDeletionsJSON
	listDeletionsJSON = true
	t.Cleanup(func() { listDeletionsJSON = oldJSON })

	var buf bytes.Buffer
	require.NoError(runListDeletionsForManager(mgr, &buf), "runListDeletionsForManager")

	var out []map[string]any
	require.NoError(json.Unmarshal(buf.Bytes(), &out), "decode JSON output")
	require.Len(out, 1, "one batch")
	assert.Equal(manifest.ID, out[0]["id"], "full id")
	assert.Equal("pending", out[0]["status"], "status")
	count, ok := out[0]["message_count"].(float64)
	require.True(ok, "message_count is a JSON number")
	assert.Equal(2, int(count), "message_count")
}

func TestDeleteStagedFailsFastWhenArchiveOwned(t *testing.T) {
	assert := assert.
		New(t)
	require :=
		require.
			New(t)

	dataDir := t.TempDir()
	withStoreResolverConfig(t, lifecycleTestConfig(dataDir))
	t.Setenv(remoteDeleteEnvVar, "1")

	savedPermanent := deletePermanent
	savedYes := deleteYes
	savedDryRun := deleteDryRun
	savedList := deleteList
	savedAccount := deleteAccount
	deletePermanent = false
	deleteYes = true
	deleteDryRun = false
	deleteList = false
	deleteAccount = ""
	t.Cleanup(func() {
		deletePermanent = savedPermanent
		deleteYes = savedYes
		deleteDryRun = savedDryRun
		deleteList = savedList
		deleteAccount = savedAccount
	})

	mgr, err := deletion.NewManager(filepath.Join(dataDir, "deletions"))
	require.NoError(
		err, "NewManager")

	_, err = mgr.CreateManifest("owned archive", []string{"gmail-1"}, deletion.Filters{})
	require.NoError(
		err, "CreateManifest")

	owner, err := tryAcquireWriteOwnerLock(dataDir)
	require.NoError(
		err, "acquire owner lock")

	t.Cleanup(func() { require.NoError(owner.Close(), "close owner lock") })

	cmd := &cobra.Command{Use: "delete-staged"}
	cmd.SetContext(context.Background())
	err = deleteStagedCmd.RunE(cmd, nil)
	require.Error(err, "delete-staged should fail while the archive is owned")
	assert.Contains(err.Error(), "write operation is in progress")
	assert.Contains(err.Error(), "cannot start")
}

func TestBuildDeleteStagedPlanPinsPlannedBatches(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	dataDir := t.TempDir()
	withStoreResolverConfig(t, lifecycleTestConfig(dataDir))

	mgr, err := deletion.NewManager(filepath.Join(dataDir, "deletions"))
	require.NoError(err, "NewManager")
	first, err := mgr.CreateManifest("first batch", []string{"gmail-1"}, deletion.Filters{Account: "alice@example.com"})
	require.NoError(err, "CreateManifest first")
	second, err := mgr.CreateManifest("second batch", []string{"gmail-2"}, deletion.Filters{Account: "alice@example.com"})
	require.NoError(err, "CreateManifest second")

	plan, err := buildDeleteStagedPlan(deleteStagedPlanOptions{
		RemoteDeleteEnabled: true,
		Yes:                 true,
	})
	require.NoError(err, "build initial plan")
	require.ElementsMatch([]string{first.ID, second.ID}, plan.PlannedBatchIDs, "planned ids")
	require.NotEmpty(plan.PlanFingerprint, "fingerprint")

	newBatch, err := mgr.CreateManifest("new batch", []string{"gmail-3"}, deletion.Filters{Account: "alice@example.com"})
	require.NoError(err, "CreateManifest new")

	pinned, err := buildDeleteStagedPlan(deleteStagedPlanOptions{
		PlannedBatchIDs:     plan.PlannedBatchIDs,
		RemoteDeleteEnabled: true,
		Yes:                 true,
	})
	require.NoError(err, "build pinned plan")
	assert.Equal(plan.PlannedBatchIDs, pinned.PlannedBatchIDs, "pinned plan preserves confirmed batch order")
	assert.NotContains(pinned.PlannedBatchIDs, newBatch.ID, "pinned plan must not include newly staged batches")

	first.GmailIDs = append(first.GmailIDs, "gmail-4")
	require.NoError(mgr.SaveManifest(first), "SaveManifest changed first")
	changed, err := buildDeleteStagedPlan(deleteStagedPlanOptions{
		PlannedBatchIDs:     plan.PlannedBatchIDs,
		RemoteDeleteEnabled: true,
		Yes:                 true,
	})
	require.NoError(err, "build changed plan")
	assert.NotEqual(plan.PlanFingerprint, changed.PlanFingerprint, "fingerprint should change when confirmed manifest content changes")
}

func TestPlanCLIDeleteStagedReportsDeletionScopeEscalation(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	_, restore := seedTokenEnv(t, gmailOnlyTokenJSON)
	defer restore()

	st := testutil.NewTestStore(t)
	_, err := st.GetOrCreateSource(sourceTypeGmail, scopeEscalationAccount)
	require.NoError(err, "GetOrCreateSource")

	mgr, err := deletion.NewManager(filepath.Join(cfg.Data.DataDir, "deletions"))
	require.NoError(err, "NewManager")
	manifest, err := mgr.CreateManifest("permanent batch", []string{"gmail-1"}, deletion.Filters{Account: scopeEscalationAccount})
	require.NoError(err, "CreateManifest")

	got, err := planCLIDeleteStaged(context.Background(), st, api.CLIDeleteStagedPlanRequest{
		Permanent:           true,
		Yes:                 true,
		RemoteDeleteEnabled: true,
	})

	require.NoError(err, "planCLIDeleteStaged")
	assert.True(got.NeedsExecution, "needs execution")
	assert.True(got.NeedsConfirmation, "permanent deletion always needs destructive confirmation")
	assert.Equal([]string{manifest.ID}, got.PlannedBatchIDs, "planned batch ids")
	assert.NotEmpty(got.PlanFingerprint, "plan fingerprint")
	assert.True(got.NeedsScopeEscalation, "gmail-only token should require deletion scope escalation")
	assert.Equal("PERMISSION UPGRADE REQUIRED", got.ScopeEscalationHeadline, "scope headline")
	assert.Contains(got.ScopeEscalationBodyLines, "Batch deletion requires elevated Gmail permissions.", "scope body")
}

func TestPlanCLIDeleteStagedEscalatesLegacyGmailTokenForPermanentDelete(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	_, restore := seedTokenEnv(t, legacyTokenJSON)
	defer restore()

	st := testutil.NewTestStore(t)
	_, err := st.GetOrCreateSource(sourceTypeGmail, scopeEscalationAccount)
	require.NoError(err, "GetOrCreateSource")

	mgr, err := deletion.NewManager(filepath.Join(cfg.Data.DataDir, "deletions"))
	require.NoError(err, "NewManager")
	_, err = mgr.CreateManifest("legacy token batch", []string{"gmail-1"}, deletion.Filters{Account: scopeEscalationAccount})
	require.NoError(err, "CreateManifest")

	got, err := planCLIDeleteStaged(context.Background(), st, api.CLIDeleteStagedPlanRequest{
		Permanent:           true,
		Yes:                 true,
		RemoteDeleteEnabled: true,
	})

	require.NoError(err, "planCLIDeleteStaged")
	assert.True(got.NeedsScopeEscalation, "legacy token must require foreground deletion scope escalation")
	assert.Equal("PERMISSION UPGRADE REQUIRED", got.ScopeEscalationHeadline, "scope headline")
}
