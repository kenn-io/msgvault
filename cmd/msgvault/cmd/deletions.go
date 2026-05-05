package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/wesm/msgvault/internal/deletion"
	"github.com/wesm/msgvault/internal/oauth"
	"github.com/wesm/msgvault/internal/store"
)

var listDeletionsCmd = &cobra.Command{
	Use:   "list-deletions",
	Short: "List pending and recent deletion batches",
	Long: `List all deletion batches across all statuses.

Shows pending, in-progress, completed, and failed deletion batches
with their ID, status, message count, and creation date.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		deletionsDir := filepath.Join(cfg.Data.DataDir, "deletions")
		manager, err := deletion.NewManager(deletionsDir)
		if err != nil {
			return fmt.Errorf("create manager: %w", err)
		}
		return runListDeletionsForManager(manager, cmd.OutOrStdout())
	},
}

func runListDeletionsForManager(mgr *deletion.Manager, w io.Writer) error {
	pending, err := mgr.ListPending()
	if err != nil {
		return fmt.Errorf("list pending deletions: %w", err)
	}
	inProgress, err := mgr.ListInProgress()
	if err != nil {
		return fmt.Errorf("list in-progress deletions: %w", err)
	}
	completed, err := mgr.ListCompleted()
	if err != nil {
		return fmt.Errorf("list completed deletions: %w", err)
	}
	failed, err := mgr.ListFailed()
	if err != nil {
		return fmt.Errorf("list failed deletions: %w", err)
	}
	cancelled, err := mgr.ListCancelled()
	if err != nil {
		return fmt.Errorf("list cancelled deletions: %w", err)
	}

	if len(pending) == 0 && len(inProgress) == 0 && len(completed) == 0 && len(failed) == 0 && len(cancelled) == 0 {
		_, _ = fmt.Fprintln(w, "No deletion batches found.")
		_, _ = fmt.Fprintln(w, "\nTo stage messages for deletion, use the TUI or create a manifest manually.")
		return nil
	}

	printManifestTable := func(status string, manifests []*deletion.Manifest) {
		if len(manifests) == 0 {
			return
		}
		_, _ = fmt.Fprintf(w, "\n%s:\n", status)
		_, _ = fmt.Fprintf(w, "  %-25s  %-10s  %10s  %s\n", "ID", "Status", "Messages", "Created")
		_, _ = fmt.Fprintf(w, "  %-25s  %-10s  %10s  %s\n", "---", "------", "--------", "-------")
		for _, m := range manifests {
			_, _ = fmt.Fprintf(w, "  %-25s  %-10s  %10d  %s\n",
				truncate(m.ID, 25),
				m.Status,
				len(m.GmailIDs),
				m.CreatedAt.Format("2006-01-02 15:04"),
			)
		}
	}

	printManifestTable("Pending", pending)
	printManifestTable("In Progress", inProgress)
	printManifestTable("Completed (recent)", limitManifests(completed, 10))
	printManifestTable("Failed", failed)
	printManifestTable("Cancelled (recent)", limitManifests(cancelled, 10))

	return nil
}

var showDeletionCmd = &cobra.Command{
	Use:   "show-deletion <batch-id>",
	Short: "Show details of a deletion batch",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		batchID := args[0]

		deletionsDir := filepath.Join(cfg.Data.DataDir, "deletions")
		manager, err := deletion.NewManager(deletionsDir)
		if err != nil {
			return fmt.Errorf("create manager: %w", err)
		}

		manifest, _, err := manager.GetManifest(batchID)
		if err != nil {
			return fmt.Errorf("get manifest: %w", err)
		}

		fmt.Print(manifest.FormatSummary())
		return nil
	},
}

var cancelAll bool

var cancelDeletionCmd = &cobra.Command{
	Use:   "cancel-deletion [batch-id]",
	Short: "Cancel pending or in-progress deletion batches",
	Long: `Cancel deletion batches by ID, or use --all to cancel all pending and in-progress batches.

Examples:
  msgvault cancel-deletion 20260202-195132-Senders-wingide-user
  msgvault cancel-deletion --all`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if cancelAll && len(args) > 0 {
			return fmt.Errorf("cannot use --all with a batch ID argument")
		}

		deletionsDir := filepath.Join(cfg.Data.DataDir, "deletions")
		manager, err := deletion.NewManager(deletionsDir)
		if err != nil {
			return fmt.Errorf("create manager: %w", err)
		}

		if cancelAll {
			count := 0
			var listErrors []error
			for _, listFn := range []func() ([]*deletion.Manifest, error){
				manager.ListPending, manager.ListInProgress,
			} {
				manifests, err := listFn()
				if err != nil {
					listErrors = append(listErrors, err)
					continue
				}
				for _, m := range manifests {
					if err := manager.CancelManifest(m.ID); err != nil {
						fmt.Printf("  Failed to cancel %s: %v\n", m.ID, err)
					} else {
						fmt.Printf("  Cancelled: %s\n", m.ID)
						count++
					}
				}
			}
			if len(listErrors) > 0 {
				for _, e := range listErrors {
					fmt.Fprintf(os.Stderr, "Warning: failed to list batches: %v\n", e)
				}
			}
			if count == 0 {
				if len(listErrors) > 0 {
					return fmt.Errorf("could not list batches to cancel")
				}
				fmt.Println("No pending or in-progress batches to cancel.")
			} else {
				fmt.Printf("Cancelled %d batch(es).\n", count)
			}
			return nil
		}

		if len(args) == 0 {
			// List available batches to help the user
			fmt.Println("No batch ID specified. Available batches:")
			fmt.Println()
			found := false
			for _, item := range []struct {
				label  string
				listFn func() ([]*deletion.Manifest, error)
			}{
				{"Pending", manager.ListPending},
				{"In Progress", manager.ListInProgress},
			} {
				manifests, err := item.listFn()
				if err != nil || len(manifests) == 0 {
					continue
				}
				for _, m := range manifests {
					fmt.Printf("  [%s] %s (%d messages)\n", item.label, m.ID, len(m.GmailIDs))
					found = true
				}
			}
			if !found {
				fmt.Println("  (none)")
			}
			fmt.Println()
			return fmt.Errorf("provide a batch ID or use --all")
		}

		batchID := args[0]
		if err := manager.CancelManifest(batchID); err != nil {
			return fmt.Errorf("cancel manifest: %w", err)
		}

		fmt.Printf("Cancelled deletion batch: %s\n", batchID)
		return nil
	},
}

var (
	// deletePermanent opts in to permanent batch deletion. Default is
	// trash (30-day Gmail recovery), which is the safer choice for the
	// v1 release: every other rung of the deletion progression
	// (dedup-hide, local hard delete) is locally reversible, so the
	// remote rung should be too unless the user explicitly says
	// otherwise.
	deletePermanent bool
	deleteYes       bool
	deleteDryRun    bool
	deleteList      bool
	deleteAccount   string
)

// remoteDeleteEnvVar gates execution of staged deletions against Gmail
// for the v1 release. Staging, listing, and inspecting manifests stay
// available unconditionally so the rest of the pipeline can be exercised;
// only the destructive Gmail-API call is gated.
const remoteDeleteEnvVar = "MSGVAULT_ENABLE_REMOTE_DELETE"

func remoteDeleteEnabled() bool {
	return os.Getenv(remoteDeleteEnvVar) == "1"
}

var deleteStagedCmd = &cobra.Command{
	Use:   "delete-staged [batch-id]",
	Short: "Execute staged deletions",
	Long: `Execute pending deletion batches.

By default, messages are moved to Gmail trash (recoverable for 30 days).
Use --permanent for batch-API permanent deletion (fast, no recovery).
The default is trash because every other rung of the deletion progression
in msgvault is locally reversible; the remote rung is too unless the user
explicitly opts out of recoverability.

Execution is gated for the v1 release. Set MSGVAULT_ENABLE_REMOTE_DELETE=1 to
opt in. Read-only modes (--list, --dry-run) work without the gate.

Examples:
  msgvault delete-staged --list         # Show staged batches (always allowed)
  msgvault delete-staged --dry-run      # Preview without executing (always allowed)
  MSGVAULT_ENABLE_REMOTE_DELETE=1 msgvault delete-staged
  MSGVAULT_ENABLE_REMOTE_DELETE=1 msgvault delete-staged batch-123
  MSGVAULT_ENABLE_REMOTE_DELETE=1 msgvault delete-staged --permanent
  MSGVAULT_ENABLE_REMOTE_DELETE=1 msgvault delete-staged --yes`,
	RunE: func(cmd *cobra.Command, args []string) error {
		deletionsDir := filepath.Join(cfg.Data.DataDir, "deletions")
		manager, err := deletion.NewManager(deletionsDir)
		if err != nil {
			return fmt.Errorf("create manager: %w", err)
		}

		// Get manifests to execute
		var manifests []*deletion.Manifest
		if len(args) > 0 {
			manifest, _, err := manager.GetManifest(args[0])
			if err != nil {
				return fmt.Errorf("get manifest: %w", err)
			}
			if manifest.Status != deletion.StatusPending && manifest.Status != deletion.StatusInProgress {
				return fmt.Errorf("batch %s is %s, cannot execute", args[0], manifest.Status)
			}
			manifests = append(manifests, manifest)
		} else {
			pending, err := manager.ListPending()
			if err != nil {
				return fmt.Errorf("list pending: %w", err)
			}
			inProgress, err := manager.ListInProgress()
			if err != nil {
				return fmt.Errorf("list in progress: %w", err)
			}
			manifests = append(manifests, pending...)
			manifests = append(manifests, inProgress...)
		}

		if len(manifests) == 0 {
			fmt.Println("No staged deletions.")
			return nil
		}

		// --list: show staged batches (pending + in-progress) and exit
		if deleteList {
			fmt.Printf("Staged deletions: %d batch(es)\n\n", len(manifests))
			fmt.Printf("  %-25s  %-12s  %10s  %s\n", "ID", "Status", "Messages", "Description")
			fmt.Printf("  %-25s  %-12s  %10s  %s\n", "---", "------", "--------", "-----------")
			totalMessages := 0
			for _, m := range manifests {
				fmt.Printf("  %-25s  %-12s  %10d  %s\n",
					truncate(m.ID, 25),
					m.Status,
					len(m.GmailIDs),
					truncate(m.Description, 40),
				)
				totalMessages += len(m.GmailIDs)
			}
			fmt.Printf("\nTotal: %d messages across %d batch(es)\n", totalMessages, len(manifests))
			fmt.Println("\nUse 'msgvault delete-staged' to execute, or 'msgvault show-deletion <id>' for details.")
			return nil
		}

		// Calculate totals
		totalMessages := 0
		for _, m := range manifests {
			totalMessages += len(m.GmailIDs)
		}

		// Show summary
		method := "trash (30-day recovery)"
		if deletePermanent {
			method = "PERMANENT DELETE (fast, no recovery)"
		}

		fmt.Printf("Deletion Summary:\n")
		fmt.Printf("  Batches:  %d\n", len(manifests))
		fmt.Printf("  Messages: %d\n", totalMessages)
		fmt.Printf("  Method:   %s\n", method)
		fmt.Println()

		// Show batch details
		for _, m := range manifests {
			fmt.Printf("  %s: %d messages - %s\n", m.ID, len(m.GmailIDs), m.Description)
		}
		fmt.Println()

		if deleteDryRun {
			fmt.Println("Dry run - no messages will be deleted.")
			return nil
		}

		// Gate the destructive Gmail-API call for the v1 release.
		// --list and --dry-run already returned above without hitting this.
		if !remoteDeleteEnabled() {
			return fmt.Errorf(
				"remote deletion is gated in this release; "+
					"set %s=1 to opt in "+
					"(use 'msgvault delete-staged --list' or --dry-run to inspect "+
					"staged batches without executing)",
				remoteDeleteEnvVar,
			)
		}

		// Require confirmation
		if deletePermanent {
			ok, err := confirmDestructive(cmd.InOrStdin(), cmd.OutOrStdout(), ConfirmModePermanent)
			if err != nil {
				return err
			}
			if !ok {
				return nil
			}
		} else if !deleteYes {
			// Trash path is reversible (~30-day Gmail recovery), so the
			// shared confirmDestructive helper's "irreversible" wording
			// would be misleading here. Hand-rolled prompt matches the
			// action's reversibility — accepts y/Y/yes/Yes for parity
			// with the shared helper's input contract.
			out := cmd.OutOrStdout()
			_, _ = fmt.Fprint(out, "Proceed with deletion? Messages move to Gmail/Trash (recoverable ~30 days). [y/N]: ")
			scanner := bufio.NewScanner(cmd.InOrStdin())
			if !scanner.Scan() {
				if err := scanner.Err(); err != nil {
					return fmt.Errorf("read confirmation: %w", err)
				}
				_, _ = fmt.Fprintln(out, "Cancelled.")
				return nil
			}
			answer := strings.TrimSpace(strings.ToLower(scanner.Text()))
			if answer != "y" && answer != "yes" {
				_, _ = fmt.Fprintln(out, "Cancelled.")
				return nil
			}
		}

		// Open database early so we can resolve account identifiers.
		dbPath := cfg.DatabaseDSN()
		s, err := store.Open(dbPath)
		if err != nil {
			return fmt.Errorf("open database: %w", err)
		}
		defer func() { _ = s.Close() }()

		if err := s.InitSchema(); err != nil {
			return fmt.Errorf("init schema: %w", err)
		}
		if err := runStartupMigrations(s); err != nil {
			return fmt.Errorf("startup migrations: %w", err)
		}

		// Collect unique accounts from manifests
		accountSet := make(map[string]bool)
		for _, m := range manifests {
			if m.Filters.Account != "" {
				accountSet[m.Filters.Account] = true
			}
		}

		// Determine which account to use
		account := deleteAccount
		if account == "" {
			accounts := make([]string, 0, len(accountSet))
			for a := range accountSet {
				accounts = append(accounts, a)
			}

			if len(accounts) == 0 {
				return fmt.Errorf("no account in deletion manifest - use --account flag")
			} else if len(accounts) == 1 {
				account = accounts[0]
			} else {
				return fmt.Errorf("multiple accounts in pending batches (%v) - use --account flag to specify which account", accounts)
			}
		} else {
			// Resolve the user-supplied value to a source.
			// IMAP identifiers are URLs (imaps://user@host:port)
			// but the user may pass the email/display name.
			resolved, err := s.GetSourcesByIdentifierOrDisplayName(account)
			if err != nil {
				return fmt.Errorf("look up source for %s: %w", account, err)
			}
			var syncable []*store.Source
			for _, c := range resolved {
				if c.SourceType == "gmail" || c.SourceType == "imap" {
					syncable = append(syncable, c)
				}
			}
			if len(syncable) == 0 {
				return fmt.Errorf("no gmail or imap source found for %s", account)
			}
			if len(syncable) > 1 {
				var types []string
				for _, c := range syncable {
					types = append(types, fmt.Sprintf("%s (%s)", c.Identifier, c.SourceType))
				}
				return fmt.Errorf("multiple accounts match %q: %s\nUse the full identifier with --account to disambiguate", account, strings.Join(types, ", "))
			}
			found := syncable[0]
			// Canonicalize to stored identifier so manifest
			// comparisons work for IMAP display-name lookups.
			account = found.Identifier

			// Verify all manifests match the resolved account
			for _, m := range manifests {
				if m.Filters.Account != "" && m.Filters.Account != account {
					return fmt.Errorf("batch %s is for account %s, not %s - filter batches by account or execute separately", m.ID, m.Filters.Account, account)
				}
			}
		}

		// Set up context with cancellation
		ctx, cancel := context.WithCancel(cmd.Context())
		defer cancel()

		// Handle Ctrl+C gracefully
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-sigChan
			fmt.Println("\nInterrupted. Saving checkpoint...")
			cancel()
		}()

		// Look up the source to determine account type (gmail vs imap).
		sources, err := s.GetSourcesByIdentifier(account)
		if err != nil {
			return fmt.Errorf("look up source for %s: %w", account, err)
		}
		var src *store.Source
		for _, candidate := range sources {
			if candidate.SourceType == "gmail" || candidate.SourceType == "imap" {
				src = candidate
				break
			}
		}
		if src == nil {
			return fmt.Errorf("no gmail or imap source found for %s", account)
		}

		// For Gmail, handle scope escalation before building the client.
		// buildAPIClient uses standard scopes; deletion may need elevated ones.
		// Service-account flows get scopes via the JWT assertion (no stored
		// token), so the scope-escalation prompt only applies to browser OAuth.
		var clientSecretsPath string
		if src.SourceType == "gmail" {
			if !cfg.OAuth.HasAnyConfig() {
				return errOAuthNotConfigured()
			}
			appName := sourceOAuthApp(src)
			isServiceAccount := cfg.OAuth.ServiceAccountKeyFor(appName) != ""

			if !isServiceAccount {
				clientSecretsPath, err = cfg.OAuth.ClientSecretsFor(appName)
				if err != nil {
					return err
				}

				needsBatchDelete := deletePermanent
				if needsBatchDelete {
					requiredScopes := oauth.ScopesDeletion
					oauthMgr, err := oauth.NewManagerWithScopes(clientSecretsPath, cfg.TokensDir(), logger, requiredScopes)
					if err != nil {
						return wrapOAuthError(fmt.Errorf("create oauth manager: %w", err))
					}
					if !oauthMgr.HasScope(account, "https://mail.google.com/") && oauthMgr.HasScopeMetadata(account) {
						if err := promptScopeEscalation(ctx, oauthMgr, account, needsBatchDelete, clientSecretsPath); err != nil {
							if errors.Is(err, errUserCanceled) {
								return nil
							}
							return err
						}
					}
				}
			}
		}

		// Build API client — reuses the same factory as sync.
		getOAuthMgr := func(appName string) (*oauth.Manager, error) {
			secretsPath := clientSecretsPath
			if secretsPath == "" {
				var err error
				secretsPath, err = cfg.OAuth.ClientSecretsFor(appName)
				if err != nil {
					return nil, err
				}
			}
			scopes := oauth.Scopes
			if deletePermanent {
				scopes = oauth.ScopesDeletion
			}
			return oauth.NewManagerWithScopes(secretsPath, cfg.TokensDir(), logger, scopes)
		}
		// For permanent deletion (not trash), service-account flows need the
		// elevated mail.google.com scope; trash-only uses the standard set.
		saScopes := oauth.Scopes
		if deletePermanent {
			saScopes = oauth.ScopesDeletion
		}
		client, err := buildAPIClient(ctx, src, getOAuthMgr, saScopes)
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()

		// Create executor
		executor := deletion.NewExecutor(manager, s, client).
			WithLogger(logger).
			WithProgress(&CLIDeletionProgress{})

		// Execute each manifest
		for i, m := range manifests {
			if i > 0 {
				fmt.Println()
			}
			fmt.Printf("  [%d/%d] %s (%d messages)\n", i+1, len(manifests), m.Description, len(m.GmailIDs))

			var execErr error
			// For in-progress manifests, honor the stored method to avoid
			// accidentally switching between trash and permanent mid-batch.
			useTrash := !deletePermanent
			if m.Status == deletion.StatusInProgress && m.Execution != nil {
				useTrash = (m.Execution.Method == deletion.MethodTrash)
			}

			if useTrash {
				// Use individual trash calls (slower but recoverable)
				opts := deletion.DefaultExecuteOptions()
				opts.Method = deletion.MethodTrash
				execErr = executor.Execute(ctx, m.ID, opts)
			} else {
				// Use batch delete for permanent deletion (fast - 1 API call per 1000 messages)
				execErr = executor.ExecuteBatch(ctx, m.ID)
			}

			if execErr != nil {
				if ctx.Err() != nil {
					fmt.Println("\nInterrupted. Run again to resume.")
					return nil
				}

				// Check if this is a scope error - offer to re-authorize (Gmail only)
				if src.SourceType == "gmail" && isInsufficientScopeError(execErr) {
					if cfg.OAuth.ServiceAccountKeyFor(sourceOAuthApp(src)) != "" {
						return fmt.Errorf(
							"service account lacks required Gmail deletion scope for %s: "+
								"authorize https://mail.google.com/ for the service account client "+
								"in Google Admin Console, then run delete-staged again",
							account,
						)
					}
					oauthMgr, mgrErr := getOAuthMgr(sourceOAuthApp(src))
					if mgrErr != nil {
						return mgrErr
					}
					if err := promptScopeEscalation(ctx, oauthMgr, account, !useTrash, clientSecretsPath); err != nil {
						if errors.Is(err, errUserCanceled) {
							return nil
						}
						return err
					}
					fmt.Println("Run delete-staged again to continue.")
					return nil
				}

				logger.Warn("deletion failed", "batch", m.ID, "error", execErr)
				continue
			}
		}

		fmt.Println("\nDeletion complete!")

		// Refresh analytics cache to reflect deleted messages.
		rebuildCacheAfterWrite(dbPath)

		return nil
	},
}

// isTTY reports whether stdout is connected to a terminal.
func isTTY() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// CLIDeletionProgress reports deletion progress to the terminal.
type CLIDeletionProgress struct {
	total        int
	resumeOffset int // messages already processed before this run
	startTime    time.Time
	lastPrint    time.Time
	tty          bool
}

func (p *CLIDeletionProgress) OnStart(total, alreadyProcessed int) {
	p.total = total
	p.resumeOffset = alreadyProcessed
	p.startTime = time.Now()
	p.lastPrint = time.Time{} // Force first print
	p.tty = isTTY()
	// Show initial progress immediately so it doesn't look like it's hanging
	p.OnProgress(alreadyProcessed, 0, 0)
}

func (p *CLIDeletionProgress) formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

func (p *CLIDeletionProgress) OnProgress(processed, succeeded, failed int) {
	if p.total <= 0 {
		return
	}
	if time.Since(p.lastPrint) < 500*time.Millisecond {
		return
	}
	p.lastPrint = time.Now()

	pct := float64(processed) / float64(p.total) * 100
	elapsed := time.Since(p.startTime)

	bar := p.progressBar(pct, 30)

	var eta string
	processedThisRun := processed - p.resumeOffset
	if processedThisRun > 0 && processed < p.total {
		remaining := time.Duration(float64(elapsed) / float64(processedThisRun) * float64(p.total-processed))
		eta = p.formatDuration(remaining) + " remaining"
	} else if processed >= p.total {
		eta = p.formatDuration(elapsed) + " elapsed"
	} else {
		eta = "calculating..."
	}

	status := fmt.Sprintf("  %s %.1f%%  %d/%d", bar, pct, processed, p.total)
	if failed > 0 {
		status += fmt.Sprintf("  (%d failed)", failed)
	}
	status += fmt.Sprintf("  %s", eta)
	if p.tty {
		fmt.Printf("\r\033[K%s", status)
	} else {
		fmt.Println(status)
	}
}

func (p *CLIDeletionProgress) progressBar(pct float64, width int) string {
	filled := int(pct / 100 * float64(width))
	if filled > width {
		filled = width
	}
	bar := make([]byte, width)
	for i := range bar {
		if i < filled {
			bar[i] = '#'
		} else {
			bar[i] = '-'
		}
	}
	return "[" + string(bar) + "]"
}

func (p *CLIDeletionProgress) OnComplete(succeeded, failed int) {
	elapsed := time.Since(p.startTime)
	// Clear the progress line
	if p.tty {
		fmt.Print("\r\033[K")
	}
	if failed == 0 {
		fmt.Printf("  Done: %d deleted in %s\n", succeeded, p.formatDuration(elapsed))
	} else {
		fmt.Printf("  Done: %d deleted, %d failed in %s\n", succeeded, failed, p.formatDuration(elapsed))
	}
}

// Helper functions
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}

func limitManifests(manifests []*deletion.Manifest, max int) []*deletion.Manifest {
	if len(manifests) <= max {
		return manifests
	}
	return manifests[:max]
}

// errUserCanceled is returned when the user declines scope escalation.
var errUserCanceled = errors.New("user canceled scope escalation")

// promptScopeEscalation prompts the user to re-authorize with elevated scopes.
// It deletes the old token, runs the OAuth browser flow, and returns nil on
// success. The caller should re-create the OAuth manager after this returns.
func promptScopeEscalation(ctx context.Context, oauthMgr *oauth.Manager, account string, batchDelete bool, clientSecretsPath string) error {
	fmt.Println("\n" + strings.Repeat("=", 70))
	fmt.Println("PERMISSION UPGRADE REQUIRED")
	fmt.Println(strings.Repeat("=", 70))
	fmt.Println()

	var requiredScopes []string
	if !batchDelete {
		fmt.Println("Trash deletion requires Gmail modify permissions.")
		fmt.Println()
		fmt.Println("Your current OAuth token doesn't include the gmail.modify scope.")
		fmt.Println("To proceed, msgvault needs to re-authorize with modify access.")
		requiredScopes = oauth.Scopes
	} else {
		fmt.Println("Batch deletion requires elevated Gmail permissions.")
		fmt.Println()
		fmt.Println("Your current OAuth token was granted with limited permissions that")
		fmt.Println("don't include batch delete. To proceed, msgvault needs to:")
		fmt.Println()
		fmt.Println("  1. Delete your existing OAuth token")
		fmt.Println("  2. Re-authorize with full Gmail access (mail.google.com scope)")
		fmt.Println()
		fmt.Println("This elevated permission allows msgvault to permanently delete")
		fmt.Println("messages in bulk. You can revoke access anytime at:")
		fmt.Println("  https://myaccount.google.com/permissions")
		requiredScopes = oauth.ScopesDeletion
	}
	fmt.Println()

	fmt.Print("Upgrade permissions now? [y/N]: ")
	var response string
	_, _ = fmt.Scanln(&response)
	if response != "y" && response != "Y" {
		if batchDelete {
			fmt.Println("Cancelled. Drop --permanent to use trash deletion without elevated permissions.")
		} else {
			fmt.Println("Cancelled.")
		}
		return errUserCanceled
	}

	// Delete old token and re-authorize
	fmt.Println("\nDeleting old token...")
	if err := oauthMgr.DeleteToken(account); err != nil {
		return fmt.Errorf("delete token: %w", err)
	}

	fmt.Println("Starting OAuth flow...")
	fmt.Println()

	newMgr, err := oauth.NewManagerWithScopes(clientSecretsPath, cfg.TokensDir(), logger, requiredScopes)
	if err != nil {
		return fmt.Errorf("create oauth manager: %w", err)
	}

	if err := newMgr.Authorize(ctx, account); err != nil {
		return fmt.Errorf("authorize: %w", err)
	}

	fmt.Println("\nAuthorization successful!")
	return nil
}

// isInsufficientScopeError checks if an error is due to missing OAuth scopes.
func isInsufficientScopeError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "ACCESS_TOKEN_SCOPE_INSUFFICIENT") ||
		strings.Contains(msg, "insufficient authentication scopes") ||
		strings.Contains(msg, "Insufficient Permission")
}

func init() {
	deleteStagedCmd.Flags().BoolVar(&deletePermanent, "permanent", false, "DESTRUCTIVE: permanently delete via batch API instead of moving to trash (fast, no recovery)")
	deleteStagedCmd.Flags().BoolVarP(&deleteYes, "yes", "y", false, "Skip confirmation")
	deleteStagedCmd.Flags().BoolVar(&deleteDryRun, "dry-run", false, "Show what would be deleted")
	deleteStagedCmd.Flags().BoolVarP(&deleteList, "list", "l", false, "List staged batches without executing")
	deleteStagedCmd.Flags().StringVar(&deleteAccount, "account", "", "Account to use (Gmail or IMAP)")

	deleteStagedCmd.MarkFlagsMutuallyExclusive("permanent", "yes")
	rootCmd.AddCommand(listDeletionsCmd)
	rootCmd.AddCommand(showDeletionCmd)
	cancelDeletionCmd.Flags().BoolVar(&cancelAll, "all", false, "Cancel all pending and in-progress batches")
	rootCmd.AddCommand(cancelDeletionCmd)
	rootCmd.AddCommand(deleteStagedCmd)
}
