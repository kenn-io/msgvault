package cmd

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/dedup"
	"go.kenn.io/msgvault/internal/opserr"
	"go.kenn.io/msgvault/internal/store"
)

const deduplicateCommandName = "deduplicate"

var deduplicateCmd = &cobra.Command{
	Use:     deduplicateCommandName,
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
	dedupPlanConfirmed        bool
	dedupPlanFingerprint      string
	dedupSourcePlans          []string
	dedupSourceID             int64
)

func runDeduplicate(cmd *cobra.Command, _ []string) error {
	if !isDaemonCLISubprocess() {
		if deduplicateCanUseDaemonRunner() {
			return runDaemonCLICommandHTTPFromCobra(cmd, nil)
		}
		return runDeduplicateInteractiveHTTP(cmd)
	}

	st, cleanup, err := openWritableStoreAndInit()
	if err != nil {
		return err
	}
	defer cleanup()

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

	var preferenceWarnings io.Writer = os.Stderr
	if dedupPlanConfirmed {
		preferenceWarnings = nil
	}
	preference := deduplicateSourcePreference(dedupPrefer, preferenceWarnings)
	scope, err := resolveDeduplicateScope(st, deduplicateScopeRequest{
		Account:    dedupAccount,
		Collection: dedupCollection,
		SourceID:   dedupSourceID,
	})
	if err != nil {
		return err
	}

	config := dedup.Config{
		SourcePreference:           preference,
		ContentHashFallback:        dedupContentHash,
		DryRun:                     dedupDryRun,
		AccountSourceIDs:           scope.SourceIDs,
		Account:                    scope.DisplayName,
		ScopeIsCollection:          scope.IsCollection,
		DeleteDupsFromSourceServer: dedupDeleteFromSourceSrvr,
		DeletionsDir:               deletionsDir,
	}

	if len(scope.SourceIDs) > 0 {
		bySource, err := loadPerSourceIdentities(st, scope.SourceIDs)
		if err != nil {
			return fmt.Errorf("load per-source identities: %w", err)
		}
		config.IdentityAddressesBySource = bySource
		if len(bySource) > 0 {
			logger.Info("dedup per-source identities loaded",
				"sources", len(bySource))
		}
	}

	if scope.IntroStdout != "" && !dedupPlanConfirmed {
		fmt.Print(scope.IntroStdout)
	}

	if len(scope.SourceIDs) == 0 {
		// Per-source path constructs its own scoped engines per
		// source, so no top-level engine is needed here.
		return runDeduplicatePerSource(cmd, st, dbPath, config)
	}

	// Single-account/single-collection path uses one engine shared
	// across the whole scope.
	engine := dedup.NewEngine(st, config, logger)
	return runDeduplicateOnce(cmd, st, dbPath, config, engine)
}

func deduplicateCanUseDaemonRunner() bool {
	return dedupDryRun || dedupYes || len(dedupUndo) > 0
}

func runDeduplicateInteractiveHTTP(cmd *cobra.Command) error {
	_ = deduplicateSourcePreference(dedupPrefer, cmd.ErrOrStderr())
	st, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	plan, err := st.PlanCLIDeduplicate(cmd.Context(), daemonclient.CLIDeduplicatePlanRequest{
		Account:                    dedupAccount,
		Collection:                 dedupCollection,
		Prefer:                     dedupPrefer,
		ContentHash:                dedupContentHash,
		DeleteDupsFromSourceServer: dedupDeleteFromSourceSrvr,
	})
	if err != nil {
		return fmt.Errorf("plan deduplicate: %w", err)
	}
	if plan == nil {
		return errors.New("deduplicate plan response was empty")
	}
	out := cmd.OutOrStdout()
	if plan.PrefixStdout != "" {
		_, _ = fmt.Fprint(out, plan.PrefixStdout)
	}

	promptReader := newDedupPromptReader(cmd)
	approved := make([]daemonclient.CLIDeduplicatePlanItem, 0, len(plan.Items))
	for _, item := range plan.Items {
		if item.Stdout != "" {
			_, _ = fmt.Fprint(out, item.Stdout)
		}
		if !item.NeedsConfirmation {
			continue
		}
		if item.PlanFingerprint == "" {
			return errors.New("deduplicate plan response did not include a fingerprint; upgrade the daemon and retry")
		}
		printDeduplicateBackfillPromptNote(cmd, item)
		printDeduplicatePrompt(cmd, item)
		ok, err := readDedupYesNo(promptReader)
		if err != nil {
			return err
		}
		if !ok {
			if item.SourceID > 0 {
				_, _ = fmt.Fprintln(out, "Skipped.")
				continue
			}
			_, _ = fmt.Fprintln(out, "Aborted.")
			return nil
		}
		approved = append(approved, item)
	}
	if plan.FooterStdout != "" {
		_, _ = fmt.Fprint(out, plan.FooterStdout)
	}
	if len(approved) == 0 {
		return nil
	}

	if err := cmd.Flags().Set("yes", "true"); err != nil {
		return fmt.Errorf("set --yes after dedup confirmation: %w", err)
	}
	if err := cmd.Flags().Set("dedup-plan-confirmed", "true"); err != nil {
		return fmt.Errorf("set --dedup-plan-confirmed after dedup confirmation: %w", err)
	}
	if len(approved) == 1 && approved[0].SourceID == 0 {
		if err := cmd.Flags().Set("dedup-plan-fingerprint", approved[0].PlanFingerprint); err != nil {
			return fmt.Errorf("set --dedup-plan-fingerprint after dedup planning: %w", err)
		}
		return runDaemonCLICommandHTTPFromCobra(cmd, nil)
	}
	for _, item := range approved {
		if item.SourceID <= 0 {
			return errors.New("deduplicate per-source plan response did not include source IDs; upgrade the daemon and retry")
		}
		value := fmt.Sprintf("%d:%s", item.SourceID, item.PlanFingerprint)
		if err := cmd.Flags().Set("dedup-source-plan", value); err != nil {
			return fmt.Errorf("set --dedup-source-plan after dedup planning: %w", err)
		}
	}
	return runDaemonCLICommandHTTPFromCobra(cmd, nil)
}

func printDeduplicateBackfillPromptNote(cmd *cobra.Command, item daemonclient.CLIDeduplicatePlanItem) {
	if item.BackfilledCount <= 0 {
		return
	}
	out := cmd.OutOrStdout()
	if item.SourceID > 0 {
		_, _ = fmt.Fprintf(
			out,
			"\nNote: scan already backfilled %d rfc822_message_id value(s) for %s from "+
				"stored MIME. This is metadata derivation and is kept regardless of your answer.\n",
			item.BackfilledCount,
			item.ScopeLabel,
		)
		return
	}
	_, _ = fmt.Fprintf(
		out,
		"\nNote: scan already backfilled %d rfc822_message_id value(s) from stored MIME. "+
			"This is metadata derivation and is kept regardless of your answer.\n",
		item.BackfilledCount,
	)
}

func printDeduplicatePrompt(cmd *cobra.Command, item daemonclient.CLIDeduplicatePlanItem) {
	out := cmd.OutOrStdout()
	if item.SourceID > 0 {
		_, _ = fmt.Fprintf(
			out,
			"\nProceed with deduplication for %s? This will hide %d duplicates "+
				"(reversible with --undo). [y/N]: ",
			item.ScopeLabel,
			item.DuplicateMessages,
		)
		return
	}
	_, _ = fmt.Fprintf(
		out,
		"\nProceed with deduplication? This will hide %d duplicates "+
			"(reversible with --undo). [y/N]: ",
		item.DuplicateMessages,
	)
}

type deduplicateScopeRequest struct {
	Account    string
	Collection string
	SourceID   int64
}

type deduplicateScope struct {
	SourceIDs    []int64
	DisplayName  string
	IsCollection bool
	IntroStdout  string
}

func deduplicateSourcePreference(prefer string, warnings io.Writer) []string {
	if prefer == "" {
		return dedup.DefaultSourcePreference
	}
	preference := strings.Split(prefer, ",")
	known := make(map[string]bool, len(dedup.DefaultSourcePreference))
	for _, t := range dedup.DefaultSourcePreference {
		known[t] = true
	}
	for i := range preference {
		preference[i] = strings.TrimSpace(preference[i])
		if !known[preference[i]] && warnings != nil {
			_, _ = fmt.Fprintf(warnings, "Warning: unknown source type in --prefer: %q\n", preference[i])
		}
	}
	return preference
}

func resolveDeduplicateScope(st *store.Store, req deduplicateScopeRequest) (deduplicateScope, error) {
	switch {
	case req.SourceID > 0:
		src, err := st.GetSourceByID(req.SourceID)
		if err != nil {
			return deduplicateScope{}, fmt.Errorf("load source %d: %w", req.SourceID, err)
		}
		if !emailAccountSource(src) {
			return deduplicateScope{}, opserr.Invalid(fmt.Errorf("source %d is not an email account", req.SourceID))
		}
		return deduplicateScope{
			SourceIDs:   []int64{src.ID},
			DisplayName: src.Identifier,
		}, nil
	case req.Account != "":
		scope, err := ResolveEmailAccountFlag(st, req.Account)
		if err != nil {
			return deduplicateScope{}, err
		}
		sourceIDs := scope.SourceIDs()
		if len(sourceIDs) == 0 {
			return deduplicateScope{}, opserr.Invalid(fmt.Errorf("--account %q resolved to zero sources", req.Account))
		}
		return deduplicateScope{
			SourceIDs:   sourceIDs,
			DisplayName: scope.DisplayName(),
		}, nil
	case req.Collection != "":
		scope, err := ResolveCollectionFlag(st, req.Collection)
		if err != nil {
			return deduplicateScope{}, err
		}
		sourceIDs, err := dedupEligibleSourceIDs(st, scope.SourceIDs())
		if err != nil {
			return deduplicateScope{}, err
		}
		if len(sourceIDs) == 0 {
			return deduplicateScope{}, opserr.Invalid(fmt.Errorf("--collection %q has no member accounts", req.Collection))
		}
		intro, err := deduplicateCollectionIntro(st, scope.DisplayName(), sourceIDs)
		if err != nil {
			return deduplicateScope{}, err
		}
		return deduplicateScope{
			SourceIDs:    sourceIDs,
			DisplayName:  scope.DisplayName(),
			IsCollection: true,
			IntroStdout:  intro,
		}, nil
	default:
		return deduplicateScope{}, nil
	}
}

func deduplicateCollectionIntro(st *store.Store, displayName string, sourceIDs []int64) (string, error) {
	allSources, err := st.ListSources("")
	if err != nil {
		return "", fmt.Errorf("list sources: %w", err)
	}
	idSet := make(map[int64]struct{}, len(sourceIDs))
	for _, id := range sourceIDs {
		idSet[id] = struct{}{}
	}
	var memberNames []string
	for _, src := range allSources {
		if _, ok := idSet[src.ID]; ok {
			memberNames = append(memberNames, src.Identifier)
		}
	}
	var out strings.Builder
	fmt.Fprintf(&out, "Deduping across collection %q (%d accounts: %s)\n",
		displayName, len(memberNames), strings.Join(memberNames, ", "))
	if len(memberNames) > 1 {
		out.WriteString(
			"  Note: cross-source dedup is reversible (--undo); " +
				"remote deletion stays same-source-only. " +
				"Re-run with --dry-run to preview.\n",
		)
	}
	return out.String(), nil
}

func planCLIDeduplicate(
	ctx context.Context,
	st *store.Store,
	req api.CLIDeduplicatePlanRequest,
) (api.CLIDeduplicatePlanResponse, error) {
	preference := deduplicateSourcePreference(req.Prefer, nil)
	deletionsDir := filepath.Join(cfg.Data.DataDir, "deletions")
	scope, err := resolveDeduplicateScope(st, deduplicateScopeRequest{
		Account:    req.Account,
		Collection: req.Collection,
	})
	if err != nil {
		return api.CLIDeduplicatePlanResponse{}, err
	}
	base := dedup.Config{
		SourcePreference:           preference,
		ContentHashFallback:        req.ContentHash,
		DeleteDupsFromSourceServer: req.DeleteDupsFromSourceServer,
		DeletionsDir:               deletionsDir,
	}
	if len(scope.SourceIDs) == 0 {
		return planCLIDeduplicatePerSource(ctx, st, base)
	}
	base.AccountSourceIDs = scope.SourceIDs
	base.Account = scope.DisplayName
	base.ScopeIsCollection = scope.IsCollection
	bySource, err := loadPerSourceIdentities(st, scope.SourceIDs)
	if err != nil {
		return api.CLIDeduplicatePlanResponse{}, fmt.Errorf("load per-source identities: %w", err)
	}
	base.IdentityAddressesBySource = bySource
	engine := dedup.NewEngine(st, base, logger)
	item, err := planCLIDeduplicateItem(ctx, engine, base, 0, scope.DisplayName, scope.IsCollection)
	if err != nil {
		return api.CLIDeduplicatePlanResponse{}, err
	}
	return api.CLIDeduplicatePlanResponse{
		PrefixStdout: scope.IntroStdout,
		Items:        []api.CLIDeduplicatePlanItem{item},
	}, nil
}

func planCLIDeduplicatePerSource(
	ctx context.Context,
	st *store.Store,
	base dedup.Config,
) (api.CLIDeduplicatePlanResponse, error) {
	sources, err := st.ListSources("")
	if err != nil {
		return api.CLIDeduplicatePlanResponse{}, fmt.Errorf("list sources: %w", err)
	}
	if len(sources) == 0 {
		return api.CLIDeduplicatePlanResponse{PrefixStdout: "No sources found.\n"}, nil
	}

	resp := api.CLIDeduplicatePlanResponse{
		PrefixStdout: "No --account specified; deduping each source independently.\n\n",
	}
	anyRan := false
	for _, src := range sources {
		if !emailAccountSource(src) {
			continue
		}
		cfgScoped := base
		cfgScoped.AccountSourceIDs = []int64{src.ID}
		cfgScoped.Account = src.Identifier
		bySource, err := loadPerSourceIdentities(st, []int64{src.ID})
		if err != nil {
			return api.CLIDeduplicatePlanResponse{}, fmt.Errorf("load identities for %s: %w", src.Identifier, err)
		}
		cfgScoped.IdentityAddressesBySource = bySource
		engineScoped := dedup.NewEngine(st, cfgScoped, logger)
		item, err := planCLIDeduplicateItem(ctx, engineScoped, cfgScoped, src.ID, src.Identifier, false)
		if err != nil {
			return api.CLIDeduplicatePlanResponse{}, fmt.Errorf("scan %s: %w", src.Identifier, err)
		}
		item.Stdout = fmt.Sprintf("--- %s (%s) ---\n", src.Identifier, src.SourceType) + item.Stdout
		if !item.NeedsConfirmation {
			item.Stdout += "  No duplicates.\n\n"
		} else {
			anyRan = true
		}
		resp.Items = append(resp.Items, item)
	}
	if !anyRan {
		resp.FooterStdout = "No duplicates found in any source.\n"
	}
	return resp, nil
}

func planCLIDeduplicateItem(
	ctx context.Context,
	engine *dedup.Engine,
	cfgScoped dedup.Config,
	sourceID int64,
	scopeLabel string,
	scopeIsCollection bool,
) (api.CLIDeduplicatePlanItem, error) {
	report, err := engine.Scan(ctx)
	if err != nil {
		return api.CLIDeduplicatePlanItem{}, err
	}
	var out strings.Builder
	if sourceID == 0 {
		out.WriteString("Scanning for duplicate messages...\n")
		out.WriteString(engine.FormatMethodology())
	}
	if sourceID == 0 || report.DuplicateGroups > 0 || report.BackfilledCount != 0 {
		out.WriteString(engine.FormatReport(report))
	}
	if sourceID == 0 && report.DuplicateGroups == 0 {
		out.WriteString("\nNo duplicates found.\n")
	}
	fingerprint, err := deduplicatePlanFingerprint(cfgScoped, report)
	if err != nil {
		return api.CLIDeduplicatePlanItem{}, err
	}
	return api.CLIDeduplicatePlanItem{
		SourceID:          sourceID,
		ScopeLabel:        scopeLabel,
		ScopeIsCollection: scopeIsCollection,
		Stdout:            out.String(),
		DuplicateMessages: report.DuplicateMessages,
		BackfilledCount:   report.BackfilledCount,
		PlanFingerprint:   fingerprint,
		NeedsConfirmation: report.DuplicateGroups > 0,
	}, nil
}

func deduplicatePlanFingerprint(cfgScoped dedup.Config, report *dedup.Report) (string, error) {
	type fingerprintGroup struct {
		Key          string  `json:"key"`
		KeyType      string  `json:"key_type"`
		SurvivorID   int64   `json:"survivor_id"`
		DuplicateIDs []int64 `json:"duplicate_ids"`
	}
	type fingerprintPayload struct {
		SourceIDs           []int64            `json:"source_ids"`
		Account             string             `json:"account"`
		ScopeIsCollection   bool               `json:"scope_is_collection"`
		ContentHashFallback bool               `json:"content_hash_fallback"`
		DeleteFromSource    bool               `json:"delete_from_source"`
		SourcePreference    []string           `json:"source_preference"`
		Groups              []fingerprintGroup `json:"groups"`
	}
	payload := fingerprintPayload{
		SourceIDs:           append([]int64(nil), cfgScoped.AccountSourceIDs...),
		Account:             cfgScoped.Account,
		ScopeIsCollection:   cfgScoped.ScopeIsCollection,
		ContentHashFallback: cfgScoped.ContentHashFallback,
		DeleteFromSource:    cfgScoped.DeleteDupsFromSourceServer,
		SourcePreference:    append([]string(nil), cfgScoped.SourcePreference...),
	}
	slices.Sort(payload.SourceIDs)
	for _, group := range report.Groups {
		if len(group.Messages) == 0 || group.Survivor < 0 || group.Survivor >= len(group.Messages) {
			continue
		}
		fpGroup := fingerprintGroup{
			Key:        group.Key,
			KeyType:    group.KeyType,
			SurvivorID: group.Messages[group.Survivor].ID,
		}
		for i, msg := range group.Messages {
			if i == group.Survivor {
				continue
			}
			fpGroup.DuplicateIDs = append(fpGroup.DuplicateIDs, msg.ID)
		}
		slices.Sort(fpGroup.DuplicateIDs)
		payload.Groups = append(payload.Groups, fpGroup)
	}
	sort.Slice(payload.Groups, func(i, j int) bool {
		if payload.Groups[i].Key != payload.Groups[j].Key {
			return payload.Groups[i].Key < payload.Groups[j].Key
		}
		if payload.Groups[i].KeyType != payload.Groups[j].KeyType {
			return payload.Groups[i].KeyType < payload.Groups[j].KeyType
		}
		return payload.Groups[i].SurvivorID < payload.Groups[j].SurvivorID
	})
	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal dedup plan fingerprint: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func parseDedupSourcePlans(values []string) (map[int64]string, error) {
	out := make(map[int64]string, len(values))
	for _, value := range values {
		idText, fp, ok := strings.Cut(value, ":")
		if !ok || strings.TrimSpace(idText) == "" || strings.TrimSpace(fp) == "" {
			return nil, fmt.Errorf("invalid dedup source plan %q", value)
		}
		id, err := strconv.ParseInt(idText, 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("invalid dedup source plan source id %q", idText)
		}
		out[id] = fp
	}
	return out, nil
}

func validateDeduplicatePlanFingerprint(cfgScoped dedup.Config, report *dedup.Report, expected string) error {
	if expected == "" {
		return errors.New("dedup confirmed plan did not include a fingerprint")
	}
	got, err := deduplicatePlanFingerprint(cfgScoped, report)
	if err != nil {
		return err
	}
	if got != expected {
		return errors.New("deduplication plan changed; rerun deduplicate")
	}
	return nil
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

	if !dedupPlanConfirmed {
		fmt.Println(
			"No --account specified; deduping each source independently.",
		)
		fmt.Println()
	}

	backedUp := false
	anyRan := false
	var executedBatches []string
	promptReader := newDedupPromptReader(cmd)
	expectedSourcePlans := map[int64]string(nil)
	if dedupPlanConfirmed {
		expectedSourcePlans, err = parseDedupSourcePlans(dedupSourcePlans)
		if err != nil {
			return err
		}
		if len(expectedSourcePlans) == 0 {
			return errors.New("dedup confirmed plan did not include approved source plans")
		}
	}
	for _, src := range sources {
		if !emailAccountSource(src) {
			continue
		}
		expectedFingerprint := ""
		if dedupPlanConfirmed {
			var ok bool
			expectedFingerprint, ok = expectedSourcePlans[src.ID]
			if !ok {
				continue
			}
		}
		cfgScoped := cfgBase
		cfgScoped.AccountSourceIDs = []int64{src.ID}
		cfgScoped.Account = src.Identifier
		bySource, err := loadPerSourceIdentities(st, []int64{src.ID})
		if err != nil {
			return fmt.Errorf("load identities for %s: %w", src.Identifier, err)
		}
		cfgScoped.IdentityAddressesBySource = bySource
		engineScoped := dedup.NewEngine(st, cfgScoped, logger)

		if !dedupPlanConfirmed {
			fmt.Printf("--- %s (%s) ---\n", src.Identifier, src.SourceType)
		}
		report, err := engineScoped.Scan(cmd.Context())
		if err != nil {
			return fmt.Errorf("scan %s: %w", src.Identifier, err)
		}
		if dedupPlanConfirmed {
			if err := validateDeduplicatePlanFingerprint(cfgScoped, report, expectedFingerprint); err != nil {
				return err
			}
		}
		if report.DuplicateGroups == 0 {
			if dedupPlanConfirmed {
				return errors.New("deduplication plan changed; rerun deduplicate")
			}
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
		if !dedupPlanConfirmed {
			fmt.Print(engineScoped.FormatReport(report))
		}
		if cfgScoped.DryRun {
			fmt.Println()
			continue
		}

		if !dedupYes && !dedupPlanConfirmed {
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
			ok, err := readDedupYesNo(promptReader)
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

	if dedupPlanConfirmed {
		if len(executedBatches) > 1 {
			printAccumulatedUndoHint(executedBatches)
		}
	} else if cfgBase.DryRun {
		fmt.Println("\nDry run complete. No changes made.")
	} else if !anyRan {
		fmt.Println("No duplicates found in any source.")
	} else if len(executedBatches) > 1 {
		printAccumulatedUndoHint(executedBatches)
	}
	return nil
}

func dedupEligibleSourceIDs(st *store.Store, sourceIDs []int64) ([]int64, error) {
	ids := make([]int64, 0, len(sourceIDs))
	for _, id := range sourceIDs {
		src, err := st.GetSourceByID(id)
		if err != nil {
			return nil, fmt.Errorf("load source %d: %w", id, err)
		}
		if !emailAccountSource(src) {
			continue
		}
		ids = append(ids, id)
	}
	return ids, nil
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
	if !dedupPlanConfirmed {
		fmt.Println("Scanning for duplicate messages...")
	}
	report, err := engine.Scan(cmd.Context())
	if err != nil {
		return fmt.Errorf("scan: %w", err)
	}
	if dedupPlanConfirmed {
		if err := validateDeduplicatePlanFingerprint(cfgScoped, report, dedupPlanFingerprint); err != nil {
			return err
		}
	}

	if !dedupPlanConfirmed {
		fmt.Print(engine.FormatMethodology())
		fmt.Print(engine.FormatReport(report))
	}

	if cfgScoped.DryRun {
		fmt.Println("\nDry run complete. No changes made.")
		return nil
	}
	if report.DuplicateGroups == 0 {
		if dedupPlanConfirmed {
			return errors.New("deduplication plan changed; rerun deduplicate")
		}
		fmt.Println("\nNo duplicates found.")
		return nil
	}

	if !dedupYes && !dedupPlanConfirmed {
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
		ok, err := readDedupYesNo(newDedupPromptReader(cmd))
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

func newDedupPromptReader(cmd *cobra.Command) *bufio.Reader {
	return bufio.NewReader(cmd.InOrStdin())
}

func readDedupYesNo(reader *bufio.Reader) (bool, error) {
	response, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("read confirmation: %w", err)
	}
	response = strings.TrimSpace(strings.ToLower(response))
	return isYesAnswer(response), nil
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
//
// PostgreSQL has no in-engine VACUUM INTO equivalent — backups go
// through pg_dump / pg_basebackup / replication, all of which require
// server-side access and credentials this CLI does not own. Refuse
// with a pointer to --no-backup so the user can make an informed
// choice (run pg_dump out-of-band, or skip the safety net).
func backupDatabase(st *store.Store, dst string) error {
	return st.BackupDatabase(dst)
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
	deduplicateCmd.Flags().BoolVar(&dedupPlanConfirmed, "dedup-plan-confirmed", false,
		"Internal daemon confirmation marker")
	deduplicateCmd.Flags().StringVar(&dedupPlanFingerprint, "dedup-plan-fingerprint", "",
		"Internal daemon dedup plan fingerprint")
	deduplicateCmd.Flags().StringArrayVar(&dedupSourcePlans, "dedup-source-plan", nil,
		"Internal daemon per-source dedup plan")
	deduplicateCmd.Flags().Int64Var(&dedupSourceID, "dedup-source-id", 0,
		"Internal daemon source scope")
	_ = deduplicateCmd.Flags().MarkHidden("dedup-plan-confirmed")
	_ = deduplicateCmd.Flags().MarkHidden("dedup-plan-fingerprint")
	_ = deduplicateCmd.Flags().MarkHidden("dedup-source-plan")
	_ = deduplicateCmd.Flags().MarkHidden("dedup-source-id")
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
