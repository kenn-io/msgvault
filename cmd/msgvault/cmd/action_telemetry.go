package cmd

import (
	"context"
	"os"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/telemetry"
	apiclient "go.kenn.io/msgvault/pkg/client/generated"
)

var cliActionTerminal = func(fd uintptr) bool { return isatty.IsTerminal(fd) || isatty.IsCygwinTerminal(fd) }

func humanCLIAction(cmd *cobra.Command) bool {
	terminal := false
	for _, stream := range []any{cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()} {
		if file, ok := stream.(*os.File); ok && cliActionTerminal(file.Fd()) {
			terminal = true
			break
		}
	}
	return terminal && !isAgentMode(invocationFromCommand(cmd)) && !isDaemonCLISubprocess() && os.Getenv("CODEX_THREAD_ID") == "" && os.Getenv("CLAUDECODE") == ""
}

func reportCLIAction(cmd *cobra.Command, client *daemonclient.Client, event, property, value string) {
	if !humanCLIAction(cmd) {
		return
	}
	inv := invocationFromCommand(cmd)
	if inv == nil {
		return
	}
	enabled := true
	if inv.cfg != nil {
		enabled = inv.cfg.Telemetry.EnabledOrDefault()
	}
	if !telemetry.Enabled(enabled) {
		return
	}
	ctx := cmd.Context()
	inv.sends.Go(func() {
		ctx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
		defer cancel()
		_ = sendActionTelemetry(ctx, client, "cli", event, property, value)
	})
}

// reportCLISyncFinished counts a completed manual sync. The Muesli completion
// hook syncs automatically, so it never counts.
func reportCLISyncFinished(cmd *cobra.Command, client *daemonclient.Client) {
	if cmd.Name() != "muesli-hook" {
		reportCLIAction(cmd, client, "sync_finished", "", "")
	}
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
