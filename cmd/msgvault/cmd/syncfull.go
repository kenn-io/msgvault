package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
	"github.com/wesm/msgvault/internal/gmail"
	imaplib "github.com/wesm/msgvault/internal/imap"
	"github.com/wesm/msgvault/internal/microsoft"
	"github.com/wesm/msgvault/internal/oauth"
	"github.com/wesm/msgvault/internal/store"
	"github.com/wesm/msgvault/internal/sync"
	"golang.org/x/oauth2"
)

var (
	syncQuery    string
	syncNoResume bool
	syncBefore   string
	syncAfter    string
	syncLimit    int
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
		if syncLimit < 0 {
			return fmt.Errorf("--limit must be a non-negative number")
		}
		if syncAfter != "" {
			if _, err := time.Parse("2006-01-02", syncAfter); err != nil {
				return fmt.Errorf("invalid --after date %q (expected YYYY-MM-DD): %w", syncAfter, err)
			}
		}
		if syncBefore != "" {
			if _, err := time.Parse("2006-01-02", syncBefore); err != nil {
				return fmt.Errorf("invalid --before date %q (expected YYYY-MM-DD): %w", syncBefore, err)
			}
		}

		// Open database
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

		getOAuthMgr := oauthManagerCache()

		// Determine which sources to sync
		var sources []*store.Source
		var syncErrors []string
		if len(args) == 1 {
			// Look up all sources matching the identifier and
			// keep only syncable types (gmail, imap). Non-syncable
			// sources like mbox/apple-mail imports share the same
			// identifier namespace but cannot be synced.
			allMatches, err := s.GetSourcesByIdentifierOrDisplayName(args[0])
			if err != nil {
				return fmt.Errorf("look up source: %w", err)
			}
			for _, src := range allMatches {
				if src.SourceType == "gmail" || src.SourceType == "imap" {
					sources = append(sources, src)
				}
			}
			if len(sources) == 0 {
				if len(allMatches) > 0 {
					// Identifier exists but has no syncable source types.
					return fmt.Errorf("account %q exists but its source type cannot be synced (only gmail and imap are supported)", args[0])
				}
				// Not in DB yet - assume Gmail (legacy behaviour)
				sources = []*store.Source{{SourceType: "gmail", Identifier: args[0]}}
			}
		} else {
			// Sync all configured sources
			allSources, err := s.ListSources("")
			if err != nil {
				return fmt.Errorf("list sources: %w", err)
			}
			if len(allSources) == 0 {
				return fmt.Errorf("no accounts configured - run 'add-account' or 'add-imap' first")
			}
			for _, src := range allSources {
				switch src.SourceType {
				case "gmail":
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
				case "imap":
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
				return fmt.Errorf("no accounts are ready to sync")
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

		// Open vector backend (optional) so newly-ingested messages
		// are enqueued for embedding.
		vf, err := setupVectorFeatures(ctx, s.DB(), dbPath)
		if err != nil {
			return fmt.Errorf("vector features: %w", err)
		}
		defer func() {
			if vf != nil && vf.Close != nil {
				if closeErr := vf.Close(); closeErr != nil {
					logger.Warn("closing vectors.db failed", "error", closeErr)
				}
			}
		}()

		for _, src := range sources {
			if ctx.Err() != nil {
				break
			}

			// Ensure credentials are available before syncing Gmail sources.
			if src.SourceType == "gmail" || src.SourceType == "" {
				appName := sourceOAuthApp(src)
				if cfg.OAuth.ServiceAccountKeyFor(appName) == "" {
					if _, err := getOAuthMgr(appName); err != nil {
						syncErrors = append(syncErrors, fmt.Sprintf("%s: %v", src.Identifier, err))
						continue
					}
				}
			}

			if err := runFullSync(ctx, s, getOAuthMgr, src, vf); err != nil {
				syncErrors = append(syncErrors, fmt.Sprintf("%s: %v", src.Identifier, err))
				continue
			}
		}

		// Rebuild analytics cache.
		rebuildCacheAfterWrite(dbPath)

		if len(syncErrors) > 0 {
			fmt.Println()
			fmt.Println("Errors:")
			for _, e := range syncErrors {
				fmt.Printf("  %s\n", e)
			}
			return fmt.Errorf("%d account(s) failed to sync: %s",
				len(syncErrors), strings.Join(syncErrors, "; "))
		}

		return nil
	},
}

// buildAPIClient creates the appropriate gmail.API client for the given
// source. saScopes is used when the resolved oauth_app is backed by a
// service account key; for browser-OAuth sources, scopes flow through the
// caller-provided getOAuthMgr factory. Pass nil to use oauth.Scopes; pass
// oauth.ScopesDeletion (or another set) for workflows that need elevated
// access.
func buildAPIClient(ctx context.Context, src *store.Source, getOAuthMgr func(string) (*oauth.Manager, error), saScopes []string) (gmail.API, error) {
	switch src.SourceType {
	case "gmail", "":
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
			tokenSource, err = getTokenSourceWithReauth(ctx, oauthMgr, src.Identifier, interactive)
			if err != nil {
				return nil, err
			}
		}
		rateLimiter := gmail.NewRateLimiter(float64(cfg.Sync.RateLimitQPS))
		return gmail.NewClient(tokenSource,
			gmail.WithLogger(logger),
			gmail.WithRateLimiter(rateLimiter),
		), nil

	case "imap":
		if !src.SyncConfig.Valid || src.SyncConfig.String == "" {
			return nil, fmt.Errorf("IMAP source %s has no config (run 'add-imap' first)", src.Identifier)
		}
		imapCfg, err := imaplib.ConfigFromJSON(src.SyncConfig.String)
		if err != nil {
			return nil, fmt.Errorf("parse IMAP config: %w", err)
		}

		var opts []imaplib.Option
		opts = append(opts, imaplib.WithLogger(logger))

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
				return nil, fmt.Errorf("microsoft OAuth not configured — add a [microsoft] section with client_id to config.toml")
			}
			msMgr := microsoft.NewManager(
				cfg.Microsoft.ClientID,
				cfg.Microsoft.EffectiveTenantID(),
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

func runFullSync(ctx context.Context, s *store.Store, getOAuthMgr func(string) (*oauth.Manager, error), src *store.Source, vf *vectorFeatures) error {
	apiClient, err := buildAPIClient(ctx, src, getOAuthMgr, nil)
	if err != nil {
		return err
	}
	defer func() { _ = apiClient.Close() }()

	// Build query from flags (Gmail only; IMAP date filters are
	// handled via WithDateFilter on the client).
	query := buildSyncQuery()
	if query != "" && src.SourceType == "imap" {
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
	opts.AttachmentsDir = cfg.AttachmentsDir()

	// IMAP page tokens are numeric offsets into a message list
	// rebuilt from live mailbox state each session. Cross-session
	// resume is unreliable because additions or deletions shift
	// the offsets. Already-imported messages are efficiently
	// skipped via MessageExistsWithRawBatch.
	if src.SourceType == "imap" {
		opts.NoResume = true
	}

	// Create syncer with progress reporter
	syncer := sync.New(apiClient, s, opts).
		WithLogger(logger).
		WithProgress(&CLIProgress{})
	if vf != nil {
		syncer.SetEmbedEnqueuer(vf.Enqueuer)
	}

	// Run sync
	startTime := time.Now()
	displayID := src.Identifier
	if src.DisplayName.Valid && src.DisplayName.String != "" {
		displayID = src.DisplayName.String
	}
	fmt.Printf("Starting full sync for %s\n", displayID)
	if query != "" && src.SourceType != "imap" {
		fmt.Printf("Query: %s\n", query)
	}
	fmt.Println()

	summary, err := syncer.Full(ctx, src.Identifier)
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

	// Print summary
	fmt.Println()
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

// buildSyncQuery constructs a Gmail search query from flags.
func buildSyncQuery() string {
	parts := []string{}

	if syncAfter != "" {
		parts = append(parts, fmt.Sprintf("after:%s", syncAfter))
	}
	if syncBefore != "" {
		parts = append(parts, fmt.Sprintf("before:%s", syncBefore))
	}
	if syncQuery != "" {
		parts = append(parts, syncQuery)
	}

	result := ""
	for i, p := range parts {
		if i > 0 {
			result += " "
		}
		result += p
	}
	return result
}

// CLIProgress implements gmail.SyncProgressWithDate for terminal output.
type CLIProgress struct {
	startTime  time.Time
	lastPrint  time.Time
	latestDate time.Time
	// Cache latest stats for combined display
	processed int64
	added     int64
	skipped   int64
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
	p.latestDate = date
	p.printProgress()
}

func (p *CLIProgress) printProgress() {
	// Throttle output to every 2 seconds
	if time.Since(p.lastPrint) < 2*time.Second {
		return
	}
	p.lastPrint = time.Now()

	elapsed := time.Since(p.startTime)
	rate := 0.0
	if elapsed.Seconds() >= 1 {
		rate = float64(p.added) / elapsed.Seconds()
	}

	// Format elapsed time nicely
	elapsedStr := formatDuration(elapsed)

	// Format latest message date if available
	dateStr := ""
	if !p.latestDate.IsZero() {
		dateStr = fmt.Sprintf(" | Latest: %s", p.latestDate.Format("Jan 2006"))
	}

	fmt.Printf("\r  Scanned: %d | Added: %d | Skipped: %d | Rate: %.1f/s | Elapsed: %s%s    ",
		p.processed, p.added, p.skipped, rate, elapsedStr, dateStr)
}

func (p *CLIProgress) OnComplete(summary *gmail.SyncSummary) {
	fmt.Println() // Clear the progress line
}

func (p *CLIProgress) OnError(err error) {
	fmt.Printf("\nError: %v\n", err)
}

// formatDuration formats a duration as "Xm Ys" or "Xh Ym" for readability.
func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second

	if h > 0 {
		return fmt.Sprintf("%dh %dm", h, m)
	}
	if m > 0 {
		return fmt.Sprintf("%dm %ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
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
	syncFullCmd.Flags().StringVar(&syncQuery, "query", "", "Gmail search query")
	syncFullCmd.Flags().BoolVar(&syncNoResume, "noresume", false, "Force fresh sync (don't resume)")
	syncFullCmd.Flags().StringVar(&syncBefore, "before", "", "Only messages before this date (YYYY-MM-DD)")
	syncFullCmd.Flags().StringVar(&syncAfter, "after", "", "Only messages after this date (YYYY-MM-DD)")
	syncFullCmd.Flags().IntVar(&syncLimit, "limit", 0, "Limit number of messages (for testing)")
	rootCmd.AddCommand(syncFullCmd)
}
