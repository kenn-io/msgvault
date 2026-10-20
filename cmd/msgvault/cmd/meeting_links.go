package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/store"
)

const meetingLinksMinAPISchemaVersion = "3.26.0"

type meetingLinkClient interface {
	MeetingLinks(ctx context.Context, messageID int64) ([]store.MeetingLink, error)
	DecideMeetingLink(ctx context.Context, messageID int64, event, decision string) error
	SuggestMeetingLinks(ctx context.Context, ref string) ([]store.MeetingSuggestion, error)
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
		return client.DecideMeetingLink(cmd.Context(), id, event, decision)
	}
	return command
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
		writer := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		if _, err := fmt.Fprintln(writer, "EVENT\tNOTE\tOVERLAP (MIN)\tSHARED ATTENDEES\tTITLE TOKENS"); err != nil {
			return fmt.Errorf("write meeting suggestions: %w", err)
		}
		for _, suggestion := range suggestions {
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
	command := &cobra.Command{Use: "links <note-id>", Short: "Show a note's linked calendar occurrences", Args: cobra.ExactArgs(1)}
	command.Flags().BoolVar(&jsonOutput, "json", false, "print linked events as JSON")
	command.RunE = func(cmd *cobra.Command, args []string) error {
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil || id <= 0 || id > 9007199254740991 {
			return usageErr(cmd, errors.New("note-id must be a positive message ID"))
		}
		client, cleanup, err := openMeetingLinkClient(cmd.Context(), open)
		if err != nil {
			return err
		}
		defer cleanup()
		links, err := client.MeetingLinks(cmd.Context(), id)
		if err != nil {
			return err
		}
		if jsonOutput {
			return json.MarshalWrite(cmd.OutOrStdout(), links)
		}
		if len(links) == 0 {
			if _, err := fmt.Fprintln(cmd.OutOrStdout(), "No linked calendar events"); err != nil {
				return fmt.Errorf("write meeting links: %w", err)
			}
			return nil
		}
		writer := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		if _, err := fmt.Fprintln(writer, "EVENT\tSTART\tEND\tTITLE"); err != nil {
			return fmt.Errorf("write meeting links: %w", err)
		}
		for _, link := range links {
			if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n", link.Ref, link.Start.Format(time.RFC3339), link.End.Format(time.RFC3339), normalizeSearchTableText(link.Subject)); err != nil {
				return fmt.Errorf("write meeting links: %w", err)
			}
		}
		return writer.Flush()
	}
	return command
}
