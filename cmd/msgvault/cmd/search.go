package cmd

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
)

var (
	searchLimit        int
	searchOffset       int
	searchJSON         bool
	searchAccount      string
	searchCollection   string
	searchMode         string
	searchExplain      bool
	searchMessageTypes []string
)

var searchCmd = &cobra.Command{
	Use:   "search <query>",
	Short: "Search messages using Gmail-like query syntax",
	Long: `Search your email archive using Gmail-like query syntax.

Uses the configured remote server when [remote].url is set; otherwise uses
the local daemon for FTS search. Use --local to use the local daemon even
when a remote is configured.

Supported operators:
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
  message_type: Message type filter (sms, mms, whatsapp, teams, email, meeting_transcript)

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

		if queryStr == "" && searchAccount == "" && searchCollection == "" && len(searchMessageTypes) == 0 {
			return usageErr(cmd, errors.New("provide a search query or --account/--collection flag"))
		}

		// Validate mode before any scope work so we fail fast on a typo.
		if searchMode != "fts" && searchMode != "vector" && searchMode != "hybrid" {
			return usageErr(cmd, fmt.Errorf("invalid --mode: %q (want fts|vector|hybrid)", searchMode))
		}

		// Validate --message-type against the known set, like --mode, so a
		// typo (e.g. carrier_pigeon) fails fast instead of silently
		// returning no results.
		for _, mt := range searchMessageTypes {
			if !query.IsKnownMessageType(mt) {
				return usageErr(cmd, fmt.Errorf(
					"invalid --message-type: %q (want one of: %s)",
					mt, strings.Join(query.KnownMessageTypes, ", "),
				))
			}
		}

		if searchLimit <= 0 {
			return usageErr(cmd, fmt.Errorf("--limit must be a positive integer, got %d", searchLimit))
		}
		if searchOffset < 0 {
			return usageErr(cmd, fmt.Errorf("--offset must be non-negative, got %d", searchOffset))
		}

		// Reject known operators with invalid values (e.g. before:2025-13-45)
		// rather than silently dropping the filter and running a wider query.
		// Checked before the empty-query test so the user sees the offending
		// value instead of a misleading "empty search query".
		if err := search.Parse(queryStr).Err(); err != nil {
			return usageErr(cmd, err)
		}
		if searchMode == "fts" {
			parsed := search.Parse(queryStr)
			parsed.MessageTypes = append(parsed.MessageTypes, searchMessageTypes...)
			if parsed.IsEmpty() && searchAccount == "" && searchCollection == "" {
				return errors.New("empty search query")
			}
		}
		if searchMode == "fts" {
			return runHTTPSearch(cmd, queryStr)
		}
		if searchMode != "fts" && searchOffset > 0 {
			return usageErr(cmd, fmt.Errorf("--offset is not supported with --mode=%s (pagination is single-page)", searchMode))
		}
		// Vector and hybrid modes need free-text terms to embed; both
		// an empty raw query and a filter-only query (e.g. `from:alice`)
		// would fail at the embed call. Check both up front and surface
		// a CLI error rather than a late engine-level one. FTS still
		// allows scoped queryless searches.
		if searchMode != "fts" {
			if queryStr == "" {
				return usageErr(cmd, fmt.Errorf("--mode=%s requires query text to embed; pass a query or use --mode=fts", searchMode))
			}
			if len(search.Parse(queryStr).TextTerms) == 0 {
				return usageErr(cmd, fmt.Errorf("--mode=%s requires free-text terms to embed; %q parsed to filters only — add a search phrase or use --mode=fts", searchMode, queryStr))
			}
		}

		return runHybridSearch(cmd, queryStr, searchMode, searchExplain)
	},
}

func runHTTPSearch(cmd *cobra.Command, queryStr string) error {
	s, info, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = s.Close() }()

	prefix := "Searching..."
	if info.Kind == HTTPStoreConfiguredRemote {
		prefix = fmt.Sprintf("Searching %s...", info.URL)
	}
	stopStatus := startSearchStatus(cmd.Context(), prefix, info)

	hasAccount := searchAccount != "" || searchCollection != ""
	logger.Info("search start",
		"query_len", len(queryStr),
		"has_account", hasAccount,
		"limit", searchLimit,
		"offset", searchOffset,
	)
	logger.Debug("search start detail",
		"query", queryStr,
		"account", searchAccount,
		"collection", searchCollection,
	)
	started := time.Now()

	resp, err := s.GetCLISearch(cmd.Context(), daemonclient.CLISearchRequest{
		Query:        queryStr,
		Account:      searchAccount,
		Collection:   searchCollection,
		MessageTypes: searchMessageTypes,
		Limit:        searchLimit,
		Offset:       searchOffset,
	})
	stopStatus()
	if err != nil {
		logger.Warn("search failed",
			"query_len", len(queryStr),
			"duration_ms", time.Since(started).Milliseconds(),
			"error", err.Error(),
		)
		return query.HintRepairEncoding(fmt.Errorf("search: %w", err))
	}
	if resp.IndexBuilt {
		// Pre-0.18 daemons built the index synchronously inside the request.
		fmt.Fprintf(os.Stderr, "Built search index (%d messages indexed).\n", resp.IndexedMessages)
	}
	switch resp.IndexState {
	case "building":
		fmt.Fprintln(os.Stderr,
			"Note: the search index is being rebuilt in the background; results may be incomplete until it finishes.")
	case "checking":
		fmt.Fprintln(os.Stderr,
			"Note: search index completeness is still being verified in the background; results may be incomplete until it finishes.")
	}
	if searchCollection != "" {
		label := resp.ScopeLabel
		if label == "" {
			label = searchCollection
		}
		n := resp.ScopeSourceCount
		suffix := "s"
		if n == 1 {
			suffix = ""
		}
		fmt.Fprintf(os.Stderr,
			"Searching collection %q (%d account%s)\n",
			label, n, suffix,
		)
	}
	logger.Info("search done",
		"query_len", len(queryStr),
		"has_account", hasAccount,
		"results", len(resp.Results),
		"duration_ms", time.Since(started).Milliseconds(),
	)

	// JSON mode must stay machine-parseable even with zero results:
	// emit an empty array, never prose.
	if searchJSON {
		return outputSearchResultsJSON(resp.Results)
	}
	if len(resp.Results) == 0 {
		fmt.Println("No messages found.")
		return nil
	}
	return outputSearchResultsTable(resp.Results)
}

// nil error return mirrors outputSearchResultsJSON so callers can return
// either uniformly; tabwriter output never fails.
//
//nolint:unparam // symmetry with error-returning outputSearchResultsJSON sibling
func outputSearchResultsTable(results []query.MessageSummary) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tDATE\tFROM\tSUBJECT\tSIZE")
	_, _ = fmt.Fprintln(w, "──\t────\t────\t───────\t────")

	for _, msg := range results {
		date := msg.SentAt.Format("2006-01-02")
		from := truncate(summaryFromDisplay(msg), 30)
		subject := truncate(msg.Subject, 50)
		size := formatSize(msg.SizeEstimate)
		_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n", msg.ID, date, from, subject, size)
	}

	_ = w.Flush()
	fmt.Printf("\n%s\n", formatShowingResults(len(results)))
	return nil
}

func summaryFromDisplay(msg query.MessageSummary) string {
	for _, value := range []string{msg.FromEmail, msg.FromName, msg.FromPhone} {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func outputSearchResultsJSON(results []query.MessageSummary) error {
	output := make([]map[string]any, len(results))
	for i, msg := range results {
		output[i] = map[string]any{
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
	searchCmd.Flags().BoolVar(&searchJSON, flagJSON, false, "Output as JSON")
	searchCmd.Flags().StringVar(&searchAccount, "account", "", "Limit results to a specific account (email address)")
	searchCmd.Flags().StringVar(&searchCollection, "collection", "",
		"Limit results to all member accounts of one collection")
	searchCmd.MarkFlagsMutuallyExclusive("account", "collection")
	searchCmd.Flags().StringVar(&searchMode, "mode", "fts", "Search mode: fts|vector|hybrid")
	searchCmd.Flags().BoolVar(&searchExplain, "explain", false, "Include per-signal scores in output (hybrid/vector modes)")
	searchCmd.Flags().StringSliceVar(&searchMessageTypes, "message-type", nil,
		"Limit results to message type(s), e.g. email, sms, calendar_event, meeting_transcript")
}
