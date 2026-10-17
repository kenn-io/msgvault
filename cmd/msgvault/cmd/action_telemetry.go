package cmd

import (
	"context"
	"os"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	apiclient "go.kenn.io/msgvault/pkg/client/generated"
)

func reportsCLIAction(cmd *cobra.Command, terminal bool) bool {
	return terminal && !isAgentMode(invocationFromCommand(cmd)) && !isDaemonCLISubprocess()
}

var cliActionTerminal = func(fd uintptr) bool { return isatty.IsTerminal(fd) || isatty.IsCygwinTerminal(fd) }

func humanCLIAction(cmd *cobra.Command) bool {
	for _, stream := range []any{cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()} {
		if file, ok := stream.(*os.File); ok && cliActionTerminal(file.Fd()) {
			return reportsCLIAction(cmd, true)
		}
	}
	return false
}

func reportCLIAction(cmd *cobra.Command, client *daemonclient.Client, event, property, value string) {
	if !humanCLIAction(cmd) {
		return
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 250*time.Millisecond)
	defer cancel()
	_ = sendActionTelemetry(ctx, client, "cli", event, property, value)
}

func reportSuccessfulCLIAction(cmd *cobra.Command, client *daemonclient.Client, event, property, value string, result *error) {
	if *result == nil {
		reportCLIAction(cmd, client, event, property, value)
	}
}

func sendActionTelemetry(ctx context.Context, client *daemonclient.Client, surface, event, property, value string) error {
	generated, err := client.GeneratedClient()
	if err != nil {
		return err
	}
	properties := map[string]any{"surface": surface}
	if property != "" {
		properties[property] = value
	}
	_, err = generated.CaptureTelemetryEvent(ctx, &apiclient.CaptureTelemetryEventRequestOptions{
		Body: &apiclient.CaptureTelemetryEventBody{Event: event, Properties: properties},
	})
	return err
}
