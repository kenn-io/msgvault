package cmd

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/store"
)

const meetingSuggestionsMinAPISchemaVersion = "5.3.0"

type meetingSuggestionClient interface {
	SuggestMeetingLinks(context.Context, string, int) (store.MeetingSuggestionPage, error)
}

func newMeetingSuggestionsCommand(deps meetingCommandDeps) *cobra.Command {
	var limit int
	var jsonOutput bool
	command := &cobra.Command{Use: "suggest <ref>", Short: "Suggest links between meeting notes and calendar events", Args: cobra.ExactArgs(1)}
	command.Flags().IntVar(&limit, "limit", 10, "maximum suggestions")
	command.Flags().BoolVar(&jsonOutput, "json", false, "print suggestions as JSON")
	command.RunE = func(cmd *cobra.Command, args []string) error {
		if limit < 1 || limit > 100 {
			return usageErr(cmd, fmt.Errorf("limit must be between 1 and 100"))
		}
		ref, err := meetingLinksRef(args[0])
		if err != nil {
			return usageErr(cmd, err)
		}
		client, cleanup, err := openMeetingCommandClient(cmd.Context(), deps, meetingSuggestionsMinAPISchemaVersion)
		if err != nil {
			return err
		}
		defer cleanup()
		backend, ok := client.(meetingSuggestionClient)
		if !ok {
			return fmt.Errorf("meeting suggestions are unavailable")
		}
		page, err := backend.SuggestMeetingLinks(cmd.Context(), ref, limit)
		if err != nil {
			return err
		}
		if jsonOutput {
			return json.MarshalWrite(cmd.OutOrStdout(), page)
		}
		out := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		if _, err := fmt.Fprintln(out, "EVENT\tNOTE\tSTART\tTITLE\tOVERLAP (MIN)\tSHARED ATTENDEES\tSHARED WORDS"); err != nil {
			return err
		}
		for _, item := range page.Suggestions {
			if _, err := fmt.Fprintf(out, "%s\t%d\t%s\t%s\t%d\t%d\t%d\n", item.Ref, item.MemberMessageID, item.Start.Format(time.RFC3339), normalizeSearchTableText(item.Title), item.OverlapMinutes, item.SharedAttendees, item.TitleTokens); err != nil {
				return err
			}
		}
		if err := out.Flush(); err != nil {
			return err
		}
		return writeCalendarExpansionWarnings(cmd.ErrOrStderr(), page.Unexpandable)
	}
	return command
}
