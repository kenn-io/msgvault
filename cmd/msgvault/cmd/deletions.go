package cmd

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/deletion"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/store"
)

var listDeletionsJSON bool

var listDeletionsCmd = &cobra.Command{
	Use:   "list-deletions",
	Short: "List pending and recent deletion batches",
	Long: `List all deletion batches across all statuses.

Shows pending, in-progress, completed, and failed deletion batches
with their ID, status, message count, and creation date. Use --json for
full, untruncated batch IDs suitable for show-deletion and delete-staged.`,
	Args: cobra.NoArgs,
	RunE: runListDeletions,
}

func runListDeletions(cmd *cobra.Command, args []string) error {
	if !isDaemonCLISubprocess() {
		return runDaemonCLICommandHTTPFromCobra(cmd, args)
	}
	deletionsDir := filepath.Join(cfg.Data.DataDir, "deletions")
	manager, err := deletion.NewManager(deletionsDir)
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}
	return runListDeletionsForManager(manager, cmd.OutOrStdout())
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

	if listDeletionsJSON {
		return writeDeletionsJSON(w, pending, inProgress, completed, failed, cancelled)
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
		// Batch IDs are the key users feed to show-deletion/delete-staged,
		// so never truncate them; tabwriter keeps the columns aligned even
		// when IDs vary in length.
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "  ID\tStatus\tMessages\tCreated")
		_, _ = fmt.Fprintln(tw, "  --\t------\t--------\t-------")
		for _, m := range manifests {
			_, _ = fmt.Fprintf(tw, "  %s\t%s\t%d\t%s\n",
				m.ID,
				m.Status,
				len(m.GmailIDs),
				m.CreatedAt.Format("2006-01-02 15:04"),
			)
		}
		_ = tw.Flush()
	}

	printManifestTable("Pending", pending)
	printManifestTable("In Progress", inProgress)
	printManifestTable("Completed (recent)", limitManifests(completed, 10))
	printManifestTable("Failed", failed)
	printManifestTable("Cancelled (recent)", limitManifests(cancelled, 10))

	return nil
}

// writeDeletionsJSON emits every deletion batch as a JSON array with full
// (untruncated) IDs so the output can be fed directly to show-deletion or
// delete-staged. Unlike the table view it is not limited to recent batches.
func writeDeletionsJSON(w io.Writer, groups ...[]*deletion.Manifest) error {
	out := make([]map[string]any, 0)
	for _, manifests := range groups {
		for _, m := range manifests {
			out = append(out, map[string]any{
				"id":            m.ID,
				"status":        m.Status,
				"message_count": len(m.GmailIDs),
				"description":   m.Description,
				"account":       m.Filters.Account,
				"created_at":    m.CreatedAt.Format(time.RFC3339),
			})
		}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

var showDeletionCmd = &cobra.Command{
	Use:   "show-deletion <batch-id>",
	Short: "Show details of a deletion batch",
	Args:  cobra.ExactArgs(1),
	RunE:  runShowDeletion,
}

func runShowDeletion(cmd *cobra.Command, args []string) error {
	batchID := strings.TrimSpace(args[0])
	if batchID == "" {
		return errors.New("batch ID is required")
	}
	if !isDaemonCLISubprocess() {
		return runDaemonCLICommandHTTPFromCobra(cmd, args)
	}

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
	RunE: runCancelDeletion,
}

func runCancelDeletion(cmd *cobra.Command, args []string) error {
	if cancelAll && len(args) > 0 {
		return usageErr(cmd, errors.New("cannot use --all with a batch ID argument"))
	}
	if !isDaemonCLISubprocess() {
		return runDaemonCLICommandHTTPFromCobra(cmd, args)
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
				return errors.New("could not list batches to cancel")
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
		return usageErr(cmd, errors.New("provide a batch ID or use --all"))
	}

	batchID := args[0]
	if err := manager.CancelManifest(batchID); err != nil {
		return fmt.Errorf("cancel manifest: %w", err)
	}

	fmt.Printf("Cancelled deletion batch: %s\n", batchID)
	return nil
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
	// deletePlannedBatchIDs is an internal daemon-runner guard. The
	// foreground CLI receives this exact set from the planning endpoint after
	// showing the summary and confirmation prompt, then passes it to the
	// daemon subprocess so execution cannot sweep in newly staged batches.
	deletePlannedBatchIDs []string
)

const (
	deleteStagedConfirmedFlag                = "confirmed"
	deleteStagedSkipPreludeFlag              = "skip-prelude"
	deleteStagedPlannedBatchFlag             = "planned-batch"
	deleteStagedPlanFingerprintFlag          = "plan-fingerprint"
	deleteStagedScopeEscalationConfirmedFlag = "scope-escalation-confirmed"
	deleteStagedConfirmModePermanent         = "permanent"
	deleteStagedConfirmModeTrash             = "trash"
	deleteStagedScopeEscalationHeadline      = "PERMISSION UPGRADE REQUIRED"
)

// remoteDeleteEnvVar gates execution of staged deletions against Gmail
// for the v1 release. Staging, listing, and inspecting manifests stay
// available unconditionally so the rest of the pipeline can be exercised;
// only the destructive Gmail-API call is gated.
const remoteDeleteEnvVar = "MSGVAULT_ENABLE_REMOTE_DELETE"

func remoteDeleteEnabled() bool {
	return os.Getenv(remoteDeleteEnvVar) == "1"
}

type deleteStagedPlanOptions struct {
	BatchID             string
	PlannedBatchIDs     []string
	Permanent           bool
	Yes                 bool
	DryRun              bool
	List                bool
	Account             string
	RemoteDeleteEnabled bool
}

type deleteStagedPlan struct {
	Manager                   *deletion.Manager
	Manifests                 []*deletion.Manifest
	PlannedBatchIDs           []string
	PlanFingerprint           string
	Stdout                    string
	NeedsExecution            bool
	NeedsConfirmation         bool
	ConfirmationMode          string
	NeedsScopeEscalation      bool
	ScopeEscalationHeadline   string
	ScopeEscalationBodyLines  []string
	ScopeEscalationCancelHint string
	ScopeEscalationAccount    string
	ScopeEscalationOAuthApp   string
	BlockedError              string
	RemoteDeleteEnvVar        string
}

func buildDeleteStagedPlan(opts deleteStagedPlanOptions) (deleteStagedPlan, error) {
	deletionsDir := filepath.Join(cfg.Data.DataDir, "deletions")
	manager, err := deletion.NewManager(deletionsDir)
	if err != nil {
		return deleteStagedPlan{}, fmt.Errorf("create manager: %w", err)
	}

	var manifests []*deletion.Manifest
	switch {
	case len(opts.PlannedBatchIDs) > 0:
		for _, batchID := range opts.PlannedBatchIDs {
			manifest, err := loadExecutableDeletionManifest(manager, batchID)
			if err != nil {
				return deleteStagedPlan{}, err
			}
			manifests = append(manifests, manifest)
		}
	case opts.BatchID != "":
		manifest, err := loadExecutableDeletionManifest(manager, opts.BatchID)
		if err != nil {
			return deleteStagedPlan{}, err
		}
		manifests = append(manifests, manifest)
	default:
		pending, err := manager.ListPending()
		if err != nil {
			return deleteStagedPlan{}, fmt.Errorf("list pending: %w", err)
		}
		inProgress, err := manager.ListInProgress()
		if err != nil {
			return deleteStagedPlan{}, fmt.Errorf("list in progress: %w", err)
		}
		manifests = append(manifests, pending...)
		manifests = append(manifests, inProgress...)
	}

	var out strings.Builder
	plan := deleteStagedPlan{
		Manager:            manager,
		Manifests:          manifests,
		PlannedBatchIDs:    deleteStagedManifestIDs(manifests),
		PlanFingerprint:    fingerprintDeleteStagedPlan(manifests),
		RemoteDeleteEnvVar: remoteDeleteEnvVar,
	}
	if len(manifests) == 0 {
		out.WriteString("No staged deletions.\n")
		plan.Stdout = out.String()
		return plan, nil
	}

	if opts.List {
		totalMessages := 0
		fmt.Fprintf(&out, "Staged deletions: %d batch(es)\n\n", len(manifests))
		fmt.Fprintf(&out, "  %-25s  %-12s  %10s  %s\n", "ID", "Status", "Messages", "Description")
		fmt.Fprintf(&out, "  %-25s  %-12s  %10s  %s\n", "---", "------", "--------", "-----------")
		for _, m := range manifests {
			fmt.Fprintf(&out, "  %-25s  %-12s  %10d  %s\n",
				truncate(m.ID, 25),
				m.Status,
				len(m.GmailIDs),
				truncate(m.Description, 40),
			)
			totalMessages += len(m.GmailIDs)
		}
		fmt.Fprintf(&out, "\nTotal: %d messages across %d batch(es)\n", totalMessages, len(manifests))
		out.WriteString("\nUse 'msgvault delete-staged' to execute, or 'msgvault show-deletion <id>' for details.\n")
		plan.Stdout = out.String()
		return plan, nil
	}

	totalMessages := 0
	for _, m := range manifests {
		totalMessages += len(m.GmailIDs)
	}

	method := "trash (30-day recovery)"
	if opts.Permanent {
		method = "PERMANENT DELETE (fast, no recovery)"
	}
	out.WriteString("Deletion Summary:\n")
	fmt.Fprintf(&out, "  Batches:  %d\n", len(manifests))
	fmt.Fprintf(&out, "  Messages: %d\n", totalMessages)
	fmt.Fprintf(&out, "  Method:   %s\n", method)
	out.WriteString("\n")
	for _, m := range manifests {
		fmt.Fprintf(&out, "  %s: %d messages - %s\n", m.ID, len(m.GmailIDs), m.Description)
	}
	out.WriteString("\n")

	if opts.DryRun {
		out.WriteString("Dry run - no messages will be deleted.\n")
		plan.Stdout = out.String()
		return plan, nil
	}

	plan.NeedsExecution = true
	if !opts.RemoteDeleteEnabled {
		plan.BlockedError = fmt.Sprintf(
			"remote deletion is gated in this release; "+
				"set %s=1 to opt in "+
				"(use 'msgvault delete-staged --list' or --dry-run to inspect "+
				"staged batches without executing)",
			remoteDeleteEnvVar,
		)
		plan.Stdout = out.String()
		return plan, nil
	}

	if opts.Permanent {
		plan.NeedsConfirmation = true
		plan.ConfirmationMode = deleteStagedConfirmModePermanent
	} else if !opts.Yes {
		plan.NeedsConfirmation = true
		plan.ConfirmationMode = deleteStagedConfirmModeTrash
	}
	plan.Stdout = out.String()
	return plan, nil
}

func loadExecutableDeletionManifest(manager *deletion.Manager, batchID string) (*deletion.Manifest, error) {
	manifest, _, err := manager.GetManifest(batchID)
	if err != nil {
		return nil, fmt.Errorf("get manifest: %w", err)
	}
	if manifest.Status != deletion.StatusPending && manifest.Status != deletion.StatusInProgress {
		return nil, fmt.Errorf("batch %s is %s, cannot execute", batchID, manifest.Status)
	}
	return manifest, nil
}

func deleteStagedManifestIDs(manifests []*deletion.Manifest) []string {
	ids := make([]string, 0, len(manifests))
	for _, manifest := range manifests {
		ids = append(ids, manifest.ID)
	}
	return ids
}

func fingerprintDeleteStagedPlan(manifests []*deletion.Manifest) string {
	if len(manifests) == 0 {
		return ""
	}
	type manifestFingerprint struct {
		ID          string           `json:"id"`
		Status      deletion.Status  `json:"status"`
		Description string           `json:"description"`
		Account     string           `json:"account,omitempty"`
		GmailIDs    []string         `json:"gmail_ids"`
		Execution   *deletion.Method `json:"execution_method,omitempty"`
	}
	parts := make([]manifestFingerprint, 0, len(manifests))
	for _, manifest := range manifests {
		part := manifestFingerprint{
			ID:          manifest.ID,
			Status:      manifest.Status,
			Description: manifest.Description,
			Account:     manifest.Filters.Account,
			GmailIDs:    append([]string(nil), manifest.GmailIDs...),
		}
		if manifest.Execution != nil {
			method := manifest.Execution.Method
			part.Execution = &method
		}
		parts = append(parts, part)
	}
	data, err := json.Marshal(parts)
	if err != nil {
		panic(fmt.Sprintf("marshal deletion plan fingerprint: %v", err))
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

type deleteStagedTarget struct {
	Account string
	Source  *store.Source
}

type deleteStagedUsageError struct {
	err error
}

func (e deleteStagedUsageError) Error() string {
	return e.err.Error()
}

func (e deleteStagedUsageError) Unwrap() error {
	return e.err
}

func newDeleteStagedUsageError(err error) error {
	return deleteStagedUsageError{err: err}
}

func isDeleteStagedUsageError(err error) bool {
	var usageErr deleteStagedUsageError
	return errors.As(err, &usageErr)
}

func resolveDeleteStagedTarget(
	st *store.Store,
	manifests []*deletion.Manifest,
	requestedAccount string,
) (deleteStagedTarget, error) {
	if requestedAccount != "" {
		return resolveExplicitDeleteStagedTarget(st, manifests, requestedAccount)
	}

	accountSet := make(map[string]bool)
	for _, manifest := range manifests {
		if manifest.Filters.Account != "" {
			accountSet[manifest.Filters.Account] = true
		}
	}

	accounts := make([]string, 0, len(accountSet))
	for account := range accountSet {
		accounts = append(accounts, account)
	}
	slices.Sort(accounts)
	switch len(accounts) {
	case 0:
		return deleteStagedTarget{}, newDeleteStagedUsageError(errors.New("no account in deletion manifest - use --account flag"))
	case 1:
		src, err := lookupDeleteStagedSyncableSource(st, accounts[0])
		if err != nil {
			return deleteStagedTarget{}, err
		}
		return deleteStagedTarget{Account: accounts[0], Source: src}, nil
	default:
		return deleteStagedTarget{}, newDeleteStagedUsageError(
			fmt.Errorf("multiple accounts in pending batches (%v) - use --account flag to specify which account", accounts),
		)
	}
}

func resolveExplicitDeleteStagedTarget(
	st *store.Store,
	manifests []*deletion.Manifest,
	requestedAccount string,
) (deleteStagedTarget, error) {
	resolved, err := st.GetSourcesByIdentifierOrDisplayName(requestedAccount)
	if err != nil {
		return deleteStagedTarget{}, fmt.Errorf("look up source for %s: %w", requestedAccount, err)
	}
	var syncable []*store.Source
	for _, candidate := range resolved {
		if candidate.SourceType == sourceTypeGmail || candidate.SourceType == sourceTypeIMAP {
			syncable = append(syncable, candidate)
		}
	}
	if len(syncable) == 0 {
		return deleteStagedTarget{}, fmt.Errorf("no gmail or imap source found for %s", requestedAccount)
	}
	if len(syncable) > 1 {
		var types []string
		for _, candidate := range syncable {
			types = append(types, fmt.Sprintf("%s (%s)", candidate.Identifier, candidate.SourceType))
		}
		return deleteStagedTarget{}, fmt.Errorf("multiple accounts match %q: %s\nUse the full identifier with --account to disambiguate", requestedAccount, strings.Join(types, ", "))
	}

	found := syncable[0]
	account := found.Identifier
	for _, manifest := range manifests {
		if manifest.Filters.Account != "" && manifest.Filters.Account != account {
			return deleteStagedTarget{}, fmt.Errorf("batch %s is for account %s, not %s - filter batches by account or execute separately", manifest.ID, manifest.Filters.Account, account)
		}
	}
	return deleteStagedTarget{Account: account, Source: found}, nil
}

func lookupDeleteStagedSyncableSource(st *store.Store, account string) (*store.Source, error) {
	sources, err := st.GetSourcesByIdentifier(account)
	if err != nil {
		return nil, fmt.Errorf("look up source for %s: %w", account, err)
	}
	for _, candidate := range sources {
		if candidate.SourceType == sourceTypeGmail || candidate.SourceType == sourceTypeIMAP {
			return candidate, nil
		}
	}
	return nil, fmt.Errorf("no gmail or imap source found for %s", account)
}

type deleteStagedScopeEscalation struct {
	Needed            bool
	Account           string
	BatchDelete       bool
	ClientSecretsPath string
	Headline          string
	BodyLines         []string
	CancelHint        string
}

func deleteStagedScopeEscalationForSource(
	account string,
	src *store.Source,
	permanent bool,
	clientSecretsPath string,
) (deleteStagedScopeEscalation, error) {
	if src == nil || src.SourceType != sourceTypeGmail {
		return deleteStagedScopeEscalation{}, nil
	}
	requiredScopes := oauth.Scopes
	if permanent {
		requiredScopes = oauth.ScopesDeletion
	}
	oauthMgr, err := oauth.NewManagerWithScopes(clientSecretsPath, cfg.TokensDir(), logger, requiredScopes)
	if err != nil {
		return deleteStagedScopeEscalation{}, wrapOAuthError(fmt.Errorf("create oauth manager: %w", err))
	}
	if !oauthMgr.HasScopeMetadata(account) {
		if permanent && oauthMgr.HasToken(account) {
			return newDeleteStagedScopeEscalation(account, permanent, clientSecretsPath), nil
		}
		return deleteStagedScopeEscalation{}, nil
	}
	for _, scope := range requiredScopes {
		if !oauthMgr.HasScope(account, scope) {
			return newDeleteStagedScopeEscalation(account, permanent, clientSecretsPath), nil
		}
	}
	return deleteStagedScopeEscalation{}, nil
}

func newDeleteStagedScopeEscalation(
	account string,
	permanent bool,
	clientSecretsPath string,
) deleteStagedScopeEscalation {
	bodyLines, cancelHint := deletionScopeEscalationPrompt(permanent)
	return deleteStagedScopeEscalation{
		Needed:            true,
		Account:           account,
		BatchDelete:       permanent,
		ClientSecretsPath: clientSecretsPath,
		Headline:          deleteStagedScopeEscalationHeadline,
		BodyLines:         bodyLines,
		CancelHint:        cancelHint,
	}
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
		if !isDaemonCLISubprocess() {
			return runDeleteStagedHTTP(cmd, args)
		}

		confirmed, err := cmd.Flags().GetBool(deleteStagedConfirmedFlag)
		if err != nil {
			return fmt.Errorf("read --%s flag: %w", deleteStagedConfirmedFlag, err)
		}
		skipPrelude, err := cmd.Flags().GetBool(deleteStagedSkipPreludeFlag)
		if err != nil {
			return fmt.Errorf("read --%s flag: %w", deleteStagedSkipPreludeFlag, err)
		}
		planFingerprint, err := cmd.Flags().GetString(deleteStagedPlanFingerprintFlag)
		if err != nil {
			return fmt.Errorf("read --%s flag: %w", deleteStagedPlanFingerprintFlag, err)
		}
		scopeEscalationConfirmed, err := cmd.Flags().GetBool(deleteStagedScopeEscalationConfirmedFlag)
		if err != nil {
			return fmt.Errorf("read --%s flag: %w", deleteStagedScopeEscalationConfirmedFlag, err)
		}
		batchID := ""
		if len(args) > 0 {
			batchID = args[0]
		}
		plan, err := buildDeleteStagedPlan(deleteStagedPlanOptions{
			BatchID:             batchID,
			PlannedBatchIDs:     deletePlannedBatchIDs,
			Permanent:           deletePermanent,
			Yes:                 deleteYes,
			DryRun:              deleteDryRun,
			List:                deleteList,
			Account:             deleteAccount,
			RemoteDeleteEnabled: remoteDeleteEnabled(),
		})
		if err != nil {
			return err
		}
		if planFingerprint != "" && plan.PlanFingerprint != planFingerprint {
			return errors.New("staged deletion plan changed since confirmation; run msgvault delete-staged again")
		}
		if !skipPrelude {
			_, _ = fmt.Fprint(cmd.OutOrStdout(), plan.Stdout)
		}
		if !plan.NeedsExecution {
			return nil
		}
		if plan.BlockedError != "" {
			return errors.New(plan.BlockedError)
		}
		if plan.NeedsConfirmation && !confirmed {
			ok, err := confirmDeleteStaged(cmd.InOrStdin(), cmd.OutOrStdout(), plan.ConfirmationMode)
			if err != nil || !ok {
				return err
			}
		}
		manager := plan.Manager
		manifests := plan.Manifests

		release, err := acquireDirectSQLiteWriteLock(cfg)
		if err != nil {
			return err
		}
		defer release()

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

		target, err := resolveDeleteStagedTarget(s, manifests, deleteAccount)
		if err != nil {
			if isDeleteStagedUsageError(err) {
				return usageErr(cmd, err)
			}
			return err
		}
		account := target.Account
		src := target.Source

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

		// For Gmail, handle scope escalation before building the client.
		// buildAPIClient uses standard scopes; deletion may need elevated ones.
		// Service-account flows get scopes via the JWT assertion (no stored
		// token), so the scope-escalation prompt only applies to browser OAuth.
		var clientSecretsPath string
		if src.SourceType == sourceTypeGmail {
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

				escalation, err := deleteStagedScopeEscalationForSource(account, src, deletePermanent, clientSecretsPath)
				if err != nil {
					return err
				}
				if escalation.Needed {
					if scopeEscalationConfirmed {
						if err := authorizeDeletionScopeEscalation(ctx, escalation.Account, escalation.BatchDelete, escalation.ClientSecretsPath); err != nil {
							return err
						}
					} else {
						if err := promptDeletionScopeEscalation(ctx, escalation.Account, escalation.BatchDelete, escalation.ClientSecretsPath); err != nil {
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
				if src.SourceType == sourceTypeGmail && isInsufficientScopeError(execErr) {
					if cfg.OAuth.ServiceAccountKeyFor(sourceOAuthApp(src)) != "" {
						return fmt.Errorf(
							"service account lacks required Gmail deletion scope for %s: "+
								"authorize https://mail.google.com/ for the service account client "+
								"in Google Admin Console, then run delete-staged again",
							account,
						)
					}
					if err := promptDeletionScopeEscalation(ctx, account, !useTrash, clientSecretsPath); err != nil {
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
		return rebuildCacheAfterWrite(dbPath)
	},
}

func runDeleteStagedHTTP(cmd *cobra.Command, args []string) error {
	batchID := ""
	if len(args) > 0 {
		batchID = args[0]
	}
	remoteDeleteAllowed := remoteDeleteEnabled()
	st, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	plan, err := st.PlanCLIDeleteStaged(cmd.Context(), daemonclient.CLIDeleteStagedPlanRequest{
		BatchID:             batchID,
		Permanent:           deletePermanent,
		Yes:                 deleteYes,
		DryRun:              deleteDryRun,
		List:                deleteList,
		Account:             deleteAccount,
		RemoteDeleteEnabled: remoteDeleteAllowed,
	})
	if err != nil {
		return err
	}
	if plan != nil && plan.Stdout != "" {
		_, _ = fmt.Fprint(cmd.OutOrStdout(), plan.Stdout)
	}
	if plan == nil || !plan.NeedsExecution {
		return nil
	}
	if plan.BlockedError != "" {
		return errors.New(plan.BlockedError)
	}
	promptInput := bufio.NewReader(cmd.InOrStdin())
	if plan.NeedsConfirmation {
		ok, err := confirmDeleteStaged(promptInput, cmd.OutOrStdout(), plan.ConfirmationMode)
		if err != nil || !ok {
			return err
		}
	}
	if plan.NeedsScopeEscalation {
		ok, err := promptScopeEscalationConfirmation(
			promptInput,
			cmd.OutOrStdout(),
			plan.ScopeEscalationHeadline,
			plan.ScopeEscalationBodyLines,
			plan.ScopeEscalationCancelHint,
		)
		if err != nil || !ok {
			return err
		}
		if err := preflightDeleteStagedScopeEscalation(cmd.Context(), plan); err != nil {
			return err
		}
		if err := cmd.Flags().Set(deleteStagedScopeEscalationConfirmedFlag, "true"); err != nil {
			return fmt.Errorf("set --%s after scope confirmation: %w", deleteStagedScopeEscalationConfirmedFlag, err)
		}
	}
	if len(plan.PlannedBatchIDs) == 0 || plan.PlanFingerprint == "" {
		return errors.New("delete-staged plan response did not include pinned batch IDs; upgrade the daemon and retry")
	}
	if err := cmd.Flags().Set(deleteStagedConfirmedFlag, "true"); err != nil {
		return fmt.Errorf("set --%s after confirmation: %w", deleteStagedConfirmedFlag, err)
	}
	if err := cmd.Flags().Set(deleteStagedSkipPreludeFlag, "true"); err != nil {
		return fmt.Errorf("set --%s after planning: %w", deleteStagedSkipPreludeFlag, err)
	}
	if plan.PlanFingerprint != "" {
		if err := cmd.Flags().Set(deleteStagedPlanFingerprintFlag, plan.PlanFingerprint); err != nil {
			return fmt.Errorf("set --%s after planning: %w", deleteStagedPlanFingerprintFlag, err)
		}
	}
	for _, batchID := range plan.PlannedBatchIDs {
		if err := cmd.Flags().Set(deleteStagedPlannedBatchFlag, batchID); err != nil {
			return fmt.Errorf("set --%s after planning: %w", deleteStagedPlannedBatchFlag, err)
		}
	}
	var env map[string]string
	if remoteDeleteAllowed {
		env = map[string]string{remoteDeleteEnvVar: "1"}
	}
	return runDaemonCLICommandHTTPFromCobraWithEnv(cmd, nil, env)
}

// preflightDeleteStagedScopeEscalation performs the confirmed Gmail scope
// upgrade in this process for local daemons, so the browser consent never
// runs in the daemon subprocess while it holds the operation gate. After a
// successful preflight the subprocess re-checks the token, finds the scopes
// present, and skips its own authorization. Remote daemons keep the
// subprocess-side flow because tokens live on that host, as do older daemons
// whose plan response does not name the escalation account.
func preflightDeleteStagedScopeEscalation(ctx context.Context, plan *daemonclient.CLIDeleteStagedPlan) error {
	if IsRemoteMode() || plan.ScopeEscalationAccount == "" {
		return nil
	}
	clientSecretsPath, err := cfg.OAuth.ClientSecretsFor(plan.ScopeEscalationOAuthApp)
	if err != nil {
		return err
	}
	return authorizeDeletionScopeEscalation(ctx, plan.ScopeEscalationAccount, deletePermanent, clientSecretsPath)
}

func confirmDeleteStaged(in io.Reader, out io.Writer, mode string) (bool, error) {
	reader := stagedDeletePromptReader(in)
	switch mode {
	case deleteStagedConfirmModePermanent:
		_, _ = fmt.Fprint(out, `Type "delete" to confirm permanent deletion (no recovery): `)
		answer, ok, err := readStagedDeletePromptLine(reader)
		if err != nil {
			return false, fmt.Errorf("read confirmation: %w", err)
		}
		if !ok || strings.TrimSpace(answer) != "delete" {
			_, _ = fmt.Fprintln(out, "Cancelled. Drop --permanent to use trash deletion without elevated permissions.")
			return false, nil
		}
		return true, nil
	case deleteStagedConfirmModeTrash:
		_, _ = fmt.Fprint(out, "Proceed with deletion? Messages move to Gmail/Trash (recoverable ~30 days). [y/N]: ")
		answer, ok, err := readStagedDeletePromptLine(reader)
		if err != nil {
			return false, fmt.Errorf("read confirmation: %w", err)
		}
		if !ok {
			_, _ = fmt.Fprintln(out, "Cancelled.")
			return false, nil
		}
		if !isYesAnswer(strings.TrimSpace(strings.ToLower(answer))) {
			_, _ = fmt.Fprintln(out, "Cancelled.")
			return false, nil
		}
		return true, nil
	default:
		return false, fmt.Errorf("unknown delete-staged confirmation mode %q", mode)
	}
}

func stagedDeletePromptReader(in io.Reader) *bufio.Reader {
	if reader, ok := in.(*bufio.Reader); ok {
		return reader
	}
	return bufio.NewReader(in)
}

func readStagedDeletePromptLine(reader *bufio.Reader) (string, bool, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		if !errors.Is(err, io.EOF) {
			return "", false, fmt.Errorf("read prompt line: %w", err)
		}
		if line == "" {
			return "", false, nil
		}
	}
	return strings.TrimSpace(line), true, nil
}

func planCLIDeleteStaged(
	_ context.Context,
	st *store.Store,
	req api.CLIDeleteStagedPlanRequest,
) (api.CLIDeleteStagedPlanResponse, error) {
	plan, err := buildDeleteStagedPlan(deleteStagedPlanOptions{
		BatchID:             req.BatchID,
		Permanent:           req.Permanent,
		Yes:                 req.Yes,
		DryRun:              req.DryRun,
		List:                req.List,
		Account:             req.Account,
		RemoteDeleteEnabled: req.RemoteDeleteEnabled,
	})
	if err != nil {
		return api.CLIDeleteStagedPlanResponse{}, err
	}
	if plan.NeedsExecution && req.RemoteDeleteEnabled {
		target, err := resolveDeleteStagedTarget(st, plan.Manifests, req.Account)
		if err != nil {
			return api.CLIDeleteStagedPlanResponse{}, err
		}
		if target.Source.SourceType == sourceTypeGmail {
			if !cfg.OAuth.HasAnyConfig() {
				return api.CLIDeleteStagedPlanResponse{}, errOAuthNotConfigured()
			}
			appName := sourceOAuthApp(target.Source)
			if cfg.OAuth.ServiceAccountKeyFor(appName) == "" {
				clientSecretsPath, err := cfg.OAuth.ClientSecretsFor(appName)
				if err != nil {
					return api.CLIDeleteStagedPlanResponse{}, err
				}
				escalation, err := deleteStagedScopeEscalationForSource(target.Account, target.Source, req.Permanent, clientSecretsPath)
				if err != nil {
					return api.CLIDeleteStagedPlanResponse{}, err
				}
				if escalation.Needed {
					plan.NeedsScopeEscalation = true
					plan.ScopeEscalationHeadline = escalation.Headline
					plan.ScopeEscalationBodyLines = escalation.BodyLines
					plan.ScopeEscalationCancelHint = escalation.CancelHint
					plan.ScopeEscalationAccount = escalation.Account
					plan.ScopeEscalationOAuthApp = appName
				}
			}
		}
	}
	return api.CLIDeleteStagedPlanResponse{
		Stdout:                    plan.Stdout,
		NeedsExecution:            plan.NeedsExecution,
		NeedsConfirmation:         plan.NeedsConfirmation,
		ConfirmationMode:          plan.ConfirmationMode,
		PlannedBatchIDs:           plan.PlannedBatchIDs,
		PlanFingerprint:           plan.PlanFingerprint,
		NeedsScopeEscalation:      plan.NeedsScopeEscalation,
		ScopeEscalationHeadline:   plan.ScopeEscalationHeadline,
		ScopeEscalationBodyLines:  plan.ScopeEscalationBodyLines,
		ScopeEscalationCancelHint: plan.ScopeEscalationCancelHint,
		ScopeEscalationAccount:    plan.ScopeEscalationAccount,
		ScopeEscalationOAuthApp:   plan.ScopeEscalationOAuthApp,
		BlockedError:              plan.BlockedError,
		RemoteDeleteEnvVar:        plan.RemoteDeleteEnvVar,
	}, nil
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
		eta = formatCLIProgressDuration(remaining, cliProgressDurationCompactMinutes) + " remaining"
	} else if processed >= p.total {
		eta = formatCLIProgressDuration(elapsed, cliProgressDurationCompactMinutes) + " elapsed"
	} else {
		eta = "calculating..."
	}

	status := fmt.Sprintf("  %s %.1f%%  %d/%d", bar, pct, processed, p.total)
	if failed > 0 {
		status += fmt.Sprintf("  (%d failed)", failed)
	}
	status += "  " + eta
	if p.tty {
		fmt.Printf("\r\033[K%s", status)
	} else {
		fmt.Println(status)
	}
}

func (p *CLIDeletionProgress) progressBar(pct float64, width int) string {
	style := cliDeletionProgressStyle
	style.Width = width
	return formatCLIProgressBar(pct, style)
}

func (p *CLIDeletionProgress) OnComplete(succeeded, failed int) {
	elapsed := time.Since(p.startTime)
	// Clear the progress line
	if p.tty {
		fmt.Print("\r\033[K")
	}
	if failed == 0 {
		fmt.Printf("  Done: %d deleted in %s\n",
			succeeded, formatCLIProgressDuration(elapsed, cliProgressDurationCompactMinutes))
	} else {
		fmt.Printf("  Done: %d deleted, %d failed in %s\n",
			succeeded, failed, formatCLIProgressDuration(elapsed, cliProgressDurationCompactMinutes))
	}
}

// Helper functions.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

func limitManifests(manifests []*deletion.Manifest, maxN int) []*deletion.Manifest {
	if len(manifests) <= maxN {
		return manifests
	}
	return manifests[:maxN]
}

// errUserCanceled is returned when the user declines scope escalation.
var errUserCanceled = errors.New("user canceled scope escalation")

func promptScopeEscalationConfirmation(
	in io.Reader,
	out io.Writer,
	headline string,
	bodyLines []string,
	cancelHint string,
) (bool, error) {
	if _, err := fmt.Fprintln(out, "\n"+strings.Repeat("=", 70)); err != nil {
		return false, fmt.Errorf("write scope escalation prompt: %w", err)
	}
	if _, err := fmt.Fprintln(out, headline); err != nil {
		return false, fmt.Errorf("write scope escalation prompt: %w", err)
	}
	if _, err := fmt.Fprintln(out, strings.Repeat("=", 70)); err != nil {
		return false, fmt.Errorf("write scope escalation prompt: %w", err)
	}
	if _, err := fmt.Fprintln(out); err != nil {
		return false, fmt.Errorf("write scope escalation prompt: %w", err)
	}
	for _, line := range bodyLines {
		if _, err := fmt.Fprintln(out, line); err != nil {
			return false, fmt.Errorf("write scope escalation prompt: %w", err)
		}
	}
	if _, err := fmt.Fprintln(out); err != nil {
		return false, fmt.Errorf("write scope escalation prompt: %w", err)
	}
	if _, err := fmt.Fprint(out, "Upgrade permissions now? [y/N]: "); err != nil {
		return false, fmt.Errorf("write scope escalation prompt: %w", err)
	}
	answer, ok, err := readStagedDeletePromptLine(stagedDeletePromptReader(in))
	if err != nil {
		return false, fmt.Errorf("read scope escalation confirmation: %w", err)
	}
	if !ok {
		return false, errors.New("no confirmation input (stdin closed)")
	}
	if !isYesAnswer(strings.TrimSpace(strings.ToLower(answer))) {
		if cancelHint != "" {
			if _, err := fmt.Fprintln(out, cancelHint); err != nil {
				return false, fmt.Errorf("write scope escalation cancellation: %w", err)
			}
		} else {
			if _, err := fmt.Fprintln(out, "Cancelled."); err != nil {
				return false, fmt.Errorf("write scope escalation cancellation: %w", err)
			}
		}
		return false, nil
	}
	return true, nil
}

// promptScopeEscalation is the generic re-consent flow. It explains why a
// permission upgrade is needed (headline + bodyLines), asks the user, and on
// "yes" re-authorizes with requiredScopes, returning nil on success. The caller
// should re-create its OAuth manager afterward.
//
// The existing token is NOT deleted up front. Authorize runs a fresh consent and
// only overwrites the token file (atomically) AFTER the new grant is validated
// for the right account; on any cancellation or failure it returns an error
// without touching the existing file. Deleting first — as this flow used to —
// would, on a headless host with no reachable browser, leave the account with no
// token at all and break its scheduled syncs. So on success the new token
// replaces the old in place, and on failure the old token is left intact.
//
// requiredScopes MUST list EVERY scope the account still needs, not just the new
// one: browserFlow uses ApprovalForce with no include_granted_scopes, so
// re-consent REPLACES the granted scope set. Omitting a previously-granted scope
// silently drops it (e.g. dropping Gmail when adding Calendar).
func promptScopeEscalation(
	ctx context.Context,
	account string,
	requiredScopes []string,
	headline string,
	bodyLines []string,
	cancelHint string,
	clientSecretsPath string,
) error {
	ok, err := promptScopeEscalationConfirmation(os.Stdin, os.Stdout, headline, bodyLines, cancelHint)
	if err != nil {
		return err
	}
	if !ok {
		return errUserCanceled
	}

	return authorizeScopeEscalation(ctx, account, requiredScopes, clientSecretsPath)
}

func authorizeScopeEscalation(
	ctx context.Context,
	account string,
	requiredScopes []string,
	clientSecretsPath string,
) error {
	// Re-authorize with the upgraded scope set. We deliberately do NOT delete
	// the existing token first: Authorize overwrites it atomically only after a
	// successful, validated grant, so the old token survives a cancelled or
	// failed flow (e.g. on a headless host).
	fmt.Println("\nStarting OAuth flow...")
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

// promptDeletionScopeEscalation is the deletion-specific wrapper that maps the
// batchDelete bool to the right scopes/copy and delegates to the generic helper.
func promptDeletionScopeEscalation(ctx context.Context, account string, batchDelete bool, clientSecretsPath string) error {
	requiredScopes, err := deletionEscalationScopesForAccount(account, batchDelete, clientSecretsPath)
	if err != nil {
		return err
	}
	bodyLines, cancelHint := deletionScopeEscalationPrompt(batchDelete)
	return promptScopeEscalation(ctx, account, requiredScopes,
		deleteStagedScopeEscalationHeadline, bodyLines, cancelHint, clientSecretsPath)
}

func authorizeDeletionScopeEscalation(ctx context.Context, account string, batchDelete bool, clientSecretsPath string) error {
	requiredScopes, err := deletionEscalationScopesForAccount(account, batchDelete, clientSecretsPath)
	if err != nil {
		return err
	}
	return authorizeScopeEscalation(ctx, account, requiredScopes, clientSecretsPath)
}

func deletionScopeEscalationPrompt(batchDelete bool) ([]string, string) {
	if !batchDelete {
		return []string{
			"Trash deletion requires Gmail modify permissions.",
			"",
			"Your current OAuth token doesn't include the gmail.modify scope.",
			"To proceed, msgvault needs to re-authorize with modify access.",
		}, "Cancelled."
	}
	return []string{
		"Batch deletion requires elevated Gmail permissions.",
		"",
		"Your current OAuth token was granted with limited permissions that",
		"don't include batch delete. To proceed, msgvault will re-authorize",
		"this account with full Gmail access (mail.google.com scope). Your",
		"existing token keeps working until the new grant succeeds.",
		"",
		"This elevated permission allows msgvault to permanently delete",
		"messages in bulk. You can revoke access anytime at:",
		"  https://myaccount.google.com/permissions",
	}, "Cancelled. Drop --permanent to use trash deletion without elevated permissions."
}

func deletionEscalationScopesForAccount(account string, batchDelete bool, clientSecretsPath string) ([]string, error) {
	mgr, err := oauth.NewManagerWithScopes(clientSecretsPath, cfg.TokensDir(), logger, oauth.ScopesGmailCalendar)
	if err != nil {
		return nil, fmt.Errorf("create oauth manager: %w", err)
	}
	return deletionEscalationScopes(batchDelete, mgr.GrantedScopes(account)), nil
}

func deletionEscalationScopes(batchDelete bool, existingScopes []string) []string {
	required := oauth.Scopes
	if batchDelete {
		required = oauth.ScopesDeletion
	}
	scopes := append([]string(nil), existingScopes...)
	for _, scope := range required {
		scopes = appendScopeIfMissing(scopes, scope)
	}
	return scopes
}

func appendScopeIfMissing(scopes []string, scope string) []string {
	if slices.Contains(scopes, scope) {
		return scopes
	}
	return append(scopes, scope)
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
	deleteStagedCmd.Flags().Bool(deleteStagedConfirmedFlag, false, "Internal confirmation marker")
	deleteStagedCmd.Flags().Bool(deleteStagedSkipPreludeFlag, false, "Internal planning marker")
	deleteStagedCmd.Flags().StringArrayVar(&deletePlannedBatchIDs, deleteStagedPlannedBatchFlag, nil, "Internal planned batch marker")
	deleteStagedCmd.Flags().String(deleteStagedPlanFingerprintFlag, "", "Internal plan fingerprint marker")
	deleteStagedCmd.Flags().Bool(deleteStagedScopeEscalationConfirmedFlag, false, "Internal scope escalation marker")
	_ = deleteStagedCmd.Flags().MarkHidden(deleteStagedConfirmedFlag)
	_ = deleteStagedCmd.Flags().MarkHidden(deleteStagedSkipPreludeFlag)
	_ = deleteStagedCmd.Flags().MarkHidden(deleteStagedPlannedBatchFlag)
	_ = deleteStagedCmd.Flags().MarkHidden(deleteStagedPlanFingerprintFlag)
	_ = deleteStagedCmd.Flags().MarkHidden(deleteStagedScopeEscalationConfirmedFlag)

	deleteStagedCmd.MarkFlagsMutuallyExclusive("permanent", "yes")
	listDeletionsCmd.Flags().BoolVar(&listDeletionsJSON, flagJSON, false, "Output as JSON with full batch IDs")
	rootCmd.AddCommand(listDeletionsCmd)
	rootCmd.AddCommand(showDeletionCmd)
	cancelDeletionCmd.Flags().BoolVar(&cancelAll, "all", false, "Cancel all pending and in-progress batches")
	rootCmd.AddCommand(cancelDeletionCmd)
	rootCmd.AddCommand(deleteStagedCmd)
}
