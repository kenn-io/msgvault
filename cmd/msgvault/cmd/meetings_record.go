package cmd

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/store"
)

const meetingRecordsMinAPISchemaVersion = "3.25.0"

type meetingRecordsCommandClient interface {
	mcpserver.MeetingRecordsBackend
	APISchemaVersion(ctx context.Context) (string, error)
	LinkMeeting(ctx context.Context, ref string, messageID int64) (store.MeetingRecord, error)
	UnlinkMeeting(ctx context.Context, ref string, messageID int64) (store.MeetingRecord, error)
}

type meetingRecordsCommandDeps struct {
	open func(context.Context) (meetingRecordsCommandClient, func(), error)
}

func defaultMeetingRecordsCommandDeps() meetingRecordsCommandDeps {
	return meetingRecordsCommandDeps{open: func(ctx context.Context) (meetingRecordsCommandClient, func(), error) {
		client, _, err := OpenHTTPStore(ctx)
		if err != nil {
			return nil, func() {}, err
		}
		return client, func() { _ = client.Close() }, nil
	}}
}
func openMeetingRecordsClient(ctx context.Context, deps meetingRecordsCommandDeps) (meetingRecordsCommandClient, func(), error) {
	if deps.open == nil {
		return nil, func() {}, errors.New("meeting record client is unavailable")
	}
	client, cleanup, err := deps.open(ctx)
	if err != nil {
		return nil, func() {}, err
	}
	if cleanup == nil {
		cleanup = func() {}
	}
	version, err := client.APISchemaVersion(ctx)
	if err != nil {
		cleanup()
		return nil, func() {}, err
	}
	if !daemonclient.APISchemaVersionAtLeast(version, meetingRecordsMinAPISchemaVersion) {
		cleanup()
		return nil, func() {}, fmt.Errorf("meeting records require daemon API schema %s or newer (daemon reports %q); upgrade the daemon", meetingRecordsMinAPISchemaVersion, version)
	}
	return client, cleanup, nil
}
func newMeetingShowCommand(deps meetingRecordsCommandDeps) *cobra.Command {
	var jsonOutput bool
	command := &cobra.Command{Use: "show <ref>", Short: "Read an archived meeting record", Long: "Read an archived meeting record with at most 10 linked items and full packets for the first 3. Use item follow_up references for message details.", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if err := store.ValidateMeetingRecordRef(args[0]); err != nil {
			return usageErr(cmd, err)
		}
		client, cleanup, err := openMeetingRecordsClient(cmd.Context(), deps)
		if err != nil {
			return err
		}
		defer cleanup()
		record, err := client.GetMeeting(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		if jsonOutput {
			return writeMeetingJSON(cmd, record)
		}
		return writeMeetingRecord(cmd, record)
	}}
	command.Flags().BoolVar(&jsonOutput, "json", false, "Print the complete meeting record as JSON")
	return command
}
func newMeetingSuggestCommand(deps meetingRecordsCommandDeps) *cobra.Command {
	var jsonOutput bool
	var limit int
	command := &cobra.Command{Use: "suggest <ref>", Short: "Suggest possible archive meeting links", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if err := store.ValidateMeetingRecordRef(args[0]); err != nil {
			return usageErr(cmd, err)
		}
		if limit < 1 || limit > 100 {
			return usageErr(cmd, errors.New("--limit must be between 1 and 100"))
		}
		client, cleanup, err := openMeetingRecordsClient(cmd.Context(), deps)
		if err != nil {
			return err
		}
		defer cleanup()
		items, err := client.SuggestMeetingLinks(cmd.Context(), args[0], limit)
		if err != nil {
			return err
		}
		if jsonOutput {
			return writeMeetingJSON(cmd, items)
		}
		for _, item := range items {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s\tmessage %d\t%d minutes overlap\t%d shared attendees\n", item.Ref, item.MemberMessageID, item.OverlapMinutes, item.SharedAttendees); err != nil {
				return fmt.Errorf("write meeting suggestion: %w", err)
			}
		}
		return nil
	}}
	command.Flags().IntVar(&limit, "limit", 10, "Maximum suggestions (1-100)")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Print suggestions as JSON")
	return command
}
func newMeetingLinkCommand(deps meetingRecordsCommandDeps, unlink bool) *cobra.Command {
	verb, short := "link", "Link an archived note to a calendar occurrence (owner only)"
	if unlink {
		verb, short = "unlink", "Split an archived note from a calendar occurrence (owner only)"
	}
	var jsonOutput bool
	command := &cobra.Command{Use: verb + " <evt-ref> <message-id>", Short: short, Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		kind, _, _, err := store.ParseMeetingRef(args[0])
		if err != nil || kind != "evt" || store.ValidateMeetingRecordRef(args[0]) != nil {
			return usageErr(cmd, errors.New("ref must identify a calendar occurrence"))
		}
		id, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil || id <= 0 || id > 9007199254740991 {
			return usageErr(cmd, errors.New("message-id must be a positive JavaScript-safe integer"))
		}
		client, cleanup, err := openMeetingRecordsClient(cmd.Context(), deps)
		if err != nil {
			return err
		}
		defer cleanup()
		var record store.MeetingRecord
		if unlink {
			record, err = client.UnlinkMeeting(cmd.Context(), args[0], id)
		} else {
			record, err = client.LinkMeeting(cmd.Context(), args[0], id)
		}
		if err != nil {
			return err
		}
		if jsonOutput {
			return writeMeetingJSON(cmd, record)
		}
		return writeMeetingRecord(cmd, record)
	}}
	command.Flags().BoolVar(&jsonOutput, "json", false, "Print the updated meeting record as JSON")
	return command
}
func writeMeetingRecord(cmd *cobra.Command, record store.MeetingRecord) error {
	if _, err := fmt.Fprintln(cmd.OutOrStdout(), record.Ref); err != nil {
		return fmt.Errorf("write meeting record: %w", err)
	}
	if record.Occurrence != nil {
		if err := writeCalendarOccurrence(cmd, *record.Occurrence); err != nil {
			return err
		}
	}
	if record.OwnerRSVP != "" {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Owner RSVP: %s\n", record.OwnerRSVP); err != nil {
			return fmt.Errorf("write meeting RSVP: %w", err)
		}
	}
	for _, item := range record.Items {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%d\t%s\t%s\t%s\n", item.MessageID, item.SentAt.Format(time.RFC3339), item.Title, item.FollowUp); err != nil {
			return fmt.Errorf("write meeting item: %w", err)
		}
		if item.Content != nil {
			for _, entry := range item.Content.Meetings {
				if entry.Content.Summary.Text != "" {
					if _, err := fmt.Fprintln(cmd.OutOrStdout(), entry.Content.Summary.Text); err != nil {
						return fmt.Errorf("write meeting summary: %w", err)
					}
				}
			}
		}
	}
	return nil
}
