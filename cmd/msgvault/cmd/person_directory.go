package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/textutil"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func newPersonDirectoryCommand() *cobra.Command {
	var after, before, sort, cursor string
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "directory",
		Short: "Browse durable people by last contact",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			query, err := personDirectoryQuery(cmd, after, before, sort, cursor)
			if err != nil {
				return err
			}
			client, _, err := OpenHTTPStore(cmd.Context())
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			response, err := daemonclient.APIResponse(client,
				func(api *apiclient.Client) (*generated.ListDirectoryPeopleResp, error) {
					return api.ListDirectoryPeopleWithResponse(cmd.Context(), &generated.ListDirectoryPeopleRequestOptions{Query: query})
				})
			if err != nil {
				return err
			}
			return writePersonDirectoryPage(cmd, response.JSON200, jsonOutput)
		},
	}
	command.Flags().StringVar(&after, "last-contact-after", "", "Contacted at or after this inclusive date (YYYY-MM-DD or RFC3339)")
	command.Flags().StringVar(&before, "last-contact-before", "", "Contacted at or before this inclusive date (YYYY-MM-DD or RFC3339)")
	command.Flags().StringVar(&sort, "sort", string(generated.LastContactDesc), "Directory order: name, last_contact_desc, or last_contact_asc")
	command.Flags().StringVar(&cursor, "cursor", "", "Opaque cursor from the previous page")
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output the Directory page as JSON")
	return command
}

func personDirectoryQuery(cmd *cobra.Command, after, before, sort, cursor string) (*generated.ListDirectoryPeopleQuery, error) {
	lower, err := parseDocumentSearchDate(after)
	if err != nil {
		return nil, usageErr(cmd, fmt.Errorf("--last-contact-after: %w", err))
	}
	upper, err := parseDocumentSearchDate(before)
	if err != nil {
		return nil, usageErr(cmd, fmt.Errorf("--last-contact-before: %w", err))
	}
	if sort == "" {
		sort = string(generated.LastContactDesc)
	}
	order := generated.ListDirectoryPeopleQuerySort(sort)
	if err := order.Validate(); err != nil {
		return nil, usageErr(cmd, errors.New("--sort: must be name, last_contact_desc, or last_contact_asc"))
	}
	query := &generated.ListDirectoryPeopleQuery{LastContactAfter: lower, LastContactBefore: upper, Sort: &order}
	if cursor != "" {
		query.Cursor = &cursor
	}
	return query, nil
}

func writePersonDirectoryPage(cmd *cobra.Command, page *generated.DirectoryPeopleResponse, jsonOutput bool) error {
	if page == nil {
		return errors.New("person directory response was empty")
	}
	if jsonOutput {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(page)
	}
	writer := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "ID\tDISPLAY NAME\tLAST CONTACT")
	for _, person := range page.People {
		_, _ = fmt.Fprintf(writer, "%d\t%s\t%s\n", person.ID, personDisplayName(person.DisplayName), personDirectoryLastContact(person.LastContactAt))
	}
	if err := writer.Flush(); err != nil {
		return fmt.Errorf("write person directory: %w", err)
	}
	if page.NextCursor != nil && *page.NextCursor != "" {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Next cursor: %s\n", textutil.SanitizeTerminal(*page.NextCursor)); err != nil {
			return fmt.Errorf("write person directory cursor: %w", err)
		}
	}
	return nil
}

func personDirectoryLastContact(value *time.Time) string {
	if value == nil {
		return "-"
	}
	return value.UTC().Format(time.RFC3339)
}
