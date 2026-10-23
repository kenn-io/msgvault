package cmd

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"maps"
	"slices"
	"sort"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/textutil"
)

func newDocbankCmd(open func(context.Context) (*daemonclient.Client, func(), error)) *cobra.Command {
	parent := &cobra.Command{Use: "docbank", Short: "Manage the Docbank attachment mirror"}
	attachments := &cobra.Command{Use: "attachments", Short: "Mirror stored attachment bytes into Docbank"}
	var jsonOutput bool
	status := &cobra.Command{Use: "status", Short: "Show consent, delivery counts and failure reasons", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		client, cleanup, err := open(cmd.Context())
		if err != nil {
			return err
		}
		defer cleanup()
		result, err := client.DocbankAttachmentStatus(cmd.Context())
		if err != nil {
			return err
		}
		if jsonOutput {
			return json.MarshalEncode(jsontext.NewEncoder(cmd.OutOrStdout()), result, json.Deterministic(true))
		}
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "State: %s\nUpload consent: %t\nFolder: %s\n", textutil.SanitizeTerminal(string(result.State)), result.UploadConsent, textutil.SanitizeTerminal(result.Folder)); err != nil {
			return fmt.Errorf("write status: %w", err)
		}
		if result.Counts == nil {
			return nil
		}
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Delivered: %d\nSending: %d\nFiles left to examine: %d\nFailed: %d\n", result.Counts.Delivered, result.Counts.Sending, result.Counts.Remaining, result.Counts.Failed); err != nil {
			return fmt.Errorf("write status: %w", err)
		}
		reasons := make([]string, 0, len(result.Counts.Reasons))
		for reason := range result.Counts.Reasons {
			reasons = append(reasons, reason)
		}
		sort.Strings(reasons)
		for _, reason := range reasons {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s: %d\n", textutil.SanitizeTerminal(reason), result.Counts.Reasons[reason]); err != nil {
				return fmt.Errorf("write status: %w", err)
			}
		}
		if result.LastError != nil {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Last error: %s\n", textutil.SanitizeTerminal(*result.LastError)); err != nil {
				return fmt.Errorf("write status: %w", err)
			}
		}
		if p := result.Processing; p != nil {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Document profile: %s\nText extracted: %d\nProcessing: %d\nNot yet requested: %d\n", textutil.SanitizeTerminal(p.Profile), p.Extracted, p.Requested, p.Waiting); err != nil {
				return fmt.Errorf("write status: %w", err)
			}
			for _, reason := range slices.Sorted(maps.Keys(p.Outcomes)) {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Without text (%s): %d\n", textutil.SanitizeTerminal(reason), p.Outcomes[reason]); err != nil {
					return fmt.Errorf("write status: %w", err)
				}
			}
			if p.Outcomes["consent"] > 0 || p.Outcomes["stale_authority"] > 0 {
				if _, err := fmt.Fprintln(cmd.OutOrStdout(), "After restoring consent or authority in Docbank, run msgvault docbank attachments backfill to retry processing."); err != nil {
					return fmt.Errorf("write status: %w", err)
				}
			}
		}
		return nil
	}}
	backfill := &cobra.Command{Use: "backfill", Short: "Discover stored attachments and retry failed uploads and processing", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		client, cleanup, err := open(cmd.Context())
		if err != nil {
			return err
		}
		defer cleanup()
		if err := client.BackfillDocbankAttachments(cmd.Context()); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), "Attachment mirror backfill scheduled. Use msgvault docbank attachments status to follow progress."); err != nil {
			return fmt.Errorf("write attachment mirror backfill status: %w", err)
		}
		return nil
	}}
	status.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output structured JSON")
	attachments.AddCommand(status, backfill)
	parent.AddCommand(attachments)
	return parent
}

func init() {
	rootCmd.AddCommand(newDocbankCmd(func(ctx context.Context) (*daemonclient.Client, func(), error) {
		client, _, err := OpenHTTPStore(ctx)
		return client, func() {
			if client != nil {
				_ = client.Close()
			}
		}, err
	}))
}
