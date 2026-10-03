package cmd

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
)

var mcpDraftCommandConstructors = map[string]func() *cobra.Command{
	api.CLIRunDraftReplyCommand:   newDraftReplyCommand,
	api.CLIRunDraftComposeCommand: newDraftComposeCommand,
	api.CLIRunDraftForwardCommand: newDraftForwardCommand,
	api.CLIRunDraftGetCommand:     newDraftGetCommand,
	api.CLIRunDraftEditCommand:    newDraftEditCommand,
	api.CLIRunDraftDeleteCommand:  newDraftDeleteCommand,
	api.CLIRunDraftRecoverCommand: newDraftRecoverCommand,
	api.CLIRunDraftSendAsCommand:  newDraftSendAsCommand,
}

func mcpDraftCommands(delegated bool) []string {
	commands := slices.Sorted(maps.Keys(mcpDraftCommandConstructors))
	if delegated {
		commands = slices.DeleteFunc(commands, func(command string) bool { return !agentDelegatedCommand(command) })
	}
	return commands
}

type daemonMCPDraftRunner struct{ client *daemonclient.Client }

func (r daemonMCPDraftRunner) RunDraftCommand(ctx context.Context, request mcpserver.DraftCommandRequest) (mcpserver.DraftCommandResult, error) {
	var result mcpserver.DraftCommandResult
	constructor, ok := mcpDraftCommandConstructors[request.Command]
	if !ok {
		return result, fmt.Errorf("unknown draft command %q", request.Command)
	}
	if strings.HasPrefix(request.Positional, "-") {
		return result, &mcpserver.DraftCommandError{Message: "invalid_args: positional value must not begin with \"-\""}
	}
	command := constructor()
	for _, name := range slices.Sorted(maps.Keys(request.Flags)) {
		for _, value := range request.Flags[name] {
			if err := command.Flags().Set(name, value); err != nil {
				return result, &mcpserver.DraftCommandError{Message: err.Error()}
			}
		}
	}
	if err := command.Flags().Set("json", "true"); err != nil {
		return result, fmt.Errorf("set draft JSON output: %w", err)
	}
	var positional []string
	if request.Positional != "" {
		positional = []string{request.Positional}
	}
	if err := command.ValidateArgs(positional); err != nil {
		return result, &mcpserver.DraftCommandError{Message: err.Error()}
	}
	if err := command.ValidateRequiredFlags(); err != nil {
		return result, &mcpserver.DraftCommandError{Message: err.Error()}
	}
	args, err := daemonCLIArgsFromCobra(command, positional)
	if err != nil {
		return result, err
	}
	var stdout, stderr strings.Builder
	err = r.client.RunCLICommand(ctx, daemonclient.CLIRunRequest{Args: args}, func(stream, data string) error {
		switch stream {
		case cliStreamStdout:
			stdout.WriteString(data)
		case cliStreamStderr:
			stderr.WriteString(data)
		}
		return nil
	})
	result = mcpserver.DraftCommandResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if _, reported := errors.AsType[*daemonclient.CLIRunError](err); reported {
		return result, &mcpserver.DraftCommandError{Message: err.Error(), Stderr: result.Stderr}
	}
	return result, err
}
