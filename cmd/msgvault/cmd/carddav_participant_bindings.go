package cmd

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func cardDAVParticipantBindingConflictExplanation(reason string) string {
	switch reason {
	case "shared_address":
		return "address shared by multiple people"
	case "existing_person":
		return "participant already belongs to another person"
	default:
		return "conflicting participant ownership"
	}
}

func newCardDAVParticipantBindingsCmd() *cobra.Command {
	var options generated.ReconcileCardDAVParticipantBindingsBody
	var jsonOutput bool
	command := &cobra.Command{
		Use: "bind-participants", Short: "Bind exact CardDAV email and phone matches to archived identities",
		Long: `Reconcile archived identities against retained cards in subscribed CardDAV books.
Email matches ignore case; phone matches use archive normalization. Shared or
conflicting ownership is reported without merging people. No CardDAV server is
contacted. Sync also reconciles unchanged cards and newly arrived participants.

Card edits and deletions release only automatic memberships.
Promoting a person or explicitly linking identities prevents automatic removal.
Correct the source card to keep undone memberships from returning on sync.`,
		Example: "  msgvault carddav bind-participants --dry-run\n  msgvault carddav bind-participants --undo --dry-run",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, _, err := OpenHTTPStore(cmd.Context())
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			response, err := daemonclient.APIResponse(client, func(api *apiclient.Client) (*generated.ReconcileCardDAVParticipantBindingsResp, error) {
				return api.ReconcileCardDAVParticipantBindingsWithResponse(cmd.Context(), &generated.ReconcileCardDAVParticipantBindingsRequestOptions{Body: &options})
			})
			if err != nil {
				return err
			}
			result := response.JSON200
			if result == nil {
				return errors.New("CardDAV participant binding response was empty")
			}
			if jsonOutput {
				return json.MarshalEncode(jsontext.NewEncoder(cmd.OutOrStdout()), result, json.Deterministic(true))
			}
			prefix := "CardDAV bindings"
			if options.DryRun {
				prefix += " (dry run)"
			}
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s: %d cards scanned, %d participants bound, %d participants unbound, %d conflicts\n", prefix, result.CardsScanned, result.ParticipantsBound, result.ParticipantsUnbound, result.Conflicts); err != nil {
				return fmt.Errorf("write CardDAV binding summary: %w", err)
			}
			for _, conflict := range result.ConflictDetails {
				explanation := cardDAVParticipantBindingConflictExplanation(string(conflict.Reason))
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "  participant %d, people %v: %s\n", conflict.ParticipantID, conflict.PersonIds, explanation); err != nil {
					return fmt.Errorf("write CardDAV binding conflict: %w", err)
				}
			}
			return nil
		},
	}
	command.Flags().BoolVar(&options.DryRun, "dry-run", false, "Preview counts without changing the archive")
	command.Flags().BoolVar(&options.Undo, "undo", false, "Remove only automatic memberships")
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output as JSON")
	return command
}
