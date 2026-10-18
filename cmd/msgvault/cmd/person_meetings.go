package cmd

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/store"
)

func newPersonMeetingsCommand(deps meetingRecordsCommandDeps) *cobra.Command {
	var from, to, cursor string
	var limit int
	var jsonOutput bool
	command := &cobra.Command{Use: "meetings <person-ref>", Short: "List a person's archived invitations and meeting notes", Long: "List permitted invitation and transcript evidence for a numeric person ID, person:<id>, or participant:<id>. Defaults to the past 365 days. Windows are at most 400 days; omitted dates on continuation reuse the first page's window.", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if _, _, err := store.ParsePersonMeetingRef(args[0]); err != nil {
			return usageErr(cmd, err)
		}
		q := store.PersonMeetingsQuery{Limit: limit, Cursor: cursor}
		for _, field := range []struct {
			name, value string
			target      *time.Time
		}{{"from", from, &q.From}, {"to", to, &q.To}} {
			if field.value != "" {
				value, err := time.Parse(time.RFC3339, field.value)
				if err != nil {
					return usageErr(cmd, fmt.Errorf("--%s must be an RFC3339 timestamp", field.name))
				}
				*field.target = value
			}
		}
		if !q.From.IsZero() && !q.To.IsZero() && (!q.To.After(q.From) || q.To.Sub(q.From) > 400*24*time.Hour) {
			return usageErr(cmd, errors.New("window must be positive and at most 400 days"))
		}
		if limit < 1 || limit > 500 {
			return usageErr(cmd, errors.New("--limit must be between 1 and 500"))
		}
		client, cleanup, err := openMeetingRecordsClient(cmd.Context(), deps)
		if err != nil {
			return err
		}
		defer cleanup()
		page, err := client.ListPersonMeetings(cmd.Context(), args[0], q)
		if err != nil {
			return err
		}
		if jsonOutput {
			return writeMeetingJSON(cmd, page)
		}
		for _, item := range page.Items {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%d notes\t%s\n", item.Ref, item.Start.Format(time.RFC3339), item.ItemCount, item.Title); err != nil {
				return fmt.Errorf("write person meeting: %w", err)
			}
		}
		if page.NextCursor != "" {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Next cursor: %s\n", page.NextCursor); err != nil {
				return fmt.Errorf("write meeting cursor: %w", err)
			}
		}
		return nil
	}}
	command.Flags().StringVar(&from, "from", "", "Inclusive RFC3339 start (default 365 days before to)")
	command.Flags().StringVar(&to, "to", "", "Exclusive RFC3339 end (default now)")
	command.Flags().IntVar(&limit, "limit", 100, "Maximum meetings (1-500)")
	command.Flags().StringVar(&cursor, "cursor", "", "Opaque continuation cursor from the same query")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Print the complete meeting page as JSON")
	return command
}
