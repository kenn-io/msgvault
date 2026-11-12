package cmd

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/textutil"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func newPersonAppreciationsCommand() *cobra.Command {
	command := &cobra.Command{Use: "appreciations", Short: "Review and dismiss appreciation suggestions"}
	command.AddCommand(newPersonAppreciationsListCommand(), newPersonAppreciationsDismissCommand())
	return command
}

func newPersonAppreciationsListCommand() *cobra.Command {
	var jsonOutput bool
	command := &cobra.Command{Use: "list <person-id>", Short: "List supported appreciation suggestions", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		id, client, cleanup, err := openPersonBriefClient(cmd, args[0])
		if err != nil {
			return err
		}
		defer cleanup()
		response, err := daemonclient.APIResponse(client, func(api *apiclient.Client) (*generated.ListPersonAppreciationsResp, error) {
			return api.ListPersonAppreciationsWithResponse(cmd.Context(), &generated.ListPersonAppreciationsRequestOptions{PathParams: &generated.ListPersonAppreciationsPath{ID: id}})
		})
		if err != nil {
			return err
		}
		if response.JSON200 == nil || response.JSON200.Appreciations == nil {
			return errors.New("person appreciations response was empty")
		}
		if jsonOutput {
			return writePersonBriefRawJSON(cmd, response.Body)
		}
		return writePersonAppreciations(cmd.OutOrStdout(), response.JSON200.Appreciations)
	}}
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output as JSON")
	return command
}

func appreciationString(s *string) string {
	if s == nil {
		return ""
	}
	return textutil.SanitizeTerminal(*s)
}

func writeAppreciationRef(w io.Writer, label, quote string, ref generated.EvidenceRef) error {
	_, err := fmt.Fprintf(w, "  %s quote: %s\n  %s source: lane=%s source_id=%d message_id=%d attachment_id=%d source_message_id=%s occurrence_key=%s chunk_key=%s span=%d:%d\n", label, textutil.SanitizeTerminal(quote), label, textutil.SanitizeTerminal(ref.SourceLane), ref.SourceID, ref.MessageID, ref.AttachmentID, textutil.SanitizeTerminal(ref.SourceMessageID), textutil.SanitizeTerminal(ref.OccurrenceKey), textutil.SanitizeTerminal(ref.ChunkKey), ref.SpanStart, ref.SpanEnd)
	return err
}

func writePersonAppreciations(w io.Writer, items []generated.PersonAppreciation) error {
	if len(items) == 0 {
		_, err := fmt.Fprintln(w, "No supported, undismissed appreciation suggestions.")
		return err
	}
	for _, item := range items {
		s := item.Suggestion
		if _, err := fmt.Fprintf(w, "Key: %s\nReason: %s\nState: %s; prior thanks %s\n", appreciationString(s.Key), textutil.SanitizeTerminal(s.Text), appreciationString(s.State), appreciationString(s.PriorThanks)); err != nil {
			return fmt.Errorf("write appreciation: %w", err)
		}
		if err := writeAppreciationRef(w, "Peer", s.Quote, item.Peer); err != nil {
			return fmt.Errorf("write appreciation: %w", err)
		}
		if item.Owner != nil {
			if err := writeAppreciationRef(w, "Owner", s.Thanked.Quote, *item.Owner); err != nil {
				return fmt.Errorf("write appreciation: %w", err)
			}
		}
	}
	return nil
}

func newPersonAppreciationsDismissCommand() *cobra.Command {
	var jsonOutput bool
	command := &cobra.Command{Use: "dismiss <person-id> <key>", Short: "Durably dismiss a suggestion", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		if args[1] == "" {
			return usageErr(cmd, errors.New("key must not be empty"))
		}
		id, client, cleanup, err := openPersonBriefClient(cmd, args[0])
		if err != nil {
			return err
		}
		defer cleanup()
		// The generated runtime substitutes path parameters without escaping.
		// Escape the opaque key so delimiters cannot change the write target.
		response, err := daemonclient.APIResponse(client, func(api *apiclient.Client) (*generated.DismissPersonAppreciationResp, error) {
			return api.DismissPersonAppreciationWithResponse(cmd.Context(), &generated.DismissPersonAppreciationRequestOptions{PathParams: &generated.DismissPersonAppreciationPath{ID: id, Key: url.PathEscape(args[1])}})
		})
		if err != nil {
			return err
		}
		if response.JSON200 == nil || response.JSON200.ContributionKey == "" {
			return errors.New("person appreciation dismissal response was empty")
		}
		if jsonOutput {
			return writePersonBriefRawJSON(cmd, response.Body)
		}
		state := response.JSON200
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Dismissed: %s\n", textutil.SanitizeTerminal(state.ContributionKey)); err != nil {
			return fmt.Errorf("write appreciation dismissal: %w", err)
		}
		if state.DismissedPersonUID != nil {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Person UID: %s\n", appreciationString(state.DismissedPersonUID)); err != nil {
				return err
			}
		}
		if state.DismissedBy != nil {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Dismissed by: %s\n", appreciationString(state.DismissedBy)); err != nil {
				return err
			}
		}
		if state.DismissedAt != nil {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Dismissed at: %s\n", state.DismissedAt.Format(time.RFC3339)); err != nil {
				return err
			}
		}
		if state.ThanksBriefID != nil {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Thanks brief ID: %d\n", *state.ThanksBriefID); err != nil {
				return err
			}
		}
		return nil
	}}
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output as JSON")
	return command
}
