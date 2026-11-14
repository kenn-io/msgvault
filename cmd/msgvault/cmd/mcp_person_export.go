package cmd

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"

	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
)

const mcpPersonExportLimit = 512 << 10
const mcpPersonExportErrorTailLimit = 64 << 10

type mcpPersonExportArgs struct {
	PersonID     int64    `json:"person_id"`
	Start        string   `json:"start"`
	End          string   `json:"end"`
	Sources      []string `json:"sources,omitempty"`
	MessageTypes []string `json:"message_types,omitempty"`
}

func (b *daemonMCPOperations) exportPersonMessages(ctx context.Context, args map[string]any) (*mcpserver.OperationResult, error) {
	input, err := decodeMCPOperationArguments[mcpPersonExportArgs](args)
	if err != nil {
		return operationFailure("invalid_arguments", false), nil
	}
	argv := []string{
		"export-messages",
		"--person-id=" + strconv.FormatInt(input.PersonID, 10),
		"--start=" + input.Start,
		"--end=" + input.End,
	}
	for _, source := range input.Sources {
		argv = append(argv, "--source="+source)
	}
	for _, messageType := range input.MessageTypes {
		argv = append(argv, "--message-type="+messageType)
	}
	var output strings.Builder
	var stderr []byte
	overflow := false
	err = b.client.RunCLICommand(ctx, daemonclient.CLIRunRequest{Args: argv}, func(stream, data string) error {
		if stream == cliStreamStderr {
			stderr = append(stderr, data...)
			if len(stderr) > mcpPersonExportErrorTailLimit {
				stderr = stderr[len(stderr)-mcpPersonExportErrorTailLimit:]
			}
			return nil
		}
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
	if err != nil {
		if _, cliRunError := errors.AsType[*daemonclient.CLIRunError](err); cliRunError {
			if code := personExportRefusalCode(stderr, input.Sources); code != "" {
				return operationFailure(code, false), nil
			}
		}
		return operationFailure("export_failed", false), nil
	}
	return &mcpserver.OperationResult{Output: mcpserver.PersonExport{JSONL: output.String()}}, nil
}

func lastMCPPersonExportErrorLine(stderr []byte) string {
	lines := strings.Split(string(stderr), "\n")
	for i := range slices.Backward(lines) {
		line := strings.TrimSuffix(lines[i], "\r")
		if strings.HasPrefix(line, "Error: ") {
			return line
		}
	}
	return ""
}

func personExportRefusalCode(stderr []byte, sources []string) string {
	diagnostic := strings.TrimRight(string(stderr), "\r\n")
	for i, source := range sources {
		if strings.HasSuffix(diagnostic, "Error: source "+source+" not found") {
			return "unknown_source_" + strconv.Itoa(i+1)
		}
	}
	line := lastMCPPersonExportErrorLine(stderr)
	switch {
	case strings.HasPrefix(line, "Error: invalid --start "):
		return "invalid_start"
	case strings.HasPrefix(line, "Error: invalid --end "):
		return "invalid_end"
	case line == "Error: --start must be before --end":
		return "invalid_window"
	case line == "Error: --message-type must be non-empty":
		return "invalid_message_type"
	case strings.HasPrefix(line, "Error: resolve --person-id: resolve durable person ") &&
		strings.HasSuffix(line, ": person not found"):
		return "unknown_person"
	}
	for i, source := range sources {
		if strings.HasPrefix(line, "Error: invalid --source "+strconv.Quote(source)+" ") {
			return "invalid_source_" + strconv.Itoa(i+1)
		}
	}
	return ""
}
