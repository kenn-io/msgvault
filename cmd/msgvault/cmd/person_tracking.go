package cmd

import (
	"bytes"
	"errors"
	"fmt"

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
		Use:   action + " <person-id>",
		Short: action + " a durable person for future profile maintenance",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			personID, err := positivePersonCLIArg(cmd, args[0], "person")
			if err != nil {
				return err
			}
			client, _, err := OpenHTTPStore(cmd.Context())
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()

			body := generated.SetPersonTrackingBody{Tracked: tracked}
			response, err := daemonclient.APIResponse(client,
				func(api *apiclient.Client) (*generated.SetPersonTrackingResp, error) {
					return api.SetPersonTrackingWithResponse(cmd.Context(),
						&generated.SetPersonTrackingRequestOptions{
							PathParams: &generated.SetPersonTrackingPath{ID: personID},
							Body:       &body,
						})
				})
			if err != nil {
				return err
			}
			if response.JSON200 == nil {
				return errors.New("person tracking response was empty")
			}
			if personJSON {
				_, err = cmd.OutOrStdout().Write(response.Body)
				if err == nil && !bytes.HasSuffix(response.Body, []byte("\n")) {
					_, err = cmd.OutOrStdout().Write([]byte("\n"))
				}
				return err
			}
			state := "untracked"
			if response.JSON200.Tracked {
				state = "tracked"
			}
			if _, err = fmt.Fprintf(cmd.OutOrStdout(), "Person %d: %s\n", personID, state); err != nil {
				return fmt.Errorf("write person tracking state: %w", err)
			}
			return nil
		},
	}
}
