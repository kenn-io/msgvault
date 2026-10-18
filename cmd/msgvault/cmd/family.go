package cmd

import (
	"crypto/rand"
	"encoding/hex"
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
	for _, kind := range []string{"link", "handled"} {
		name, short := "link", "Link explicitly selected archived copies"
		if kind == "handled" {
			name = "mark-handled"
			short = "Confirm handling of these exact archived copies"
		}
		var apply bool
		var key string
		var familyRevision string
		var selected []int64
		decision := &cobra.Command{Use: name + " <message-id>...", Short: short + " (preview unless --apply)", Args: cobra.MinimumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			var scope []int64
			if cmd.Flags().Changed("source-id") {
				scope = selected
			}
			return runFamilyDecisionWithRevision(cmd, args, kind, apply, key, scope, familyRevision)
		}}
		decision.Flags().BoolVar(&apply, "apply", false, "Save the previewed local decision")
		decision.Flags().StringVar(&key, "request-key", "", "Stable 32-character lowercase hex key for safe retries")
		decision.Flags().StringVar(&familyRevision, "family-revision", "", "Use the revision from a reviewed preview; stable retries may reuse their old revision")
		decision.Flags().Int64SliceVar(&selected, "source-id", nil, "Narrow to these source IDs")
		family.AddCommand(decision)
	}
	var undoPreview bool
	var expected int64
	var revision string
	undo := &cobra.Command{Use: "undo <decision-id>", Short: "Undo using current decision and family revisions", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil || id < 1 || (!undoPreview && (expected < 1 || revision == "")) {
			return errors.New("a positive decision ID and either --preview or both --revision and --family-revision are required")
		}
		client, _, err := OpenHTTPStore(cmd.Context(), "3.25.0")
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		if undoPreview {
			result, err := client.GetEmailFamilyDecisionContext(cmd.Context(), id, store.EmailFamilyOptions{})
			if err != nil {
				return err
			}
			return writeFamilyJSON(cmd, result)
		}
		result, err := client.UndoEmailFamilyDecisionContext(cmd.Context(), id, expected, revision, store.EmailFamilyOptions{})
		if err != nil {
			return err
		}
		return writeFamilyJSON(cmd, result)
	}}
	undo.Flags().BoolVar(&undoPreview, "preview", false, "Read current decision and surviving family revisions without undoing")
	undo.Flags().Int64Var(&expected, "revision", 0, "Current decision revision")
	undo.Flags().StringVar(&revision, "family-revision", "", "Current family revision from undo --preview")
	history := &cobra.Command{Use: "history <message-id>", Short: "List the latest 20 local decisions for a live copy", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil || id < 1 {
			return errors.New("message ID must be positive")
		}
		client, _, err := OpenHTTPStore(cmd.Context(), "3.25.0")
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		result, err := client.ListEmailFamilyDecisionsContext(cmd.Context(), id, store.EmailFamilyOptions{})
		if err != nil {
			return err
		}
		return writeFamilyJSON(cmd, result)
	}}
	family.AddCommand(undo, history)
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

func writeFamilyJSON(cmd *cobra.Command, value any) error {
	return json.MarshalEncode(jsontext.NewEncoder(cmd.OutOrStdout(), jsontext.WithIndent("  ")), value)
}

func runFamilyDecision(cmd *cobra.Command, args []string, kind string, apply bool, key string, sources []int64) error {
	return runFamilyDecisionWithRevision(cmd, args, kind, apply, key, sources, "")
}

func runFamilyDecisionWithRevision(cmd *cobra.Command, args []string, kind string, apply bool, key string, sources []int64, expectedFamilyRevision string) error {
	ids := make([]int64, 0, len(args))
	for _, arg := range args {
		id, err := strconv.ParseInt(arg, 10, 64)
		if err != nil || id < 1 {
			return errors.New("message IDs must be positive archive IDs")
		}
		ids = append(ids, id)
	}
	if key == "" {
		var token [16]byte
		if _, err := rand.Read(token[:]); err != nil {
			return fmt.Errorf("generate family request key: %w", err)
		}
		key = hex.EncodeToString(token[:])
	}
	client, _, err := OpenHTTPStore(cmd.Context(), "3.25.0")
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	input := store.EmailFamilyDecisionInput{RequestKey: key, Kind: kind, MessageIDs: ids, ActorKind: "cli", SourceIDs: sources}
	if apply {
		if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "Request key: %s\n", key); err != nil {
			return fmt.Errorf("write family request key: %w", err)
		}
		if expectedFamilyRevision != "" {
			input.ExpectedFamilyRevision = expectedFamilyRevision
			result, err := client.ApplyEmailFamilyDecisionContext(cmd.Context(), input)
			if err != nil {
				return err
			}
			return writeFamilyJSON(cmd, result)
		}
	}
	preview, err := client.PreviewEmailFamilyDecisionContext(cmd.Context(), input)
	if err != nil {
		return err
	}
	if !apply {
		return writeFamilyJSON(cmd, preview)
	}
	input.ExpectedFamilyRevision = preview.BeforeRevision
	result, err := client.ApplyEmailFamilyDecisionContext(cmd.Context(), input)
	if err != nil {
		return err
	}
	return writeFamilyJSON(cmd, result)
}
