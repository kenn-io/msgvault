package cmd

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"go.kenn.io/kit/agenthook"
	"go.kenn.io/msgvault/internal/daemonclient"
)

// runHybridSearch executes vector or hybrid search through the configured
// remote server or local daemon. It preserves the historical CLI renderer while
// keeping vector backend ownership inside the daemon.
func runHybridSearch(cmd *cobra.Command, queryStr, mode string, explain bool) (result error) {
	state := invocationFromCommand(cmd)
	if state == nil || state.logger == nil {
		return errors.New("invocation state is unavailable")
	}
	logger := state.logger
	s, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = s.Close() }()
	kind := "hybrid"
	if mode == "vector" {
		kind = "semantic"
	}
	if strings.TrimSpace(queryStr) != "" {
		defer reportSuccessfulCLIAction(cmd, s, "search_run", "search_type", kind, &result)
	}

	logger.Info("vector search start",
		"mode", mode,
		"query_len", len(queryStr),
		"limit", searchLimit,
		"explain", explain,
	)
	started := time.Now()

	resp, err := s.GetCLIHybridSearch(cmd.Context(), daemonclient.CLIHybridSearchRequest{
		Query:        queryStr,
		Account:      searchAccount,
		Collection:   searchCollection,
		MessageTypes: searchMessageTypes,
		Mode:         mode,
		Limit:        searchLimit,
	})
	if err != nil {
		logger.Warn("vector search failed",
			"mode", mode,
			"duration_ms", time.Since(started).Milliseconds(),
			"error", err.Error(),
		)
		return err
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

	logger.Info("vector search done",
		"mode", mode,
		"results", len(resp.Results),
		"duration_ms", time.Since(started).Milliseconds(),
	)

	if searchSort == "date" {
		sortSemanticResultsByDate(resp.Results)
	}

	if strings.TrimSpace(queryStr) != "" && len(resp.Results) == 0 {
		defer reportSuccessfulCLIAction(cmd, s, "search_empty", "search_type", kind, &result)
	}
	if searchJSON {
		next := ""
		if resp.HasMore {
			next, err = semanticMoreResultsCommand(cmd, queryStr)
			if err != nil {
				return err
			}
		}
		return outputHybridResultsJSON(resp, explain, next)
	}
	if message := semanticMoreResultsMessage(resp); message != "" {
		defer func() { _, _ = fmt.Fprintln(cmd.ErrOrStderr(), "Hint: "+message) }()
	}
	return outputHybridResultsTable(resp, explain)
}

func outputHybridResultsTable(resp *daemonclient.CLIHybridSearch, explain bool) error {
	if len(resp.Results) == 0 {
		fmt.Println("No messages found.")
		fmt.Printf("\nGeneration #%d (%s, fingerprint=%q)\n",
			resp.Generation.ID, resp.Generation.State, resp.Generation.Fingerprint)
		outputHybridTimings(resp, explain)
		return nil
	}

	if err := writeHybridResultsTable(os.Stdout, resp.Results, explain); err != nil {
		return err
	}
	fmt.Printf("\n%s (generation #%d %s, fingerprint=%q)\n",
		formatShowingResults(len(resp.Results)), resp.Generation.ID, resp.Generation.State, resp.Generation.Fingerprint)
	outputHybridTimings(resp, explain)
	return nil
}

func writeHybridResultsTable(out io.Writer, results []daemonclient.CLIHybridSearchResult, explain bool) error {
	return writeHybridResultsTableWidth(out, results, explain, searchTableTerminalWidth(out))
}

func writeHybridResultsTableWidth(out io.Writer, results []daemonclient.CLIHybridSearchResult, explain bool, width int) error {
	headers := []string{"ID", "DATE", "FROM", "SUBJECT"}
	if explain {
		headers = append(headers, "RRF", "BM25", "VEC")
	}
	if searchShowSnippet {
		headers = append(headers, "SNIPPET")
	}
	rows := make([][]searchTableCell, 0, len(results))
	for _, r := range results {
		from := r.FromEmail
		if strings.TrimSpace(from) == "" {
			from = summaryFromDisplay(r.Message)
		}
		subject := searchTableCell{text: summaryTableText(r.Subject, r.Message.Snippet)}
		if r.SubjectBoosted {
			subject.marker = " *"
		}
		row := []searchTableCell{
			{text: strconv.FormatInt(r.ID, 10)},
			{text: r.SentAt.Format("2006-01-02")},
			{text: normalizeSearchTableText(from)},
			subject,
		}
		if explain {
			row = append(row,
				searchTableCell{text: formatOptionalScorePtr(r.RRFScore)},
				searchTableCell{text: formatOptionalScorePtr(r.BM25Score)},
				searchTableCell{text: formatOptionalScorePtr(r.VectorScore)},
			)
		}
		if searchShowSnippet {
			row = append(row, searchTableCell{text: searchSnippetText(r.Message.Snippet)})
		}
		rows = append(rows, row)
	}
	return writeSearchTable(out, headers, rows, width)
}

func outputHybridTimings(resp *daemonclient.CLIHybridSearch, explain bool) {
	if !explain {
		return
	}
	if resp.Accelerator != "" {
		fmt.Printf("Accelerator: %s\n", resp.Accelerator)
	}
	fmt.Printf("Timings: total=%dms query_embedding=%dms retrieval=%dms hydration=%dms\n",
		resp.TookMS, resp.Timings.QueryEmbeddingMS, resp.Timings.RetrievalMS, resp.Timings.HydrationMS)
}

func outputHybridResultsJSON(resp *daemonclient.CLIHybridSearch, explain bool, next string) error {
	rows := make([]map[string]any, len(resp.Results))
	for i, r := range resp.Results {
		row := map[string]any{
			"id":                  r.ID,
			"subject":             r.Subject,
			"from_email":          r.FromEmail,
			"sent_at":             r.SentAt.Format(time.RFC3339),
			"boosted":             r.SubjectBoosted,
			"conversation_id":     r.Message.ConversationID,
			"snippet":             r.Message.Snippet,
			"hidden_text_present": r.Message.HiddenTextPresent,
		}
		if r.AttachmentCount != nil {
			row["attachment_count"] = *r.AttachmentCount
		}
		if r.Message.AttachmentNames != nil {
			row["attachment_names"] = r.Message.AttachmentNames
		}
		if r.Message.WebURL != "" {
			row["web_url"] = r.Message.WebURL
		}
		if r.RRFScore != nil && !math.IsNaN(*r.RRFScore) {
			row["rrf_score"] = *r.RRFScore
		}
		if explain && r.BM25Score != nil && !math.IsNaN(*r.BM25Score) {
			row["bm25_score"] = *r.BM25Score
		}
		if explain && r.VectorScore != nil && !math.IsNaN(*r.VectorScore) {
			row["vector_score"] = *r.VectorScore
		}
		rows[i] = row
	}
	output := map[string]any{
		"generation": map[string]any{
			"id":          resp.Generation.ID,
			"model":       resp.Generation.Model,
			"dimension":   resp.Generation.Dimension,
			"fingerprint": resp.Generation.Fingerprint,
			"state":       resp.Generation.State,
		},
		"pool_saturated": resp.PoolSaturated,
		"returned_count": resp.ReturnedCount,
		"took_ms":        resp.TookMS,
		"timings":        resp.Timings,
		"results":        rows,
	}
	if resp.Accelerator != "" {
		output["accelerator"] = resp.Accelerator
	}
	if message := semanticMoreResultsMessage(resp); message != "" {
		hint := map[string]string{"code": "more_available", "message": message}
		if next != "" {
			hint["next"] = next
		}
		output["hints"] = []map[string]string{hint}
	}
	return printJSON(output)
}

func formatOptionalScorePtr(v *float64) string {
	if v == nil || math.IsNaN(*v) {
		return "-"
	}
	return fmt.Sprintf("%.4f", *v)
}

func sortSemanticResultsByDate(results []daemonclient.CLIHybridSearchResult) {
	slices.SortStableFunc(results, func(a, b daemonclient.CLIHybridSearchResult) int {
		if order := b.SentAt.Compare(a.SentAt); order != 0 {
			return order
		}
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
}

func semanticMoreResultsCommand(cmd *cobra.Command, queryStr string) (string, error) {
	args := []string{"msgvault", "search"}
	cmd.Flags().Visit(func(flag *pflag.Flag) {
		if flag.Name != "limit" {
			args = appendDaemonCLIFlag(args, flag)
		}
	})
	args = append(args, "--limit", strconv.Itoa(searchLimit*2), "--", queryStr)
	commands, err := agenthook.BuildCommand(args[0], args[1:]...)
	if err != nil {
		return "", fmt.Errorf("build search retry: %w", err)
	}
	if runtime.GOOS == "windows" {
		return commands.PowerShell, nil
	}
	return commands.POSIX, nil
}

func semanticMoreResultsMessage(resp *daemonclient.CLIHybridSearch) string {
	switch {
	case resp.Mode == "hybrid" && resp.PoolSaturated:
		capSetting := ""
		if resp.RankingLimitReached > 0 {
			capSetting = fmt.Sprintf("vector.search.max_page_size_hybrid above %d and ", resp.RankingLimitReached)
		}
		return "hybrid candidate pool may omit matches; raise " + capSetting + "vector.search.k_per_signal on the server and restart it, then raise --limit, or narrow the query"
	case resp.RankingLimitReached > 0:
		return fmt.Sprintf("more messages match; raise vector.search.max_page_size_hybrid above %d on the server and restart it, then raise --limit", resp.RankingLimitReached)
	case resp.HasMore:
		return "more messages match; raise --limit"
	}
	return ""
}
