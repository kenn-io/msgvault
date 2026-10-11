package cmd

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/personscope"
	"go.kenn.io/msgvault/internal/textutil"
)

func newMediaCmd(open func(context.Context) (*daemonclient.Client, func(), error)) *cobra.Command {
	parent := &cobra.Command{Use: "media", Short: "Search recording transcripts"}
	var request daemonclient.MediaSearchOptions
	var directions []string
	var jsonOutput bool
	search := &cobra.Command{Use: "search <query>", Short: "Find spoken words in visible messages", Args: cobra.MinimumNArgs(1), RunE: func(command *cobra.Command, args []string) error {
		request.Query = strings.Join(args, " ")
		if request.Limit < 1 || (command.Flags().Changed("person") && request.PersonID <= 0) {
			return errors.New("limit and explicit --person must be positive")
		}
		request.Directions = make([]personscope.Direction, len(directions))
		for i, direction := range directions {
			request.Directions[i] = personscope.Direction(direction)
		}
		client, cleanup, err := open(command.Context())
		if err != nil {
			return err
		}
		defer cleanup()
		response, err := client.SearchMedia(command.Context(), request)
		if err != nil {
			return err
		}
		if jsonOutput {
			return json.MarshalEncode(jsontext.NewEncoder(command.OutOrStdout()), response, json.Deterministic(true))
		}
		writer := tabwriter.NewWriter(command.OutOrStdout(), 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(writer, "MESSAGE\tCONVERSATION\tATTACHMENT\tORIGIN\tTIMING MS\tEXCERPT")
		for _, result := range response.Results {
			timing := ""
			if result.StartMs != nil && result.EndMs != nil {
				timing = fmt.Sprintf("%d-%d", *result.StartMs, *result.EndMs)
			}
			_, _ = fmt.Fprintf(writer, "%d\t%d\t%d\t%s\t%s\t%s\n", result.MessageID, result.ConversationID, result.AttachmentID, result.Origin, timing, strings.Join(strings.Fields(textutil.SanitizeTerminal(result.Excerpt)), " "))
		}
		if err := writer.Flush(); err != nil {
			return fmt.Errorf("render media search results: %w", err)
		}
		_, err = fmt.Fprintf(command.OutOrStdout(), "Coverage %s; pending %d; unavailable %d; attribution unavailable %d; partial %t; truncated %t\n", textutil.SanitizeTerminal(response.Coverage.State), response.PendingOccurrences, response.UnavailableOccurrences, response.AttributionUnavailable, response.Partial, response.Truncated)
		if err != nil {
			return fmt.Errorf("render media search coverage: %w", err)
		}
		return nil
	}}
	search.Flags().StringVar(&request.Mode, "mode", "lexical", "Search mode: lexical")
	search.Flags().Int64Var(&request.PersonID, "person", 0, "Limit to one durable person")
	search.Flags().StringSliceVar(&directions, "direction", nil, "Person relation: from_person, to_person, or group")
	search.Flags().IntVarP(&request.Limit, "limit", "n", 20, "Maximum occurrences, 1 to 100")
	search.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output structured JSON")
	parent.AddCommand(search)
	return parent
}

func init() {
	rootCmd.AddCommand(newMediaCmd(func(ctx context.Context) (*daemonclient.Client, func(), error) {
		client, _, err := OpenHTTPStore(ctx)
		return client, func() {
			if client != nil {
				_ = client.Close()
			}
		}, err
	}))
}
