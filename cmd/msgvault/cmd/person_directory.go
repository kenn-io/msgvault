package cmd

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func newPersonDirectoryCommand() *cobra.Command {
	var after, before, sort, cursor, queryText, address, cadence string
	var dueWithinDays int64
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "directory",
		Short: "Find saved people by name, address, or last contact",
		Long: `Find durable people without semantic search or changing profiles.
--query uses lexical matching over current curated names, contact points,
and organizations. --address matches a literal, case-insensitive substring
of current bound participant addresses and identifiers or curated contact
points. Observed participants without a saved person are not returned.
Filters combine to narrow the existing Directory page.`,
		Example: `  msgvault person directory --query "Alex Example"
  msgvault person directory --address alex@example.test --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			query, err := personDirectoryQuery(cmd, after, before, sort, cursor, queryText, address)
			if err != nil {
				return err
			}
			switch cadence {
			case "", store.CadenceOverdue, "due", "no_list", store.CadenceUnknown:
			default:
				return usageErr(cmd, errors.New("--cadence: must be overdue, due, no_list, or unknown"))
			}
			if cmd.Flags().Changed("due-within-days") {
				if cadence != "due" || dueWithinDays < 0 || dueWithinDays > 3660 {
					return usageErr(cmd, errors.New("--due-within-days requires --cadence due and a value from 0 to 3660"))
				}
				query.DueWithinDays = &dueWithinDays
			}
			if cadence != "" {
				query.Cadence = &cadence
			}
			client, _, err := OpenHTTPStore(cmd.Context())
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			if err := client.RequireDirectoryAddress(cmd.Context(), address); err != nil {
				return err
			}
			if cadence != "" || query.DueWithinDays != nil || sort == "most_overdue" || sort == "next_due" {
				supported, err := client.SupportsAPISchemaVersion(cmd.Context(), daemonclient.DirectoryCadenceMinAPISchemaVersion)
				if err != nil {
					return err
				}
				if !supported {
					return errors.New("upgrade the daemon to use contact-list filters or sorting")
				}
			}
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
	command.Flags().StringVar(&queryText, "query", "", "Lexical query over current curated names, contact points, and organizations")
	command.Flags().StringVar(&address, "address", "", "Literal case-insensitive address or identifier substring (saved people only)")
	command.Flags().StringVar(&cadence, "cadence", "", "Filter: overdue, due, no_list, or unknown")
	command.Flags().Int64Var(&dueWithinDays, "due-within-days", 0, "UTC days ahead for --cadence due (0..3660)")
	command.Flags().StringVar(&after, "last-contact-after", "", "Contacted at or after this inclusive date (YYYY-MM-DD or RFC3339)")
	command.Flags().StringVar(&before, "last-contact-before", "", "Contacted at or before this inclusive date (YYYY-MM-DD or RFC3339)")
	command.Flags().StringVar(&sort, "sort", string(generated.LastContactDesc), "Directory order: name, last_contact_desc, last_contact_asc, most_overdue, or next_due")
	command.Flags().StringVar(&cursor, "cursor", "", "Opaque cursor from the previous page")
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output the Directory page as JSON")
	return command
}

func personDirectoryQuery(cmd *cobra.Command, after, before, sort, cursor, queryText, address string) (*generated.ListDirectoryPeopleQuery, error) {
	lower, err := parsePersonSearchDate(after)
	if err != nil {
		return nil, usageErr(cmd, fmt.Errorf("--last-contact-after: %w", err))
	}
	upper, err := parsePersonSearchDate(before)
	if err != nil {
		return nil, usageErr(cmd, fmt.Errorf("--last-contact-before: %w", err))
	}
	if sort == "" {
		sort = string(generated.LastContactDesc)
	}
	order := generated.ListDirectoryPeopleQuerySort(sort)
	if err := order.Validate(); err != nil {
		return nil, usageErr(cmd, errors.New("--sort: must be name, last_contact_desc, last_contact_asc, most_overdue, or next_due"))
	}
	query := &generated.ListDirectoryPeopleQuery{LastContactAfter: lower, LastContactBefore: upper, Sort: &order}
	if queryText != "" {
		query.Q = &queryText
	}
	if address = strings.TrimSpace(address); address != "" {
		query.Address = &address
	}
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
		return json.MarshalEncode(jsontext.NewEncoder(cmd.OutOrStdout()), page, json.Deterministic(true))
	}
	writer := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "ID\tDISPLAY NAME\tLIST\tLAST CONTACT\tDUE\tSTATUS")
	for _, person := range page.People {
		list, due, status := "-", "-", store.CadenceUnknown
		if cadence := person.Cadence; cadence != nil {
			if cadence.List != nil && *cadence.List != "" {
				list = textutil.SanitizeTerminal(*cadence.List)
			} else if cadence.FrequencyDays != nil {
				list = fmt.Sprintf("%d days", *cadence.FrequencyDays)
			}
			due = personDirectoryLastContact(cadence.DueAt)
			if cadence.Status != "" {
				status = textutil.SanitizeTerminal(cadence.Status)
			}
		}
		_, _ = fmt.Fprintf(writer, "%d\t%s\t%s\t%s\t%s\t%s\n", person.ID, personDisplayName(person.DisplayName), list, personDirectoryLastContact(person.LastContactAt), due, status)
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

func parsePersonSearchDate(value string) (*time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil //nolint:nilnil // An omitted optional date has no value and no error.
	}
	for _, layout := range []string{"2006-01-02", time.RFC3339} {
		parsed, err := time.Parse(layout, value)
		if err == nil {
			parsed = parsed.UTC()
			return &parsed, nil
		}
	}
	return nil, errors.New("must be YYYY-MM-DD or RFC3339")
}
