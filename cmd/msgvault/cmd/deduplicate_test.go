package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/opserr"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestDeduplicateNonInteractiveFormsUseDaemonRunner(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		want   []string
		stdout string
	}{
		{
			name:   "dry-run",
			args:   []string{"--account", "alice@example.com", "--dry-run"},
			want:   []string{deduplicateCommandName, "--account=alice@example.com", "--dry-run"},
			stdout: "Dry run complete. No changes made.\n",
		},
		{
			name:   "yes",
			args:   []string{"--account", "alice@example.com", "--yes"},
			want:   []string{deduplicateCommandName, "--account=alice@example.com", "--yes"},
			stdout: "Deduplication complete.\n",
		},
		{
			name:   "undo",
			args:   []string{"--undo", "batch-a", "--undo", "batch-b"},
			want:   []string{deduplicateCommandName, "--undo=batch-a", "--undo=batch-b"},
			stdout: "Restored 2 messages.\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			resetDeduplicateRoutingGlobals(t)

			stdoutJSON, err := json.Marshal(tt.stdout)
			require.NoError(err, "marshal stdout")
			server, requests := newDaemonCLIRunnerTestServer(t, func(req daemonCLIRunTestRequest) {
				assert.Equal(tt.want, req.Args, "args")
			}, `{"type":"stdout","data":`+string(stdoutJSON)+`}`, `{"type":"complete"}`)
			configureRemoteDaemonForTest(t, server.URL)

			cmd := newDeduplicateRoutingTestCommand()
			var stdout bytes.Buffer
			cmd.SetOut(&stdout)
			cmd.SetArgs(tt.args)

			require.NoError(cmd.Execute(), "deduplicate")
			assert.Equal(1, int(requests.Load()), "runner endpoint calls")
			assert.Equal(tt.stdout, stdout.String(), "stdout")
		})
	}
}

func TestDeduplicateInteractiveAccountPlansPromptsAndExecutesThroughDaemon(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	resetDeduplicateRoutingGlobals(t)

	server, runRequests, planRequests := newDaemonCLIDeduplicateTestServer(t, func(req daemonCLIDeduplicatePlanTestRequest) {
		assert.Equal("alice@example.com", req.Account, "account")
		assert.Empty(req.Collection, "collection")
	}, map[string]any{
		"items": []map[string]any{
			{
				"stdout":              "Scanning for duplicate messages...\n\n=== Deduplication Report ===\nDuplicate groups found: 1\nMessages to prune:      2\n",
				"duplicate_messages":  2,
				"plan_fingerprint":    "fp-account",
				"needs_confirmation":  true,
				"scope_label":         "alice@example.com",
				"source_id":           0,
				"scope_is_collection": false,
			},
		},
	}, func(req daemonCLIRunTestRequest) {
		assert.Equal([]string{
			deduplicateCommandName,
			"--account=alice@example.com",
			"--dedup-plan-confirmed",
			"--dedup-plan-fingerprint=fp-account",
			"--yes",
		}, req.Args, "runner args")
	}, `{"type":"stdout","data":"Merging duplicates...\n"}`, `{"type":"complete"}`)
	configureRemoteDaemonForTest(t, server.URL)

	cmd := newDeduplicateRoutingTestCommand()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetIn(strings.NewReader("y\n"))
	cmd.SetArgs([]string{"--account", "alice@example.com"})

	require.NoError(cmd.Execute(), "deduplicate")
	assert.Equal(1, int(planRequests.Load()), "plan endpoint calls")
	assert.Equal(1, int(runRequests.Load()), "runner endpoint calls")
	assert.Contains(stdout.String(), "Scanning for duplicate messages...", "plan stdout")
	assert.Contains(stdout.String(), "Proceed with deduplication? This will hide 2 duplicates", "prompt")
	assert.Contains(stdout.String(), "Merging duplicates...", "runner stdout")
}

func TestDeduplicateInteractiveAccountCancelDoesNotExecute(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	resetDeduplicateRoutingGlobals(t)

	server, runRequests, planRequests := newDaemonCLIDeduplicateTestServer(t, nil, map[string]any{
		"items": []map[string]any{
			{
				"stdout":             "Scanning for duplicate messages...\nDuplicate groups found: 1\n",
				"duplicate_messages": 1,
				"plan_fingerprint":   "fp-account",
				"needs_confirmation": true,
			},
		},
	}, nil)
	configureRemoteDaemonForTest(t, server.URL)

	cmd := newDeduplicateRoutingTestCommand()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetIn(strings.NewReader("n\n"))
	cmd.SetArgs([]string{"--account", "alice@example.com"})

	require.NoError(cmd.Execute(), "deduplicate")
	assert.Equal(1, int(planRequests.Load()), "plan endpoint calls")
	assert.Equal(0, int(runRequests.Load()), "runner endpoint calls")
	assert.Contains(stdout.String(), "Aborted.", "cancel output")
}

func TestDeduplicateInteractivePerSourcePromptsShareInput(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	resetDeduplicateRoutingGlobals(t)

	server, runRequests, planRequests := newDaemonCLIDeduplicateTestServer(t, nil, map[string]any{
		"prefix_stdout": "No --account specified; deduping each source independently.\n\n",
		"items": []map[string]any{
			{
				"stdout":             "--- alice@example.com (gmail) ---\nDuplicate groups found: 1\n",
				"duplicate_messages": 1,
				"plan_fingerprint":   "fp-alice",
				"needs_confirmation": true,
				"source_id":          101,
				"scope_label":        "alice@example.com",
			},
			{
				"stdout":             "--- bob@example.com (gmail) ---\nDuplicate groups found: 1\n",
				"duplicate_messages": 1,
				"plan_fingerprint":   "fp-bob",
				"needs_confirmation": true,
				"source_id":          202,
				"scope_label":        "bob@example.com",
			},
		},
	}, func(req daemonCLIRunTestRequest) {
		assert.Contains(req.Args, "--dedup-source-plan=101:fp-alice", "alice approval")
		assert.Contains(req.Args, "--dedup-source-plan=202:fp-bob", "bob approval")
	}, `{"type":"complete"}`)
	configureRemoteDaemonForTest(t, server.URL)

	cmd := newDeduplicateRoutingTestCommand()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetIn(strings.NewReader("y\ny\n"))

	require.NoError(cmd.Execute(), "deduplicate")
	assert.Equal(1, int(planRequests.Load()), "plan endpoint calls")
	assert.Equal(1, int(runRequests.Load()), "runner endpoint calls")
	assert.Contains(stdout.String(), "alice@example.com", "first prompt")
	assert.Contains(stdout.String(), "bob@example.com", "second prompt")
}

func resetDeduplicateRoutingGlobals(t *testing.T) {
	t.Helper()
	savedDryRun := dedupDryRun
	savedNoBackup := dedupNoBackup
	savedPrefer := dedupPrefer
	savedContentHash := dedupContentHash
	savedUndo := dedupUndo
	savedAccount := dedupAccount
	savedCollection := dedupCollection
	savedDeleteFromSource := dedupDeleteFromSourceSrvr
	savedYes := dedupYes
	savedPlanConfirmed := dedupPlanConfirmed
	savedPlanFingerprint := dedupPlanFingerprint
	savedSourcePlans := dedupSourcePlans
	savedSourceID := dedupSourceID
	dedupDryRun = false
	dedupNoBackup = false
	dedupPrefer = ""
	dedupContentHash = false
	dedupUndo = nil
	dedupAccount = ""
	dedupCollection = ""
	dedupDeleteFromSourceSrvr = false
	dedupYes = false
	dedupPlanConfirmed = false
	dedupPlanFingerprint = ""
	dedupSourcePlans = nil
	dedupSourceID = 0
	t.Cleanup(func() {
		dedupDryRun = savedDryRun
		dedupNoBackup = savedNoBackup
		dedupPrefer = savedPrefer
		dedupContentHash = savedContentHash
		dedupUndo = savedUndo
		dedupAccount = savedAccount
		dedupCollection = savedCollection
		dedupDeleteFromSourceSrvr = savedDeleteFromSource
		dedupYes = savedYes
		dedupPlanConfirmed = savedPlanConfirmed
		dedupPlanFingerprint = savedPlanFingerprint
		dedupSourcePlans = savedSourcePlans
		dedupSourceID = savedSourceID
	})
}

func newDeduplicateRoutingTestCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:  deduplicateCommandName,
		RunE: runDeduplicate,
	}
	cmd.Flags().BoolVar(&dedupDryRun, "dry-run", false, "")
	cmd.Flags().BoolVar(&dedupNoBackup, "no-backup", false, "")
	cmd.Flags().StringVar(&dedupPrefer, "prefer", "", "")
	cmd.Flags().BoolVar(&dedupContentHash, "content-hash", false, "")
	cmd.Flags().StringArrayVar(&dedupUndo, "undo", nil, "")
	cmd.Flags().StringVar(&dedupAccount, "account", "", "")
	cmd.Flags().StringVar(&dedupCollection, "collection", "", "")
	cmd.Flags().BoolVar(&dedupDeleteFromSourceSrvr, "delete-dups-from-source-server", false, "")
	cmd.Flags().BoolVarP(&dedupYes, "yes", "y", false, "")
	cmd.Flags().BoolVar(&dedupPlanConfirmed, "dedup-plan-confirmed", false, "")
	cmd.Flags().StringVar(&dedupPlanFingerprint, "dedup-plan-fingerprint", "", "")
	cmd.Flags().StringArrayVar(&dedupSourcePlans, "dedup-source-plan", nil, "")
	cmd.Flags().Int64Var(&dedupSourceID, "dedup-source-id", 0, "")
	return cmd
}

// TestDeduplicateMutualExclusion confirms that passing both --account and
// --collection to the deduplicate command is rejected by cobra.
func TestDeduplicateMutualExclusion(t *testing.T) {
	// Build a minimal parent so Execute() returns errors rather than printing
	// them and swallowing them via the global rootCmd error handler.
	var a, b string
	cmd := &cobra.Command{Use: "dedup-test", SilenceErrors: true}
	sub := &cobra.Command{Use: "deduplicate", RunE: func(cmd *cobra.Command, args []string) error { return nil }}
	sub.Flags().StringVar(&a, "account", "", "")
	sub.Flags().StringVar(&b, "collection", "", "")
	sub.MarkFlagsMutuallyExclusive("account", "collection")
	cmd.AddCommand(sub)
	cmd.SetArgs([]string{"deduplicate", "--account", "alpha@example.com", "--collection", "work"})

	err := cmd.Execute()
	require.Error(t, err, "expected error when both --account and --collection are set")
	msg := err.Error()
	assert.Contains(t, msg, "account", "error should mention account flag name")
	assert.Contains(t, msg, "collection", "error should mention collection flag name")
	_ = a
	_ = b
}

func TestDeduplicateAccountResolutionExcludesCalendarSources(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, accountID, _ := setupScopeFixture(t)

	cal, err := f.Store.GetOrCreateSource(sourceTypeCalendar, accountID+"/primary")
	require.NoError(err, "GetOrCreateSource calendar")
	cfg, err := json.Marshal(map[string]string{
		"account_email": accountID,
		"calendar_id":   "primary",
	})
	require.NoError(err, "marshal sync_config")
	require.NoError(f.Store.UpdateSourceSyncConfig(cal.ID, string(cfg)), "UpdateSourceSyncConfig")

	scope, err := ResolveEmailAccountFlag(f.Store, accountID)
	require.NoError(err)

	assert.ElementsMatch([]int64{f.Source.ID}, scope.SourceIDs())
	assert.NotContains(scope.SourceIDs(), cal.ID, "dedup account scope must not include Calendar sources")
}

// TestDeduplicateCollectionResolution confirms that --collection resolves
// successfully when the name matches a real collection in the store.
func TestDeduplicateCollectionResolution(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, _, collectionName := setupScopeFixture(t)

	scope, err := ResolveCollectionFlag(f.Store, collectionName)
	require.NoError(err)
	require.NotNil(scope.Collection, "expected Collection to be populated")
	assert.Equal(collectionName, scope.Collection.Name, "collection name")
	ids := scope.SourceIDs()
	assert.NotEmpty(ids, "expected non-empty SourceIDs for collection")
}

// TestDeduplicateCollectionResolution_MultiSource confirms SourceIDs expands
// to all members when a collection has more than one source.
func TestDeduplicateCollectionResolution_MultiSource(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)

	src2, err := f.Store.GetOrCreateSource("mbox", "backup@example.com")
	require.NoError(err, "GetOrCreateSource src2")

	collName := "two-account-collection"
	_, err = f.Store.CreateCollection(collName, "", []int64{f.Source.ID, src2.ID})
	require.NoError(err, "CreateCollection")

	scope, err := ResolveCollectionFlag(f.Store, collName)
	require.NoError(err)
	ids := scope.SourceIDs()
	assert.Len(ids, 2, "expected 2 source IDs, got %v", ids)
	assert.Equal(collName, scope.DisplayName(), "DisplayName")
}

func TestResolveDeduplicateScopeNonEmailCollectionReturnsInvalidError(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)

	calendarSource, err := f.Store.GetOrCreateSource("gcal", "calendar@example.com")
	require.NoError(err, "GetOrCreateSource calendar")
	_, err = f.Store.CreateCollection("calendars", "", []int64{calendarSource.ID})
	require.NoError(err, "CreateCollection")

	_, err = resolveDeduplicateScope(f.Store, deduplicateScopeRequest{
		Collection: "calendars",
	})

	require.Error(err, "expected non-email collection to be rejected")
	assert.Equal(opserr.KindInvalid, opserr.KindOf(err), "error kind")
	assert.Contains(err.Error(), `--collection "calendars" has no member accounts`, "error message")
}

// TestPrintAccumulatedUndoHint asserts the helper's behavior:
// no-op for <2 batches, prints recipe for ≥2. Iter15 follow-up:
// the exit-on-Execute-error path now also calls this helper so a
// user who hits an error mid-loop still sees how to undo what
// already ran.
func TestPrintAccumulatedUndoHint(t *testing.T) {
	for _, tc := range []struct {
		name         string
		batches      []string
		wantContains []string
		wantNoOutput bool
	}{
		{
			name:         "no batches",
			batches:      nil,
			wantNoOutput: true,
		},
		{
			name:         "single batch",
			batches:      []string{"dedup-1"},
			wantNoOutput: true,
		},
		{
			name:    "two batches",
			batches: []string{"dedup-a", "dedup-b"},
			wantContains: []string{
				"To undo all of the above",
				"--undo dedup-a",
				"--undo dedup-b",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			done := captureStdout(t)
			printAccumulatedUndoHint(tc.batches)
			out := done()
			if tc.wantNoOutput {
				assert.Empty(t, out, "expected no output")
				return
			}
			for _, want := range tc.wantContains {
				assert.Contains(t, out, want, "output missing %q", want)
			}
		})
	}
}
