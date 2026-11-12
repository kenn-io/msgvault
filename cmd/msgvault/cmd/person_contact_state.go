package cmd

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/textutil"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func newPersonContactStateCommand() *cobra.Command {
	var jsonOutput bool
	command := &cobra.Command{
		Use: "contact-state <person-id>", Short: "Show a person's archived contact history", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := positivePersonCLIArg(cmd, args[0], personValue)
			if err != nil {
				return err
			}
			client, _, err := OpenHTTPStore(cmd.Context())
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			response, err := daemonclient.APIResponse(client, func(api *apiclient.Client) (*generated.GetPersonContactStateResp, error) {
				return api.GetPersonContactStateWithResponse(cmd.Context(), &generated.GetPersonContactStateRequestOptions{PathParams: &generated.GetPersonContactStatePath{ID: id}})
			})
			if err != nil {
				return err
			}
			return writePersonContactState(cmd, response.JSON200, jsonOutput)
		},
	}
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output as JSON")
	return command
}

func writePersonContactState(cmd *cobra.Command, state *generated.ContactState, jsonOutput bool) error {
	if state == nil {
		return errors.New("person contact state response was empty")
	}
	if jsonOutput {
		return json.MarshalEncode(jsontext.NewEncoder(cmd.OutOrStdout()), state, json.Deterministic(true))
	}
	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Person: %d\n", state.PersonID); err != nil {
		return err
	}
	if state.FirstSeenAt != nil {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "First seen: %s\n", state.FirstSeenAt.UTC().Format("2006-01-02 15:04:05 UTC")); err != nil {
			return err
		}
	}
	if state.FirstContactKind != nil {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "First appearance: %s\n", textutil.SanitizeTerminal(*state.FirstContactKind)); err != nil {
			return err
		}
	}
	if state.Introducer != nil {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Introducer: %s\n", textutil.SanitizeTerminal(state.Introducer.DisplayName)); err != nil {
			return err
		}
	}
	if state.FirstContactAt != nil {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "First direct contact: %s\n", state.FirstContactAt.UTC().Format("2006-01-02 15:04:05 UTC")); err != nil {
			return err
		}
	}
	return nil
}
