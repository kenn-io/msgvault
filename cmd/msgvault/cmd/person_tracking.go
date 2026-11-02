package cmd

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

var (
	personTrackCmd   = newPersonTrackingCommand("track", true)
	personUntrackCmd = newPersonTrackingCommand("untrack", false)
)

func newPersonTrackingCommand(action string, tracked bool) *cobra.Command {
	return &cobra.Command{
		Use:   action + " <person-id>...",
		Short: strings.ToUpper(action[:1]) + action[1:] + " durable people for future profile maintenance",
		Long: strings.ToUpper(action[:1]) + action[1:] + ` each person in order. Every ID is checked before
any request, and the command stops at the first person the daemon rejects;
people before it keep their new state. With --json, each person's state is one
JSON object per line.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			personIDs := make([]int64, len(args))
			for i, arg := range args {
				personID, err := positivePersonCLIArg(cmd, arg, personValue)
				if err != nil {
					return err
				}
				personIDs[i] = personID
			}
			client, _, err := OpenHTTPStore(cmd.Context())
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()

			body := generated.SetPersonTrackingBody{Tracked: tracked}
			for _, personID := range personIDs {
				response, err := daemonclient.APIResponse(client,
					func(api *apiclient.Client) (*generated.SetPersonTrackingResp, error) {
						return api.SetPersonTrackingWithResponse(cmd.Context(),
							&generated.SetPersonTrackingRequestOptions{
								PathParams: &generated.SetPersonTrackingPath{ID: personID},
								Body:       &body,
							})
					})
				if err != nil {
					return fmt.Errorf("%s person %d: %w", action, personID, err)
				}
				if response.JSON200 == nil {
					return fmt.Errorf("%s person %d: person tracking response was empty", action, personID)
				}
				if err = writePersonTrackingState(cmd, personID, response); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func writePersonTrackingState(cmd *cobra.Command, personID int64, response *generated.SetPersonTrackingResp) error {
	if personJSON {
		body := bytes.TrimRight(response.Body, "\n")
		if _, err := cmd.OutOrStdout().Write(append(body, '\n')); err != nil {
			return fmt.Errorf("write person tracking state: %w", err)
		}
		return nil
	}
	state := "untracked"
	if response.JSON200.Tracked {
		state = "tracked"
	}
	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Person %d: %s\n", personID, state); err != nil {
		return fmt.Errorf("write person tracking state: %w", err)
	}
	return nil
}
