package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/gmail"
	imaplib "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/microsoft"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/sync"
	"golang.org/x/oauth2"
)

var (
	syncQuery       string
	syncNoResume    bool
	syncBefore      string
	syncAfter       string
	syncLimit       int
	syncOperationID string
	syncFolders     []string // folder names to include (from --folder flag)
	syncSkipFolders []string // folder names to exclude (from --skip-folder flag)
)

var syncFullCmd = &cobra.Command{
	Use:   "sync-full [email]",
	Short: "Perform a full sync of Gmail accounts",
	Long: `Perform a full synchronization of a Gmail account.

Downloads all messages matching the query (or all messages if no query).
Supports resumption from interruption - just run again to continue.

If no email is specified, syncs all configured accounts sequentially.

Date filters:
  --after 2024-01-01     Only messages on or after this date
  --before 2024-12-31    Only messages before this date

Examples:
  msgvault sync-full                             # Sync all accounts
  msgvault sync-full you@gmail.com
  msgvault sync-full you@gmail.com --after 2024-01-01
  msgvault sync-full you@gmail.com --query "from:someone@example.com"
  msgvault sync-full you@gmail.com --noresume    # Force fresh sync`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := validateSyncFullFlags(cmd); err != nil {
			return err
		}
		if isDaemonCLISubprocess() {
			return runSyncFullLocal(cmd, args)
		}
		return runSyncFullHTTP(cmd, args)
	},
}

func validateSyncFullFlags(cmd *cobra.Command) error {
	if syncLimit < 0 {
		return usageErr(cmd, errors.New("--limit must be a non-negative number"))
	}
	if syncAfter != "" {
		if _, err := time.Parse("2006-01-02", syncAfter); err != nil {
			return usageErr(cmd, fmt.Errorf("invalid --after date %q (expected YYYY-MM-DD): %w", syncAfter, err))
		}
	}
	if syncBefore != "" {
		if _, err := time.Parse("2006-01-02", syncBefore); err != nil {
			return usageErr(cmd, fmt.Errorf("invalid --before date %q (expected YYYY-MM-DD): %w", syncBefore, err))
		}
	}
	return nil
}

func runSyncFullLocal(cmd *cobra.Command, args []string) error {
	selector, selectorSet, err := syncSourceSelector(cmd, args)
	if err != nil {
		return usageErr(cmd, err)
	}
	s, cleanup, err := openWritableStoreAndInit()
	if err != nil {
		return err
	}
	defer cleanup()
	dbPath := cfg.DatabaseDSN()

	getOAuthMgr := oauthManagerCache()

	// Determine which sources to sync
	var sources []*store.Source
	var syncErrors []string
	if selectorSet {
		// Look up all sources matching the identifier and
		// keep only syncable types (gmail, imap). Non-syncable
		// sources like mbox/apple-mail imports share the same
		// identifier namespace but cannot be synced.
		allMatches, legacy, err := resolveSyncSources(s, selector)
		if err != nil {
			return err
		}
		for _, src := range allMatches {
			if src.SourceType == sourceTypeGmail || src.SourceType == sourceTypeIMAP || src.SourceType == "" {
				sources = append(sources, src)
			}
		}
		if len(sources) == 0 {
			if len(allMatches) > 0 {
				// Identifier exists but has no syncable source types.
				return fmt.Errorf("%s exists but its source type cannot be synced (only gmail and imap are supported)", syncSelectorLabel(selector))
			}
			if legacy {
				// Token not in DB yet - assume Gmail (legacy behaviour).
				sources = []*store.Source{{SourceType: sourceTypeGmail, Identifier: selector.Account}}
			}
		}
	} else {
		// Sync all configured sources
		allSources, err := s.ListSources("")
		if err != nil {
			return fmt.Errorf("list sources: %w", err)
		}
		if len(allSources) == 0 {
			return errors.New("no accounts configured - run 'add-account' or 'add-imap' first")
		}
		for _, src := range allSources {
			switch src.SourceType {
			case sourceTypeGmail:
				if !cfg.OAuth.HasAnyConfig() {
					fmt.Printf("Skipping %s (OAuth not configured)\n", src.Identifier)
					continue
				}
				appName := sourceOAuthApp(src)
				// Service accounts are always ready — no per-user token needed
				if cfg.OAuth.ServiceAccountKeyFor(appName) == "" {
					mgr, err := getOAuthMgr(appName)
					if err != nil {
						syncErrors = append(syncErrors, fmt.Sprintf("%s: %v", src.Identifier, err))
						continue
					}
					if !mgr.HasToken(src.Identifier) {
						fmt.Printf("Skipping %s (no OAuth token - run 'add-account' first)\n", src.Identifier)
						continue
					}
				}
			case sourceTypeIMAP:
				skipMsg, parseErr := imapSkipReason(src)
				if parseErr != nil {
					syncErrors = append(syncErrors, fmt.Sprintf("%s: malformed sync_config: %v", src.Identifier, parseErr))
					continue
				}
				if skipMsg != "" {
					fmt.Println(skipMsg)
					continue
				}
			default:
				fmt.Printf("Skipping %s (unsupported source type %q)\n", src.Identifier, src.SourceType)
				continue
			}
			sources = append(sources, src)
		}
		if len(sources) == 0 {
			if len(syncErrors) > 0 {
				return fmt.Errorf("%s", syncErrors[0])
			}
			return errors.New("no accounts are ready to sync")
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

	// Embedding is no longer driven by sync: newly-ingested messages
	// get embed_gen = NULL by column default and the scan-and-fill
	// embed worker (msgvault embeddings build / the serve daemon)
	// picks them up.

	for _, src := range sources {
		if ctx.Err() != nil {
			break
		}

		// Ensure credentials are available before syncing Gmail sources.
		if src.SourceType == sourceTypeGmail || src.SourceType == "" {
			appName := sourceOAuthApp(src)
			if cfg.OAuth.ServiceAccountKeyFor(appName) == "" {
				if _, err := getOAuthMgr(appName); err != nil {
					syncErrors = append(syncErrors, fmt.Sprintf("%s: %v", src.Identifier, err))
					continue
				}
			}
		}

		if err := runFullSync(ctx, s, getOAuthMgr, src); err != nil {
			syncErrors = append(syncErrors, fmt.Sprintf("%s: %v", src.Identifier, err))
			continue
		}
	}

	// Rebuild analytics cache.
	cacheErr := rebuildCacheAfterWrite(dbPath)

	if len(syncErrors) > 0 {
		fmt.Println()
		fmt.Println("Errors:")
		for _, e := range syncErrors {
			fmt.Printf("  %s\n", e)
		}
		return errors.Join(
			fmt.Errorf("%d account(s) failed to sync: %s", len(syncErrors), strings.Join(syncErrors, "; ")),
			cacheErr,
		)
	}

	return cacheErr
}

// buildAPIClient creates the appropriate gmail.API client for the given
// source. saScopes is used when the resolved oauth_app is backed by a
// service account key; for browser-OAuth sources, scopes flow through the
// caller-provided getOAuthMgr factory. Pass nil to use oauth.Scopes; pass
// oauth.ScopesDeletion (or another set) for workflows that need elevated
// access.
func buildAPIClient(ctx context.Context, src *store.Source, getOAuthMgr func(string) (*oauth.Manager, error), saScopes []string, imapOpts ...imaplib.Option) (gmail.API, error) {
	switch src.SourceType {
	case sourceTypeGmail, "":
		appName := sourceOAuthApp(src)
		var tokenSource oauth2.TokenSource

		// Check for service account configuration
		if saKeyPath := cfg.OAuth.ServiceAccountKeyFor(appName); saKeyPath != "" {
			scopes := saScopes
			if len(scopes) == 0 {
				scopes = oauth.Scopes
			}
			saMgr, err := oauth.NewServiceAccountManager(saKeyPath, scopes)
			if err != nil {
				return nil, fmt.Errorf("service account: %w", err)
			}
			tokenSource, err = saMgr.TokenSource(ctx, src.Identifier)
			if err != nil {
				return nil, err
			}
		} else {
			oauthMgr, err := getOAuthMgr(appName)
			if err != nil {
				return nil, err
			}
			interactive := isatty.IsTerminal(os.Stdin.Fd()) ||
				isatty.IsCygwinTerminal(os.Stdin.Fd())
			tokenSource, err = getTokenSourceWithReauth(ctx, oauthMgr, src.Identifier, interactive, gmailReauthHint)
			if err != nil {
				return nil, err
			}
		}
		rateLimiter := gmail.NewRateLimiter(float64(cfg.Sync.RateLimitQPS))
		return gmail.NewClient(tokenSource,
			gmail.WithLogger(logger),
			gmail.WithRateLimiter(rateLimiter),
		), nil

	case sourceTypeIMAP:
		if !src.SyncConfig.Valid || src.SyncConfig.String == "" {
			return nil, fmt.Errorf("IMAP source %s has no config (run 'add-imap' first)", src.Identifier)
		}
		imapCfg, err := imaplib.ConfigFromJSON(src.SyncConfig.String)
		if err != nil {
			return nil, fmt.Errorf("parse IMAP config: %w", err)
		}

		var opts []imaplib.Option
		opts = append(opts, imaplib.WithLogger(logger))
		opts = append(opts, imapOpts...)

		var since, before time.Time
		if syncAfter != "" {
			t, err := time.Parse("2006-01-02", syncAfter)
			if err != nil {
				return nil, fmt.Errorf("invalid --after date %q (expected YYYY-MM-DD): %w", syncAfter, err)
			}
			since = t
		}
		if syncBefore != "" {
			t, err := time.Parse("2006-01-02", syncBefore)
			if err != nil {
				return nil, fmt.Errorf("invalid --before date %q (expected YYYY-MM-DD): %w", syncBefore, err)
			}
			before = t
		}
		if !since.IsZero() || !before.IsZero() {
			opts = append(opts, imaplib.WithDateFilter(since, before))
		}

		switch imapCfg.EffectiveAuthMethod() {
		case imaplib.AuthXOAuth2:
			if cfg.Microsoft.ClientID == "" {
				return nil, errors.New("microsoft OAuth not configured — add a [microsoft] section with client_id to config.toml")
			}
			msMgr := microsoft.NewManager(
				cfg.Microsoft.ClientID,
				cfg.Microsoft.EffectiveTenantID(),
				cfg.Microsoft.EffectiveRedirectURI(),
				cfg.TokensDir(),
				logger,
			)
			tokenFn, err := msMgr.TokenSource(ctx, imapCfg.Username)
			if err != nil {
				return nil, fmt.Errorf("load Microsoft token: %w (run 'add-o365' first)", err)
			}
			opts = append(opts, imaplib.WithTokenSource(tokenFn))
			return imaplib.NewClient(imapCfg, "", opts...), nil
		default:
			password, err := imaplib.LoadCredentials(cfg.TokensDir(), src.Identifier)
			if err != nil {
				return nil, fmt.Errorf("load IMAP credentials: %w (run 'add-imap' first)", err)
			}
			return imaplib.NewClient(imapCfg, password, opts...), nil
		}

	default:
		return nil, fmt.Errorf("unsupported source type %q", src.SourceType)
	}
}

// loadIMAPFolderStates returns the saved per-mailbox states in the map
// form the IMAP client consumes.
func loadIMAPFolderStates(s *store.Store, sourceID int64) (map[string]imaplib.FolderState, error) {
	saved, err := s.GetIMAPFolderStates(sourceID)
	if err != nil {
		return nil, err
	}
	knownUIDs, err := s.GetIMAPKnownUIDs(sourceID)
	if err != nil {
		return nil, err
	}
	states := make(map[string]imaplib.FolderState, len(saved))
	for _, st := range saved {
		states[st.Mailbox] = imaplib.FolderState{
			UIDValidity:   st.UIDValidity,
			UIDNext:       st.UIDNext,
			HighestModSeq: st.HighestModSeq,
			KnownUIDs:     append([]uint32{}, knownUIDs[st.Mailbox]...),
		}
	}
	return states, nil
}

// imapFolderStateOptions retains saved UIDVALIDITY metadata for identity
// validation. forceRescan disables listing shortcuts without discarding it.
func imapFolderStateOptions(
	s *store.Store,
	src *store.Source,
	forceRescan bool,
) []imaplib.Option {
	if src.SourceType != sourceTypeIMAP {
		return nil
	}

	var opts []imaplib.Option
	states, err := loadIMAPFolderStates(s, src.ID)
	if err != nil {
		logger.Warn("failed to load IMAP folder states", "source", src.Identifier, "error", err)
	} else if len(states) > 0 {
		opts = append(opts, imaplib.WithFolderStates(states))
	}
	opts = append(opts, imaplib.WithSourceMessageAliasLoader(
		func(mailbox string, uids []uint32) (map[string]string, error) {
			return s.GetIMAPSourceMessageAliases(src.ID, mailbox, uids)
		}))
	if forceRescan {
		opts = append(opts, imaplib.WithForceFullEnumeration())
	}
	return opts
}

func applyIMAPMailboxDeltas(
	ctx context.Context,
	s *store.Store,
	src *store.Source,
	syncRunID int64,
	deltas []imaplib.MailboxDelta,
	observations []imaplib.MembershipObservation,
) error {
	type membershipKey struct {
		mailbox     string
		uidValidity uint32
		uid         uint32
	}
	normalizedObservations := make([]store.IMAPMembershipObservation, 0, len(observations))
	observationIndexes := make(map[membershipKey]int, len(observations))
	for _, observation := range observations {
		key := membershipKey{
			mailbox: observation.Mailbox, uidValidity: observation.UIDValidity, uid: observation.UID,
		}
		if index, exists := observationIndexes[key]; exists {
			existing := &normalizedObservations[index]
			if existing.CanonicalSourceMessageID == "" {
				existing.CanonicalSourceMessageID = observation.CanonicalSourceMessageID
			}
			if existing.RFC822MessageID == "" {
				existing.RFC822MessageID = observation.RFC822MessageID
			}
			if existing.RawSHA256 == ([32]byte{}) {
				existing.RawSHA256 = observation.RawSHA256
			}
			if existing.RawSize == 0 {
				existing.RawSize = observation.RawSize
			}
			continue
		}
		observationIndexes[key] = len(normalizedObservations)
		normalizedObservations = append(normalizedObservations, store.IMAPMembershipObservation{
			Mailbox:                  observation.Mailbox,
			UIDValidity:              observation.UIDValidity,
			UID:                      observation.UID,
			SourceMessageID:          observation.SourceMessageID,
			CanonicalSourceMessageID: observation.CanonicalSourceMessageID,
			RFC822MessageID:          observation.RFC822MessageID,
			RawSHA256:                observation.RawSHA256,
			RawSize:                  observation.RawSize,
			Flags:                    append([]string(nil), observation.Flags...),
		})
	}
	byMailbox := make(map[string][]store.IMAPMembershipObservation)
	for _, observation := range normalizedObservations {
		byMailbox[observation.Mailbox] = append(byMailbox[observation.Mailbox], observation)
	}

	storeDeltas := make([]store.IMAPMailboxDelta, 0, len(deltas))
	seenMailboxes := make(map[string]struct{}, len(deltas))
	for _, delta := range deltas {
		if _, exists := seenMailboxes[delta.Mailbox]; exists {
			return fmt.Errorf("duplicate IMAP mailbox delta for %q", delta.Mailbox)
		}
		seenMailboxes[delta.Mailbox] = struct{}{}
		vanished := make([]uint32, len(delta.VanishedUIDs))
		for i, uid := range delta.VanishedUIDs {
			vanished[i] = uint32(uid)
		}
		storeDeltas = append(storeDeltas, store.IMAPMailboxDelta{
			Mailbox: delta.Mailbox,
			State: store.IMAPFolderState{
				Mailbox:       delta.Mailbox,
				UIDValidity:   delta.State.UIDValidity,
				UIDNext:       delta.State.UIDNext,
				HighestModSeq: delta.State.HighestModSeq,
			},
			Memberships:  byMailbox[delta.Mailbox],
			VanishedUIDs: vanished,
			Reset:        delta.Reset,
		})
		delete(byMailbox, delta.Mailbox)
	}
	for mailbox := range byMailbox {
		return fmt.Errorf("IMAP membership observation for mailbox %q has no mailbox delta", mailbox)
	}
	return s.ApplyIMAPMailboxDeltasForSyncContext(ctx, src.ID, syncRunID, storeDeltas)
}

func imapSyncCanCommit(ctx context.Context, summary *gmail.SyncSummary, limit int) bool {
	return summary != nil &&
		ctx.Err() == nil &&
		summary.Errors == 0 &&
		!summary.WasResumed &&
		limit == 0
}

// saveIMAPFolderStates atomically persists mailbox membership and cursor
// observations only after a clean, unlimited, unfiltered sync.
func saveIMAPFolderStates(
	ctx context.Context,
	s *store.Store,
	src *store.Source,
	apiClient gmail.API,
	summary *gmail.SyncSummary,
	limit int,
) error {
	imapClient, ok := apiClient.(*imaplib.Client)
	if !ok || !imapSyncCanCommit(ctx, summary, limit) {
		return nil
	}
	if imapClient.LabelsSnapshotFiltered() || !imapClient.LabelsSnapshotComplete() {
		return nil
	}
	deltas := imapClient.ObservedMailboxDeltas()
	if deltas == nil {
		return nil
	}
	if err := applyIMAPMailboxDeltas(
		ctx, s, src, summary.SyncRunID, deltas, imapClient.ObservedMemberships(),
	); err != nil {
		return fmt.Errorf("apply IMAP mailbox deltas: %w", err)
	}
	return nil
}

func runFullSync(ctx context.Context, s *store.Store, getOAuthMgr func(string) (*oauth.Manager, error), src *store.Source) error {
	progress := &CLIProgress{}

	// --noresume promises a fresh sync, so it must also bypass the
	// saved folder high water marks and re-enumerate every mailbox. A clean
	// completed run still saves fresh high water marks afterwards.
	imapOpts := imapFolderStateOptions(s, src, syncNoResume)

	// Pass CLI folder filter strings to the IMAP client. The IMAP
	// client selects the effective include list (CLI --folder when
	// set, otherwise config Folders) and applies CLI --skip-folder
	// as an exclusion on top. Both CLI filters are applied in a
	// single call to filterMailboxes so --folder can fully
	// override configuration and --skip-folder works together with
	// --folder.
	imapOpts = append(imapOpts,
		imaplib.WithFolderFilter(
			parseFolderFilter(syncFolders),
			parseFolderFilter(syncSkipFolders),
		))

	if src.SourceType == sourceTypeIMAP {
		imapOpts = append(imapOpts,
			imaplib.WithListProgress(progress.OnIMAPListProgress),
		)
	}
	apiClient, err := buildAPIClient(ctx, src, getOAuthMgr, nil, imapOpts...)
	if err != nil {
		return err
	}
	defer func() { _ = apiClient.Close() }()

	// Build query from flags (Gmail only; IMAP date filters are
	// handled via WithDateFilter on the client).
	query := buildSyncQuery()
	if query != "" && src.SourceType == sourceTypeIMAP {
		// --after/--before are handled natively by IMAP SEARCH;
		// only warn about --query which has no IMAP equivalent.
		if syncQuery != "" {
			fmt.Printf("Warning: --query is not supported for IMAP sources and will be ignored.\n\n")
		}
		query = ""
	}

	// Set up sync options
	opts := sync.DefaultOptions()
	opts.SourceType = src.SourceType
	opts.Query = query
	opts.NoResume = syncNoResume
	opts.Limit = syncLimit
	opts.OperationID = syncOperationID
	opts.AttachmentsDir = cfg.AttachmentsDir()

	// IMAP page tokens are numeric offsets into a message list
	// rebuilt from live mailbox state each session. Cross-session
	// resume is unreliable because additions or deletions shift
	// the offsets. Already-imported messages are efficiently
	// skipped via MessageExistsWithRawBatch.
	if src.SourceType == sourceTypeIMAP {
		opts.NoResume = true
	}

	// Create syncer with progress reporter
	syncer := newMessageSyncer(apiClient, s, opts).
		WithLogger(logger).
		WithProgress(progress)

	// Run sync
	startTime := time.Now()
	displayID := src.Identifier
	if src.DisplayName.Valid && src.DisplayName.String != "" {
		displayID = src.DisplayName.String
	}
	fmt.Printf("Starting full sync for %s\n", displayID)
	if query != "" && src.SourceType != sourceTypeIMAP {
		fmt.Printf("Query: %s\n", query)
	}
	fmt.Println()
	syncSource := src
	if syncSource.ID == 0 {
		sourceType := syncSource.SourceType
		if sourceType == "" {
			sourceType = sourceTypeGmail
		}
		syncSource, err = s.GetOrCreateSource(sourceType, syncSource.Identifier)
		if err != nil {
			return fmt.Errorf("get/create source: %w", err)
		}
	}

	summary, err := syncer.FullWithFinalizer(
		ctx,
		syncSource,
		func(summary *gmail.SyncSummary) error {
			if src.SourceType != sourceTypeIMAP {
				return nil
			}
			if err := saveIMAPFolderStates(
				ctx, s, src, apiClient, summary, opts.Limit,
			); err != nil {
				return fmt.Errorf("save IMAP incremental state: %w", err)
			}
			return nil
		},
	)
	if err != nil {
		if ctx.Err() != nil {
			if opts.NoResume {
				fmt.Println("\nSync interrupted. Run again to restart (already-imported messages will be skipped).")
			} else {
				fmt.Println("\nSync interrupted. Run again to resume.")
			}
			return nil
		}
		return fmt.Errorf("sync failed: %w", err)
	}

	// Print summary; skip the spacer when no progress lines were
	// printed so a no-op sync doesn't emit stacked blank lines.
	if progress.printedAnything() {
		fmt.Println()
	}
	fmt.Println("Sync complete!")
	fmt.Printf("  Duration:      %s\n", summary.Duration.Round(time.Second))
	fmt.Printf("  Messages:      %d found, %d added, %d skipped\n",
		summary.MessagesFound, summary.MessagesAdded, summary.MessagesSkipped)
	fmt.Printf("  Downloaded:    %.2f MB\n", float64(summary.BytesDownloaded)/(1024*1024))
	if summary.Errors > 0 {
		fmt.Printf("  Errors:        %d\n", summary.Errors)
	}
	if summary.WasResumed {
		fmt.Printf("  (Resumed from checkpoint)\n")
	}

	// Print timing stats
	if summary.MessagesAdded > 0 {
		messagesPerSec := float64(summary.MessagesAdded) / summary.Duration.Seconds()
		fmt.Printf("  Rate:          %.1f messages/sec\n", messagesPerSec)
	}

	elapsed := time.Since(startTime)
	logger.Info("sync completed",
		"identifier", displayID,
		"messages_added", summary.MessagesAdded,
		"elapsed", elapsed,
	)

	return nil
}

// trimFolderFilter trims whitespace from folder names and removes empty
// entries. Returns nil when the input slice is empty.
func parseFolderFilter(folders []string) []string {
	if len(folders) == 0 {
		return nil
	}
	kept := make([]string, 0, len(folders))
	for _, f := range folders {
		f = strings.TrimSpace(f)
		if f != "" {
			kept = append(kept, f)
		}
	}
	return kept
}
func buildSyncQuery() string {
	parts := []string{}

	if syncAfter != "" {
		parts = append(parts, "after:"+syncAfter)
	}
	if syncBefore != "" {
		parts = append(parts, "before:"+syncBefore)
	}
	if syncQuery != "" {
		parts = append(parts, syncQuery)
	}

	result := ""
	var resultSb447 strings.Builder
	for i, p := range parts {
		if i > 0 {
			resultSb447.WriteString(" ")
		}
		resultSb447.WriteString(p)
	}
	result += resultSb447.String()
	return result
}

// CLIProgress implements gmail.SyncProgressWithDate for terminal output.
// progressOutputMode selects how CLIProgress renders updates.
type progressOutputMode int

const (
	// progressModeAuto detects the mode from stdout on first use.
	progressModeAuto progressOutputMode = iota
	// progressModeTTY redraws a single status line in place with \r.
	progressModeTTY
	// progressModePlain emits one newline-terminated update at a lower
	// cadence. Used when stdout is a pipe — the daemon CLI subprocess,
	// redirected output, CI — where \r overwriting cannot work and would
	// interleave with stderr into one unreadable blob.
	progressModePlain
)

const (
	cliProgressTTYInterval   = 2 * time.Second
	cliProgressPlainInterval = 30 * time.Second
	// Folder listing is much faster per item than message fetching, so
	// plain-mode listing updates can come more often without flooding.
	cliListPlainInterval = 15 * time.Second
)

type CLIProgress struct {
	startTime  time.Time
	lastPrint  time.Time
	latestDate time.Time
	// Cache latest stats for combined display
	processed int64
	added     int64
	skipped   int64
	mode      progressOutputMode
	out       io.Writer // defaults to os.Stdout; tests inject a buffer

	printedProgress bool      // a sync progress line has been printed
	printedList     bool      // a folder-listing line has been printed
	lastListPrint   time.Time // throttle for intermediate listing updates
}

// printedAnything reports whether any progress output was emitted,
// so callers can avoid stacking blank lines around silent syncs.
func (p *CLIProgress) printedAnything() bool {
	return p.printedProgress || p.printedList
}

func (p *CLIProgress) OnStart(total int64) {
	now := time.Now()
	p.startTime = now
	p.lastPrint = now
	// Don't print Gmail's estimate - it's often wildly inaccurate
}

func (p *CLIProgress) OnProgress(processed, added, skipped int64) {
	if p.startTime.IsZero() {
		now := time.Now()
		p.startTime = now
		p.lastPrint = now
	}
	p.processed = processed
	p.added = added
	p.skipped = skipped
	p.printProgress()
}

func (p *CLIProgress) OnLatestDate(date time.Time) {
	if p.startTime.IsZero() {
		now := time.Now()
		p.startTime = now
		p.lastPrint = now
	}
	// Record only; the next OnProgress renders it. Printing here would
	// consume the throttle window with whatever counters happen to be
	// cached — in plain mode that emits a permanent line with stale (or
	// zero) Scanned/Added values and suppresses the accurate one that
	// follows.
	p.latestDate = date
}

// OnIMAPListProgress renders mailbox-enumeration progress for IMAP
// syncs (the phase before any message is fetched, which is otherwise
// silent). The first and final updates always print; the final one is
// a permanent summary line so even an instant all-skipped resync shows
// what happened.
func (p *CLIProgress) OnIMAPListProgress(done, total int, mailbox string, found, unchanged int) {
	tty := p.outputMode() == progressModeTTY

	if done >= total {
		prefix := ""
		if tty && p.printedList {
			prefix = "\r" // overwrite the in-place listing line
		}
		skipNote := ""
		if unchanged > 0 {
			skipNote = fmt.Sprintf(", %d unchanged (skipped)", unchanged)
		}
		// Trailing spaces overwrite leftovers of a longer in-place line.
		_, _ = fmt.Fprintf(p.writer(),
			"%s  Checked %d folders: %d messages to examine%s                    \n",
			prefix, total, found, skipNote)
		p.printedList = true
		return
	}

	interval := cliProgressTTYInterval
	if !tty {
		interval = cliListPlainInterval
	}
	if p.printedList && time.Since(p.lastListPrint) < interval {
		return
	}
	p.printedList = true
	p.lastListPrint = time.Now()

	if tty {
		_, _ = fmt.Fprintf(p.writer(),
			"\r  Checking folders: %d/%d (%s)    ", done, total, mailbox)
		return
	}
	if done == 0 {
		_, _ = fmt.Fprintf(p.writer(), "  Checking %d folders...\n", total)
		return
	}
	_, _ = fmt.Fprintf(p.writer(), "  Checking folders: %d/%d\n", done, total)
}

func (p *CLIProgress) outputMode() progressOutputMode {
	if p.mode == progressModeAuto {
		if isatty.IsTerminal(os.Stdout.Fd()) || isatty.IsCygwinTerminal(os.Stdout.Fd()) {
			p.mode = progressModeTTY
		} else {
			p.mode = progressModePlain
		}
	}
	return p.mode
}

func (p *CLIProgress) writer() io.Writer {
	if p.out == nil {
		return os.Stdout
	}
	return p.out
}

func (p *CLIProgress) printProgress() {
	// Throttle: an in-place line can refresh every 2 seconds, but each
	// plain-mode update is a permanent line, so those come every 30.
	// The first line always prints so a slow fetch (IMAP especially)
	// shows signs of life as soon as the first page completes.
	interval := cliProgressTTYInterval
	if p.outputMode() == progressModePlain {
		interval = cliProgressPlainInterval
	}
	if p.printedProgress && time.Since(p.lastPrint) < interval {
		return
	}
	p.printedProgress = true
	p.lastPrint = time.Now()

	elapsed := time.Since(p.startTime)
	rate := 0.0
	if elapsed.Seconds() >= 1 {
		rate = float64(p.added) / elapsed.Seconds()
	}

	// Format elapsed time nicely
	elapsedStr := formatCLIProgressDuration(elapsed, cliProgressDurationSpaced)

	// Format latest message date if available
	dateStr := ""
	if !p.latestDate.IsZero() {
		dateStr = " | Latest: " + p.latestDate.Format("Jan 2006")
	}

	if p.outputMode() == progressModePlain {
		_, _ = fmt.Fprintf(p.writer(),
			"  Scanned: %d | Added: %d | Skipped: %d | Rate: %.1f/s | Elapsed: %s%s\n",
			p.processed, p.added, p.skipped, rate, elapsedStr, dateStr)
		return
	}
	_, _ = fmt.Fprintf(p.writer(),
		"\r  Scanned: %d | Added: %d | Skipped: %d | Rate: %.1f/s | Elapsed: %s%s    ",
		p.processed, p.added, p.skipped, rate, elapsedStr, dateStr)
}

func (p *CLIProgress) OnComplete(summary *gmail.SyncSummary) {
	if p.outputMode() == progressModePlain {
		return // every plain-mode update already ended its line
	}
	_, _ = fmt.Fprintln(p.writer()) // terminate the in-place progress line
}

func (p *CLIProgress) OnError(err error) {
	_, _ = fmt.Fprintf(p.writer(), "\nError: %v\n", err)
}

// imapSkipReason checks whether an IMAP source has the credentials needed to
// sync. Return values:
//   - ("", nil)     — credentials present, source is ready
//   - ("msg", nil)  — credentials absent; print the message and skip
//   - ("", err)     — sync_config is malformed; add to the error list
func imapSkipReason(src *store.Source) (string, error) {
	if !src.SyncConfig.Valid || src.SyncConfig.String == "" {
		if !imaplib.HasCredentials(cfg.TokensDir(), src.Identifier) {
			return fmt.Sprintf("Skipping %s (no credentials — run 'add-imap' or 'add-o365' first)", src.Identifier), nil
		}
		return "", nil
	}
	imapCfg, err := imaplib.ConfigFromJSON(src.SyncConfig.String)
	if err != nil {
		return "", err
	}
	switch imapCfg.EffectiveAuthMethod() {
	case imaplib.AuthXOAuth2:
		if cfg.Microsoft.ClientID == "" {
			return fmt.Sprintf("Skipping %s (Microsoft OAuth not configured — add client_id to [microsoft] in config.toml)", src.Identifier), nil
		}
		msMgr := microsoft.NewManager(
			cfg.Microsoft.ClientID,
			cfg.Microsoft.EffectiveTenantID(),
			cfg.Microsoft.EffectiveRedirectURI(),
			cfg.TokensDir(),
			logger,
		)
		if !msMgr.HasToken(imapCfg.Username) {
			return fmt.Sprintf("Skipping %s (no Microsoft token — run 'add-o365' first)", src.Identifier), nil
		}
	default:
		if !imaplib.HasCredentials(cfg.TokensDir(), src.Identifier) {
			return fmt.Sprintf("Skipping %s (no credentials — run 'add-imap' first)", src.Identifier), nil
		}
	}
	return "", nil
}

func init() {
	syncFullCmd.Flags().Int64("source-id", 0, "Exact source ID to sync")
	syncFullCmd.Flags().StringVar(&syncOperationID, "sync-operation-id", "", "Attribute runs to a daemon sync operation")
	_ = syncFullCmd.Flags().MarkHidden("sync-operation-id")
	syncFullCmd.Flags().StringVar(&syncQuery, "query", "", "Gmail search query")
	syncFullCmd.Flags().BoolVar(&syncNoResume, "noresume", false, "Force fresh sync (don't resume; re-enumerates all IMAP folders)")
	syncFullCmd.Flags().StringVar(&syncBefore, "before", "", "Only messages before this date (YYYY-MM-DD)")
	syncFullCmd.Flags().StringVar(&syncAfter, "after", "", "Only messages after this date (YYYY-MM-DD)")
	syncFullCmd.Flags().IntVar(&syncLimit, "limit", 0, "Limit number of messages (for testing)")
	syncFullCmd.Flags().StringArrayVar(&syncFolders, "folder", []string{}, "IMAP folder to scan (repeatable)")
	syncFullCmd.Flags().StringArrayVar(&syncSkipFolders, "skip-folder", []string{}, "IMAP folder to skip (repeatable)")
	rootCmd.AddCommand(syncFullCmd)
}
