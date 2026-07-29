package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

var personJSON bool

var personCmd = &cobra.Command{
	Use:   "person",
	Short: "Manage durable person profiles",
}

var personPromoteCmd = &cobra.Command{
	Use:   "promote <participant-id>",
	Short: "Promote a participant's identity cluster to a durable person",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		participantID, err := positivePersonCLIArg(cmd, args[0], "participant")
		if err != nil {
			return err
		}
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		body := generated.CreatePersonBody{ParticipantID: participantID}
		resp, err := daemonclient.APIResponse(client,
			func(api *apiclient.Client) (*generated.CreatePersonResp, error) {
				return api.CreatePersonWithResponse(cmd.Context(),
					&generated.CreatePersonRequestOptions{Body: &body})
			})
		if err != nil {
			return err
		}
		return writeCLIPerson(cmd, resp.JSON201)
	},
}

var personGetCmd = &cobra.Command{
	Use:   "get <person-id>",
	Short: "Get a durable person profile",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := positivePersonCLIArg(cmd, args[0], "person")
		if err != nil {
			return err
		}
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		resp, err := getCLIPerson(cmd, client, id)
		if err != nil {
			return err
		}
		return writeCLIPerson(cmd, resp.JSON200)
	},
}

var personListCmd = &cobra.Command{
	Use:   cmdUseList,
	Short: "List durable person profiles",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		resp, err := daemonclient.APIResponse(client,
			func(api *apiclient.Client) (*generated.ListPersonsResp, error) {
				return api.ListPersonsWithResponse(cmd.Context())
			})
		if err != nil {
			return err
		}
		if personJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(resp.JSON200.Persons)
		}
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "ID\tDISPLAY NAME\tVCARD UID\tPARTICIPANTS\tREVISION")
		for _, person := range resp.JSON200.Persons {
			_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%d\t%d\n", person.ID,
				personDisplayName(person.DisplayName), person.VcardUID,
				len(person.ParticipantIds), person.Revision)
		}
		return w.Flush()
	},
}

var personSetDisplayNameCmd = &cobra.Command{
	Use:   "set-display-name <person-id> <display-name>",
	Short: "Set a durable person's display-name override",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := positivePersonCLIArg(cmd, args[0], "person")
		if err != nil {
			return err
		}
		displayName := strings.TrimSpace(args[1])
		if displayName == "" {
			return usageErr(cmd, errors.New("display name must not be empty"))
		}
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		current, err := getCLIPerson(cmd, client, id)
		if err != nil {
			return err
		}
		if current.JSON200 == nil {
			return errors.New("person response was empty")
		}
		etag := fmt.Sprintf(`"person-%d-r%d"`, id, current.JSON200.Revision)
		body := generated.PatchPersonBody{DisplayName: &displayName}
		resp, err := daemonclient.APIResponse(client,
			func(api *apiclient.Client) (*generated.PatchPersonResp, error) {
				return api.PatchPersonWithResponse(cmd.Context(), &generated.PatchPersonRequestOptions{
					PathParams: &generated.PatchPersonPath{ID: id},
					Header:     &generated.PatchPersonHeaders{IfMatch: etag},
					Body:       &body,
				})
			})
		if err != nil {
			return err
		}
		return writeCLIPerson(cmd, resp.JSON200)
	},
}

func getCLIPerson(
	cmd *cobra.Command, client *daemonclient.Client, id int64,
) (*generated.GetPersonProfileResp, error) {
	return daemonclient.APIResponse(client,
		func(api *apiclient.Client) (*generated.GetPersonProfileResp, error) {
			return api.GetPersonProfileWithResponse(cmd.Context(),
				&generated.GetPersonProfileRequestOptions{
					PathParams: &generated.GetPersonProfilePath{ID: id},
				})
		})
}

func writeCLIPerson(cmd *cobra.Command, person *generated.Person) error {
	if person == nil {
		return errors.New("person response was empty")
	}
	if personJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(person)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(),
		"Person: %d\nDisplay name: %s\nvCard UID: %s\nParticipants: %v\nRevision: %d\n",
		person.ID, personDisplayName(person.DisplayName), person.VcardUID,
		person.ParticipantIds, person.Revision)
	return nil
}

func personDisplayName(name *string) string {
	if name == nil {
		return "-"
	}
	return *name
}

func positivePersonCLIArg(cmd *cobra.Command, raw, kind string) (int64, error) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, usageErr(cmd, fmt.Errorf("%s ID must be a positive integer", kind))
	}
	return id, nil
}

func init() {
	rootCmd.AddCommand(personCmd)
	personCmd.AddCommand(personPromoteCmd, personGetCmd, personListCmd, personSetDisplayNameCmd)
	for _, command := range []*cobra.Command{personPromoteCmd, personGetCmd, personListCmd, personSetDisplayNameCmd} {
		command.Flags().BoolVar(&personJSON, flagJSON, false, "Output as JSON")
	}
}
