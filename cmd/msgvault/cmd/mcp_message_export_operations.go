package cmd

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/apiprotocol"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/store"
)

const (
	mcpPersonExportTool    = "export_person_messages"
	mcpPersonExportLimit   = 512 << 10
	mcpPersonExportInvalid = "invalid_message_export"
	mcpPersonExportSource  = "source"
)

type mcpPersonExportInput struct {
	PersonID     int64                          `json:"person_id"`
	Start        string                         `json:"start"`
	End          string                         `json:"end"`
	MessageTypes []string                       `json:"message_types"`
	Sources      []exportMessagesSourceSelector `json:"sources"`
}

func messageExportMCPCapabilities(commands []apiprotocol.MCPCommandDescriptor) []string {
	for _, command := range commands {
		if command.Name != "export-messages" || command.Delegated {
			continue
		}
		required := []string{"person-id", "start", "end", "format", "message-type", mcpPersonExportSource}
		if allMCPExportFlags(command.Flags, required) {
			return []string{mcpPersonExportTool}
		}
	}
	return nil
}

func allMCPExportFlags(flags, required []string) bool {
	for _, flag := range required {
		if !slices.Contains(flags, flag) {
			return false
		}
	}
	return true
}

func mcpPersonExportArguments(args map[string]any) (mcpPersonExportInput, []string, error) {
	input, err := decodeMCPOperationArguments[mcpPersonExportInput](args)
	if err != nil {
		return input, nil, err
	}
	for _, value := range args {
		if value == nil {
			return input, nil, errors.New("null export argument")
		}
	}
	if !mcpPositiveSafeID(input.PersonID) {
		return input, nil, errors.New("invalid export person")
	}
	start, end, err := parseMessageExportBounds(input.Start, input.End)
	if err != nil {
		return input, nil, err
	}
	input.Start, input.End = start.Format(time.RFC3339Nano), end.Format(time.RFC3339Nano)
	input.MessageTypes, err = normalizeMessageExportTypes(input.MessageTypes)
	if err != nil {
		return input, nil, err
	}
	selectors := make([]string, 0, len(input.Sources))
	for _, source := range input.Sources {
		// A colon in the type cannot round-trip through the owner's typed selector.
		if strings.Contains(source.SourceType, ":") {
			return input, nil, errors.New("invalid source type")
		}
		selectors = append(selectors, source.SourceType+":"+source.Identifier)
	}
	input.Sources, err = normalizeMessageExportSelectors(selectors)
	if err != nil {
		return input, nil, err
	}
	argv := []string{"export-messages", "--format=jsonl", "--person-id=" + strconv.FormatInt(input.PersonID, 10), "--start=" + input.Start, "--end=" + input.End}
	for _, typ := range input.MessageTypes {
		argv = append(argv, "--message-type="+typ)
	}
	for _, source := range input.Sources {
		argv = append(argv, "--source="+source.SourceType+":"+source.Identifier)
	}
	return input, argv, nil
}

func (b *daemonMCPOperations) executePersonMessageExport(ctx context.Context, args map[string]any) (*mcpserver.OperationResult, error) {
	input, argv, err := mcpPersonExportArguments(args)
	if err != nil {
		return operationFailure(mcpFactInvalidArgumentCode, false), nil //nolint:nilerr // Parser diagnostics stay private.
	}
	stream, err := b.client.RunMCPCLICommand(ctx, argv, "")
	if err != nil || stream == nil || stream.Failed {
		code := "message_export_incomplete"
		if stream != nil && stream.ErrorCode == "output_limit_exceeded" {
			code = "output_limit_exceeded"
		}
		return operationFailure(code, false), err
	}
	result, err := validateMCPPersonExport(stream.Stdout, input)
	if err != nil {
		return operationFailure(mcpPersonExportInvalid, false), err
	}
	data, err := json.Marshal(result)
	if err != nil {
		return operationFailure(mcpPersonExportInvalid, false), err
	}
	if len(data) > mcpPersonExportLimit {
		return operationFailure("output_limit_exceeded", false), nil
	}
	return &mcpserver.OperationResult{Output: result}, nil
}

// Validate the owner's closed record contract without rebuilding its resolver or
// database query. Retain the original JSON so provenance and exact text survive.
func validateMCPPersonExport(stdout string, input mcpPersonExportInput) (mcpserver.PersonMessagesExport, error) {
	result := mcpserver.PersonMessagesExport{Records: []jsontext.Value{}}
	decoder := jsontext.NewDecoder(strings.NewReader(stdout))
	counts := exportMessagesCounts{}
	sources := map[exportMessagesSourceSelector]bool{}
	type conversationKey struct {
		source exportMessagesSourceSelector
		id     string
	}
	conversations := map[conversationKey]bool{}
	messages := map[conversationKey]bool{}
	phase := 0
	start, end, err := parseMessageExportBounds(input.Start, input.End)
	if err != nil {
		return result, err
	}
	invalid := errors.New(mcpPersonExportInvalid)
	for {
		raw, err := decoder.ReadValue()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return result, err
		}
		if result.Complete {
			return result, invalid
		}
		var tag struct {
			RecordType string `json:"record_type"`
		}
		if err := json.Unmarshal(raw, &tag); err != nil {
			return result, err
		}
		switch tag.RecordType {
		case "manifest":
			var row exportMessagesManifest
			if phase != 0 || decodeMCPExportRecord(raw, &row, "record_type", "schema", "msgvault_version", "window", "filters") != nil || row.Schema != messageExportSchema || row.Filters.PersonID == nil || !mcpPositiveSafeID(*row.Filters.PersonID) || !row.Window.Start.Equal(start) || !row.Window.End.Equal(end) || !slices.Equal(row.Filters.MessageTypes, input.MessageTypes) || !slices.Equal(row.Filters.Sources, input.Sources) {
				return result, invalid
			}
			phase = 1
		case mcpPersonExportSource:
			var row exportMessagesSourceRecord
			if phase != 1 || decodeMCPExportRecord(raw, &row, "record_type", "source_type", "identifier", "display_name", "last_successful_sync_at") != nil || row.SourceType == "" || row.Identifier == "" {
				return result, invalid
			}
			key := exportMessagesSourceSelector{row.SourceType, row.Identifier}
			if sources[key] || (len(input.Sources) > 0 && !slices.Contains(input.Sources, key)) {
				return result, invalid
			}
			sources[key] = true
			counts.Sources++
		case "conversation":
			var row exportMessagesConversationRecord
			if phase < 1 || phase > 2 || decodeMCPExportRecord(raw, &row, "record_type", "source_type", "source_identifier", "id", "title", "conversation_type", "parent_id") != nil || row.ID == "" || !validMCPExportConversationType(row.ConversationType) {
				return result, invalid
			}
			key := conversationKey{exportMessagesSourceSelector{row.SourceType, row.SourceIdentifier}, row.ID}
			if !sources[key.source] || conversations[key] {
				return result, invalid
			}
			conversations[key] = true
			counts.Conversations++
			phase = 2
		case "message":
			var row exportMessagesMessageRecord
			if phase < 2 || decodeMCPExportRecord(raw, &row, "record_type", "source_type", "source_identifier", "id", "conversation_id", "message_type", "subject", "text", "author", "occurred_at", "deleted_from_source") != nil || row.ID == "" || row.MessageType == "" || row.OccurredAt.Before(start) || !row.OccurredAt.Before(end) || (len(input.MessageTypes) > 0 && !slices.Contains(input.MessageTypes, row.MessageType)) {
				return result, invalid
			}
			source := exportMessagesSourceSelector{row.SourceType, row.SourceIdentifier}
			key := conversationKey{source, row.ID}
			if !conversations[conversationKey{source, row.ConversationID}] || messages[key] {
				return result, invalid
			}
			messages[key] = true
			counts.Messages++
			phase = 3
		case "complete":
			var row exportMessagesComplete
			if phase < 1 || decodeMCPExportRecord(raw, &row, "record_type", "counts") != nil || row.Counts != counts {
				return result, invalid
			}
			result.Complete = true
		default:
			return result, invalid
		}
		result.Records = append(result.Records, slices.Clone(raw))
	}
	if !result.Complete {
		return result, invalid
	}
	return result, nil
}

func decodeMCPExportRecord(raw jsontext.Value, target any, required ...string) error {
	if err := json.Unmarshal(raw, target, json.RejectUnknownMembers(true)); err != nil {
		return err
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for _, field := range required {
		value, ok := fields[field]
		if !ok || (string(value) == "null" && field != "author" && field != "parent_id" && field != "last_successful_sync_at") {
			return errors.New(mcpPersonExportInvalid)
		}
	}
	// The decoder's zero values cannot distinguish omitted/null count fields
	// from a genuine zero-message completion. Check the native nested contract.
	switch target.(type) {
	case *exportMessagesManifest:
		if err := requireMCPExportObject(fields["window"], "start", "end"); err != nil {
			return err
		}
		return requireMCPExportObject(fields["filters"], "person_id", "message_types", "sources")
	case *exportMessagesComplete:
		return requireMCPExportObject(fields["counts"], "sources", "conversations", "messages")
	case *exportMessagesMessageRecord:
		if string(fields["author"]) != "null" {
			return requireMCPExportObject(fields["author"], "display_name", "address")
		}
	}
	return nil
}

func requireMCPExportObject(raw jsontext.Value, required ...string) error {
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for _, field := range required {
		value, ok := fields[field]
		if !ok || string(value) == "null" {
			return errors.New(mcpPersonExportInvalid)
		}
	}
	return nil
}

func validMCPExportConversationType(value store.MessageExportConversationType) bool {
	return slices.Contains([]store.MessageExportConversationType{store.MessageExportConversationEmailThread, store.MessageExportConversationChannel, store.MessageExportConversationThread, store.MessageExportConversationDirectChat, store.MessageExportConversationGroupChat, store.MessageExportConversationMeeting, store.MessageExportConversationCalendar, store.MessageExportConversationOther}, value)
}
