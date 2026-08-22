package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

var (
	personJSON             bool
	personClearDisplayName bool
)

const personValue = "person"

var personCmd = &cobra.Command{
	Use:   personValue,
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
		resp, err := daemonclient.APIResponseWithStatuses(client,
			[]int{http.StatusOK, http.StatusCreated},
			func(api *apiclient.Client) (*generated.CreatePersonResp, error) {
				return api.CreatePersonWithResponse(cmd.Context(),
					&generated.CreatePersonRequestOptions{Body: &body})
			})
		if err != nil {
			return err
		}
		// 201 carries a newly created person; 200 an idempotent re-promotion.
		person := resp.JSON201
		if person == nil {
			person = resp.JSON200
		}
		return writeCLIPerson(cmd, person)
	},
}

var personGetCmd = &cobra.Command{
	Use:   "get <person-id>",
	Short: "Get a durable person profile",
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
			func(api *apiclient.Client) (*generated.ListPeopleResp, error) {
				return api.ListPeopleWithResponse(cmd.Context())
			})
		if err != nil {
			return err
		}
		if personJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(resp.JSON200.People)
		}
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "ID\tDISPLAY NAME\tVCARD UID\tPARTICIPANTS\tREVISION")
		for _, person := range resp.JSON200.People {
			_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%d\t%d\n", person.ID,
				personDisplayName(person.DisplayName), person.VcardUID,
				len(person.ParticipantIds), person.Revision)
		}
		return w.Flush()
	},
}

var personSetDisplayNameCmd = &cobra.Command{
	Use:   "set-display-name <person-id> [display-name]",
	Short: "Set a durable person's display-name override",
	Args: func(cmd *cobra.Command, args []string) error {
		switch {
		case personClearDisplayName && len(args) != 1:
			return usageErr(cmd, errors.New("--clear cannot be used with a display name"))
		case !personClearDisplayName && len(args) != 2:
			return usageErr(cmd, errors.New("display name is required unless --clear is used"))
		}
		return nil
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := positivePersonCLIArg(cmd, args[0], personValue)
		if err != nil {
			return err
		}
		var displayName *string
		if !personClearDisplayName {
			value := strings.TrimSpace(args[1])
			if value == "" {
				return usageErr(cmd, errors.New("display name must not be empty"))
			}
			displayName = &value
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
		body := generated.PatchPersonBody{DisplayName: displayName}
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

var personDeleteCmd = &cobra.Command{
	Use:   "delete <person-id>",
	Short: "Permanently delete a durable person profile",
	Long: "Permanently delete a durable person profile. The person's participant\n" +
		"bindings are removed and its vCard UID is retired forever; re-promoting\n" +
		"the same cluster afterwards creates a new person with a new UID.",
	Args: cobra.ExactArgs(1),
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
		current, err := getCLIPerson(cmd, client, id)
		if err != nil {
			return err
		}
		if current.JSON200 == nil {
			return errors.New("person response was empty")
		}
		etag := fmt.Sprintf(`"person-%d-r%d"`, id, current.JSON200.Revision)
		if _, err := daemonclient.APIResponseWithStatuses(client,
			[]int{http.StatusNoContent},
			func(api *apiclient.Client) (*generated.DeletePersonResp, error) {
				return api.DeletePersonWithResponse(cmd.Context(), &generated.DeletePersonRequestOptions{
					PathParams: &generated.DeletePersonPath{ID: id},
					Header:     &generated.DeletePersonHeaders{IfMatch: etag},
				})
			}); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Deleted person %d\n", id)
		return nil
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
	personCmd.AddCommand(newPersonProviderCommand(defaultPersonProviderCommandDeps()))
	personCmd.AddCommand(personPromoteCmd, personGetCmd, personListCmd,
		personSetDisplayNameCmd, personDeleteCmd, personTrackCmd, personUntrackCmd,
		personNotesCmd, newPersonFilesCommand(defaultPersonFilesCommandDeps()),
		personSearchCmd)
	for _, command := range []*cobra.Command{
		personPromoteCmd, personGetCmd, personListCmd, personSetDisplayNameCmd,
		personTrackCmd, personUntrackCmd,
	} {
		command.Flags().BoolVar(&personJSON, flagJSON, false, "Output as JSON")
	}
	personSetDisplayNameCmd.Flags().BoolVar(
		&personClearDisplayName, "clear", false, "Clear the display-name override",
	)
}
