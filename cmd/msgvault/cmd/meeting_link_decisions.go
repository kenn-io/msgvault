package cmd

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/store"
)

const meetingDecisionsMinAPISchemaVersion = "5.2.0"

type meetingDecisionClientBackend interface {
	DecideMeetingLink(ctx context.Context, left, right, decision string) (store.MeetingLinkDecisionResult, error)
}

func newMeetingDecisionCommand(deps meetingCommandDeps, decision string) *cobra.Command {
	name := decision
	if decision == "clear" {
		name = "reset-link"
	}
	return &cobra.Command{
		Use:   name + " <ref> <ref>",
		Short: name + " two archived meeting copies",
		Long:  "Save a meeting choice or clear it to restore automatic matching.",
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
			result, err := backend.DecideMeetingLink(cmd.Context(), left, right, decision)
			if err != nil {
				return err
			}
			label := "Saved " + result.State + ":"
			if result.State == "automatic" {
				label = "No saved choice:"
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s %s %s\n", label, result.Left, result.Right)
			if err != nil {
				return fmt.Errorf("write meeting choice: %w", err)
			}
			return nil
		},
	}
}
