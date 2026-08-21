package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/deletion"
	"go.kenn.io/msgvault/internal/store"
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

func TestBuildDeleteStagedPlanFiltersVersionTwoBySourceID(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	dataDir := t.TempDir()
	withStoreResolverConfig(t, lifecycleTestConfig(dataDir))
	mgr, err := deletion.NewManager(filepath.Join(dataDir, "deletions"))
	require.NoError(err)
	first := deletion.NewManifestForSource("first", []string{"gm-1"}, deletion.SourceReference{
		ID: 11, Type: "gmail", Identifier: "first@example.invalid",
	})
	second := deletion.NewManifestForSource("second", []string{"gm-2"}, deletion.SourceReference{
		ID: 22, Type: "gmail", Identifier: "second@example.invalid",
	})
	require.NoError(mgr.SaveManifest(first))
	require.NoError(mgr.SaveManifest(second))

	plan, err := buildDeleteStagedPlan(deleteStagedPlanOptions{
		SourceID: 22, SourceIDSet: true, List: true,
		ResolvedSourceType: "gmail", ResolvedSourceIdentifier: "second@example.invalid",
	})
	require.NoError(err)
	assert.Equal([]string{second.ID}, plan.PlannedBatchIDs)
	assert.NotContains(plan.Stdout, first.Description)
	assert.Contains(plan.Stdout, second.Description)
}

func TestBuildDeleteStagedPlanDoesNotSelectVersionTwoByLegacyFilterAccount(t *testing.T) {
	dataDir := t.TempDir()
	withStoreResolverConfig(t, lifecycleTestConfig(dataDir))
	mgr, err := deletion.NewManager(filepath.Join(dataDir, "deletions"))
	require.NoError(t, err)
	manifest := deletion.NewManifestForSource("durable source", []string{"gm-1"}, deletion.SourceReference{
		ID: 11, Type: "gmail", Identifier: "source@example.invalid",
	})
	manifest.Filters.Account = "other@example.invalid"
	require.NoError(t, mgr.SaveManifest(manifest))

	_, err = buildDeleteStagedPlan(deleteStagedPlanOptions{
		BatchID: manifest.ID, Account: "other@example.invalid", List: true,
	})
	require.ErrorContains(t, err, "does not match the requested source")
}

func TestBuildDeleteStagedPlanAllowsExplicitSelectorForUnboundLegacyManifest(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	dataDir := t.TempDir()
	withStoreResolverConfig(t, lifecycleTestConfig(dataDir))
	mgr, err := deletion.NewManager(filepath.Join(dataDir, "deletions"))
	require.NoError(err)
	manifest, err := mgr.CreateManifest("legacy", []string{"gm-1"}, deletion.Filters{})
	require.NoError(err)

	plan, err := buildDeleteStagedPlan(deleteStagedPlanOptions{
		Account: "source@example.invalid", List: true,
	})
	require.NoError(err)
	assert.Equal([]string{manifest.ID}, plan.PlannedBatchIDs)
}

func TestBuildDeleteStagedPlanAllowsSourceIDForLegacyManifestWithMatchingAccount(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	dataDir := t.TempDir()
	withStoreResolverConfig(t, lifecycleTestConfig(dataDir))
	mgr, err := deletion.NewManager(filepath.Join(dataDir, "deletions"))
	require.NoError(err)
	manifest, err := mgr.CreateManifest("legacy", []string{"gm-1"}, deletion.Filters{
		Account: "source@example.invalid",
	})
	require.NoError(err)
	other, err := mgr.CreateManifest("other legacy", []string{"gm-2"}, deletion.Filters{
		Account: "other@example.invalid",
	})
	require.NoError(err)

	plan, err := buildDeleteStagedPlan(deleteStagedPlanOptions{
		SourceID: 11, SourceIDSet: true, List: true,
		ResolvedSourceType: "gmail", ResolvedSourceIdentifier: "source@example.invalid",
	})
	require.NoError(err)
	assert.Equal([]string{manifest.ID}, plan.PlannedBatchIDs)
	assert.NotContains(plan.Stdout, other.Description)
}

func TestBuildDeleteStagedPlanInspectsUnboundLegacyManifestMixedWithVersionTwo(t *testing.T) {
	tests := []struct {
		name string
		opts deleteStagedPlanOptions
		want string
	}{
		{name: "list", opts: deleteStagedPlanOptions{List: true}, want: "Staged deletions: 2 batch(es)"},
		{name: "dry run", opts: deleteStagedPlanOptions{DryRun: true}, want: "Dry run - no messages will be deleted."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			dataDir := t.TempDir()
			withStoreResolverConfig(t, lifecycleTestConfig(dataDir))
			mgr, err := deletion.NewManager(filepath.Join(dataDir, "deletions"))
			require.NoError(err)
			legacy, err := mgr.CreateManifest("legacy", []string{"gm-1"}, deletion.Filters{})
			require.NoError(err)
			bound := deletion.NewManifestForSource("bound", []string{"gm-2"}, deletion.SourceReference{
				ID: 11, Type: "gmail", Identifier: "source@example.invalid",
			})
			require.NoError(mgr.SaveManifest(bound))

			plan, err := buildDeleteStagedPlan(tt.opts)
			require.NoError(err)
			assert.ElementsMatch([]string{legacy.ID, bound.ID}, plan.PlannedBatchIDs)
			assert.Contains(plan.Stdout, tt.want)
			assert.False(plan.NeedsExecution)
		})
	}
}

func TestBuildDeleteStagedPlanRejectsUnboundLegacyManifestMixedWithVersionTwoDuringExecution(t *testing.T) {
	require := require.New(t)
	dataDir := t.TempDir()
	withStoreResolverConfig(t, lifecycleTestConfig(dataDir))
	mgr, err := deletion.NewManager(filepath.Join(dataDir, "deletions"))
	require.NoError(err)
	_, err = mgr.CreateManifest("legacy", []string{"gm-1"}, deletion.Filters{})
	require.NoError(err)
	require.NoError(mgr.SaveManifest(deletion.NewManifestForSource("bound", []string{"gm-2"}, deletion.SourceReference{
		ID: 11, Type: "gmail", Identifier: "source@example.invalid",
	})))

	_, err = buildDeleteStagedPlan(deleteStagedPlanOptions{RemoteDeleteEnabled: true, Yes: true})
	require.ErrorContains(err, "legacy deletion manifest")
}

func TestResolveDeleteStagedTargetUsesStableTupleAfterSourceIDChanges(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "source@example.invalid")
	require.NoError(t, err)
	manifest := deletion.NewManifestForSource("stale snapshot", []string{"gm-1"}, deletion.SourceReference{
		ID: source.ID + 1000, Type: source.SourceType, Identifier: source.Identifier,
	})

	target, err := resolveDeleteStagedTargetWithSourceID(
		st, []*deletion.Manifest{manifest}, "", source.ID, true,
	)
	require.NoError(t, err)
	assert.Equal(t, source.ID, target.Source.ID)
}

func TestResolveDeleteStagedTargetRejectsSnapshotIDThatNowNamesAnotherSource(t *testing.T) {
	st := testutil.NewTestStore(t)
	intended, err := st.GetOrCreateSource("gmail", "intended@example.invalid")
	require.NoError(t, err)
	other, err := st.GetOrCreateSource("imap", "other@example.invalid")
	require.NoError(t, err)
	manifest := deletion.NewManifestForSource("mismatched snapshot", []string{"gm-1"}, deletion.SourceReference{
		ID: other.ID, Type: intended.SourceType, Identifier: intended.Identifier,
	})

	_, err = resolveDeleteStagedTargetWithSourceID(
		st, []*deletion.Manifest{manifest}, "", other.ID, true,
	)
	require.ErrorContains(t, err, "does not match manifest source")
}

func TestResolveDeleteStagedTargetRejectsRequestedAccountOutsideDurableTuple(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "source@example.invalid")
	require.NoError(t, err)
	_, err = st.GetOrCreateSource("gmail", "other@example.invalid")
	require.NoError(t, err)
	manifest := deletion.NewManifestForSource("bound", []string{"gm-1"}, deletion.SourceReference{
		ID: source.ID, Type: source.SourceType, Identifier: source.Identifier,
	})

	_, err = resolveDeleteStagedTarget(st, []*deletion.Manifest{manifest}, "other@example.invalid")
	require.ErrorContains(t, err, "does not match manifest source")
}

func TestDeleteStagedRejectsUnsupportedSourceBeforeClaim(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dataDir := t.TempDir()
	withStoreResolverConfig(t, lifecycleTestConfig(dataDir))
	t.Setenv(remoteDeleteEnvVar, "1")
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	resetDeleteStagedRoutingGlobals(t)

	st, err := store.Open(cfg.DatabaseDSN())
	require.NoError(err)
	require.NoError(st.InitSchema())
	source, err := st.GetOrCreateSource("slack", "workspace.example.invalid")
	require.NoError(err)
	require.NoError(st.Close())

	mgr, err := deletion.NewManager(filepath.Join(dataDir, "deletions"))
	require.NoError(err)
	manifest := deletion.NewManifestForSource("unsupported source", []string{"remote-1"}, deletion.SourceReference{
		ID: source.ID, Type: source.SourceType, Identifier: source.Identifier,
	})
	require.NoError(mgr.SaveManifest(manifest))

	cmd := newDeleteStagedRoutingTestCommand()
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"--yes", manifest.ID})
	err = cmd.Execute()
	require.ErrorContains(err, "not a gmail or imap source")
	assert.FileExists(filepath.Join(mgr.PendingDir(), manifest.ID+".json"))
	assert.NoFileExists(filepath.Join(mgr.InProgressDir(), manifest.ID+".json"))
}

func TestDeleteStagedOAuthSetupFailureLeavesManifestPending(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dataDir := t.TempDir()
	withStoreResolverConfig(t, lifecycleTestConfig(dataDir))
	t.Setenv(remoteDeleteEnvVar, "1")
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	resetDeleteStagedRoutingGlobals(t)

	st, err := store.Open(cfg.DatabaseDSN())
	require.NoError(err)
	require.NoError(st.InitSchema())
	source, err := st.GetOrCreateSource("gmail", "account@example.invalid")
	require.NoError(err)
	require.NoError(st.Close())

	mgr, err := deletion.NewManager(filepath.Join(dataDir, "deletions"))
	require.NoError(err)
	manifest := deletion.NewManifestForSource("missing OAuth setup", []string{"remote-1"}, deletion.SourceReference{
		ID: source.ID, Type: source.SourceType, Identifier: source.Identifier,
	})
	require.NoError(mgr.SaveManifest(manifest))

	cmd := newDeleteStagedRoutingTestCommand()
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"--yes", manifest.ID})
	err = cmd.Execute()
	require.ErrorContains(err, "OAuth client secrets not configured")
	assert.FileExists(filepath.Join(mgr.PendingDir(), manifest.ID+".json"))
	assert.NoFileExists(filepath.Join(mgr.InProgressDir(), manifest.ID+".json"))
}

func TestRunDeleteStagedExecutionRefreshesAfterPartialRunFailure(t *testing.T) {
	claimErr := errors.New("claim second batch")
	refreshCalls := 0

	err := runDeleteStagedExecution(func(markCacheDirty func()) error {
		markCacheDirty()
		return claimErr
	}, func() error {
		refreshCalls++
		return nil
	})

	require.ErrorIs(t, err, claimErr)
	assert.Equal(t, 1, refreshCalls)
}

func TestRunDeleteStagedExecutionSkipsRefreshBeforeExecution(t *testing.T) {
	claimErr := errors.New("claim first batch")
	refreshCalls := 0

	err := runDeleteStagedExecution(func(func()) error {
		return claimErr
	}, func() error {
		refreshCalls++
		return nil
	})

	require.ErrorIs(t, err, claimErr)
	assert.Zero(t, refreshCalls)
}

func TestBuildDeleteStagedPlanInspectsMultipleVersionTwoSources(t *testing.T) {
	tests := []struct {
		name string
		opts deleteStagedPlanOptions
		want string
	}{
		{name: "list", opts: deleteStagedPlanOptions{List: true}, want: "Staged deletions: 2 batch(es)"},
		{name: "dry run", opts: deleteStagedPlanOptions{DryRun: true}, want: "Dry run - no messages will be deleted."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			dataDir := t.TempDir()
			withStoreResolverConfig(t, lifecycleTestConfig(dataDir))
			mgr, err := deletion.NewManager(filepath.Join(dataDir, "deletions"))
			require.NoError(err)
			first := deletion.NewManifestForSource("first", []string{"gm-1"}, deletion.SourceReference{
				ID: 11, Type: "gmail", Identifier: "first@example.invalid",
			})
			second := deletion.NewManifestForSource("second", []string{"gm-2"}, deletion.SourceReference{
				ID: 22, Type: "gmail", Identifier: "second@example.invalid",
			})
			require.NoError(mgr.SaveManifest(first))
			require.NoError(mgr.SaveManifest(second))

			plan, err := buildDeleteStagedPlan(tt.opts)
			require.NoError(err)
			assert.ElementsMatch([]string{first.ID, second.ID}, plan.PlannedBatchIDs)
			assert.Contains(plan.Stdout, tt.want)
			assert.False(plan.NeedsExecution)
		})
	}
}

func TestBuildDeleteStagedPlanRejectsMultipleVersionTwoSourcesDuringExecution(t *testing.T) {
	require := require.New(t)
	dataDir := t.TempDir()
	withStoreResolverConfig(t, lifecycleTestConfig(dataDir))
	mgr, err := deletion.NewManager(filepath.Join(dataDir, "deletions"))
	require.NoError(err)
	require.NoError(mgr.SaveManifest(deletion.NewManifestForSource("first", []string{"gm-1"}, deletion.SourceReference{
		ID: 11, Type: "gmail", Identifier: "first@example.invalid",
	})))
	require.NoError(mgr.SaveManifest(deletion.NewManifestForSource("second", []string{"gm-2"}, deletion.SourceReference{
		ID: 22, Type: "gmail", Identifier: "second@example.invalid",
	})))

	_, err = buildDeleteStagedPlan(deleteStagedPlanOptions{RemoteDeleteEnabled: true, Yes: true})
	require.ErrorContains(err, "multiple sources")
}

func TestDeleteStagedFingerprintIncludesSourceReference(t *testing.T) {
	manifest := deletion.NewManifestForSource("confirmed", []string{"gm-1"}, deletion.SourceReference{
		ID: 11, Type: "gmail", Identifier: "first@example.invalid",
	})
	before := fingerprintDeleteStagedPlan([]*deletion.Manifest{manifest})
	manifest.Source.Identifier = "changed@example.invalid"
	after := fingerprintDeleteStagedPlan([]*deletion.Manifest{manifest})
	assert.NotEqual(t, before, after)
}

// TestBuildDeleteStagedPlanRejectsMethodFlagMismatch verifies that resuming an
// in-progress batch with a --permanent flag that disagrees with the method it
// was started with is refused during planning. The summary, confirmation
// prompt, and OAuth scopes are all derived from the flag, so continuing would
// take a trash confirmation for an unrecoverable deletion (or the reverse).
func TestBuildDeleteStagedPlanRejectsMethodFlagMismatch(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	dataDir := t.TempDir()
	withStoreResolverConfig(t, lifecycleTestConfig(dataDir))

	mgr, err := deletion.NewManager(filepath.Join(dataDir, "deletions"))
	require.NoError(err, "NewManager")
	batch, err := mgr.CreateManifest("resumed batch", []string{"gmail-1"}, deletion.Filters{Account: "alice@example.com"})
	require.NoError(err, "CreateManifest")

	// A batch already claimed for permanent deletion.
	claimed, err := mgr.ClaimManifest(batch.ID, deletion.MethodDelete)
	require.NoError(err, "ClaimManifest")
	require.Equal(deletion.MethodDelete, claimed.Execution.Method)

	_, err = buildDeleteStagedPlan(deleteStagedPlanOptions{RemoteDeleteEnabled: true, Yes: true})
	require.Error(err, "trash-flag resume of a permanent batch must be refused")
	assert.Contains(err.Error(), "must be resumed with it")
	assert.Contains(err.Error(), "--permanent")

	// The same batch plans cleanly once the flag matches the stored method.
	plan, err := buildDeleteStagedPlan(deleteStagedPlanOptions{
		RemoteDeleteEnabled: true, Yes: true, Permanent: true,
	})
	require.NoError(err, "matching flag plans cleanly")
	assert.Contains(plan.Stdout, "PERMANENT DELETE", "summary describes the confirmed method")
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
	assert.Equal(scopeEscalationAccount, got.ScopeEscalationAccount,
		"plan names the account so the frontend can authorize client-side")
	assert.Empty(got.ScopeEscalationOAuthApp, "default app binding")
}

func TestPlanCLIDeleteStagedResolvesDisplayNameBeforeFiltering(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	dataDir := t.TempDir()
	withStoreResolverConfig(t, lifecycleTestConfig(dataDir))
	st := testutil.NewTestStore(t)
	gmailSource, err := st.GetOrCreateSource(sourceTypeGmail, "source@example.invalid")
	require.NoError(err)
	source, err := st.GetOrCreateSource(sourceTypeIMAP, "source@example.invalid")
	require.NoError(err)
	require.NoError(st.UpdateSourceDisplayName(source.ID, "Work"))

	mgr, err := deletion.NewManager(filepath.Join(dataDir, "deletions"))
	require.NoError(err)
	manifest := deletion.NewManifestForSource("display name target", []string{"remote-1"}, deletion.SourceReference{
		ID: source.ID, Type: source.SourceType, Identifier: source.Identifier,
	})
	require.NoError(mgr.SaveManifest(manifest))
	gmailManifest := deletion.NewManifestForSource("other source type", []string{"remote-2"}, deletion.SourceReference{
		ID: gmailSource.ID, Type: gmailSource.SourceType, Identifier: gmailSource.Identifier,
	})
	require.NoError(mgr.SaveManifest(gmailManifest))

	got, err := planCLIDeleteStaged(context.Background(), st, api.CLIDeleteStagedPlanRequest{
		Account: "Work", Yes: true, RemoteDeleteEnabled: true,
	})
	require.NoError(err)
	assert.Equal([]string{manifest.ID}, got.PlannedBatchIDs)
	require.NotNil(got.ResolvedSourceID)
	assert.Equal(source.ID, *got.ResolvedSourceID)
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
