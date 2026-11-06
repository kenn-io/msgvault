package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/pkg/client/generated"
)

const meetingLinksMinAPISchemaVersion = "3.47.0"

type meetingLinkClient interface {
	MeetingRecord(ctx context.Context, ref string) (store.MeetingRecord, error)
	DecideMeetingLink(ctx context.Context, messageID int64, event, decision string) (generated.MeetingLinkDecisionResponse, error)
	SuggestMeetingLinks(ctx context.Context, ref string) (store.MeetingSuggestionPage, error)
	APISchemaVersion(ctx context.Context) (string, error)
}

type meetingLinkOpen func(context.Context) (meetingLinkClient, func(), error)

func openMeetingLinks(ctx context.Context) (meetingLinkClient, func(), error) {
	client, _, err := OpenHTTPStore(ctx)
	if err != nil {
		return nil, func() {}, fmt.Errorf("open daemon: %w", err)
	}
	return client, func() { _ = client.Close() }, nil
}

func openMeetingLinkClient(ctx context.Context, open meetingLinkOpen) (meetingLinkClient, func(), error) {
	client, cleanup, err := open(ctx)
	if err != nil {
		return nil, func() {}, err
	}
	version, err := client.APISchemaVersion(ctx)
	if err == nil && !daemonclient.APISchemaVersionAtLeast(version, meetingLinksMinAPISchemaVersion) {
		err = fmt.Errorf("meeting links require daemon API schema %s or newer (daemon reports %q); upgrade the daemon", meetingLinksMinAPISchemaVersion, version)
	}
	if err != nil {
		cleanup()
		return nil, func() {}, err
	}
	return client, cleanup, nil
}

func newMeetingLinkDecisionCommand(decision string, open meetingLinkOpen) *cobra.Command {
	var event string
	command := &cobra.Command{Use: decision + " <message-id> --event <ref>", Short: decision + " a meeting note and calendar occurrence", Args: cobra.ExactArgs(1)}
	command.Flags().StringVar(&event, "event", "", "event ref from meetings suggest or calendar events --json")
	_ = command.MarkFlagRequired("event")
	command.RunE = func(cmd *cobra.Command, args []string) error {
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil || id <= 0 || id > 9007199254740991 {
			return usageErr(cmd, errors.New("message-id must be a positive message ID"))
		}
		if event == "" {
			return usageErr(cmd, errors.New("--event is required"))
		}
		client, cleanup, err := openMeetingLinkClient(cmd.Context(), open)
		if err != nil {
			return err
		}
		defer cleanup()
		return decideMeetingLink(cmd, client, id, event, decision)
	}
	return command
}

func decideMeetingLink(cmd *cobra.Command, client meetingLinkClient, id int64, event, decision string) error {
	response, err := client.DecideMeetingLink(cmd.Context(), id, event, decision)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Cache state: %s\n", response.CacheState); err != nil {
		return fmt.Errorf("write meeting link decision: %w", err)
	}
	return nil
}

func newMeetingSuggestCommand(open meetingLinkOpen) *cobra.Command {
	var jsonOutput bool
	command := &cobra.Command{Use: "suggest <ref>", Short: "Suggest links for msg:<id> or evt:<id>[@occurrence key]", Args: cobra.ExactArgs(1)}
	command.Flags().BoolVar(&jsonOutput, "json", false, "print suggestions as JSON")
	command.RunE = func(cmd *cobra.Command, args []string) error {
		client, cleanup, err := openMeetingLinkClient(cmd.Context(), open)
		if err != nil {
			return err
		}
		defer cleanup()
		suggestions, err := client.SuggestMeetingLinks(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		if jsonOutput {
			return json.MarshalWrite(cmd.OutOrStdout(), suggestions)
		}
		if len(suggestions.Unexpandable) > 0 {
			if _, err := fmt.Fprintln(cmd.ErrOrStderr(), "Warning: some calendar entries could not be fully expanded; suggestions may omit occurrences:"); err != nil {
				return fmt.Errorf("write meeting suggestion warning: %w", err)
			}
			for _, entry := range suggestions.Unexpandable {
				if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "  event %d: %s\n", entry.MessageID, entry.Reason); err != nil {
					return fmt.Errorf("write meeting suggestion warning: %w", err)
				}
			}
		}
		writer := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		if _, err := fmt.Fprintln(writer, "EVENT\tNOTE\tOVERLAP (MIN)\tSHARED ATTENDEES\tTITLE WORDS"); err != nil {
			return fmt.Errorf("write meeting suggestions: %w", err)
		}
		for _, suggestion := range suggestions.Suggestions {
			if _, err := fmt.Fprintf(writer, "%s\t%d\t%d\t%d\t%d\n", suggestion.Ref, suggestion.MemberMessageID, suggestion.OverlapMinutes, suggestion.SharedAttendees, suggestion.TitleTokens); err != nil {
				return fmt.Errorf("write meeting suggestions: %w", err)
			}
		}
		return writer.Flush()
	}
	return command
}

func newMeetingLinksCommand(open meetingLinkOpen) *cobra.Command {
	var jsonOutput bool
	command := &cobra.Command{Use: "links <note-id|ref>", Short: "Show linked calendar events and notes", Long: "Show linked calendar events and notes. Takes a note ID, msg:<id>, or evt:<id>[@occurrence key].", Args: cobra.ExactArgs(1)}
	command.Flags().BoolVar(&jsonOutput, "json", false, "print the meeting record as JSON")
	command.RunE = func(cmd *cobra.Command, args []string) error {
		ref, err := meetingLinksRef(args[0])
		if err != nil {
			return usageErr(cmd, err)
		}
		client, cleanup, err := openMeetingLinkClient(cmd.Context(), open)
		if err != nil {
			return err
		}
		defer cleanup()
		record, err := client.MeetingRecord(cmd.Context(), ref)
		if err != nil {
			return err
		}
		if jsonOutput {
			return json.MarshalWrite(cmd.OutOrStdout(), record)
		}
		return writeMeetingRecord(cmd.OutOrStdout(), record)
	}
	return command
}

// A bare number names a note; msg: and evt: refs pass through for the daemon to validate.
func meetingLinksRef(arg string) (string, error) {
	if strings.Contains(arg, ":") {
		return arg, nil
	}
	id, err := strconv.ParseInt(arg, 10, 64)
	if err != nil || id <= 0 || id > 9007199254740991 {
		return "", errors.New("note-id must be a positive message ID or a msg:/evt: reference")
	}
	return fmt.Sprintf("msg:%d", id), nil
}

func writeMeetingRecord(out io.Writer, record store.MeetingRecord) error {
	if len(record.Links) == 0 {
		if _, err := fmt.Fprintln(out, "No linked calendar events"); err != nil {
			return fmt.Errorf("write meeting links: %w", err)
		}
	} else {
		writer := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		if _, err := fmt.Fprintln(writer, "EVENT\tSTART\tEND\tTITLE"); err != nil {
			return fmt.Errorf("write meeting links: %w", err)
		}
		for _, link := range record.Links {
			if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n", link.Ref, link.Start.Format(time.RFC3339), link.End.Format(time.RFC3339), normalizeSearchTableText(link.Subject)); err != nil {
				return fmt.Errorf("write meeting links: %w", err)
			}
		}
		if err := writer.Flush(); err != nil {
			return fmt.Errorf("write meeting links: %w", err)
		}
	}
	notes := make([]string, len(record.MessageIDs))
	for i, id := range record.MessageIDs {
		notes[i] = strconv.FormatInt(id, 10)
	}
	if _, err := fmt.Fprintf(out, "Notes: %s\n", strings.Join(notes, ", ")); err != nil {
		return fmt.Errorf("write meeting links: %w", err)
	}
	return nil
}
