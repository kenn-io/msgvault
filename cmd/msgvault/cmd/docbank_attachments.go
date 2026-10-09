package cmd

import (
	"encoding/json/v2"
	"fmt"

	"github.com/spf13/cobra"
)

func newDocbankCmd() *cobra.Command {
	parent := &cobra.Command{Use: "docbank", Short: "Manage the Docbank attachment mirror"}
	attachments := &cobra.Command{Use: "attachments", Short: "Mirror stored attachment bytes into Docbank"}
	status := &cobra.Command{Use: "status", Short: "Show consent, delivery counts and failure reasons", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		result, err := client.DocbankAttachmentStatus(cmd.Context())
		if err != nil {
			return err
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), string(encoded)); err != nil {
			return fmt.Errorf("write attachment mirror status: %w", err)
		}
		return nil
	}}
	backfill := &cobra.Command{Use: "backfill", Short: "Schedule discovery of all stored attachments and retry failed uploads", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		if err := client.BackfillDocbankAttachments(cmd.Context()); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), "Attachment mirror backfill scheduled. Use msgvault docbank attachments status to follow progress."); err != nil {
			return fmt.Errorf("write attachment mirror backfill status: %w", err)
		}
		return nil
	}}
	attachments.AddCommand(status, backfill)
	parent.AddCommand(attachments)
	return parent
}

func init() { rootCmd.AddCommand(newDocbankCmd()) }
