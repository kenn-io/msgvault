package cmd

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/store"
)

const meetingLinksMinAPISchemaVersion = "5.1.0"

func newMeetingLinksCommand(deps meetingCommandDeps) *cobra.Command {
	var jsonOutput bool
	command := &cobra.Command{Use: "links <note-id|ref>", Short: "Show linked calendar events and notes", Long: "Show linked calendar events and notes. Takes a note ID, msg:<id>, or evt:<id>[@occurrence key].", Args: cobra.ExactArgs(1)}
	command.Flags().BoolVar(&jsonOutput, "json", false, "print the meeting record as JSON")
	command.RunE = func(cmd *cobra.Command, args []string) error {
		ref, err := meetingLinksRef(args[0])
		if err != nil {
			return usageErr(cmd, err)
		}
		client, cleanup, err := openMeetingCommandClient(cmd.Context(), deps, meetingLinksMinAPISchemaVersion)
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
		if err := writeMeetingRecord(cmd.OutOrStdout(), record); err != nil {
			return err
		}
		return writeCalendarExpansionWarnings(cmd.ErrOrStderr(), record.Unexpandable)
	}
	return command
}

// A bare number names a note; msg: and evt: refs pass through for the daemon to validate.
func meetingLinksRef(arg string) (string, error) {
	if strings.Contains(arg, ":") {
		return arg, nil
	}
	id, err := strconv.ParseInt(arg, 10, 64)
	if err != nil || id <= 0 || id > apiprotocol.MaxJSONSafeInteger {
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
		if _, err := fmt.Fprintln(writer, "EVENT\tSTART\tEND\tSTATUS\tTITLE"); err != nil {
			return fmt.Errorf("write meeting links: %w", err)
		}
		for _, link := range record.Links {
			status := calendarOccurrenceStatus(store.CalendarOccurrence{Status: link.Status, DeletedFromSource: link.DeletedFromSource})
			if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n", link.Ref, link.Start.Format(time.RFC3339), link.End.Format(time.RFC3339), status, normalizeSearchTableText(link.Subject)); err != nil {
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
