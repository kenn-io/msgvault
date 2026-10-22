package cmd

import (
	"encoding/json/v2"
	"errors"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/zoomcalls"
)

func zoomTokenCommand(text string) ([]string, error) {
	var argv []string
	if err := json.Unmarshal([]byte(text), &argv); err != nil || len(argv) == 0 || argv[0] == "" {
		return nil, errors.New("zoom --token-command must be a nonempty JSON argv array")
	}
	return argv, nil
}

func addZoomFetchMode(cmd *cobra.Command, o *zoomOptions, open callOpener, write callWriter) {
	var tokenCommand string
	cmd.Flags().StringVar(&tokenCommand, "token-command", "", "JSON argv of a command that prints a Zoom access token; requires meeting:read:list_past_participants")
	cmd.Short = "Import Zoom attendance from a supplied roster or an exact past instance"
	cmd.Flags().Lookup("roster").Usage = "Complete participant API JSON file; omit to fetch from Zoom"
	supplied := cmd.RunE
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if o.roster != "" {
			if tokenCommand != "" {
				return errors.New("choose --roster or --token-command")
			}
			return supplied(cmd, args)
		}
		argv, err := zoomTokenCommand(tokenCommand)
		if err != nil {
			return err
		}
		bindings, err := o.bindingMap()
		if err != nil {
			return err
		}
		if o.uuid == "" {
			return errors.New("zoom --uuid is required")
		}
		client, cleanup, err := open(cmd.Context())
		if err != nil {
			return err
		}
		defer cleanup()
		raw, err := zoomcalls.NewClient(argv).Meeting(cmd.Context(), o.uuid)
		if err != nil {
			return err
		}
		observations, skipped, err := stageZoomMeetings(cmd.Context(), client, *o, []zoomcalls.MeetingRecord{{UUID: o.uuid, Roster: raw}}, bindings)
		if err != nil {
			return err
		}
		return postZoomObservations(cmd, client, observations, zoomCounts{Skipped: skipped}, write)
	}
}
