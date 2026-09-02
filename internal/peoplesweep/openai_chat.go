package peoplesweep

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	structuredSystemInstruction = "Return one JSON value that strictly matches the supplied JSON Schema."
	jsonObjectInstruction       = "Return one JSON object that strictly matches this JSON Schema:\n"
	promptJSONInstruction       = "Return only one JSON value that strictly matches this JSON Schema:\n"
)

// OpenAIChatDriver implements one exact saved OpenAI Chat Completions
// representation. It does not negotiate capabilities or retry.
type OpenAIChatDriver struct {
	http *httpDriver
}

// NewOpenAIChatDriver constructs an OpenAI Chat network driver.
func NewOpenAIChatDriver(client *http.Client) *OpenAIChatDriver {
	return &OpenAIChatDriver{http: newHTTPDriver(client)}
}

func (d *OpenAIChatDriver) Prepare(
	profile ProviderProfile,
	request StructuredRequest,
) (PreparedStructuredRequest, error) {
	if err := profile.Validate(); err != nil {
		return PreparedStructuredRequest{}, err
	}
	if profile.Protocol != ProtocolOpenAIChat {
		return PreparedStructuredRequest{}, errors.New("OpenAI Chat driver requires openai_chat profile")
	}
	body := map[string]any{
		"model":                     profile.Model,
		"messages":                  messagesFor(request, profile.OutputMode),
		profile.TokenLimitParameter: request.MaxOutputTokens,
	}
	switch profile.OutputMode {
	case OutputModeNativeJSONSchema:
		body["response_format"] = map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name": request.SchemaName, "strict": true, "schema": request.JSONSchema,
			},
		}
	case OutputModeJSONObject:
		body["response_format"] = map[string]any{"type": "json_object"}
	case OutputModePromptJSON:
	default:
		return PreparedStructuredRequest{}, errors.New("OpenAI Chat profile has unsupported output mode")
	}
	if profile.ReasoningEffort != "" {
		body["reasoning_effort"] = profile.ReasoningEffort
	}
	switch profile.ReasoningMode {
	case "", reasoningModeProviderDefault:
	case "enabled":
		body["reasoning"] = map[string]any{"enabled": true}
	case "disabled":
		body["reasoning"] = map[string]any{"enabled": false}
	default:
		return PreparedStructuredRequest{}, errors.New("OpenAI Chat profile has unsupported reasoning mode")
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return PreparedStructuredRequest{}, errors.New("encode inference provider request")
	}
	return NewPreparedStructuredRequest(request, payload)
}

func messagesFor(request StructuredRequest, mode OutputMode) []map[string]string {
	instruction := structuredSystemInstruction
	switch mode {
	case OutputModeNativeJSONSchema:
	case OutputModeJSONObject:
		instruction = jsonObjectInstruction + string(request.JSONSchema)
	case OutputModePromptJSON:
		instruction = promptJSONInstruction + string(request.JSONSchema)
	}
	return []map[string]string{
		{"role": "system", "content": instruction},
		{"role": "user", "content": request.InputText},
	}
}

type openAIChatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
	} `json:"usage"`
}

func (d *OpenAIChatDriver) GeneratePrepared(
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
	target := strings.TrimRight(profile.Endpoint, "/") + "/chat/completions"
	httpResponse, err := d.http.post(ctx, target, profile, credential, prepared.WireRequest())
	if err != nil {
		return DriverResponse{}, err
	}
	var envelope openAIChatResponse
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
			InputTokens: envelope.Usage.PromptTokens, OutputTokens: envelope.Usage.CompletionTokens,
		}
		if result.Usage.InputTokens < 0 || result.Usage.OutputTokens < 0 {
			return result, errors.Join(ErrInvalidStructuredOutput, errors.New("provider returned invalid token usage"))
		}
	}
	if len(envelope.Choices) == 0 || strings.TrimSpace(envelope.Choices[0].Message.Content) == "" {
		return result, errors.Join(ErrInvalidStructuredOutput, errors.New("provider response has no structured content"))
	}
	if !safeProviderMetadata(envelope.Model) {
		return result, errors.Join(ErrInvalidStructuredOutput, errors.New("provider response is missing model version"))
	}
	result.ModelVersion = envelope.Model
	content := []byte(envelope.Choices[0].Message.Content)
	var decoded any
	if err := decodeSingleJSONUseNumber(content, &decoded); err != nil {
		return result, errors.Join(ErrInvalidStructuredOutput, errors.New("provider returned invalid structured JSON"))
	}
	result.CandidateJSON = append(json.RawMessage(nil), content...)
	return result, nil
}

func decodeSingleJSON(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	return requireJSONEOF(decoder)
}

func decodeSingleJSONUseNumber(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	return requireJSONEOF(decoder)
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

var _ StructuredDriver = (*OpenAIChatDriver)(nil)
