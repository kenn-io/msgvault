package cmd

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/meetingcontent"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func newMeetingSubtitlesCommand(deps meetingCommandDeps) *cobra.Command {
	var ids []int64
	var format, output string
	command := &cobra.Command{
		Use:     "subtitles",
		Short:   "Export one archived timed meeting as SRT or WebVTT",
		Long:    "Export complete retained cue timing and speaker labels through the daemon.\nThe full JSON meeting context must fit within 1 MiB; incomplete evidence fails.",
		Example: "  msgvault meetings subtitles --id 42 --format vtt -o meeting.vtt",
		Args:    cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if err := validateMeetingIDs("--id", ids, true); err != nil {
				return usageErr(command, err)
			}
			if len(ids) != 1 {
				return usageErr(command, errors.New("subtitles requires exactly one --id"))
			}
			format = strings.ToLower(strings.TrimSpace(format))
			if format != "srt" && format != "vtt" {
				return usageErr(command, errors.New("--format must be srt or vtt"))
			}
			client, cleanup, err := openMeetingCommandClient(command.Context(), deps)
			if err != nil {
				return err
			}
			defer cleanup()
			requestFormat, includeTranscript, maxBytes := generated.JSON, true, int64(1048576)
			result, err := client.GetMeetingContext(command.Context(), generated.GetMeetingContextBody{
				MessageIds: &ids, Format: &requestFormat, IncludeTranscript: &includeTranscript, MaxBytes: &maxBytes,
			})
			if err != nil {
				return meetingCommandError(err)
			}
			if result == nil {
				return errors.New("subtitle meeting context response was empty")
			}
			var packet meetingcontent.Packet
			if err := json.Unmarshal([]byte(result.Content), &packet); err != nil {
				return fmt.Errorf("decode subtitle meeting context: %w", err)
			}
			if result.SchemaVersion != 1 || packet.SchemaVersion != 1 || result.Format != meetingcontent.FormatJSON {
				return errors.New("unsupported subtitle meeting context schema or format")
			}
			if result.Truncated || packet.Truncated || len(result.OmittedMessageIDs) > 0 || len(packet.OmittedMessageIDs) > 0 || len(packet.Truncations) > 0 {
				return errors.New("subtitle export requires complete meeting context: evidence truncated or omitted by the 1 MiB size limit")
			}
			if len(packet.Meetings) != 1 || packet.Meetings[0].Meeting.MessageID != ids[0] || len(packet.RequestedMessageIDs) != 1 || packet.RequestedMessageIDs[0] != ids[0] {
				return errors.New("subtitle meeting context did not contain exactly the requested meeting")
			}
			content, err := meetingcontent.RenderSubtitles(packet.Meetings[0], format)
			if err != nil {
				return err
			}
			return writeMeetingOutput(command, output, content, "meeting subtitles")
		},
	}
	command.Flags().Int64SliceVar(&ids, "id", nil, "One meeting message ID from search (required)")
	command.Flags().StringVar(&format, "format", "srt", "Subtitle format: srt or vtt")
	command.Flags().StringVarP(&output, "output", "o", "", "Output file (default stdout, use - for stdout)")
	return command
}
