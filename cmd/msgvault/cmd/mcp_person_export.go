package cmd

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
)

const mcpPersonExportLimit = 512 << 10

type mcpPersonExportArgs struct {
	PersonID     int64    `json:"person_id"`
	Start        string   `json:"start"`
	End          string   `json:"end"`
	Sources      []string `json:"sources,omitempty"`
	MessageTypes []string `json:"message_types,omitempty"`
}

func (b *daemonMCPOperations) exportPersonMessages(ctx context.Context, args map[string]any) (*mcpserver.OperationResult, error) {
	refuse := func(code string) (*mcpserver.OperationResult, error) {
		return operationFailure(code, false), nil
	}
	input, err := decodeMCPOperationArguments[mcpPersonExportArgs](args)
	if err != nil {
		return refuse("invalid_arguments")
	}
	command := newExportMessagesCmd(defaultExportMessagesDeps())
	flags := command.Flags()
	for name, value := range map[string]string{"person-id": strconv.FormatInt(input.PersonID, 10), "start": input.Start, "end": input.End, "format": "jsonl"} {
		if err := flags.Set(name, value); err != nil {
			return refuse("invalid_arguments")
		}
	}
	for _, value := range input.Sources {
		if err := flags.Set("source", value); err != nil {
			return refuse("invalid_arguments")
		}
	}
	for _, value := range input.MessageTypes {
		if err := flags.Set("message-type", value); err != nil {
			return refuse("invalid_arguments")
		}
	}
	argv, err := daemonCLIArgsFromCobra(command, nil)
	if err != nil {
		return refuse("invalid_arguments")
	}
	var output strings.Builder
	overflow := false
	err = b.client.RunCLICommand(ctx, daemonclient.CLIRunRequest{Args: argv}, func(stream, data string) error {
		if stream != cliStreamStdout {
			return nil
		}
		if len(data) > mcpPersonExportLimit-output.Len() {
			overflow = true
			return errors.New("export limit exceeded")
		}
		output.WriteString(data)
		return nil
	})
	if overflow {
		return operationFailure("export_limit_exceeded", false), nil
	}
	if local, ok := errors.AsType[*daemonclient.LocalDaemonUnavailableError](err); ok {
		return operationFailure(local.APIErrorCode(), false), nil
	}
	if err != nil || ctx.Err() != nil {
		return refuse("export_failed")
	}
	return &mcpserver.OperationResult{Output: mcpserver.PersonExport{JSONL: output.String()}}, nil
}
