package cmd

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

func newInboxTriageCmd() *cobra.Command {
	parent := &cobra.Command{Use: "triage", Short: "Preview and apply bounded tag-only inbox proposals through the daemon"}
	var previewPath string
	preview := &cobra.Command{Use: "preview", Short: "Read a proposal for 1–100 explicit targets and configured categories", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		var input inboxcontrol.TriageInput
		if err := readInboxCLIJSON(cmd, previewPath, &input); err != nil {
			return usageErr(cmd, err)
		}
		if err := input.Validate(); err != nil {
			return usageErr(cmd, err)
		}
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		proposal, err := client.PreviewInboxTriage(cmd.Context(), input)
		if err != nil {
			return err
		}
		return writeInboxTriageCLIJSON(cmd, proposal, false)
	}}
	preview.Flags().StringVar(&previewPath, "request", "", "JSON request file, or - for standard input (maximum 1 MiB)")
	_ = preview.MarkFlagRequired("request")
	var applyPath string
	var apply bool
	applyCmd := &cobra.Command{Use: "apply", Short: "Apply a complete reviewed proposal once and output ordered receipts", Long: "Apply the complete caller-bound proposal from triage preview. Requires --apply. The daemon checks current grants, incoming messages, mapping and archive revisions, native state, and expiry. Preserve item keys and output receipts, including partial or unknown outcomes. This command adds mapped tags and never archives, marks read, or sends.", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if !apply {
			return usageErr(cmd, fmt.Errorf("%w: triage apply requires --apply", inboxcontrol.ErrInvalid))
		}
		var proposal inboxcontrol.TriageProposal
		if err := readInboxCLIJSON(cmd, applyPath, &proposal); err != nil {
			return usageErr(cmd, err)
		}
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		results, callErr := client.ApplyInboxTriage(cmd.Context(), proposal)
		if results != nil {
			if err := writeInboxTriageCLIJSON(cmd, results, true); err != nil {
				return err
			}
		}
		return callErr
	}}
	applyCmd.Flags().StringVar(&applyPath, "request", "", "Complete proposal JSON file, or - for standard input (maximum 1 MiB)")
	applyCmd.Flags().BoolVar(&apply, "apply", false, "Execute the reviewed proposal or recover its existing receipts")
	_ = applyCmd.MarkFlagRequired("request")
	parent.AddCommand(preview, applyCmd)
	return parent
}

func writeInboxTriageCLIJSON(cmd *cobra.Command, value any, applying bool) error {
	data, err := json.Marshal(value, jsontext.WithIndent("  "))
	if err == nil {
		err = writeInboxCLIOutputLine(cmd, data)
	}
	if err != nil {
		if applying {
			return fmt.Errorf("%w: could not output triage receipts", inboxcontrol.ErrOutcomeUnknown)
		}
		return fmt.Errorf("write triage proposal: %w", err)
	}
	return nil
}
