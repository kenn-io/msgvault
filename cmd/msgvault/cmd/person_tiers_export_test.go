package cmd

import (
	"context"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
)

// PersonTiersCommandForTest binds the real command to a synthetic HTTP daemon.
func PersonTiersCommandForTest(ctx context.Context, url string) *cobra.Command {
	inv := newInvocation()
	inv.cfg = &config.Config{Remote: config.RemoteConfig{URL: url, AllowInsecure: true}}
	command := newPersonTiersCommand()
	ensureSilenceUsageWrapped(command)
	command.SetContext(withInvocation(ctx, inv))
	return command
}

// PersonManualGrantCommandForTest binds a manual grant command to a synthetic daemon.
func PersonManualGrantCommandForTest(ctx context.Context, url, action string) *cobra.Command {
	inv := newInvocation()
	inv.cfg = &config.Config{Remote: config.RemoteConfig{URL: url, AllowInsecure: true}}
	var command *cobra.Command
	switch action {
	case "track", "untrack":
		command = newPersonTrackingCommand(action, action == "track")
		personJSON = false
		command.Flags().BoolVar(&personJSON, flagJSON, false, "Output as JSON")
	default:
		command = newPersonBriefEnrollmentCommand(action, action == "enroll")
	}
	command.SetContext(withInvocation(ctx, inv))
	return command
}

// PersonTierMCPOptionsForTest builds the real daemon capability configuration.
func PersonTierMCPOptionsForTest(ctx context.Context, client *daemonclient.Client) mcpserver.ServeOptions {
	return daemonMCPServeOptions(ctx, client, newInvocation())
}

// PersonRootCommandForTest returns the registered person command tree.
func PersonRootCommandForTest() *cobra.Command { return personCmd }
