package cmd

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"errors"
	"slices"
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
	input, err := decodeMCPOperationArguments[mcpPersonExportArgs](args)
	start, end, boundsErr := parseMessageExportBounds(input.Start, input.End)
	types, typesErr := normalizeMessageExportTypes(input.MessageTypes)
	sources, sourcesErr := normalizeMessageExportSelectors(input.Sources)
	if err != nil || input.PersonID <= 0 || boundsErr != nil || typesErr != nil || sourcesErr != nil {
		return operationFailure("invalid_arguments", false), nil
	}
	command := newExportMessagesCmd(defaultExportMessagesDeps())
	flags := command.Flags()
	for name, value := range map[string]string{"person-id": strconv.FormatInt(input.PersonID, 10), "start": input.Start, "end": input.End, "format": "jsonl"} {
		if flags.Set(name, value) != nil {
			return operationFailure("invalid_arguments", false), nil
		}
	}
	for _, value := range input.Sources {
		if flags.Set("source", value) != nil {
			return operationFailure("invalid_arguments", false), nil
		}
	}
	for _, value := range input.MessageTypes {
		if flags.Set("message-type", value) != nil {
			return operationFailure("invalid_arguments", false), nil
		}
	}
	argv, err := daemonCLIArgsFromCobra(command, nil)
	if err != nil {
		return operationFailure("invalid_arguments", false), nil
	}
	var output strings.Builder
	overflow := false
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	err = b.client.RunCLICommand(runCtx, daemonclient.CLIRunRequest{Args: argv}, func(stream, data string) error {
		if stream != cliStreamStdout {
			return nil
		}
		if len(data) > mcpPersonExportLimit-output.Len() {
			overflow = true
			cancel()
			return errors.New("export limit exceeded")
		}
		output.WriteString(data)
		return nil
	})
	if overflow {
		return operationFailure("export_limit_exceeded", false), nil
	}
	if err != nil || ctx.Err() != nil {
		return operationFailure("export_failed", false), nil
	}
	expected := exportMessagesManifest{Schema: messageExportSchema, Window: exportMessagesWindow{Start: start, End: end}, Filters: exportMessagesFilters{PersonID: &input.PersonID, MessageTypes: types, Sources: sources}}
	if !validPersonExport(output.String(), expected) {
		return operationFailure("invalid_export", false), nil
	}
	return &mcpserver.OperationResult{Output: mcpserver.PersonExport{JSONL: output.String()}}, nil
}

// Validate the envelope before disclosing any of the streamed records.
func validPersonExport(data string, expected exportMessagesManifest) bool {
	if !strings.HasSuffix(data, "\n") {
		return false
	}
	scanner := bufio.NewScanner(strings.NewReader(data))
	scanner.Buffer(make([]byte, 4096), mcpPersonExportLimit)
	count := exportMessagesCounts{}
	index, phase := 0, 0
	complete := false
	for scanner.Scan() {
		line := scanner.Bytes()
		var header struct {
			RecordType string `json:"record_type"`
		}
		if json.Unmarshal(line, &header) != nil || complete {
			return false
		}
		if index == 0 {
			var manifest exportMessagesManifest
			if header.RecordType != "manifest" || json.Unmarshal(line, &manifest) != nil || manifest.Schema != expected.Schema || !manifest.Window.Start.Equal(expected.Window.Start) || !manifest.Window.End.Equal(expected.Window.End) || manifest.Filters.PersonID == nil || *manifest.Filters.PersonID != *expected.Filters.PersonID || !slices.Equal(manifest.Filters.MessageTypes, expected.Filters.MessageTypes) || !slices.Equal(manifest.Filters.Sources, expected.Filters.Sources) {
				return false
			}
		} else {
			switch header.RecordType {
			case "source":
				var record exportMessagesSourceRecord
				if json.Unmarshal(line, &record) != nil || record.SourceType == "" || record.Identifier == "" {
					return false
				}
				if phase > 0 {
					return false
				}
				count.Sources++
			case "conversation":
				var record exportMessagesConversationRecord
				if json.Unmarshal(line, &record) != nil || record.SourceType == "" || record.SourceIdentifier == "" || record.ID == "" {
					return false
				}
				if phase > 1 {
					return false
				}
				phase = 1
				count.Conversations++
			case "message":
				var record exportMessagesMessageRecord
				if json.Unmarshal(line, &record) != nil || record.SourceType == "" || record.SourceIdentifier == "" || record.ID == "" || record.OccurredAt.IsZero() {
					return false
				}
				phase = 2
				count.Messages++
			case "complete":
				var receipt struct {
					Counts *struct {
						Sources       *int `json:"sources"`
						Conversations *int `json:"conversations"`
						Messages      *int `json:"messages"`
					} `json:"counts"`
				}
				if json.Unmarshal(line, &receipt) != nil || receipt.Counts == nil || receipt.Counts.Sources == nil || receipt.Counts.Conversations == nil || receipt.Counts.Messages == nil || *receipt.Counts.Sources != count.Sources || *receipt.Counts.Conversations != count.Conversations || *receipt.Counts.Messages != count.Messages {
					return false
				}
				complete = true
			default:
				return false
			}
		}
		index++
	}
	return scanner.Err() == nil && complete
}
