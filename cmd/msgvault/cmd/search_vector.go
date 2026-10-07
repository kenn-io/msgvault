package cmd

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
)

// runHybridSearch executes vector or hybrid search through the configured
// remote server or local daemon. It preserves the historical CLI renderer while
// keeping vector backend ownership inside the daemon.
func runHybridSearch(cmd *cobra.Command, queryStr, mode string, explain bool) error {
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

	logger.Info("vector search start",
		"mode", mode,
		"query_len", len(queryStr),
		"limit", searchLimit,
		"explain", explain,
	)
	started := time.Now()

	// An omitted --rerank leaves the choice to the daemon's
	// [vector.rerank].default; an explicit value overrides it either way.
	var rerank *bool
	if cmd.Flags().Changed("rerank") {
		rerank = &searchRerank
	}
	resp, err := s.GetCLIHybridSearch(cmd.Context(), daemonclient.CLIHybridSearchRequest{
		Query:        queryStr,
		Account:      searchAccount,
		Collection:   searchCollection,
		MessageTypes: searchMessageTypes,
		Mode:         mode,
		Limit:        searchLimit,
		Rerank:       rerank,
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
	if resp.Rerank != nil && resp.Rerank.Fallback != "" {
		// Stderr keeps --json output parseable while still telling the
		// user the order is not the reranked one they asked for.
		fmt.Fprintf(os.Stderr, "Reranking failed (%s); showing %s order\n", resp.Rerank.Fallback, mode)
	}

	if searchJSON {
		return outputHybridResultsJSON(resp, explain)
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

	reranked := resp.Rerank != nil && resp.Rerank.Applied
	if err := writeHybridResultsTable(os.Stdout, resp.Results, explain, reranked); err != nil {
		return err
	}
	fmt.Printf("\n%s (generation #%d %s, fingerprint=%q)\n",
		formatShowingResults(len(resp.Results)), resp.Generation.ID, resp.Generation.State, resp.Generation.Fingerprint)
	if reranked {
		fmt.Printf("Reranked the top %d with %s\n", resp.Rerank.Candidates, resp.Rerank.Model)
	}
	outputHybridTimings(resp, explain)
	return nil
}

func writeHybridResultsTable(out io.Writer, results []daemonclient.CLIHybridSearchResult, explain, reranked bool) error {
	return writeHybridResultsTableWidth(out, results, explain, reranked, searchTableTerminalWidth(out))
}

func writeHybridResultsTableWidth(out io.Writer, results []daemonclient.CLIHybridSearchResult, explain, reranked bool, width int) error {
	headers := []string{"ID", "DATE", "FROM", "SUBJECT"}
	if explain {
		if reranked {
			headers = append(headers, "RERANK")
		}
		headers = append(headers, "RRF", "BM25", "VEC")
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
			if reranked {
				row = append(row, searchTableCell{text: formatOptionalScorePtr(r.RerankScore)})
			}
			row = append(row,
				searchTableCell{text: formatOptionalScorePtr(r.RRFScore)},
				searchTableCell{text: formatOptionalScorePtr(r.BM25Score)},
				searchTableCell{text: formatOptionalScorePtr(r.VectorScore)},
			)
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
	fmt.Printf("Timings: total=%dms query_embedding=%dms retrieval=%dms hydration=%dms",
		resp.TookMS, resp.Timings.QueryEmbeddingMS, resp.Timings.RetrievalMS, resp.Timings.HydrationMS)
	if resp.Rerank != nil {
		fmt.Printf(" rerank=%dms", resp.Timings.RerankMS)
	}
	fmt.Println()
}

func outputHybridResultsJSON(resp *daemonclient.CLIHybridSearch, explain bool) error {
	rows := make([]map[string]any, len(resp.Results))
	for i, r := range resp.Results {
		row := map[string]any{
			"id":         r.ID,
			"subject":    r.Subject,
			"from_email": r.FromEmail,
			"sent_at":    r.SentAt.Format(time.RFC3339),
			"boosted":    r.SubjectBoosted,
		}
		if r.Message.WebURL != "" {
			row["web_url"] = r.Message.WebURL
		}
		if r.RRFScore != nil && !math.IsNaN(*r.RRFScore) {
			row["rrf_score"] = *r.RRFScore
		}
		if r.RerankScore != nil {
			row["rerank_score"] = *r.RerankScore
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
	if resp.Rerank != nil {
		output["rerank"] = resp.Rerank
	}
	return printJSON(output)
}

func formatOptionalScorePtr(v *float64) string {
	if v == nil || math.IsNaN(*v) {
		return "-"
	}
	return fmt.Sprintf("%.4f", *v)
}
