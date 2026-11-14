package cmd

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"slices"
	"strconv"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
)

const mcpPersonExportLimit = 512 << 10

// Reserve space for the JSON-RPC envelope around the encoded tool result.
const mcpPersonExportResponseMargin = 4 << 10

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
		return operationFailure("invalid_arguments", false), nil //nolint:nilerr // Invalid arguments are fixed refusals.
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
			if len(data) >= mcpPersonExportErrorTailLimit {
				stderr = append(stderr[:0], data[len(data)-mcpPersonExportErrorTailLimit:]...)
				return nil
			}
			if excess := len(stderr) + len(data) - mcpPersonExportErrorTailLimit; excess > 0 {
				stderr = stderr[excess:]
			}
			stderr = append(stderr, data...)
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
			if code := personExportRefusalCode(stderr, input); code != "" {
				return operationFailure(code, false), nil
			}
		}
		return operationFailure("export_failed", false), nil
	}
	personExport := mcpserver.PersonExport{JSONL: output.String()}
	structured, err := json.Marshal(
		personExport,
		json.Deterministic(true),
		jsontext.EscapeForHTML(true),
		jsontext.EscapeForJS(true),
	)
	if err != nil {
		return operationFailure("export_failed", false), nil //nolint:nilerr // Encoding failures are fixed refusals.
	}
	encoded, err := (&sdkmcp.CallToolResult{
		Content:           []sdkmcp.Content{&sdkmcp.TextContent{Text: string(structured)}},
		StructuredContent: jsontext.Value(structured),
	}).MarshalJSON()
	if err != nil {
		return operationFailure("export_failed", false), nil //nolint:nilerr // Encoding failures are fixed refusals.
	}
	if len(encoded) > mcpPersonExportLimit-mcpPersonExportResponseMargin {
		return operationFailure("export_limit_exceeded", false), nil
	}
	return &mcpserver.OperationResult{Output: personExport}, nil
}

func personExportRefusalCode(stderr []byte, input mcpPersonExportArgs) string {
	diagnostic := strings.TrimRight(string(stderr), "\r\n")
	for i, source := range input.Sources {
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
	case strings.HasPrefix(line, "Error: resolve --person-id: resolve durable person "+strconv.FormatInt(input.PersonID, 10)+": ") &&
		strings.HasSuffix(line, ": person not found"):
		return "unknown_person"
	}
	for i, source := range input.Sources {
		if strings.HasPrefix(line, "Error: invalid --source "+strconv.Quote(source)+" ") {
			return "invalid_source_" + strconv.Itoa(i+1)
		}
	}
	return ""
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
