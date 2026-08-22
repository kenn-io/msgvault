package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

const (
	defaultPersonSearchLimit = 20
	maximumPersonSearchLimit = 100
)

var (
	personSearchLimit = defaultPersonSearchLimit
	personSearchJSON  bool
)

type personSearchCLIResult struct {
	ID          int64   `json:"id"`
	DisplayName *string `json:"display_name"`
	Score       float64 `json:"score"`
}

type personSearchCLIResponse struct {
	Results []personSearchCLIResult `json:"results"`
}

var personSearchCmd = &cobra.Command{
	Use:   "search <free-text>",
	Short: "Search durable person profiles semantically",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		query := strings.TrimSpace(strings.Join(args, " "))
		if query == "" {
			return usageErr(cmd, errors.New("query must contain non-whitespace text"))
		}
		if personSearchLimit < 1 || personSearchLimit > maximumPersonSearchLimit {
			return usageErr(cmd, fmt.Errorf("--limit must be between 1 and 100, got %d", personSearchLimit))
		}

		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()

		limit := int64(personSearchLimit)
		body := generated.SearchPeopleBody{Query: query, Limit: &limit}
		response, err := daemonclient.APIResponse(client,
			func(api *apiclient.Client) (*generated.SearchPeopleResp, error) {
				return api.SearchPeopleWithResponse(cmd.Context(), &generated.SearchPeopleRequestOptions{
					Body: &body,
				})
			})
		if err != nil {
			return err
		}
		if response.JSON200 == nil {
			return errors.New("person search response was empty")
		}

		output := personSearchCLIResponse{Results: make([]personSearchCLIResult, len(response.JSON200.Results))}
		for i, result := range response.JSON200.Results {
			output.Results[i] = personSearchCLIResult{
				ID: result.Person.ID, DisplayName: result.Person.DisplayName, Score: result.Score,
			}
		}
		if personSearchJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(output)
		}

		writer := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(writer, "ID\tDISPLAY NAME\tSCORE")
		for _, result := range output.Results {
			_, _ = fmt.Fprintf(writer, "%d\t%s\t%.6f\n",
				result.ID, personDisplayName(result.DisplayName), result.Score)
		}
		return writer.Flush()
	},
}

func init() {
	personSearchCmd.Flags().IntVar(
		&personSearchLimit, "limit", defaultPersonSearchLimit, "Maximum number of results",
	)
	personSearchCmd.Flags().BoolVar(&personSearchJSON, flagJSON, false, "Output as JSON")
}
