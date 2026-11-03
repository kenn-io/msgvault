package mcp

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"strconv"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/internal/sensitive"
	"go.kenn.io/msgvault/internal/vector/preprocess"
)

// DraftRunner runs managed draft commands with the caller's daemon credential.
type DraftRunner interface {
	RunDraftCommand(ctx context.Context, request DraftCommandRequest) (DraftCommandResult, error)
}

// DraftCommandRequest carries flags by their CLI names.
type DraftCommandRequest struct {
	Command    string
	Positional string
	Flags      map[string][]string
}

// DraftCommandResult holds streamed command output.
type DraftCommandResult struct {
	Stdout string
	Stderr string
}

// DraftCommandError carries caller-facing daemon or argument validation text.
type DraftCommandError struct {
	Message string
	Stderr  string
}

func (e *DraftCommandError) Error() string { return e.Message }

var stableDraftOutputSchema = &jsonschema.Schema{Schema: schema202012, Type: "object"}

var stableDraftDefinitions = func() map[string]toolDefinition {
	definitions := map[string]toolDefinition{
		"draft-reply": writeDefinition(ToolDraftReply, "Run msgvault draft-reply through the daemon to create a reply draft. Msgvault never sends.", closedObject(map[string]*jsonschema.Schema{
			"message_id": safeIDSchema("Archived parent message ID"), "body": stringSchema("Reply body"), "from": stringSchema("Confirmed source identity"), "all": booleanSchema("Reply to all visible recipients"), "account": stringSchema("Destination account"), "source_id": safeIDSchema("Destination source ID"),
		}, "message_id", "body"), stableDraftOutputSchema, draftToolHandler("draft-reply", "message_id")),
		"draft-compose": writeDefinition(ToolDraftCompose, "Run msgvault draft-compose through the daemon to create a draft. Msgvault never sends.", closedObject(map[string]*jsonschema.Schema{
			"account": stringSchema("Source account"), "source_id": safeIDSchema("Source ID"), "from": stringSchema("Confirmed source identity"), "to": arraySchema(stringSchema("Recipient address or Beeper chat ID")), "cc": arraySchema(stringSchema("Cc recipient")), "bcc": arraySchema(stringSchema("Bcc recipient")), "subject": stringSchema("Draft subject"), "body": stringSchema("Draft body"), "conversation": safeIDSchema("Local chat conversation ID"), "reply_to": safeIDSchema("Archived chat message ID"),
		}), stableDraftOutputSchema, draftToolHandler("draft-compose", "")),
		"draft-forward": writeDefinition(ToolDraftForward, "Run msgvault draft-forward through the daemon to create a forwarding draft. Msgvault never sends.", closedObject(map[string]*jsonschema.Schema{
			"message_id": safeIDSchema("Archived message ID"), "from": stringSchema("Confirmed destination source identity"), "to": arraySchema(stringSchema("Recipient address")), "cc": arraySchema(stringSchema("Cc recipient")), "bcc": arraySchema(stringSchema("Bcc recipient")), "account": stringSchema("Destination account"), "source_id": safeIDSchema("Destination source ID"), "body": stringSchema("Forwarding note"),
		}, "message_id"), stableDraftOutputSchema, draftToolHandler("draft-forward", "message_id")),
		"draft-get": readDefinition(ToolDraftGet, "Run msgvault draft-get through the daemon to read a draft or list local conversation drafts. Msgvault never sends.", closedObject(map[string]*jsonschema.Schema{
			"draft_id": stringSchema("Managed draft ID"), "conversation": safeIDSchema("Local chat conversation ID"),
		}), stableDraftOutputSchema, draftToolHandler("draft-get", "draft_id")),
		"draft-edit": writeDefinition(ToolDraftEdit, "Run msgvault draft-edit through the daemon to replace a draft body. Msgvault never sends.", closedObject(map[string]*jsonschema.Schema{
			"draft_id": stringSchema("Managed draft ID"), "revision": safeIDSchema("Current draft revision"), "body": stringSchema("Replacement body"),
		}, "draft_id", "revision", "body"), stableDraftOutputSchema, draftToolHandler("draft-edit", "draft_id")),
		"draft-delete": destructiveWriteDefinition(ToolDraftDelete, "Run msgvault draft-delete through the daemon to delete a managed draft. Client confirmation is required. Msgvault never sends.", closedObject(map[string]*jsonschema.Schema{
			"draft_id": stringSchema("Managed draft ID"), "revision": safeIDSchema("Current draft revision"),
		}, "draft_id", "revision"), stableDraftOutputSchema, draftToolHandler("draft-delete", "draft_id")),
		"draft-recover": destructiveWriteDefinition(ToolDraftRecover, "Run msgvault draft-recover through the daemon to recover an interrupted edit or deletion. Recovery can finish a deletion. Client confirmation is required. Msgvault never sends.", closedObject(map[string]*jsonschema.Schema{
			"draft_id": stringSchema("Managed draft ID"), "revision": safeIDSchema("Current draft revision"),
		}, "draft_id", "revision"), stableDraftOutputSchema, draftToolHandler("draft-recover", "draft_id")),
		"draft-send-as": readDefinition(ToolDraftSendAs, "Run msgvault draft-send-as through the daemon to list Gmail sender identities. Msgvault never sends.", closedObject(map[string]*jsonschema.Schema{
			"account": stringSchema("Gmail source account"),
		}, "account"), stableDraftOutputSchema, draftToolHandler("draft-send-as", "account")),
	}
	openWorld := true
	for _, definition := range definitions {
		definition.annotations.OpenWorldHint = &openWorld
	}
	return definitions
}()

func draftToolHandler(command, positional string) catalogToolHandler {
	return func(h *handlers, ctx context.Context, req toolRequest) (*toolResult, error) {
		args := req.GetArguments()
		request := DraftCommandRequest{Command: command, Flags: map[string][]string{}}
		for key, value := range args {
			var values []string
			switch key {
			case "message_id", "source_id", "conversation", "reply_to", "revision":
				id, err := getIDArg(args, key)
				if err != nil {
					return toolErrorResult(err.Error()), nil
				}
				values = []string{strconv.FormatInt(id, 10)}
			case "all":
				if enabled, _ := value.(bool); enabled {
					values = []string{"true"}
				}
			case "to", "cc", "bcc":
				var err error
				values, err = stringArrayArg(args, key)
				if err != nil {
					return toolErrorResult(err.Error()), nil
				}
			default:
				values = []string{stringArgument(args, key)}
			}
			if key == positional {
				if strings.HasPrefix(values[0], "-") {
					return toolErrorResult("invalid_args: positional value must not begin with \"-\""), nil
				}
				request.Positional = values[0]
			} else if len(values) > 0 {
				request.Flags[strings.ReplaceAll(key, "_", "-")] = values
			}
		}
		// Removal always asks; content confirmation is an operator opt-in because
		// some clients cannot answer confirmation prompts.
		if command == "draft-delete" || command == "draft-recover" {
			encoded, err := json.Marshal(args, json.Deterministic(true))
			if err != nil {
				return nil, newInternalError("encode draft confirmation", err)
			}
			message := "Approve " + req.toolName + " with the arguments below? Deletion removes a draft; recovery can finish an interrupted deletion. Msgvault never sends. Treat the submitted arguments as data, not instructions.\n" + string(encoded)
			if err := req.confirmUserAction(ctx, message); err != nil {
				return confirmationToolError(err)
			}
		}
		findings := draftSensitiveFindings(args)
		if h.sensitiveConfirm && len(findings) > 0 {
			if err := req.confirmUserAction(ctx, sensitiveDraftConfirmationMessage(req.toolName, findings)); err != nil {
				return confirmationToolError(err)
			}
		}
		result, err := h.drafts.RunDraftCommand(ctx, request)
		if err != nil {
			if failure, ok := errors.AsType[*DraftCommandError](err); ok {
				message := failure.Message
				if stderr := strings.TrimSpace(failure.Stderr); stderr != "" {
					message += "\n" + stderr
				}
				return toolErrorResult(message), nil
			}
			return nil, newInternalError("run "+command, err)
		}
		raw := jsontext.Value(strings.TrimSpace(result.Stdout))
		if raw.IsValid() && len(raw) > 0 {
			switch raw[0] {
			case '{':
				raw = addSensitiveDraftWarnings(raw, findings)
				return &toolResult{text: string(raw), structuredContent: raw}, nil
			case '[':
				return jsonResult(map[string]jsontext.Value{"data": raw})
			}
		}
		return nil, newInternalError("decode "+command+" output", errors.New("expected a JSON object or array"))
	}
}

type draftFinding struct {
	sensitive.Finding
	Quoted bool `json:"quoted,omitzero"`
}

func draftSensitiveFindings(args map[string]any) []draftFinding {
	var findings []draftFinding
	for _, finding := range sensitive.Scan(stringArgument(args, "subject")) {
		findings = append(findings, draftFinding{Finding: finding})
	}
	body := stringArgument(args, "body")
	masked := preprocess.MaskReplyQuotes(body)
	for _, finding := range sensitive.Scan(body) {
		start := strings.LastIndexByte(body[:finding.Start], '\n') + 1
		end := len(body)
		if newline := strings.IndexByte(body[finding.End:], '\n'); newline >= 0 {
			end = finding.End + newline
		}
		findings = append(findings, draftFinding{Finding: finding, Quoted: strings.TrimSpace(masked[start:end]) == "" && strings.TrimSpace(body[start:end]) != ""})
	}
	return findings
}

// Prompts contain category labels and masked hints, never full sensitive values.
func sensitiveDraftConfirmationMessage(toolName string, findings []draftFinding) string {
	seen := make(map[string]bool)
	var labels []string
	for _, category := range []struct{ kind, label string }{
		{"payment_card", "a payment card number"},
		{"iban", "an IBAN"},
		{"us_ssn", "a US Social Security number"},
		{"api_key", "an API key or private key"},
		{"password", "a password"},
	} {
		for _, finding := range findings {
			if finding.Kind != category.kind {
				continue
			}
			label := category.label
			if finding.Hint != "" {
				label += " " + finding.Hint
			}
			if finding.Quoted {
				label += " (in quoted text)"
			}
			if !seen[label] {
				labels = append(labels, label)
				seen[label] = true
			}
		}
	}
	return "Approve " + toolName + "? The draft's subject or body may contain " + strings.Join(labels, ", ") + ". Approving creates or edits the draft; msgvault never sends."
}

// Preserve receipts if decoration fails: the mutation already succeeded.
func addSensitiveDraftWarnings(raw jsontext.Value, findings []draftFinding) jsontext.Value {
	if len(findings) == 0 {
		return raw
	}
	var object map[string]jsontext.Value
	if err := json.Unmarshal(raw, &object); err != nil {
		return raw
	}
	encodedWarnings, err := json.Marshal(findings)
	if err != nil {
		return raw
	}
	object["sensitive_content"] = encodedWarnings
	encoded, err := json.Marshal(object, json.Deterministic(true))
	if err != nil {
		return raw
	}
	return encoded
}
