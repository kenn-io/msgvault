package cmd

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/store"
	"strconv"
)

func newFamilyCommand() *cobra.Command {
	family := &cobra.Command{Use: "family", Short: "Inspect related email copies"}
	var asJSON bool
	var sources []int64
	show := &cobra.Command{Use: "show <message-id>", Short: "Show related copies and explicit reply evidence", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		var selected []int64
		if cmd.Flags().Changed("source-id") {
			selected = sources
		}
		return runFamilyShow(cmd, args, asJSON, selected)
	}}
	show.Flags().BoolVar(&asJSON, "json", false, "Print the complete family response as JSON")
	show.Flags().Int64SliceVar(&sources, "source-id", nil, "Narrow to these source IDs (repeat or comma-separate)")
	family.AddCommand(show)
	return family
}

func runFamilyShow(cmd *cobra.Command, args []string, asJSON bool, sources []int64) error {
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil || id < 1 {
		return errors.New("message ID must be a positive archive ID")
	}
	client, _, err := OpenHTTPStore(cmd.Context(), "3.24.0")
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	family, err := client.EmailFamilyContext(cmd.Context(), id, store.EmailFamilyOptions{SourceIDs: sources})
	if err != nil {
		return err
	}
	if asJSON {
		return json.MarshalEncode(jsontext.NewEncoder(cmd.OutOrStdout(), jsontext.WithIndent("  ")), family)
	}
	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%d related copies · %s\n", len(family.Members), family.Handling.State); err != nil {
		return fmt.Errorf("write email family summary: %w", err)
	}
	for _, member := range family.Members {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%d\t%s\t%s\n", member.MessageID, member.SourceIdentifier, member.Subject); err != nil {
			return fmt.Errorf("write email family member: %w", err)
		}
	}
	if family.Truncated {
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), "Family limit reached; handling evidence is incomplete."); err != nil {
			return fmt.Errorf("write email family limit: %w", err)
		}
	}
	return nil
}

func init() { rootCmd.AddCommand(newFamilyCommand()) }
