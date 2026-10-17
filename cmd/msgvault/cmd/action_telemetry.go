package cmd

import (
	"context"
	"errors"
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
	enabled := true
	if inv := invocationFromCommand(cmd); inv != nil && inv.cfg != nil {
		enabled = inv.cfg.Telemetry.EnabledOrDefault()
	}
	if !telemetry.Enabled(enabled) {
		return
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 250*time.Millisecond)
	defer cancel()
	_ = sendActionTelemetry(ctx, client, "cli", event, property, value)
}

func reportCLIActionResult(cmd *cobra.Command, client *daemonclient.Client, event, property, value string, result *error) {
	if *result == nil {
		reportCLIAction(cmd, client, event, property, value)
	} else if !errors.Is(*result, context.Canceled) && !errors.Is(*result, ErrSyncPaused) {
		kind := telemetry.FailureKind(*result)
		reportCLIAction(cmd, client, telemetry.EventErrorShown, "kind", kind)
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

func reportCLIEmptySearch(cmd *cobra.Command, client *daemonclient.Client, kind string, empty bool, result *error) {
	if empty && *result == nil {
		reportCLIAction(cmd, client, telemetry.EventSearchEmpty, "search_type", kind)
	}
}
