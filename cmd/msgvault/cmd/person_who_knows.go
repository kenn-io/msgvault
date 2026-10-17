package cmd

import (
	"encoding/json/v2"
	"errors"
	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/whoknows"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
	"strings"
)

func newPersonWhoKnowsCommand() *cobra.Command {
	command := &cobra.Command{Use: "who-knows <topic words...>", Short: "Find people with cited knowledge about a topic", Args: cobra.MinimumNArgs(1)}
	options := newWhoKnowsOptions(command)
	command.RunE = func(cmd *cobra.Command, args []string) error {
		p, err := options.params()
		if err != nil {
			return err
		}
		topic := strings.TrimSpace(strings.Join(args, " "))
		if topic == "" {
			return whoknows.ErrMissingFreeText
		}
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		response, err := daemonclient.APIResponse(client, func(api *apiclient.Client) (*generated.FindPeopleWhoKnowResp, error) {
			return api.FindPeopleWhoKnowWithResponse(cmd.Context(), daemonclient.WhoKnowsRequestOptions(topic, p))
		})
		if err != nil {
			return err
		}
		if len(response.Body) == 0 {
			return errors.New("who-knows response was empty")
		}
		if options.json {
			_, err = cmd.OutOrStdout().Write(response.Body)
			return err
		}
		var result whoknows.Response
		if err := json.Unmarshal(response.Body, &result); err != nil {
			return err
		}
		return writeWhoKnowsResponse(cmd, result)
	}
	return command
}
