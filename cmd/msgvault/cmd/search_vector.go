package cmd

import (
	"fmt"
	"math"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
)

// runHybridSearch executes vector or hybrid search through the configured
// remote server or local daemon. It preserves the historical CLI renderer while
// keeping vector backend ownership inside the daemon.
func runHybridSearch(cmd *cobra.Command, queryStr, mode string, explain bool) error {
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
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	if explain {
		_, _ = fmt.Fprintln(w, "ID\tDATE\tFROM\tSUBJECT\tRRF\tBM25\tVEC")
		_, _ = fmt.Fprintln(w, "──\t────\t────\t───────\t───\t────\t───")
	} else {
		_, _ = fmt.Fprintln(w, "ID\tDATE\tFROM\tSUBJECT")
		_, _ = fmt.Fprintln(w, "──\t────\t────\t───────")
	}
	for _, r := range resp.Results {
		date := r.SentAt.Format("2006-01-02")
		from := truncate(r.FromEmail, 30)
		subject := truncate(r.Subject, 50)
		if r.SubjectBoosted {
			subject += " *"
		}
		if explain {
			_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
				r.ID, date, from, subject,
				formatOptionalScorePtr(r.RRFScore),
				formatOptionalScorePtr(r.BM25Score),
				formatOptionalScorePtr(r.VectorScore))
		} else {
			_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\n",
				r.ID, date, from, subject)
		}
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("flush table output: %w", err)
	}
	fmt.Printf("\n%s (generation #%d %s, fingerprint=%q)\n",
		formatShowingResults(len(resp.Results)), resp.Generation.ID, resp.Generation.State, resp.Generation.Fingerprint)
	return nil
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
	return printJSON(map[string]any{
		"generation": map[string]any{
			"id":          resp.Generation.ID,
			"model":       resp.Generation.Model,
			"dimension":   resp.Generation.Dimension,
			"fingerprint": resp.Generation.Fingerprint,
			"state":       resp.Generation.State,
		},
		"pool_saturated": resp.PoolSaturated,
		"returned_count": resp.ReturnedCount,
		"results":        rows,
	})
}

func formatOptionalScorePtr(v *float64) string {
	if v == nil || math.IsNaN(*v) {
		return "-"
	}
	return fmt.Sprintf("%.4f", *v)
}
