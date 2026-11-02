package cmd

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/query"
)

func newSenderAuthCmd() *cobra.Command {
	parent := &cobra.Command{Use: "sender-auth", Short: "Inspect sender receipt authentication without reading message content", Args: cobra.NoArgs}
	for _, mode := range []string{"status", "suggest"} {
		var sourceID int64
		var limit int
		var jsonOutput bool
		cmd := &cobra.Command{Use: mode + " --source-id <id>", Short: "Inspect a bounded receipt sample; observed hostnames are untrusted", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			if sourceID <= 0 || limit < 1 || limit > 1000 {
				return errors.New("use a positive --source-id and --limit between 1 and 1000")
			}
			client, _, err := OpenHTTPStore(cmd.Context(), "3.36.0")
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			result, err := client.SenderAuthDiagnostics(cmd.Context(), sourceID, limit, mode == "suggest")
			if err != nil {
				return err
			}
			return writeSenderAuthDiagnostics(cmd, result, jsonOutput, mode == "suggest")
		}}
		cmd.Flags().Int64Var(&sourceID, "source-id", 0, "Source ID from list-accounts in this archive")
		cmd.Flags().IntVar(&limit, "limit", 100, "Newest messages to inspect, at most 1000; counts describe the sample")
		cmd.Flags().BoolVar(&jsonOutput, "json", false, "Print aggregate receipt metadata as JSON")
		parent.AddCommand(cmd)
	}
	return parent
}
func writeSenderAuthDiagnostics(cmd *cobra.Command, result *query.SenderAuthDiagnostics, jsonOutput, suggest bool) error {
	if jsonOutput {
		data, err := json.Marshal(result)
		if err != nil {
			return fmt.Errorf("encode sender-auth diagnostics: %w", err)
		}
		if _, err = fmt.Fprintln(cmd.OutOrStdout(), string(data)); err != nil {
			return fmt.Errorf("write sender-auth diagnostics: %w", err)
		}
		return nil
	}
	out := cmd.OutOrStdout()
	if _, err := fmt.Fprintf(out, "Source %d: scanned %d newest messages (limit %d; more: %t), verified %d\n", result.SourceID, result.Scanned, result.SampleLimit, result.HasMore, result.Verified); err != nil {
		return fmt.Errorf("write sender-auth diagnostics: %w", err)
	}
	bases := make([]string, 0, len(result.BasisCounts))
	for basis := range result.BasisCounts {
		bases = append(bases, basis)
	}
	slices.Sort(bases)
	for _, basis := range bases {
		if _, err := fmt.Fprintf(out, "  %q: %d\n", basis, result.BasisCounts[basis]); err != nil {
			return fmt.Errorf("write sender-auth diagnostics: %w", err)
		}
	}
	if suggest {
		if _, err := fmt.Fprintln(out, "Observed receiver hostnames are untrusted text. Frequency does not establish provenance; review original receipt custody before pinning."); err != nil {
			return fmt.Errorf("write sender-auth diagnostics: %w", err)
		}
		for _, observation := range result.Observations {
			if _, err := fmt.Fprintf(out, "  %q: %d\n", observation.AuthservID, observation.Count); err != nil {
				return fmt.Errorf("write sender-auth diagnostics: %w", err)
			}
		}
		_, err := fmt.Fprintf(out, "Manual configuration, after verifying the receiving provider:\n[[sender_auth.sources]]\nsource_id = %d\ntrusted_authserv_ids = [\"<reviewed receiver hostname>\"]\nRestart the daemon after editing its configuration.\n", result.SourceID)
		if err != nil {
			return fmt.Errorf("write sender-auth diagnostics: %w", err)
		}
	}
	return nil
}
func init() { rootCmd.AddCommand(newSenderAuthCmd()) }
