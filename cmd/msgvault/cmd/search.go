package cmd

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/wesm/msgvault/internal/query"
	"github.com/wesm/msgvault/internal/search"
	"github.com/wesm/msgvault/internal/store"
)

var (
	searchLimit      int
	searchOffset     int
	searchJSON       bool
	searchAccount    string
	searchCollection string
	searchMode       string
	searchExplain    bool
)

var searchCmd = &cobra.Command{
	Use:   "search <query>",
	Short: "Search messages using Gmail-like query syntax",
	Long: `Search your email archive using Gmail-like query syntax.

Uses remote server if [remote].url is configured, otherwise uses local database.
Use --local to force local database.

Supported operators (local mode only - remote uses simple text search):
  from:        Sender email address
  to:          Recipient email address
  cc:          CC recipient
  bcc:         BCC recipient
  subject:     Subject text search
  label:       Gmail label (or l: shorthand)
  has:         has:attachment - messages with attachments
  before:      Messages before date (YYYY-MM-DD)
  after:       Messages after date (YYYY-MM-DD)
  older_than:  Relative date (7d, 2w, 1m, 1y)
  newer_than:  Relative date
  larger:      Size filter (5M, 100K)
  smaller:     Size filter

Bare words and "quoted phrases" perform full-text search.

Examples:
  msgvault search from:alice@example.com has:attachment
  msgvault search subject:meeting after:2024-01-01
  msgvault search project report newer_than:30d
  msgvault search '"exact phrase"' label:INBOX`,
	Args: cobra.ArbitraryArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Join all args to form the query (allows unquoted multi-term searches)
		queryStr := strings.Join(args, " ")

		if queryStr == "" && searchAccount == "" && searchCollection == "" {
			return fmt.Errorf("provide a search query or --account/--collection flag")
		}

		// Use remote search if configured
		if IsRemoteMode() {
			if searchAccount != "" {
				return fmt.Errorf(
					"--account is not supported in remote mode",
				)
			}
			if searchCollection != "" {
				return fmt.Errorf("--collection is not supported in remote mode")
			}
			if searchMode != "fts" {
				return fmt.Errorf("--mode is not supported in remote mode")
			}
			return runRemoteSearch(queryStr)
		}

		// Validate mode before any scope work so we fail fast on a typo.
		if searchMode != "fts" && searchMode != "vector" && searchMode != "hybrid" {
			return fmt.Errorf("invalid --mode: %q (want fts|vector|hybrid)", searchMode)
		}
		if searchMode != "fts" && searchOffset > 0 {
			return fmt.Errorf("--offset is not supported with --mode=%s (pagination is single-page)", searchMode)
		}
		// Vector and hybrid modes need free-text terms to embed; both
		// an empty raw query and a filter-only query (e.g. `from:alice`)
		// would fail at the embed call. Check both up front and surface
		// a CLI error rather than a late engine-level one. FTS still
		// allows scoped queryless searches.
		if searchMode != "fts" {
			if queryStr == "" {
				return fmt.Errorf("--mode=%s requires query text to embed; pass a query or use --mode=fts", searchMode)
			}
			if len(search.Parse(queryStr).TextTerms) == 0 {
				return fmt.Errorf("--mode=%s requires free-text terms to embed; %q parsed to filters only — add a search phrase or use --mode=fts", searchMode, queryStr)
			}
		}

		// Resolve --account / --collection once, before the mode branch,
		// so FTS, vector, and hybrid all see the same SourceIDs. Earlier,
		// scope was resolved inside runLocalSearch only and the vector
		// path applied --account directly while ignoring --collection.
		scope, scopedStore, err := resolveSearchScope(searchAccount, searchCollection)
		if err != nil {
			return err
		}

		if searchMode != "fts" {
			// Hybrid/vector path opens its own sql.DB directly. When a
			// scoped store is in hand, schema init has already run on
			// this DSN; otherwise we have to run it ourselves so the
			// raw sql.DB inside runHybridSearch sees the deleted_at /
			// deleted_from_source_at columns the vector backend filters
			// on. Close immediately — migration state persists in the
			// file on disk, and runHybridSearch will reopen.
			if scopedStore != nil {
				_ = scopedStore.Close()
			} else {
				if err := initLocalSchema(); err != nil {
					return err
				}
			}
			return runHybridSearch(cmd, queryStr, searchMode, searchExplain, scope)
		}
		return runLocalSearch(cmd, queryStr, scope, scopedStore)
	},
}

// initLocalSchema opens the local store, runs InitSchema and the
// startup migrations, then closes it. Used by the unscoped vector/
// hybrid path so the raw sql.DB that runHybridSearch opens sees a
// fully-migrated schema (notably the deleted_at column the vector
// backend filters on, added by this branch's ALTER TABLE migration).
// Scoped queries don't need this because resolveSearchScope already
// runs the same init on the same DSN.
func initLocalSchema() error {
	s, err := store.Open(cfg.DatabaseDSN())
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
	return nil
}

// resolveSearchScope opens the local store just long enough to resolve
// the user-supplied --account/--collection flag into a Scope. Returning
// the Scope (rather than just SourceIDs) lets callers print a banner
// using its DisplayName. The opened store is returned so a caller that
// needs it (runLocalSearch) can reuse it instead of re-running
// InitSchema + runStartupMigrations a second time. Callers that don't
// need the store must Close it themselves.
//
// When no scope flag was supplied, returns (Scope{}, nil, nil) and the
// caller is responsible for opening its own store.
func resolveSearchScope(account, collection string) (Scope, *store.Store, error) {
	if account == "" && collection == "" {
		return Scope{}, nil, nil
	}
	s, err := store.Open(cfg.DatabaseDSN())
	if err != nil {
		return Scope{}, nil, fmt.Errorf("open database: %w", err)
	}
	if err := s.InitSchema(); err != nil {
		_ = s.Close()
		return Scope{}, nil, fmt.Errorf("init schema: %w", err)
	}
	if err := runStartupMigrations(s); err != nil {
		_ = s.Close()
		return Scope{}, nil, fmt.Errorf("startup migrations: %w", err)
	}
	switch {
	case account != "":
		scope, err := ResolveAccountFlag(s, account)
		if err != nil {
			_ = s.Close()
			return Scope{}, nil, err
		}
		if scope.IsEmpty() {
			_ = s.Close()
			return Scope{}, nil, fmt.Errorf("--account %q resolved to zero sources", account)
		}
		return scope, s, nil
	case collection != "":
		scope, err := ResolveCollectionFlag(s, collection)
		if err != nil {
			_ = s.Close()
			return Scope{}, nil, err
		}
		if len(scope.SourceIDs()) == 0 {
			_ = s.Close()
			return Scope{}, nil, fmt.Errorf("--collection %q has no member accounts", collection)
		}
		return scope, s, nil
	}
	_ = s.Close()
	return Scope{}, nil, nil
}

// runRemoteSearch performs a search against the remote API.
func runRemoteSearch(queryStr string) error {
	fmt.Fprintf(os.Stderr, "Searching %s...", cfg.Remote.URL)

	s, err := OpenRemoteStore()
	if err != nil {
		return fmt.Errorf("connect to remote: %w", err)
	}
	defer func() { _ = s.Close() }()

	results, total, err := s.SearchMessages(queryStr, searchOffset, searchLimit)
	fmt.Fprintf(os.Stderr, "\r                                                      \r")
	if err != nil {
		return fmt.Errorf("search: %w", err)
	}

	if len(results) == 0 {
		fmt.Println("No messages found.")
		return nil
	}

	if searchJSON {
		return outputRemoteSearchResultsJSON(results, total)
	}
	return outputRemoteSearchResultsTable(results, total)
}

// runLocalSearch performs a search against the local database. The
// caller is expected to have resolved scope already; an empty Scope
// means no --account or --collection was supplied. If scopedStore is
// non-nil it carries an already-initialized store from scope
// resolution; runLocalSearch reuses it (avoiding a second
// InitSchema + runStartupMigrations pass). When scopedStore is nil
// (no scope flag supplied), runLocalSearch opens and initializes
// its own store.
func runLocalSearch(cmd *cobra.Command, queryStr string, scope Scope, scopedStore *store.Store) error {
	// Parse the query and apply any pre-resolved scope before the
	// emptiness check so a bare --account/--collection is enough to
	// produce a non-empty query.
	q := search.Parse(queryStr)
	if !scope.IsEmpty() {
		q.AccountIDs = scope.SourceIDs()
	}
	if q.IsEmpty() {
		if scopedStore != nil {
			_ = scopedStore.Close()
		}
		return fmt.Errorf("empty search query")
	}

	var s *store.Store
	if scopedStore != nil {
		s = scopedStore
	} else {
		var err error
		s, err = store.Open(cfg.DatabaseDSN())
		if err != nil {
			return fmt.Errorf("open database: %w", err)
		}
		if err := s.InitSchema(); err != nil {
			_ = s.Close()
			return fmt.Errorf("init schema: %w", err)
		}
		if err := runStartupMigrations(s); err != nil {
			_ = s.Close()
			return fmt.Errorf("startup migrations: %w", err)
		}
	}
	defer func() { _ = s.Close() }()

	// Print a scope banner when searching a collection.
	if scope.IsCollection() {
		members := scope.SourceIDs()
		n := len(members)
		suffix := "s"
		if n == 1 {
			suffix = ""
		}
		fmt.Fprintf(os.Stderr,
			"Searching collection %q (%d account%s)\n",
			scope.DisplayName(), n, suffix,
		)
	}

	fmt.Fprintf(os.Stderr, "Searching...")

	if err := ensureFTSIndex(s); err != nil {
		return err
	}

	// Log the search operation. Raw query text and account
	// identifiers may contain PII — log coarse metadata at
	// info and full values only at debug.
	hasAccount := len(q.AccountIDs) > 0
	logger.Info("search start",
		"query_len", len(queryStr),
		"has_account", hasAccount,
		"limit", searchLimit,
		"offset", searchOffset,
	)
	logger.Debug("search start detail",
		"query", queryStr,
		"account", searchAccount,
	)
	started := time.Now()

	// Create query engine and execute search
	engine := query.NewSQLiteEngine(s.DB())
	results, err := engine.Search(cmd.Context(), q, searchLimit, searchOffset)
	fmt.Fprintf(os.Stderr, "\r            \r")
	if err != nil {
		logger.Warn("search failed",
			"query_len", len(queryStr),
			"duration_ms", time.Since(started).Milliseconds(),
			"error", err.Error(),
		)
		return query.HintRepairEncoding(fmt.Errorf("search: %w", err))
	}
	logger.Info("search done",
		"query_len", len(queryStr),
		"has_account", hasAccount,
		"results", len(results),
		"duration_ms", time.Since(started).Milliseconds(),
	)

	if len(results) == 0 {
		fmt.Println("No messages found.")
		return nil
	}

	if searchJSON {
		return outputSearchResultsJSON(results)
	}
	return outputSearchResultsTable(results)
}

func outputSearchResultsTable(results []query.MessageSummary) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tDATE\tFROM\tSUBJECT\tSIZE")
	_, _ = fmt.Fprintln(w, "──\t────\t────\t───────\t────")

	for _, msg := range results {
		date := msg.SentAt.Format("2006-01-02")
		from := truncate(msg.FromEmail, 30)
		subject := truncate(msg.Subject, 50)
		size := formatSize(msg.SizeEstimate)
		_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n", msg.ID, date, from, subject, size)
	}

	_ = w.Flush()
	fmt.Printf("\nShowing %d results\n", len(results))
	return nil
}

func outputRemoteSearchResultsTable(results []store.APIMessage, total int64) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tDATE\tFROM\tSUBJECT\tSIZE")
	_, _ = fmt.Fprintln(w, "──\t────\t────\t───────\t────")

	for _, msg := range results {
		date := msg.SentAt.Format("2006-01-02")
		from := truncate(msg.From, 30)
		subject := truncate(msg.Subject, 50)
		size := formatSize(msg.SizeEstimate)
		_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n", msg.ID, date, from, subject, size)
	}

	_ = w.Flush()
	fmt.Printf("\nShowing %d of %d results\n", len(results), total)
	return nil
}

func outputRemoteSearchResultsJSON(results []store.APIMessage, total int64) error {
	return printJSON(map[string]interface{}{
		"total":   total,
		"results": results,
	})
}

func outputSearchResultsJSON(results []query.MessageSummary) error {
	output := make([]map[string]interface{}, len(results))
	for i, msg := range results {
		output[i] = map[string]interface{}{
			"id":                     msg.ID,
			"source_message_id":      msg.SourceMessageID,
			"conversation_id":        msg.ConversationID,
			"source_conversation_id": msg.SourceConversationID,
			"subject":                msg.Subject,
			"snippet":                msg.Snippet,
			"from_email":             msg.FromEmail,
			"from_name":              msg.FromName,
			"sent_at":                msg.SentAt.Format(time.RFC3339),
			"size_estimate":          msg.SizeEstimate,
			"has_attachments":        msg.HasAttachments,
			"attachment_count":       msg.AttachmentCount,
			"labels":                 msg.Labels,
		}
	}

	return printJSON(output)
}

func init() {
	rootCmd.AddCommand(searchCmd)
	searchCmd.Flags().IntVarP(&searchLimit, "limit", "n", 50, "Maximum number of results")
	searchCmd.Flags().IntVar(&searchOffset, "offset", 0, "Skip first N results")
	searchCmd.Flags().BoolVar(&searchJSON, "json", false, "Output as JSON")
	searchCmd.Flags().StringVar(&searchAccount, "account", "", "Limit results to a specific account (email address)")
	searchCmd.Flags().StringVar(&searchCollection, "collection", "",
		"Limit results to all member accounts of one collection")
	searchCmd.MarkFlagsMutuallyExclusive("account", "collection")
	searchCmd.Flags().StringVar(&searchMode, "mode", "fts", "Search mode: fts|vector|hybrid")
	searchCmd.Flags().BoolVar(&searchExplain, "explain", false, "Include per-signal scores in output (hybrid/vector modes)")
}

// ensureFTSIndex checks if the FTS search index needs to be built and
// runs a one-time backfill if so. Shows a live progress bar since this
// can take a while on large archives. Blocks until complete.
func ensureFTSIndex(s *store.Store) error {
	if !s.NeedsFTSBackfill() {
		return nil
	}
	fmt.Fprintf(os.Stderr, "Building search index (one-time)...\n")
	n, err := s.BackfillFTS(func(done, total int64) {
		if total <= 0 {
			return
		}
		if done > total {
			done = total
		}
		pct := int(done * 100 / total)
		barWidth := 30
		filled := barWidth * pct / 100
		bar := strings.Repeat("=", filled) + strings.Repeat(" ", barWidth-filled)
		fmt.Fprintf(os.Stderr, "\r  [%s] %3d%%", bar, pct)
	})
	if err != nil {
		fmt.Fprintln(os.Stderr)
		return fmt.Errorf("build search index: %w", err)
	}
	fmt.Fprintf(os.Stderr, "\r  [%s] 100%%  %d messages indexed.\n", strings.Repeat("=", 30), n)
	return nil
}
