package cmd

import (
	"context"
	"errors"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/store"
)

const meetingDecisionsMinAPISchemaVersion = "5.2.0"

type meetingDecisionClientBackend interface {
	DecideMeetingLink(context.Context, string, string, string) (store.MeetingLinkDecisionResult, error)
}

func newMeetingDecisionCommand(deps meetingCommandDeps, decision string) *cobra.Command {
	return &cobra.Command{
		Use:   decision + " <ref> <ref>",
		Short: decision + " two archived meeting copies",
		Long:  "Save a meeting choice. The opposite command clears a saved choice and restores automatic matching.",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			left, err := meetingLinksRef(args[0])
			if err != nil {
				return usageErr(cmd, err)
			}
			right, err := meetingLinksRef(args[1])
			if err != nil {
				return usageErr(cmd, err)
			}
			client, cleanup, err := openMeetingCommandClient(cmd.Context(), deps, meetingDecisionsMinAPISchemaVersion)
			if err != nil {
				return err
			}
			defer cleanup()
			backend, ok := client.(meetingDecisionClientBackend)
			if !ok {
				return errors.New("meeting decisions are unavailable")
			}
			_, err = backend.DecideMeetingLink(cmd.Context(), left, right, decision)
			return err
		},
	}
}
