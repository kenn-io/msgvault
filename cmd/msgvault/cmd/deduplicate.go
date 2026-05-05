package cmd

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/wesm/msgvault/internal/dedup"
	"github.com/wesm/msgvault/internal/store"
)

var deduplicateCmd = &cobra.Command{
	Use:     "deduplicate",
	Aliases: []string{"dedup", "dedupe"},
	Short:   "Find and merge duplicate messages within an account",
	Long: `Find and merge duplicate messages within a single account
(for example, the same mbox imported twice, or stored MIME that
generates two copies of the same RFC822 Message-ID inside one ingest
source). Cross-source comparison requires --collection.

Duplicates are grouped by the RFC822 Message-ID header. For each group the
engine selects a survivor, unions the labels from every copy onto the
survivor, and hides the pruned copies in the msgvault database.

By default, deduplicate ONLY modifies the msgvault database. Your original
source files and remote servers are never modified. Hidden rows can be
restored with --undo, so a dedup run is fully reversible.

Terminology:
  "account"     One ingest source/archive (a single Gmail OAuth
                connection, one mbox import, one IMAP source, etc.).
  "collection"  A named, user-defined grouping of accounts.

Scope:
  --account <name>      Scope dedup to one account. Never crosses
                        source boundaries.
  --collection <name>   Dedup across every member account of a collection.
                        This is the only way to compare messages across
                        sources, and it is an explicit user opt-in:
                        a duplicate Message-ID or matching content hash
                        across two accounts in the collection will hide
                        the loser locally. Use --dry-run first to
                        review what would be merged. Cross-source pruning
                        is local-only and reversible with --undo;
                        --delete-dups-from-source-server only stages
                        remote deletion when the loser and the survivor
                        share a source (same-source-only).
  (no flag)             Dedup runs per-account independently for every
                        account. Source boundaries are never crossed.

Use --dry-run to scan and report without writing anything.
Use --content-hash to also group messages by normalized raw MIME when
Message-ID matching is insufficient.
Use --undo <batch-id> to reverse a previous dedup run. Pass --undo
multiple times to reverse several batches in one invocation; failures
on one batch do not skip later batches, and any errors are aggregated
and reported at the end.`,
	RunE: runDeduplicate,
}

var (
	dedupDryRun               bool
	dedupNoBackup             bool
	dedupPrefer               string
	dedupContentHash          bool
	dedupUndo                 []string
	dedupAccount              string
	dedupCollection           string
	dedupDeleteFromSourceSrvr bool
	dedupYes                  bool
)

func runDeduplicate(cmd *cobra.Command, _ []string) error {
	st, err := openStoreAndInit()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	// dbPath is the on-disk filesystem path used by VACUUM INTO
	// backup; resolving it now also rejects non-file DSNs (e.g.
	// postgres://) up-front rather than at the first backup attempt.
	dbPath, err := cfg.DatabasePath()
	if err != nil {
		return fmt.Errorf("resolve database path: %w", err)
	}

	deletionsDir := filepath.Join(cfg.Data.DataDir, "deletions")

	// --undo operates on a recorded batch ID; scope is captured in the
	// batch itself. Cobra rejects --undo combined with --account or
	// --collection, so by the time we reach this branch undo can run
	// without resolving scope flags (a stale or renamed account would
	// otherwise block a valid undo).
	if len(dedupUndo) > 0 {
		undoConfig := dedup.Config{DeletionsDir: deletionsDir}
		engine := dedup.NewEngine(st, undoConfig, logger)
		var allStillRunning []string
		var undoErrs []error
		for _, batchID := range dedupUndo {
			restored, stillRunning, err := engine.Undo(batchID)
			// Undo is best-effort: database rows may have been restored
			// even if cancelling pending manifests failed. Always report
			// the restored count and any still-running manifests before
			// continuing so the user isn't left thinking the undo did
			// nothing. Errors aggregate across batches so a failure on
			// one batch ID doesn't skip the rest.
			fmt.Printf("Restored %d messages from batch %q.\n",
				restored, batchID)
			allStillRunning = append(allStillRunning, stillRunning...)
			if err != nil {
				fmt.Fprintf(os.Stderr,
					"\nError cancelling one or more pending manifests "+
						"for batch %q:\n  %v\n", batchID, err)
				undoErrs = append(undoErrs, fmt.Errorf("undo dedup %q: %w", batchID, err))
			}
		}
		printStillRunningWarning(allStillRunning)
		return errors.Join(undoErrs...)
	}

	preference := dedup.DefaultSourcePreference
	if dedupPrefer != "" {
		preference = strings.Split(dedupPrefer, ",")
		known := make(map[string]bool, len(dedup.DefaultSourcePreference))
		for _, t := range dedup.DefaultSourcePreference {
			known[t] = true
		}
		for i := range preference {
			preference[i] = strings.TrimSpace(preference[i])
			if !known[preference[i]] {
				fmt.Fprintf(os.Stderr, "Warning: unknown source type in --prefer: %q\n", preference[i])
			}
		}
	}

	var (
		accountSourceIDs  []int64
		canonicalAccount  string
		scopeIsCollection bool
	)
	switch {
	case dedupAccount != "":
		scope, err := ResolveAccountFlag(st, dedupAccount)
		if err != nil {
			return err
		}
		accountSourceIDs = scope.SourceIDs()
		if len(accountSourceIDs) == 0 {
			return fmt.Errorf("--account %q resolved to zero sources", dedupAccount)
		}
		canonicalAccount = scope.DisplayName()
	case dedupCollection != "":
		scope, err := ResolveCollectionFlag(st, dedupCollection)
		if err != nil {
			return err
		}
		accountSourceIDs = scope.SourceIDs()
		if len(accountSourceIDs) == 0 {
			return fmt.Errorf("--collection %q has no member accounts", dedupCollection)
		}
		canonicalAccount = scope.DisplayName()
		scopeIsCollection = true
	}

	config := dedup.Config{
		SourcePreference:           preference,
		ContentHashFallback:        dedupContentHash,
		DryRun:                     dedupDryRun,
		AccountSourceIDs:           accountSourceIDs,
		Account:                    canonicalAccount,
		ScopeIsCollection:          scopeIsCollection,
		DeleteDupsFromSourceServer: dedupDeleteFromSourceSrvr,
		DeletionsDir:               deletionsDir,
	}

	if len(accountSourceIDs) > 0 {
		bySource, err := loadPerSourceIdentities(st, accountSourceIDs)
		if err != nil {
			return fmt.Errorf("load per-source identities: %w", err)
		}
		config.IdentityAddressesBySource = bySource
		if len(bySource) > 0 {
			logger.Info("dedup per-source identities loaded",
				"sources", len(bySource))
		}
	}

	if scopeIsCollection {
		allSources, err := st.ListSources("")
		if err != nil {
			return fmt.Errorf("list sources: %w", err)
		}
		idSet := make(map[int64]struct{}, len(accountSourceIDs))
		for _, id := range accountSourceIDs {
			idSet[id] = struct{}{}
		}
		var memberNames []string
		for _, src := range allSources {
			if _, ok := idSet[src.ID]; ok {
				memberNames = append(memberNames, src.Identifier)
			}
		}
		fmt.Printf("Deduping across collection %q (%d accounts: %s)\n",
			canonicalAccount, len(memberNames), strings.Join(memberNames, ", "))
		// When the collection spans more than one account, dedup is
		// crossing source boundaries — a duplicate Message-ID or matching
		// content hash between two accounts will hide the loser locally.
		// Print a one-line hint so the user can confirm they meant to
		// cross those boundaries (and remind them that --dry-run would
		// preview without writing).
		if len(memberNames) > 1 {
			fmt.Println(
				"  Note: cross-source dedup is reversible (--undo); " +
					"remote deletion stays same-source-only. " +
					"Re-run with --dry-run to preview.",
			)
		}
	}

	if len(accountSourceIDs) == 0 {
		// Per-source path constructs its own scoped engines per
		// source, so no top-level engine is needed here.
		return runDeduplicatePerSource(cmd, st, dbPath, config)
	}

	// Single-account/single-collection path uses one engine shared
	// across the whole scope.
	engine := dedup.NewEngine(st, config, logger)
	return runDeduplicateOnce(cmd, st, dbPath, config, engine)
}

func runDeduplicatePerSource(
	cmd *cobra.Command,
	st *store.Store,
	dbPath string,
	cfgBase dedup.Config,
) error {
	sources, err := st.ListSources("")
	if err != nil {
		return fmt.Errorf("list sources: %w", err)
	}
	if len(sources) == 0 {
		fmt.Println("No sources found.")
		return nil
	}

	fmt.Println(
		"No --account specified; deduping each source independently.",
	)
	fmt.Println()

	backedUp := false
	anyRan := false
	var executedBatches []string
	for _, src := range sources {
		cfgScoped := cfgBase
		cfgScoped.AccountSourceIDs = []int64{src.ID}
		cfgScoped.Account = src.Identifier
		bySource, err := loadPerSourceIdentities(st, []int64{src.ID})
		if err != nil {
			return fmt.Errorf("load identities for %s: %w", src.Identifier, err)
		}
		cfgScoped.IdentityAddressesBySource = bySource
		engineScoped := dedup.NewEngine(st, cfgScoped, logger)

		fmt.Printf("--- %s (%s) ---\n", src.Identifier, src.SourceType)
		report, err := engineScoped.Scan(cmd.Context())
		if err != nil {
			return fmt.Errorf("scan %s: %w", src.Identifier, err)
		}
		if report.DuplicateGroups == 0 {
			// Scan can backfill rfc822_message_id even when no duplicate
			// groups are produced (idempotent metadata derivation). Report
			// that side effect so the user knows the scan did something
			// before falling through to the "No duplicates." message.
			if report.BackfilledCount != 0 {
				fmt.Print(engineScoped.FormatReport(report))
			}
			fmt.Println("  No duplicates.")
			fmt.Println()
			continue
		}

		anyRan = true
		fmt.Print(engineScoped.FormatReport(report))
		if cfgScoped.DryRun {
			fmt.Println()
			continue
		}

		if !dedupYes {
			// See runDeduplicateOnce for the rationale on the
			// rfc822-backfill note: scan already performed it
			// (idempotent metadata derivation) regardless of the
			// answer below, so the prompt explicitly scopes "hide N
			// duplicates" to the merge that follows.
			if report.BackfilledCount > 0 {
				fmt.Printf(
					"\nNote: scan already backfilled %d "+
						"rfc822_message_id value(s) for %s from "+
						"stored MIME. This is metadata derivation "+
						"and is kept regardless of your answer.\n",
					report.BackfilledCount, src.Identifier,
				)
			}
			fmt.Printf(
				"\nProceed with deduplication for %s? "+
					"This will hide %d duplicates "+
					"(reversible with --undo). [y/N]: ",
				src.Identifier, report.DuplicateMessages,
			)
			ok, err := readDedupYesNo(cmd)
			if err != nil {
				return err
			}
			if !ok {
				fmt.Println("Skipped.")
				continue
			}
		}

		if !backedUp && !dedupNoBackup {
			backedUp = true
			backupPath := fmt.Sprintf(
				"%s.dedup-backup-%s", dbPath,
				time.Now().Format("20060102-150405"),
			)
			fmt.Printf("Backing up database to %s...\n",
				filepath.Base(backupPath))
			if err := backupDatabase(st, backupPath); err != nil {
				return fmt.Errorf("backup database: %w", err)
			}
		}

		batchID := fmt.Sprintf(
			"dedup-%s-%d-%s-%s",
			time.Now().Format("20060102-150405"),
			src.ID,
			dedup.SanitizeFilenameComponent(src.Identifier),
			randomBatchToken(),
		)
		summary, err := engineScoped.Execute(
			cmd.Context(), report, batchID,
		)
		if err != nil {
			if summary != nil && summary.GroupsMerged > 0 {
				printDedupSummary(summary)
				fmt.Println()
			}
			// Surface the undo hint for any prior sources that DID
			// succeed in this run before returning the error. Without
			// this, a user who hit an error on source N has no
			// visibility into how to undo sources 1..N-1's changes
			// without grepping the slog output.
			printAccumulatedUndoHint(executedBatches)
			return fmt.Errorf("execute %s: %w", src.Identifier, err)
		}
		executedBatches = append(executedBatches, summary.BatchID)
		printDedupSummary(summary)
		fmt.Println()
	}

	if cfgBase.DryRun {
		fmt.Println("\nDry run complete. No changes made.")
	} else if !anyRan {
		fmt.Println("No duplicates found in any source.")
	} else if len(executedBatches) > 1 {
		printAccumulatedUndoHint(executedBatches)
	}
	return nil
}

// printAccumulatedUndoHint prints the multi-batch undo recipe for an
// in-progress per-source dedup run. Called from both the happy path
// (after all sources complete) and the Execute-error path (so a user
// who hit an error mid-loop still sees how to undo what already ran).
// No-op for fewer than 2 batches.
func printAccumulatedUndoHint(executedBatches []string) {
	if len(executedBatches) < 2 {
		return
	}
	var b strings.Builder
	b.WriteString("\nTo undo all of the above:\n  msgvault deduplicate")
	for _, id := range executedBatches {
		fmt.Fprintf(&b, " --undo %s", id)
	}
	b.WriteString("\n")
	fmt.Print(b.String())
}

func runDeduplicateOnce(
	cmd *cobra.Command,
	st *store.Store,
	dbPath string,
	cfgScoped dedup.Config,
	engine *dedup.Engine,
) error {
	fmt.Println("Scanning for duplicate messages...")
	report, err := engine.Scan(cmd.Context())
	if err != nil {
		return fmt.Errorf("scan: %w", err)
	}

	fmt.Print(engine.FormatMethodology())
	fmt.Print(engine.FormatReport(report))

	if cfgScoped.DryRun {
		fmt.Println("\nDry run complete. No changes made.")
		return nil
	}
	if report.DuplicateGroups == 0 {
		fmt.Println("\nNo duplicates found.")
		return nil
	}

	if !dedupYes {
		// Surface the rfc822 backfill that scan already performed so
		// the user knows what state the database is in before they
		// answer. The backfill is idempotent metadata derivation
		// (fills a previously-NULL column from stored MIME, never
		// overwrites or changes content) and is kept regardless of
		// this answer; the prompt and the backup that follows are
		// scoped to the dedup merge itself.
		if report.BackfilledCount > 0 {
			fmt.Printf(
				"\nNote: scan already backfilled %d rfc822_message_id "+
					"value(s) from stored MIME. This is metadata "+
					"derivation and is kept regardless of your answer.\n",
				report.BackfilledCount,
			)
		}
		fmt.Printf(
			"\nProceed with deduplication? This will hide %d "+
				"duplicates (reversible with --undo). [y/N]: ",
			report.DuplicateMessages,
		)
		ok, err := readDedupYesNo(cmd)
		if err != nil {
			return err
		}
		if !ok {
			fmt.Println("Aborted.")
			return nil
		}
	}

	if !dedupNoBackup {
		backupPath := fmt.Sprintf(
			"%s.dedup-backup-%s", dbPath,
			time.Now().Format("20060102-150405"),
		)
		fmt.Printf("Backing up database to %s...\n",
			filepath.Base(backupPath))
		if err := backupDatabase(st, backupPath); err != nil {
			return fmt.Errorf("backup database: %w", err)
		}
	}

	batchID := fmt.Sprintf(
		"dedup-%s-run-%s",
		time.Now().Format("20060102-150405"),
		randomBatchToken(),
	)
	fmt.Println("Merging duplicates...")
	summary, err := engine.Execute(cmd.Context(), report, batchID)
	if err != nil {
		if summary != nil && summary.GroupsMerged > 0 {
			printDedupSummary(summary)
			fmt.Println()
		}
		return fmt.Errorf("execute: %w", err)
	}

	printDedupSummary(summary)
	// The analytics cache picks up dedup hides on the next TUI launch
	// (cacheNeedsBuild detects deleted_at after LastSyncAt and forces a
	// full rebuild). No manual rebuild required.
	return nil
}

func printDedupSummary(summary *dedup.ExecutionSummary) {
	fmt.Printf("\n=== Deduplication Complete ===\n")
	fmt.Printf("Batch ID:            %s\n", summary.BatchID)
	fmt.Printf("Groups merged:       %d\n", summary.GroupsMerged)
	fmt.Printf("Messages pruned:     %d\n", summary.MessagesRemoved)
	fmt.Printf("Labels transferred:  %d\n", summary.LabelsTransferred)
	fmt.Printf("Raw MIME backfilled: %d\n", summary.RawMIMEBackfilled)

	if len(summary.StagedManifests) > 0 {
		fmt.Println("\nStaged deletion manifests (pending):")
		for _, m := range summary.StagedManifests {
			fmt.Printf("  %s  [%s]  %d messages  (%s)\n",
				m.ManifestID, m.SourceType, m.MessageCount, m.Account)
		}
		fmt.Println(
			"\nRun 'msgvault delete-staged --list' to inspect, or " +
				"MSGVAULT_ENABLE_REMOTE_DELETE=1 msgvault delete-staged " +
				"to remove the duplicates from the remote server.",
		)
	}
	fmt.Printf("\nTo undo: msgvault deduplicate --undo %s\n",
		summary.BatchID)
}

func readDedupYesNo(cmd *cobra.Command) (bool, error) {
	reader := bufio.NewReader(cmd.InOrStdin())
	response, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("read confirmation: %w", err)
	}
	response = strings.TrimSpace(strings.ToLower(response))
	return response == "y" || response == "yes", nil
}

// randomBatchToken returns a short random hex token used to disambiguate
// single-run dedup batch IDs from per-source batch IDs that may have been
// generated in the same second.
func randomBatchToken() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%08x", time.Now().UnixNano()&0xffffffff)
	}
	return hex.EncodeToString(b[:])
}

// backupDatabase writes a point-in-time consistent copy of the SQLite
// database to dst using VACUUM INTO. Unlike a file-system copy of the
// main/-wal/-shm triple, this is atomic and handles uncheckpointed WAL
// pages without any external coordination.
func backupDatabase(st *store.Store, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("backup target already exists: %s", dst)
	}
	if _, err := st.DB().Exec("VACUUM INTO ?", dst); err != nil {
		return fmt.Errorf("vacuum into %s: %w", dst, err)
	}
	return nil
}

// loadPerSourceIdentities builds a per-source identity map for the given
// source IDs by calling GetIdentitiesForScope once per source. Addresses
// are normalized via store.NormalizeIdentifierForCompare so the dedup
// engine's lookup uses the same case-aware rule as the store layer:
// email-shaped identities lowercase, synthetic identifiers (Matrix
// MXIDs, chat handles, phone E.164) preserve case. Without this,
// blanket-lowercasing would misclassify case-sensitive synthetic
// identifiers as sent copies.
func loadPerSourceIdentities(st *store.Store, sourceIDs []int64) (map[int64]map[string]struct{}, error) {
	out := make(map[int64]map[string]struct{}, len(sourceIDs))
	for _, id := range sourceIDs {
		addrs, err := st.GetIdentitiesForScope([]int64{id})
		if err != nil {
			return nil, fmt.Errorf("get identities for source %d: %w", id, err)
		}
		if len(addrs) == 0 {
			continue
		}
		normalized := make(map[string]struct{}, len(addrs))
		for addr := range addrs {
			normalized[store.NormalizeIdentifierForCompare(addr)] = struct{}{}
		}
		out[id] = normalized
	}
	return out, nil
}

func printStillRunningWarning(ids []string) {
	if len(ids) == 0 {
		return
	}
	// "Currently executing" specifically — these manifests have already
	// been promoted from pending to in-progress, so they can't be
	// cancelled (the executor will run them to completion). This is a
	// different class of message from a pending-cancel *failure*
	// (which surfaces as a returned error from Undo, not via this
	// warning).
	fmt.Printf(
		"\nWarning: the following deletion manifests are currently " +
			"executing\nand cannot be cancelled (the executor will run " +
			"them to completion):\n",
	)
	for _, id := range ids {
		fmt.Printf("  - %s\n", id)
	}
}

func init() {
	rootCmd.AddCommand(deduplicateCmd)
	deduplicateCmd.Flags().BoolVar(&dedupDryRun, "dry-run", false,
		"Scan and report only; do not modify data")
	deduplicateCmd.Flags().BoolVar(&dedupNoBackup, "no-backup", false,
		"Skip database backup before merging (backup covers pre-dedup state for all sources, not per-batch)")
	deduplicateCmd.Flags().StringVar(&dedupPrefer, "prefer", "",
		"Comma-separated source type preference order "+
			"(default: gmail,imap,mbox,emlx,hey)")
	deduplicateCmd.Flags().BoolVar(&dedupContentHash, "content-hash", false,
		"Also detect duplicates by normalized raw MIME content")
	deduplicateCmd.Flags().StringArrayVar(&dedupUndo, "undo", nil,
		"Undo a previous dedup run by batch ID "+
			"(repeat for multiple batches; failures on one batch do not "+
			"skip later batches and errors are aggregated; cannot be "+
			"combined with --account or --collection)")
	deduplicateCmd.Flags().StringVar(&dedupAccount, "account", "",
		"Scope dedup to one account; never crosses source boundaries")
	deduplicateCmd.Flags().StringVar(&dedupCollection, "collection", "",
		"Dedup across every member of a collection; opts into "+
			"cross-source comparison (use --dry-run to preview)")
	deduplicateCmd.MarkFlagsMutuallyExclusive("account", "collection")
	// --undo executes a write; --dry-run promises no writes. Reject the
	// combination explicitly rather than silently letting --undo win.
	deduplicateCmd.MarkFlagsMutuallyExclusive("dry-run", "undo")
	// --undo is keyed by batch ID; the batch already records its scope.
	// Combining --undo with --account/--collection is meaningless and
	// would force a stale-account lookup before reaching the undo path.
	deduplicateCmd.MarkFlagsMutuallyExclusive("undo", "account")
	deduplicateCmd.MarkFlagsMutuallyExclusive("undo", "collection")
	deduplicateCmd.Flags().BoolVar(&dedupDeleteFromSourceSrvr,
		"delete-dups-from-source-server", false,
		"DESTRUCTIVE: stage pruned duplicates for remote deletion "+
			"(execution requires MSGVAULT_ENABLE_REMOTE_DELETE=1)")
	deduplicateCmd.Flags().BoolVarP(&dedupYes, "yes", "y", false,
		"Skip confirmation prompt")
	// --undo restores rows from a recorded batch; none of the
	// scan/merge/stage flags below apply. Reject the combinations
	// explicitly so a user invoking
	// `msgvault deduplicate --undo X --delete-dups-from-source-server`
	// gets an error instead of having the destructive flag silently
	// ignored.
	deduplicateCmd.MarkFlagsMutuallyExclusive("undo", "delete-dups-from-source-server")
	deduplicateCmd.MarkFlagsMutuallyExclusive("undo", "prefer")
	deduplicateCmd.MarkFlagsMutuallyExclusive("undo", "content-hash")
	deduplicateCmd.MarkFlagsMutuallyExclusive("undo", "no-backup")
	deduplicateCmd.MarkFlagsMutuallyExclusive("undo", "yes")
}
