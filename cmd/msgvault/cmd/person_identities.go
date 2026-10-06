package cmd

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/textutil"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func newPersonIdentitiesCommand() *cobra.Command {
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "identities <person-id>",
		Short: "List a person's archived email addresses, phone numbers, and chat IDs",
		Args:  cobra.ExactArgs(1),
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
			response, err := daemonclient.APIResponse(client,
				func(api *apiclient.Client) (*generated.ListPersonIdentitiesResp, error) {
					return api.ListPersonIdentitiesWithResponse(cmd.Context(),
						&generated.ListPersonIdentitiesRequestOptions{
							PathParams: &generated.ListPersonIdentitiesPath{ID: id},
						})
				})
			if err != nil {
				return err
			}
			return writePersonIdentities(cmd, response.JSON200, jsonOutput)
		},
	}
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output as JSON")
	return command
}

func writePersonIdentities(cmd *cobra.Command, page *generated.PersonIdentitiesResponse, jsonOutput bool) error {
	if page == nil {
		return errors.New("person identities response was empty")
	}
	if jsonOutput {
		return json.MarshalEncode(jsontext.NewEncoder(cmd.OutOrStdout()), page, json.Deterministic(true))
	}
	if len(page.Identities) == 0 {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Person %d has no archived identities\n", page.PersonID); err != nil {
			return fmt.Errorf("write person identities: %w", err)
		}
		return nil
	}
	writer := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "KIND\tVALUE\tDRAFTS")
	for _, identity := range page.Identities {
		drafts := "unsupported"
		if identity.Supported {
			drafts = "supported"
		}
		_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\n",
			textutil.SanitizeTerminal(identity.Kind), textutil.SanitizeTerminal(identity.Value), drafts)
	}
	if err := writer.Flush(); err != nil {
		return fmt.Errorf("write person identities: %w", err)
	}
	return nil
}
