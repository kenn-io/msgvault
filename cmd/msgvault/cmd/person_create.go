package cmd

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/mail"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func newPersonCreateCommand() *cobra.Command {
	var input store.PersonCreateInput
	var emails, phones []string
	var jsonOutput, publish bool
	var ifExists string
	command := &cobra.Command{
		Use: "create", Short: "Create a person without message participants", Args: cobra.NoArgs,
	}
	flags := command.Flags()
	flags.StringVar(&ifExists, "if-exists", "error", "Existing contacts: error or return the matching person")
	flags.StringVar(&input.Name, "name", "", "Person's display name (required)")
	flags.StringArrayVar(&emails, "email", nil,
		"Email address with optional :type; repeat for multiple addresses")
	flags.StringArrayVar(&phones, "phone", nil,
		"Phone number with optional :type; repeat for multiple numbers")
	flags.StringVar(&input.Org, "org", "", "Current organization")
	flags.StringVar(&input.Title, "title", "", "Job title at --org (requires --org)")
	flags.StringVar(&input.Address, "address", "", "Postal address")
	flags.StringVar(&input.Note, "note", "", "Private profile note")
	flags.BoolVar(&jsonOutput, flagJSON, false, "Output the created person as JSON")
	flags.BoolVar(&publish, "publish", false,
		"Publish the new person to CardDAV")
	command.RunE = func(cmd *cobra.Command, _ []string) error {
		input.IfExists = store.PersonIfExists(ifExists)
		// Omit the default so creation still works against older daemons.
		if input.IfExists == store.PersonIfExistsError {
			input.IfExists = ""
		}
		input.Emails = parsePersonCreateContacts(emails)
		input.Phones = parsePersonCreateContacts(phones)
		if err := store.ValidatePersonCreateInput(input); err != nil {
			return usageErr(cmd, err)
		}
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		if input.IfExists == store.PersonIfExistsReturn {
			supported, err := client.SupportsAPISchemaVersion(cmd.Context(), personCreateIfExistsMinAPISchemaVersion)
			if err != nil {
				return fmt.Errorf("check daemon person creation return support: %w", err)
			}
			if !supported {
				return fmt.Errorf("this daemon is too old for person create --if-exists return; upgrade it to API schema %s or newer", personCreateIfExistsMinAPISchemaVersion)
			}
		}
		person, err := client.CreateStandalonePerson(cmd.Context(), input)
		if err != nil {
			var conflict *daemonclient.APIError
			if errors.As(err, &conflict) && (conflict.Code == "person_ambiguous" || conflict.Code == "person_matches_participant") {
				if jsonOutput {
					if writeErr := json.MarshalEncode(jsontext.NewEncoder(cmd.OutOrStdout()), struct {
						Error   string         `json:"error"`
						Message string         `json:"message"`
						Details jsontext.Value `json:"details,omitempty"`
					}{conflict.Code, conflict.Message, conflict.Details}); writeErr != nil {
						return writeErr
					}
				}
				return fmt.Errorf("%s: %s", conflict.Code, conflict.Message)
			}
			return err
		}
		if person == nil {
			return errors.New("person creation response was empty")
		}
		out := cmd.OutOrStdout()
		if jsonOutput {
			err = json.MarshalEncode(jsontext.NewEncoder(out), person, json.Deterministic(true))
		} else if person.Ensure != nil && person.Ensure.Status == "matched_existing" {
			name := ""
			if person.DisplayName != nil {
				name = *person.DisplayName
			}
			if len(person.Ensure.MatchBasis) > 0 {
				match := person.Ensure.MatchBasis[0]
				_, err = fmt.Fprintf(out, "Matched person %d (%s) by %s %s\n", person.ID,
					textutil.SanitizeTerminal(name), match.Kind, textutil.SanitizeTerminal(match.Value))
			} else {
				_, err = fmt.Fprintf(out, "Matched person %d (%s)\n", person.ID, textutil.SanitizeTerminal(name))
			}
		} else {
			_, err = fmt.Fprintf(out, "Created person %d: %s\n",
				person.ID, textutil.SanitizeTerminal(strings.TrimSpace(input.Name)))
		}
		if err != nil {
			return fmt.Errorf("write created person: %w", err)
		}
		if !publish || (input.IfExists == store.PersonIfExistsReturn && (person.Ensure == nil || person.Ensure.Status != "created")) {
			return nil
		}
		_, err = daemonclient.APIResponse(client,
			func(api *apiclient.Client) (*generated.PublishCardDAVPersonResp, error) {
				return api.PublishCardDAVPersonWithResponse(cmd.Context(),
					&generated.PublishCardDAVPersonRequestOptions{
						PathParams: &generated.PublishCardDAVPersonPath{PersonID: person.ID},
					})
			})
		if err != nil {
			return fmt.Errorf("person %d was created; CardDAV publication failed: %w; "+
				"retry with msgvault person publish %d "+
				"(use --preview and --approve if review is required)",
				person.ID, err, person.ID)
		}
		if !jsonOutput {
			_, _ = fmt.Fprintf(out, "Person %d CardDAV publication updated\n", person.ID)
		}
		return nil
	}
	return command
}

// parsePersonCreateContacts splits an optional trailing :type from each value.
// A bare email address keeps its full value.
func parsePersonCreateContacts(values []string) []store.PersonCreateContact {
	contacts := make([]store.PersonCreateContact, 0, len(values))
	for _, raw := range values {
		value, kind := raw, ""
		trimmed := strings.TrimSpace(raw)
		address, err := mail.ParseAddress(trimmed)
		if err != nil || address.Address != trimmed {
			if prefix, suffix, found := strings.CutLast(raw, ":"); found {
				value, kind = prefix, suffix
			}
		}
		contacts = append(contacts, store.PersonCreateContact{Value: value, Type: kind})
	}
	return contacts
}

func init() { personCmd.AddCommand(newPersonCreateCommand()) }
