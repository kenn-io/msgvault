package cmd

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeletionManifestCommandsUseDaemonRunner(t *testing.T) {
	savedCancelAll := cancelAll
	t.Cleanup(func() {
		cancelAll = savedCancelAll
	})

	tests := []struct {
		name   string
		cmd    func() *cobra.Command
		args   []string
		want   []string
		stdout string
	}{
		{
			name: "list",
			cmd: func() *cobra.Command {
				return &cobra.Command{
					Use:  "list-deletions",
					RunE: runListDeletions,
				}
			},
			want:   []string{"list-deletions"},
			stdout: "No deletion batches found.\n",
		},
		{
			name: "show",
			cmd: func() *cobra.Command {
				return &cobra.Command{
					Use:  "show-deletion <batch-id>",
					Args: cobra.ExactArgs(1),
					RunE: runShowDeletion,
				}
			},
			args:   []string{"batch-123"},
			want:   []string{"show-deletion", "batch-123"},
			stdout: "Deletion batch: batch-123\n",
		},
		{
			name:   "cancel-all",
			cmd:    newCancelDeletionRoutingTestCommand,
			args:   []string{"--all"},
			want:   []string{"cancel-deletion", "--all"},
			stdout: "Cancelled 2 batch(es).\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requestAssert := assert.New(t)
			requestRequire := require.New(t)
			stdoutJSON, err := json.Marshal(tt.stdout)
			requestRequire.NoError(err, "marshal stdout event")
			server, requests := newDaemonCLIRunnerTestServer(t, func(req daemonCLIRunTestRequest) {
				requestAssert.Equal(tt.want, req.Args, "args")
			}, `{"type":"stdout","data":`+string(stdoutJSON)+`}`, `{"type":"complete"}`)
			configureRemoteDaemonForTest(t, server.URL)

			var stdout bytes.Buffer
			cmd := tt.cmd()
			cmd.SetOut(&stdout)
			cmd.SetArgs(tt.args)

			requestRequire.NoError(cmd.Execute(), tt.name)
			requestAssert.Equal(1, int(requests.Load()), "runner endpoint calls")
			requestAssert.Equal(tt.stdout, stdout.String(), "stdout")
		})
	}
}

func TestDeleteStagedTrashPromptsBeforeDaemonRunner(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	resetDeleteStagedRoutingGlobals(t)
	t.Setenv(remoteDeleteEnvVar, "1")

	server, runRequests, planRequests := newDaemonCLIDeleteStagedTestServer(t, func(req daemonCLIDeleteStagedPlanTestRequest) {
		assert.Equal("batch-123", req.BatchID, "batch id")
		assert.False(req.Permanent, "permanent")
		assert.False(req.Yes, "yes")
		assert.False(req.DryRun, "dry run")
		assert.False(req.List, "list")
		assert.Equal("alice@example.com", req.Account, "account")
		assert.True(req.RemoteDeleteEnabled, "remote delete enabled")
	}, map[string]any{
		"stdout":                "Deletion Summary:\n  Batches:  1\n  Messages: 2\n  Method:   trash (30-day recovery)\n\n",
		"needs_execution":       true,
		"needs_confirmation":    true,
		"confirmation_mode":     "trash",
		"planned_batch_ids":     []string{"batch-123"},
		"plan_fingerprint":      "fp-trash",
		"remote_delete_env_var": remoteDeleteEnvVar,
	}, func(req daemonCLIRunTestRequest) {
		assert.Equal([]string{
			"delete-staged",
			"--account=alice@example.com",
			"--confirmed",
			"--plan-fingerprint=fp-trash",
			"--planned-batch=batch-123",
			"--skip-prelude",
		}, req.Args, "args")
		assert.Equal(map[string]string{remoteDeleteEnvVar: "1"}, req.Env, "env")
	}, `{"type":"stdout","data":"Deletion complete!\n"}`, `{"type":"complete"}`)
	configureRemoteDaemonForTest(t, server.URL)

	cmd := newDeleteStagedRoutingTestCommand()
	var stdout bytes.Buffer
	cmd.SetIn(bytes.NewBufferString("y\n"))
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"--account", "alice@example.com", "batch-123"})

	require.NoError(cmd.Execute(), "delete-staged")
	assert.Equal(1, int(planRequests.Load()), "plan endpoint calls")
	assert.Equal(1, int(runRequests.Load()), "runner endpoint calls")
	assert.Contains(stdout.String(), "Deletion Summary:", "plan summary")
	assert.Contains(stdout.String(), "Proceed with deletion?", "frontend prompt")
	assert.Contains(stdout.String(), "Deletion complete!", "daemon output")
}

func TestDeleteStagedPermanentPromptsBeforeDaemonRunner(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	resetDeleteStagedRoutingGlobals(t)
	t.Setenv(remoteDeleteEnvVar, "1")

	server, runRequests, planRequests := newDaemonCLIDeleteStagedTestServer(t, func(req daemonCLIDeleteStagedPlanTestRequest) {
		assert.Equal("batch-123", req.BatchID, "batch id")
		assert.True(req.Permanent, "permanent")
		assert.False(req.Yes, "yes")
		assert.True(req.RemoteDeleteEnabled, "remote delete enabled")
	}, map[string]any{
		"stdout":                "Deletion Summary:\n  Batches:  1\n  Messages: 2\n  Method:   PERMANENT DELETE (fast, no recovery)\n\n",
		"needs_execution":       true,
		"needs_confirmation":    true,
		"confirmation_mode":     "permanent",
		"planned_batch_ids":     []string{"batch-123"},
		"plan_fingerprint":      "fp-permanent",
		"remote_delete_env_var": remoteDeleteEnvVar,
	}, func(req daemonCLIRunTestRequest) {
		assert.Equal([]string{
			"delete-staged",
			"--confirmed",
			"--permanent",
			"--plan-fingerprint=fp-permanent",
			"--planned-batch=batch-123",
			"--skip-prelude",
		}, req.Args, "args")
		assert.Equal(map[string]string{remoteDeleteEnvVar: "1"}, req.Env, "env")
	}, `{"type":"stdout","data":"Deletion complete!\n"}`, `{"type":"complete"}`)
	configureRemoteDaemonForTest(t, server.URL)

	cmd := newDeleteStagedRoutingTestCommand()
	var stdout bytes.Buffer
	cmd.SetIn(bytes.NewBufferString("delete\n"))
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"--permanent", "batch-123"})

	require.NoError(cmd.Execute(), "delete-staged")
	assert.Equal(1, int(planRequests.Load()), "plan endpoint calls")
	assert.Equal(1, int(runRequests.Load()), "runner endpoint calls")
	assert.Contains(stdout.String(), "PERMANENT DELETE", "plan summary")
	assert.Contains(stdout.String(), `Type "delete" to confirm permanent deletion`, "frontend prompt")
	assert.Contains(stdout.String(), "Deletion complete!", "daemon output")
}

func TestDeleteStagedWithoutBatchPinsPlannedBatchesForDaemonRunner(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	resetDeleteStagedRoutingGlobals(t)
	t.Setenv(remoteDeleteEnvVar, "1")

	server, runRequests, planRequests := newDaemonCLIDeleteStagedTestServer(t, func(req daemonCLIDeleteStagedPlanTestRequest) {
		assert.Empty(req.BatchID, "batch id")
		assert.True(req.Yes, "yes")
		assert.True(req.RemoteDeleteEnabled, "remote delete enabled")
	}, map[string]any{
		"stdout":                "Deletion Summary:\n  Batches:  2\n  Messages: 4\n  Method:   trash (30-day recovery)\n\n",
		"needs_execution":       true,
		"needs_confirmation":    false,
		"planned_batch_ids":     []string{"batch-a", "batch-b"},
		"plan_fingerprint":      "fp-two-batches",
		"remote_delete_env_var": remoteDeleteEnvVar,
	}, func(req daemonCLIRunTestRequest) {
		assert.Equal([]string{
			"delete-staged",
			"--confirmed",
			"--plan-fingerprint=fp-two-batches",
			"--planned-batch=batch-a",
			"--planned-batch=batch-b",
			"--skip-prelude",
			"--yes",
		}, req.Args, "args")
		assert.Equal(map[string]string{remoteDeleteEnvVar: "1"}, req.Env, "env")
	}, `{"type":"stdout","data":"Deletion complete!\n"}`, `{"type":"complete"}`)
	configureRemoteDaemonForTest(t, server.URL)

	cmd := newDeleteStagedRoutingTestCommand()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"--yes"})

	require.NoError(cmd.Execute(), "delete-staged")
	assert.Equal(1, int(planRequests.Load()), "plan endpoint calls")
	assert.Equal(1, int(runRequests.Load()), "runner endpoint calls")
	assert.Contains(stdout.String(), "Batches:  2", "plan summary")
	assert.Contains(stdout.String(), "Deletion complete!", "daemon output")
}

func TestDeleteStagedScopeEscalationPromptsBeforeDaemonRunner(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	resetDeleteStagedRoutingGlobals(t)
	t.Setenv(remoteDeleteEnvVar, "1")

	server, runRequests, planRequests := newDaemonCLIDeleteStagedTestServer(t, func(req daemonCLIDeleteStagedPlanTestRequest) {
		assert.Equal("batch-123", req.BatchID, "batch id")
		assert.True(req.Permanent, "permanent")
		assert.True(req.RemoteDeleteEnabled, "remote delete enabled")
	}, map[string]any{
		"stdout":                       "Deletion Summary:\n  Batches:  1\n  Messages: 2\n  Method:   PERMANENT DELETE (fast, no recovery)\n\n",
		"needs_execution":              true,
		"needs_confirmation":           false,
		"planned_batch_ids":            []string{"batch-123"},
		"plan_fingerprint":             "fp-scope",
		"needs_scope_escalation":       true,
		"scope_escalation_headline":    "PERMISSION UPGRADE REQUIRED",
		"scope_escalation_body_lines":  []string{"Batch deletion requires elevated Gmail permissions."},
		"scope_escalation_cancel_hint": "Cancelled. Drop --permanent to use trash deletion without elevated permissions.",
		"remote_delete_env_var":        remoteDeleteEnvVar,
	}, func(req daemonCLIRunTestRequest) {
		assert.Equal([]string{
			"delete-staged",
			"--confirmed",
			"--permanent",
			"--plan-fingerprint=fp-scope",
			"--planned-batch=batch-123",
			"--scope-escalation-confirmed",
			"--skip-prelude",
		}, req.Args, "args")
		assert.Equal(map[string]string{remoteDeleteEnvVar: "1"}, req.Env, "env")
	}, `{"type":"stdout","data":"Deletion complete!\n"}`, `{"type":"complete"}`)
	configureRemoteDaemonForTest(t, server.URL)

	cmd := newDeleteStagedRoutingTestCommand()
	var stdout bytes.Buffer
	cmd.SetIn(bytes.NewBufferString("y\n"))
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"--permanent", "batch-123"})

	require.NoError(cmd.Execute(), "delete-staged")
	assert.Equal(1, int(planRequests.Load()), "plan endpoint calls")
	assert.Equal(1, int(runRequests.Load()), "runner endpoint calls")
	assert.Contains(stdout.String(), "PERMISSION UPGRADE REQUIRED", "frontend scope prompt")
	assert.Contains(stdout.String(), "Deletion complete!", "daemon output")
}

func TestDeleteStagedConfirmationAndScopePromptsShareInput(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	resetDeleteStagedRoutingGlobals(t)
	t.Setenv(remoteDeleteEnvVar, "1")

	server, runRequests, planRequests := newDaemonCLIDeleteStagedTestServer(t, nil, map[string]any{
		"stdout":                       "Deletion Summary:\n  Batches:  1\n  Messages: 2\n  Method:   PERMANENT DELETE (fast, no recovery)\n\n",
		"needs_execution":              true,
		"needs_confirmation":           true,
		"confirmation_mode":            "permanent",
		"planned_batch_ids":            []string{"batch-123"},
		"plan_fingerprint":             "fp-both-prompts",
		"needs_scope_escalation":       true,
		"scope_escalation_headline":    "PERMISSION UPGRADE REQUIRED",
		"scope_escalation_body_lines":  []string{"Batch deletion requires elevated Gmail permissions."},
		"scope_escalation_cancel_hint": "Cancelled. Drop --permanent to use trash deletion without elevated permissions.",
		"remote_delete_env_var":        remoteDeleteEnvVar,
	}, func(req daemonCLIRunTestRequest) {
		assert.Equal([]string{
			"delete-staged",
			"--confirmed",
			"--permanent",
			"--plan-fingerprint=fp-both-prompts",
			"--planned-batch=batch-123",
			"--scope-escalation-confirmed",
			"--skip-prelude",
		}, req.Args, "args")
	}, `{"type":"stdout","data":"Deletion complete!\n"}`, `{"type":"complete"}`)
	configureRemoteDaemonForTest(t, server.URL)

	cmd := newDeleteStagedRoutingTestCommand()
	var stdout bytes.Buffer
	cmd.SetIn(bytes.NewBufferString("delete\ny\n"))
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"--permanent", "batch-123"})

	require.NoError(cmd.Execute(), "delete-staged")
	assert.Equal(1, int(planRequests.Load()), "plan endpoint calls")
	assert.Equal(1, int(runRequests.Load()), "runner endpoint calls")
	assert.Contains(stdout.String(), `Type "delete" to confirm permanent deletion`, "deletion prompt")
	assert.Contains(stdout.String(), "PERMISSION UPGRADE REQUIRED", "scope prompt")
	assert.Contains(stdout.String(), "Deletion complete!", "daemon output")
}

func TestCancelDeletionUsageErrorBeforeDaemonRunner(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	savedCancelAll := cancelAll
	t.Cleanup(func() {
		cancelAll = savedCancelAll
	})

	server, requests := newDaemonCLIRunnerTestServer(t, nil, `{"type":"complete"}`)
	configureRemoteDaemonForTest(t, server.URL)

	cmd := newCancelDeletionRoutingTestCommand()
	cmd.SetArgs([]string{"--all", "batch-123"})

	err := cmd.Execute()

	require.Error(err, "cancel-deletion should reject --all plus batch ID before HTTP routing")
	assert.Contains(err.Error(), "cannot use --all with a batch ID argument")
	assert.Equal(0, int(requests.Load()), "runner endpoint calls")
}

func resetDeleteStagedRoutingGlobals(t *testing.T) {
	t.Helper()
	savedPermanent := deletePermanent
	savedYes := deleteYes
	savedDryRun := deleteDryRun
	savedList := deleteList
	savedAccount := deleteAccount
	savedPlannedBatchIDs := deletePlannedBatchIDs
	deletePermanent = false
	deleteYes = false
	deleteDryRun = false
	deleteList = false
	deleteAccount = ""
	deletePlannedBatchIDs = nil
	t.Cleanup(func() {
		deletePermanent = savedPermanent
		deleteYes = savedYes
		deleteDryRun = savedDryRun
		deleteList = savedList
		deleteAccount = savedAccount
		deletePlannedBatchIDs = savedPlannedBatchIDs
	})
}

func newDeleteStagedRoutingTestCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:  "delete-staged [batch-id]",
		Args: cobra.MaximumNArgs(1),
		RunE: deleteStagedCmd.RunE,
	}
	cmd.Flags().BoolVar(&deletePermanent, "permanent", false, "Permanent")
	cmd.Flags().BoolVarP(&deleteYes, "yes", "y", false, "Skip confirmation")
	cmd.Flags().BoolVar(&deleteDryRun, "dry-run", false, "Dry run")
	cmd.Flags().BoolVarP(&deleteList, "list", "l", false, "List")
	cmd.Flags().StringVar(&deleteAccount, "account", "", "Account")
	cmd.Flags().Bool("confirmed", false, "Internal confirmation marker")
	cmd.Flags().Bool("skip-prelude", false, "Internal prelude marker")
	cmd.Flags().StringArrayVar(&deletePlannedBatchIDs, "planned-batch", nil, "Internal planned batch marker")
	cmd.Flags().String("plan-fingerprint", "", "Internal plan fingerprint marker")
	cmd.Flags().Bool("scope-escalation-confirmed", false, "Internal scope escalation marker")
	_ = cmd.Flags().MarkHidden("confirmed")
	_ = cmd.Flags().MarkHidden("skip-prelude")
	_ = cmd.Flags().MarkHidden("planned-batch")
	_ = cmd.Flags().MarkHidden("plan-fingerprint")
	_ = cmd.Flags().MarkHidden("scope-escalation-confirmed")
	cmd.MarkFlagsMutuallyExclusive("permanent", "yes")
	return cmd
}

func newCancelDeletionRoutingTestCommand() *cobra.Command {
	cancelAll = false
	cmd := &cobra.Command{
		Use:  "cancel-deletion [batch-id]",
		Args: cobra.MaximumNArgs(1),
		RunE: runCancelDeletion,
	}
	cmd.Flags().BoolVar(&cancelAll, "all", false, "Cancel all")
	return cmd
}
