package cmd

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/textutil"
	"go.kenn.io/msgvault/internal/timeline"
)

const timelineMinAPISchemaVersion = "3.47.0"

type timelineClient interface {
	APISchemaVersion(ctx context.Context) (string, error)
	Timeline(ctx context.Context, req timeline.Request) (*timeline.Timeline, string, error)
}

var timelineOpen = func(ctx context.Context) (timelineClient, func(), error) {
	client, _, err := OpenHTTPStore(ctx)
	if err != nil {
		return nil, nil, err
	}
	return client, func() { _ = client.Close() }, nil
}

func init() {
	rootCmd.AddCommand(newTimelineCommand())
}

func newTimelineCommand() *cobra.Command {
	var req timeline.Request
	var jsonOutput bool
	command := &cobra.Command{
		Use:     "timeline",
		Short:   "Read activity by day, person, or anchor",
		Long:    "Read a bounded timeline of archived messages, meetings, calls, and notes.\nUse --json for each entry's detail tool and arguments.",
		Args:    cobra.NoArgs,
		Example: "  msgvault timeline\n  msgvault timeline --with person:7\n  msgvault timeline --around msg:42 --before 5 --after 5\n  msgvault timeline --brief --json",
	}
	flags := command.Flags()
	flags.StringVar(&req.Date, "date", "", "One local date (YYYY-MM-DD)")
	flags.StringVar(&req.From, "from", "", "First local date, inclusive; requires --to")
	flags.StringVar(&req.To, "to", "", "Last local date, inclusive; requires --from")
	flags.IntVar(&req.Days, "days", 0, "Days ending today (1–31); defaults to two without other date bounds")
	flags.StringVar(&req.With, "with", "", "Person, participant, email, phone, or name reference")
	flags.StringVar(&req.Around, "around", "", "Message, conversation, event, burst, date, or datetime reference")
	flags.IntVar(&req.Before, "before", 5, "Anchor neighbors before the target (0–50)")
	flags.IntVar(&req.After, "after", 5, "Anchor neighbors after the target (0–50)")
	flags.BoolVar(&req.Conversation, "conversation", false, "Show individual messages in the anchor conversation")
	flags.IntSliceVar(&req.Tiers, "tier", nil, "Filter person tiers (1,2,3)")
	flags.BoolVar(&req.IncludeCold, "include-cold", false, "Include archived cold activity")
	flags.BoolVar(&req.IncludeNoise, "include-noise", false, "Include activity excluded as noise")
	flags.BoolVar(&req.Brief, "brief", false, "Use shorter sections")
	flags.IntVar(&req.Budget, "budget", 0, "Final output bytes (2048–65536); default 16384, or 6144 with --brief")
	flags.StringVar(&req.Cursor, "cursor", "", "Continue with the same filters and format")
	flags.BoolVar(&jsonOutput, flagJSON, false, "Output compact JSON instead of Markdown")
	command.RunE = func(cmd *cobra.Command, _ []string) error {
		if cmd.Flags().Changed("days") && req.Days == 0 {
			return errors.New("--days must be between 1 and 31")
		}
		if cmd.Flags().Changed("budget") && req.Budget == 0 {
			return errors.New("--budget must be between 2048 and 65536")
		}
		req.BeforeSet = cmd.Flags().Changed("before")
		req.AfterSet = cmd.Flags().Changed("after")
		client, closeClient, err := timelineOpen(cmd.Context())
		if err != nil {
			return err
		}
		defer closeClient()
		version, err := client.APISchemaVersion(cmd.Context())
		if err != nil {
			return fmt.Errorf("check daemon timeline capability: %w", err)
		}
		if !daemonclient.APISchemaVersionAtLeast(version, timelineMinAPISchemaVersion) {
			return fmt.Errorf("timeline requires daemon API schema %s or newer (daemon reports %q); upgrade and restart the daemon", timelineMinAPISchemaVersion, version)
		}
		req.Format = "markdown"
		if jsonOutput {
			req.Format = "json"
		}
		result, markdown, err := client.Timeline(cmd.Context(), req)
		if err != nil {
			if ambiguous, ok := errors.AsType[*timeline.ErrAmbiguousReference](err); ok {
				for _, candidate := range ambiguous.Candidates {
					var ref string
					if candidate.PersonID > 0 {
						ref = fmt.Sprintf("person:%d", candidate.PersonID)
					} else if len(candidate.ParticipantIDs) > 0 {
						ref = fmt.Sprintf("participant:%d", candidate.ParticipantIDs[0])
					} else {
						continue
					}
					if _, writeErr := fmt.Fprintf(cmd.ErrOrStderr(), "%s\t%s\t%s\n", ref, textutil.SanitizeTerminal(candidate.DisplayName), textutil.SanitizeTerminal(candidate.Address)); writeErr != nil {
						return fmt.Errorf("write timeline candidates: %w", writeErr)
					}
				}
			}
			return err
		}
		if !jsonOutput {
			_, err = io.WriteString(cmd.OutOrStdout(), markdown)
			return err
		}
		data, err := json.Marshal(result, json.Deterministic(true), json.FormatNilSliceAsNull(false), jsontext.AllowInvalidUTF8(true))
		if err != nil {
			return fmt.Errorf("encode timeline: %w", err)
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), string(data))
		if err != nil {
			return fmt.Errorf("write timeline: %w", err)
		}
		return nil
	}
	return command
}
