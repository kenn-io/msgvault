package cmd

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/dedup"
	"go.kenn.io/msgvault/internal/opserr"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestPlanCLIDeduplicateRequiresConfirmationForDerivableBackfill(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	withDeduplicateTestConfig(t)

	readyID := f.CreateMessage("ready-metadata")
	require.NoError(f.Store.UpsertMessageRaw(readyID, []byte(
		"Message-ID: <ready-metadata@example.test>\r\n\r\nBody")))
	failedID := f.CreateMessage("malformed-metadata")
	require.NoError(f.Store.UpsertMessageRaw(failedID, []byte(
		"From: sender@example.test\r\nSubject: no identifier\r\n\r\nBody")))

	plan, err := planCLIDeduplicate(t.Context(), f.Store, api.CLIDeduplicatePlanRequest{
		Account: f.Source.Identifier,
	})

	require.NoError(err)
	require.Len(plan.Items, 1)
	item := plan.Items[0]
	assert.True(item.NeedsConfirmation, "derivable metadata is executable work")
	assert.Equal(int64(-1), item.BackfilledCount, "signed compatibility field")
	assert.Contains(item.Stdout, "2 messages with missing RFC822 Message-ID were inspected.")
	assert.Contains(item.Stdout, "1 RFC822 Message-ID value is ready to be derived from stored MIME after confirmation.")
	assert.Contains(item.Stdout, "1 message could not provide a usable Message-ID and will be skipped.")
}

func TestPlanCLIDeduplicateMalformedOnlyBackfillDoesNotRequireConfirmation(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	withDeduplicateTestConfig(t)

	messageID := f.CreateMessage("malformed-only")
	require.NoError(f.Store.UpsertMessageRaw(messageID, []byte(
		"From: sender@example.test\r\nSubject: no identifier\r\n\r\nBody")))

	plan, err := planCLIDeduplicate(t.Context(), f.Store, api.CLIDeduplicatePlanRequest{
		Account: f.Source.Identifier,
	})

	require.NoError(err)
	require.Len(plan.Items, 1)
	item := plan.Items[0]
	assert.False(item.NeedsConfirmation, "malformed-only metadata is not executable work")
	assert.Equal(int64(0), item.BackfilledCount, "signed compatibility field")
	assert.Contains(item.Stdout, "1 message with missing RFC822 Message-ID was inspected.")
	assert.NotContains(item.Stdout, "ready to be derived")
	assert.Contains(item.Stdout, "1 message could not provide a usable Message-ID and will be skipped.")
}

func TestDeduplicatePlanFingerprintBindsExactBackfillPlan(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	messageID := f.CreateMessage("fingerprint-metadata")
	require.NoError(f.Store.UpsertMessageRaw(messageID, []byte(
		"Message-ID: <first-plan@example.test>\r\n\r\nFirst body")))

	cfgScoped := dedup.Config{
		AccountSourceIDs: []int64{f.Source.ID},
		Account:          f.Source.Identifier,
	}
	engine := dedup.NewEngine(f.Store, cfgScoped, nil)
	firstReport, err := engine.Scan(t.Context())
	require.NoError(err)
	firstFingerprint, err := deduplicatePlanFingerprint(t.Context(), cfgScoped, firstReport)
	require.NoError(err)

	require.NoError(f.Store.UpsertMessageRaw(messageID, []byte(
		"Message-ID: <second-plan@example.test>\r\n\r\nSecond body")))
	secondReport, err := engine.Scan(t.Context())
	require.NoError(err)
	secondFingerprint, err := deduplicatePlanFingerprint(t.Context(), cfgScoped, secondReport)
	require.NoError(err)

	assert.Equal(firstReport.BackfillCandidates, secondReport.BackfillCandidates, "candidate count")
	assert.Equal(firstReport.PendingRFC822IDBackfill(), secondReport.PendingRFC822IDBackfill(), "ready count")
	assert.NotEqual(firstReport.BackfillPlanDigest, secondReport.BackfillPlanDigest, "store plan digest")
	assert.NotEqual(firstFingerprint, secondFingerprint, "CLI fingerprint must bind the exact derivation plan")
}

func TestPrintDedupSummaryOmitsUndoForBackfillOnlyExecution(t *testing.T) {
	assert := assert.New(t)
	done := captureStdout(t)
	printDedupSummary(&dedup.ExecutionSummary{
		BatchID:             "metadata-only-batch",
		RFC822IDsBackfilled: 2,
		RawMIMEBackfilled:   3,
	})
	out := done()

	assert.Contains(out, "RFC822 Message-IDs derived: 2")
	assert.Contains(out, "Raw MIME backfilled: 3", "raw MIME remains a separate metric")
	assert.NotContains(out, "Batch ID:", "metadata-only execution creates no dedup batch")
	assert.NotContains(out, "To undo:", "metadata-only execution has no reversible dedup batch")
}

func TestDeduplicateSingleAndMultiSourceBackfillOnlyOmitUndo(t *testing.T) {
	t.Run("dry run stays read-only without prompting", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		resetDeduplicateRoutingGlobals(t)
		f := storetest.New(t)
		messageID := createPendingRFC822Message(t, f.Store, f.Source.ID, f.ConvID,
			"dry-run-metadata", "dry-run-metadata@example.test")
		cfgScoped := dedup.Config{
			DryRun:           true,
			AccountSourceIDs: []int64{f.Source.ID},
			Account:          f.Source.Identifier,
		}
		cmd := &cobra.Command{}
		cmd.SetContext(t.Context())
		cmd.SetIn(strings.NewReader("y\n"))

		done := captureStdout(t)
		err := runDeduplicateOnce(
			cmd, f.Store, "", cfgScoped,
			dedup.NewEngine(f.Store, cfgScoped, nil),
		)
		out := done()

		require.NoError(err)
		assert.Empty(storedRFC822IDForCLI(t, f.Store, messageID))
		assert.Contains(out, "Dry run complete. No changes made.")
		assert.NotContains(out, "Proceed with deduplication")
		assert.NotContains(out, "RFC822 Message-IDs derived:")
	})

	t.Run("single source", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		resetDeduplicateRoutingGlobals(t)
		dedupYes = true
		dedupNoBackup = true
		f := storetest.New(t)
		messageID := createPendingRFC822Message(t, f.Store, f.Source.ID, f.ConvID,
			"single-metadata", "single-metadata@example.test")
		cfgScoped := dedup.Config{
			AccountSourceIDs: []int64{f.Source.ID},
			Account:          f.Source.Identifier,
		}
		cmd := &cobra.Command{}
		cmd.SetContext(t.Context())

		done := captureStdout(t)
		err := runDeduplicateOnce(
			cmd, f.Store, "", cfgScoped,
			dedup.NewEngine(f.Store, cfgScoped, nil),
		)
		out := done()

		require.NoError(err)
		assert.Equal("single-metadata@example.test", storedRFC822IDForCLI(t, f.Store, messageID))
		assert.Contains(out, "Deriving RFC822 Message-IDs...")
		assert.NotContains(out, "Merging duplicates...")
		assert.Contains(out, "RFC822 Message-IDs derived: 1")
		assert.NotContains(out, "Batch ID:")
		assert.NotContains(out, "To undo:")
		assert.NotContains(out, "To undo all of the above:")
	})

	t.Run("multiple per-source executions", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		resetDeduplicateRoutingGlobals(t)
		dedupYes = true
		dedupNoBackup = true
		f := storetest.New(t)
		firstID := createPendingRFC822Message(t, f.Store, f.Source.ID, f.ConvID,
			"first-metadata", "first-metadata@example.test")
		secondSource, err := f.Store.GetOrCreateSource("mbox", "backup@example.test")
		require.NoError(err)
		secondConversation, err := f.Store.EnsureConversation(secondSource.ID, "second-thread", "Second")
		require.NoError(err)
		secondID := createPendingRFC822Message(t, f.Store, secondSource.ID, secondConversation,
			"second-metadata", "second-metadata@example.test")
		cmd := &cobra.Command{}
		cmd.SetContext(t.Context())

		done := captureStdout(t)
		err = runDeduplicatePerSource(cmd, f.Store, "", dedup.Config{})
		out := done()

		require.NoError(err)
		assert.Equal("first-metadata@example.test", storedRFC822IDForCLI(t, f.Store, firstID))
		assert.Equal("second-metadata@example.test", storedRFC822IDForCLI(t, f.Store, secondID))
		assert.Equal(2, strings.Count(out, "Deriving RFC822 Message-IDs..."), "one execution prelude per source")
		assert.NotContains(out, "Merging duplicates...")
		assert.Equal(2, strings.Count(out, "RFC822 Message-IDs derived: 1"), "one execution summary per source")
		assert.NotContains(out, "Batch ID:")
		assert.NotContains(out, "To undo:")
		assert.NotContains(out, "To undo all of the above:")
	})
}

func TestDeduplicateLocalAndPerSourceMergeOutputIncludesBatch(t *testing.T) {
	t.Run("single source", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		resetDeduplicateRoutingGlobals(t)
		dedupYes = true
		dedupNoBackup = true
		f := storetest.New(t)
		messageIDs := f.CreateMessages(2)
		_, err := f.Store.DB().Exec(f.Store.Rebind(
			`UPDATE messages SET rfc822_message_id = ? WHERE id IN (?, ?)`),
			"local-merge@example.test", messageIDs[0], messageIDs[1])
		require.NoError(err)
		cfgScoped := dedup.Config{
			AccountSourceIDs: []int64{f.Source.ID},
			Account:          f.Source.Identifier,
		}
		cmd := &cobra.Command{}
		cmd.SetContext(t.Context())

		done := captureStdout(t)
		err = runDeduplicateOnce(
			cmd, f.Store, "", cfgScoped,
			dedup.NewEngine(f.Store, cfgScoped, nil),
		)
		out := done()

		require.NoError(err)
		assert.Contains(out, "Merging duplicates...")
		assert.NotContains(out, "Deriving RFC822 Message-IDs...")
		assert.Contains(out, "Batch ID:")
		assert.Contains(out, "Groups merged:       1")
	})

	t.Run("per source", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		resetDeduplicateRoutingGlobals(t)
		dedupYes = true
		dedupNoBackup = true
		f := storetest.New(t)
		messageIDs := f.CreateMessages(2)
		_, err := f.Store.DB().Exec(f.Store.Rebind(
			`UPDATE messages SET rfc822_message_id = ? WHERE id IN (?, ?)`),
			"per-source-merge@example.test", messageIDs[0], messageIDs[1])
		require.NoError(err)
		cmd := &cobra.Command{}
		cmd.SetContext(t.Context())

		done := captureStdout(t)
		err = runDeduplicatePerSource(cmd, f.Store, "", dedup.Config{})
		out := done()

		require.NoError(err)
		assert.Contains(out, "Merging duplicates...")
		assert.NotContains(out, "Deriving RFC822 Message-IDs...")
		assert.Contains(out, "Batch ID:")
		assert.Contains(out, "Groups merged:       1")
	})
}

func TestDeduplicateLocalAndDaemonPromptDescribeDerivationFence(t *testing.T) {
	const promptNote = "This will derive 1 RFC822 Message-ID value(s) from stored MIME after confirmation. " +
		"If that reveals a different duplicate plan, no messages will be hidden; rerun deduplicate to review it."

	t.Run("local", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		resetDeduplicateRoutingGlobals(t)
		dedupNoBackup = true
		f := storetest.New(t)
		messageID := createPendingRFC822Message(t, f.Store, f.Source.ID, f.ConvID,
			"local-prompt", "local-prompt@example.test")
		cfgScoped := dedup.Config{
			AccountSourceIDs: []int64{f.Source.ID},
			Account:          f.Source.Identifier,
		}
		cmd := &cobra.Command{}
		cmd.SetContext(t.Context())
		cmd.SetIn(strings.NewReader("n\n"))

		done := captureStdout(t)
		err := runDeduplicateOnce(cmd, f.Store, "", cfgScoped, dedup.NewEngine(f.Store, cfgScoped, nil))
		out := done()

		require.NoError(err)
		assert.Contains(out, promptNote)
		assert.Contains(out, "Proceed with RFC822 Message-ID derivation? [y/N]:")
		assert.NotContains(out, "reversible with --undo")
		assert.Contains(out, "Aborted.")
		assert.Empty(storedRFC822IDForCLI(t, f.Store, messageID), "declined derivation remains unapplied")
	})

	t.Run("daemon-backed", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		resetDeduplicateRoutingGlobals(t)
		f := storetest.New(t)
		messageID := createPendingRFC822Message(t, f.Store, f.Source.ID, f.ConvID,
			"daemon-prompt", "daemon-prompt@example.test")
		serverCfg := config.NewDefaultConfig()
		serverCfg.Data.DataDir = t.TempDir()
		apiServer := api.NewServerWithOptions(api.ServerOptions{
			Config:        serverCfg,
			Store:         &storeAPIAdapter{store: f.Store},
			Logger:        slog.New(slog.DiscardHandler),
			DaemonVersion: Version,
		})
		httpServer := httptest.NewServer(apiServer.Router())
		t.Cleanup(httpServer.Close)
		configureRemoteDaemonForTest(t, httpServer.URL)

		cmd := newDeduplicateRoutingTestCommand()
		var stdout bytes.Buffer
		cmd.SetOut(&stdout)
		cmd.SetIn(strings.NewReader("n\n"))
		cmd.SetArgs([]string{"--account", f.Source.Identifier})

		require.NoError(cmd.Execute())
		assert.Contains(stdout.String(), promptNote)
		assert.Contains(stdout.String(), "Proceed with RFC822 Message-ID derivation? [y/N]:")
		assert.NotContains(stdout.String(), "reversible with --undo")
		assert.Contains(stdout.String(), "Aborted.")
		assert.Empty(storedRFC822IDForCLI(t, f.Store, messageID), "planning and cancellation stay read-only")
	})

	t.Run("per-source", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		resetDeduplicateRoutingGlobals(t)
		dedupNoBackup = true
		f := storetest.New(t)
		messageID := createPendingRFC822Message(t, f.Store, f.Source.ID, f.ConvID,
			"per-source-prompt", "per-source-prompt@example.test")
		cmd := &cobra.Command{}
		cmd.SetContext(t.Context())
		cmd.SetIn(strings.NewReader("n\n"))

		done := captureStdout(t)
		err := runDeduplicatePerSource(cmd, f.Store, "", dedup.Config{})
		out := done()

		require.NoError(err)
		assert.Contains(out, "Proceed with RFC822 Message-ID derivation for test@example.com? [y/N]:")
		assert.NotContains(out, "reversible with --undo")
		assert.Contains(out, "Skipped.")
		assert.Empty(storedRFC822IDForCLI(t, f.Store, messageID))
	})

	t.Run("multiple sources", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		resetDeduplicateRoutingGlobals(t)
		dedupNoBackup = true
		f := storetest.New(t)
		firstID := createPendingRFC822Message(t, f.Store, f.Source.ID, f.ConvID,
			"multi-first-prompt", "multi-first-prompt@example.test")
		secondSource, err := f.Store.GetOrCreateSource("mbox", "backup@example.test")
		require.NoError(err)
		secondConversation, err := f.Store.EnsureConversation(secondSource.ID, "prompt-thread", "Prompt")
		require.NoError(err)
		secondID := createPendingRFC822Message(t, f.Store, secondSource.ID, secondConversation,
			"multi-second-prompt", "multi-second-prompt@example.test")
		cmd := &cobra.Command{}
		cmd.SetContext(t.Context())
		cmd.SetIn(strings.NewReader("n\nn\n"))

		done := captureStdout(t)
		err = runDeduplicatePerSource(cmd, f.Store, "", dedup.Config{})
		out := done()

		require.NoError(err)
		assert.Contains(out, "Proceed with RFC822 Message-ID derivation for test@example.com? [y/N]:")
		assert.Contains(out, "Proceed with RFC822 Message-ID derivation for backup@example.test? [y/N]:")
		assert.Equal(2, strings.Count(out, "Proceed with RFC822 Message-ID derivation for"))
		assert.NotContains(out, "reversible with --undo")
		assert.Empty(storedRFC822IDForCLI(t, f.Store, firstID))
		assert.Empty(storedRFC822IDForCLI(t, f.Store, secondID))
	})
}

func TestDeduplicatePromptRetainsUndoGuidanceForDuplicateMerge(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	resetDeduplicateRoutingGlobals(t)
	dedupNoBackup = true
	f := storetest.New(t)
	messageIDs := f.CreateMessages(2)
	_, err := f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE messages SET rfc822_message_id = ? WHERE id IN (?, ?)`),
		"prompt-duplicate@example.test", messageIDs[0], messageIDs[1])
	require.NoError(err)
	cfgScoped := dedup.Config{
		AccountSourceIDs: []int64{f.Source.ID},
		Account:          f.Source.Identifier,
	}
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	cmd.SetIn(strings.NewReader("n\n"))

	done := captureStdout(t)
	err = runDeduplicateOnce(cmd, f.Store, "", cfgScoped, dedup.NewEngine(f.Store, cfgScoped, nil))
	out := done()

	require.NoError(err)
	assert.Contains(out, "Proceed with deduplication? This will hide 1 duplicates (reversible with --undo). [y/N]:")
	assert.NotContains(out, "Proceed with RFC822 Message-ID derivation?")
}

func TestDeduplicatePlanChangedOutputReportsCommitAndNoBatch(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	resetDeduplicateRoutingGlobals(t)
	dedupYes = true
	dedupNoBackup = true
	f := storetest.New(t)
	messageIDs := f.CreateMessages(2)
	const sharedID = "revealed-duplicate@example.test"
	_, err := f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE messages SET rfc822_message_id = ? WHERE id = ?`), sharedID, messageIDs[0])
	require.NoError(err)
	require.NoError(f.Store.UpsertMessageRaw(messageIDs[1], []byte(
		"Message-ID: <"+sharedID+">\r\n\r\nRevealed duplicate")))
	cfgScoped := dedup.Config{
		AccountSourceIDs: []int64{f.Source.ID},
		Account:          f.Source.Identifier,
	}
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())

	done := captureStdout(t)
	err = runDeduplicateOnce(
		cmd, f.Store, "", cfgScoped,
		dedup.NewEngine(f.Store, cfgScoped, nil),
	)
	out := done()

	require.Error(err)
	require.ErrorIs(err, dedup.ErrPlanChangedAfterRFC822Backfill)
	assert.Contains(err.Error(), "1 RFC822 Message-ID derivation was committed")
	assert.Contains(err.Error(), "no duplicate messages were hidden")
	assert.Contains(err.Error(), "no dedup batch was created")
	assert.Contains(err.Error(), "rerun deduplicate to review the updated plan")
	assert.NotContains(out, "Batch ID:")
	assert.Equal(sharedID, storedRFC822IDForCLI(t, f.Store, messageIDs[1]), "derivation committed")
	var hidden int
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(
		`SELECT COUNT(*) FROM messages WHERE id IN (?, ?) AND deleted_at IS NOT NULL`),
		messageIDs[0], messageIDs[1],
	).Scan(&hidden))
	assert.Zero(hidden, "plan fence hides no messages")
}

func withDeduplicateTestConfig(t *testing.T) {
	t.Helper()
	savedCfg := cfg
	t.Cleanup(func() { cfg = savedCfg })
	cfg = config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
}

func createPendingRFC822Message(
	t *testing.T,
	st *store.Store,
	sourceID, conversationID int64,
	sourceMessageID, rfc822MessageID string,
) int64 {
	t.Helper()
	id, err := st.UpsertMessage(&store.Message{
		ConversationID:  conversationID,
		SourceID:        sourceID,
		SourceMessageID: sourceMessageID,
		MessageType:     "email",
		SizeEstimate:    1000,
	})
	require.NoError(t, err)
	require.NoError(t, st.UpsertMessageRaw(id, []byte(
		"Message-ID: <"+rfc822MessageID+">\r\nFrom: sender@example.test\r\n\r\nBody")))
	return id
}

func storedRFC822IDForCLI(t *testing.T, st *store.Store, messageID int64) string {
	t.Helper()
	var value sql.NullString
	require.NoError(t, st.DB().QueryRow(st.Rebind(
		`SELECT rfc822_message_id FROM messages WHERE id = ?`), messageID).Scan(&value))
	return value.String
}

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

func TestDeduplicatePlanFingerprintChangesWhenRemoteDeletionTargetsChange(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	messageIDs := f.CreateMessages(2)

	const rfc822MessageID = "remote-target-change@example.test"
	_, err := f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages
		SET rfc822_message_id = ?
		WHERE id IN (?, ?)`), rfc822MessageID, messageIDs[0], messageIDs[1])
	require.NoError(err, "set shared RFC822 Message-ID")

	raw := []byte("Message-ID: <remote-target-change@example.test>\r\n" +
		"From: sender@example.test\r\nSubject: Same message\r\n\r\nSame body")
	require.NoError(f.Store.UpsertMessageRaw(messageIDs[0], raw), "store first raw MIME")
	require.NoError(f.Store.UpsertMessageRaw(messageIDs[1], raw), "store second raw MIME")

	cfg := dedup.Config{
		AccountSourceIDs:           []int64{f.Source.ID},
		Account:                    f.Source.Identifier,
		DeleteDupsFromSourceServer: true,
	}
	engine := dedup.NewEngine(f.Store, cfg, nil)
	confirmedReport, err := engine.Scan(t.Context())
	require.NoError(err, "scan confirmed plan")
	confirmedFingerprint, err := deduplicatePlanFingerprint(t.Context(), cfg, confirmedReport)
	require.NoError(err, "fingerprint confirmed plan")

	changedRaw := []byte("Message-ID: <remote-target-change@example.test>\r\n" +
		"From: sender@example.test\r\nSubject: Different message\r\n\r\nDifferent body")
	require.NoError(f.Store.UpsertMessageRaw(messageIDs[1], changedRaw), "change duplicate raw MIME")
	changedReport, err := engine.Scan(t.Context())
	require.NoError(err, "rescan changed plan")
	changedFingerprint, err := deduplicatePlanFingerprint(t.Context(), cfg, changedReport)
	require.NoError(err, "fingerprint changed plan")

	require.Len(confirmedReport.Groups, 1, "confirmed duplicate groups")
	require.Len(changedReport.Groups, 1, "changed duplicate groups")
	assert.Equal(
		confirmedReport.Groups[0].Messages[confirmedReport.Groups[0].Survivor].ID,
		changedReport.Groups[0].Messages[changedReport.Groups[0].Survivor].ID,
		"survivor ID",
	)
	assert.NotEqual(confirmedFingerprint, changedFingerprint,
		"fingerprint must cover MIME-dependent remote deletion targets")
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
