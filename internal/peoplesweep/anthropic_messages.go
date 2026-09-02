package peoplesweep

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

const defaultAnthropicVersion = "2023-06-01"

type anthropicRequest struct {
	Model      string               `json:"model"`
	System     string               `json:"system"`
	Messages   []anthropicMessage   `json:"messages"`
	MaxTokens  int                  `json:"max_tokens"`
	Tools      []anthropicTool      `json:"tools,omitempty"`
	ToolChoice *anthropicToolChoice `json:"tool_choice,omitempty"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// AnthropicMessagesDriver implements one exact saved Anthropic Messages
// representation. It does not negotiate capabilities or retry.
type AnthropicMessagesDriver struct {
	http *httpDriver
}

// NewAnthropicMessagesDriver constructs an Anthropic Messages network driver.
func NewAnthropicMessagesDriver(client *http.Client) *AnthropicMessagesDriver {
	return &AnthropicMessagesDriver{http: newHTTPDriver(client)}
}

func (d *AnthropicMessagesDriver) Prepare(
	profile ProviderProfile,
	request StructuredRequest,
) (PreparedStructuredRequest, error) {
	if err := profile.Validate(); err != nil {
		return PreparedStructuredRequest{}, err
	}
	if profile.Protocol != ProtocolAnthropicMessages {
		return PreparedStructuredRequest{}, errors.New("anthropic messages driver requires anthropic_messages profile")
	}
	if profile.Auth != AuthXAPIKey {
		return PreparedStructuredRequest{}, errors.New("anthropic messages profile requires x_api_key authentication")
	}
	if profile.ReasoningEffort != "" ||
		(profile.ReasoningMode != "" && profile.ReasoningMode != reasoningModeProviderDefault) {
		return PreparedStructuredRequest{}, errors.New("anthropic messages profile has unsupported reasoning settings")
	}
	body := anthropicRequest{
		Model: profile.Model, System: structuredSystemInstruction,
		Messages:  []anthropicMessage{{Role: "user", Content: request.InputText}},
		MaxTokens: request.MaxOutputTokens,
	}
	switch profile.OutputMode {
	case OutputModeNativeJSONSchema:
		toolName := request.SchemaName
		body.Tools = []anthropicTool{{Name: toolName, InputSchema: request.JSONSchema}}
		body.ToolChoice = &anthropicToolChoice{Type: "tool", Name: toolName}
	case OutputModePromptJSON:
		body.System = promptJSONInstruction + string(request.JSONSchema)
	default:
		return PreparedStructuredRequest{}, errors.New("anthropic messages profile has unsupported output mode")
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return PreparedStructuredRequest{}, errors.New("encode inference provider request")
	}
	return NewPreparedStructuredRequest(request, payload)
}

type anthropicEnvelope struct {
	Type    string            `json:"type"`
	Role    string            `json:"role"`
	Model   string            `json:"model"`
	Content []json.RawMessage `json:"content"`
	Usage   *struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
}

type anthropicContentBlock struct {
	Type  string          `json:"type"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
	Text  *string         `json:"text"`
}

func (d *AnthropicMessagesDriver) GeneratePrepared(
	ctx context.Context,
	profile ProviderProfile,
	credential Credential,
	prepared PreparedStructuredRequest,
) (DriverResponse, error) {
	if err := profile.Validate(); err != nil {
		return DriverResponse{}, err
	}
	if err := prepared.validateWireHash(); err != nil {
		return DriverResponse{}, err
	}
	expected, err := d.Prepare(profile, prepared.Request())
	if err != nil {
		return DriverResponse{}, fmt.Errorf("re-encode prepared inference provider request: %w", err)
	}
	if !bytes.Equal(prepared.WireRequest(), expected.WireRequest()) {
		return DriverResponse{}, errors.New("prepared structured request does not match deterministic provider encoding")
	}

	target := strings.TrimRight(profile.Endpoint, "/") + "/v1/messages"
	httpResponse, err := d.http.postWithHeaders(
		ctx, target, profile, credential, prepared.WireRequest(),
		map[string]string{"anthropic-version": defaultAnthropicVersion},
	)
	if err != nil {
		return DriverResponse{}, err
	}
	var envelope anthropicEnvelope
	if err := decodeSingleJSON(httpResponse.body, &envelope); err != nil {
		return DriverResponse{}, errors.Join(ErrInvalidStructuredOutput, errors.New("decode provider response"))
	}
	result := DriverResponse{
		ProviderRequestID: httpResponse.requestID,
		ProviderVersion:   profile.DriverVersion,
	}
	if envelope.Usage != nil {
		result.UsageKnown = true
		result.Usage = TokenUsage{
			InputTokens: envelope.Usage.InputTokens, OutputTokens: envelope.Usage.OutputTokens,
		}
		if result.Usage.InputTokens < 0 || result.Usage.OutputTokens < 0 {
			return result, errors.Join(ErrInvalidStructuredOutput, errors.New("provider returned invalid token usage"))
		}
	}
	if envelope.Type != "message" || envelope.Role != "assistant" {
		return result, errors.Join(ErrInvalidStructuredOutput, errors.New("provider response is not an assistant message"))
	}
	if !safeProviderMetadata(envelope.Model) {
		return result, errors.Join(ErrInvalidStructuredOutput, errors.New("provider response is missing model version"))
	}
	result.ModelVersion = envelope.Model

	candidate, err := extractAnthropicCandidate(profile.OutputMode, prepared.Request().SchemaName, envelope.Content)
	if err != nil {
		return result, err
	}
	result.CandidateJSON = candidate
	return result, nil
}

func extractAnthropicCandidate(
	mode OutputMode,
	toolName string,
	content []json.RawMessage,
) (json.RawMessage, error) {
	if len(content) != 1 {
		return nil, errors.Join(ErrInvalidStructuredOutput, errors.New("provider response must contain exactly one structured content block"))
	}
	var block anthropicContentBlock
	if err := decodeSingleJSON(content[0], &block); err != nil {
		return nil, errors.Join(ErrInvalidStructuredOutput, errors.New("decode provider content block"))
	}

	var candidate json.RawMessage
	switch mode {
	case OutputModeNativeJSONSchema:
		if block.Type != "tool_use" || block.Name != toolName || block.Text != nil {
			return nil, errors.Join(ErrInvalidStructuredOutput, errors.New("provider response has invalid structured tool content"))
		}
		trimmed := bytes.TrimSpace(block.Input)
		if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
			return nil, errors.Join(ErrInvalidStructuredOutput, errors.New("provider response has empty structured tool input"))
		}
		var object map[string]any
		if err := decodeSingleJSONUseNumber(trimmed, &object); err != nil || object == nil {
			return nil, errors.Join(ErrInvalidStructuredOutput, errors.New("provider returned invalid structured JSON"))
		}
		candidate = trimmed
	case OutputModePromptJSON:
		if block.Type != "text" || block.Text == nil || block.Name != "" || len(block.Input) != 0 || block.ID != "" {
			return nil, errors.Join(ErrInvalidStructuredOutput, errors.New("provider response has invalid structured text content"))
		}
		trimmed := strings.TrimSpace(*block.Text)
		if trimmed == "" {
			return nil, errors.Join(ErrInvalidStructuredOutput, errors.New("provider response has empty structured text"))
		}
		var decoded any
		if err := decodeSingleJSONUseNumber([]byte(trimmed), &decoded); err != nil {
			return nil, errors.Join(ErrInvalidStructuredOutput, errors.New("provider returned invalid structured JSON"))
		}
		candidate = json.RawMessage(trimmed)
	default:
		return nil, errors.Join(ErrInvalidStructuredOutput, errors.New("provider response uses unsupported output mode"))
	}
	return append(json.RawMessage(nil), candidate...), nil
}

var _ StructuredDriver = (*AnthropicMessagesDriver)(nil)
