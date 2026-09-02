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

type responsesRequest struct {
	Model           string             `json:"model"`
	Input           []responseItem     `json:"input"`
	Text            *responseText      `json:"text,omitempty"`
	MaxOutputTokens int                `json:"max_output_tokens"`
	Reasoning       *responseReasoning `json:"reasoning,omitempty"`
}

type responseItem struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type responseText struct {
	Format responseTextFormat `json:"format"`
}

type responseTextFormat struct {
	Type   string          `json:"type"`
	Name   string          `json:"name,omitempty"`
	Strict bool            `json:"strict,omitempty"`
	Schema json.RawMessage `json:"schema,omitempty"`
}

type responseReasoning struct {
	Effort string `json:"effort"`
}

// OpenAIResponsesDriver implements one exact saved OpenAI Responses
// representation. It does not negotiate capabilities or retry.
type OpenAIResponsesDriver struct {
	http *httpDriver
}

// NewOpenAIResponsesDriver constructs an OpenAI Responses network driver.
func NewOpenAIResponsesDriver(client *http.Client) *OpenAIResponsesDriver {
	return &OpenAIResponsesDriver{http: newHTTPDriver(client)}
}

func (d *OpenAIResponsesDriver) Prepare(
	profile ProviderProfile,
	request StructuredRequest,
) (PreparedStructuredRequest, error) {
	if err := profile.Validate(); err != nil {
		return PreparedStructuredRequest{}, err
	}
	if profile.Protocol != ProtocolOpenAIResponses {
		return PreparedStructuredRequest{}, errors.New("OpenAI Responses driver requires openai_responses profile")
	}
	if profile.ReasoningMode != "" && profile.ReasoningMode != reasoningModeProviderDefault {
		return PreparedStructuredRequest{}, errors.New("OpenAI Responses profile has unsupported reasoning mode")
	}

	instruction := structuredSystemInstruction
	body := responsesRequest{
		Model: profile.Model,
		Input: []responseItem{
			{Role: "system", Content: instruction},
			{Role: "user", Content: request.InputText},
		},
		MaxOutputTokens: request.MaxOutputTokens,
	}
	switch profile.OutputMode {
	case OutputModeNativeJSONSchema:
		body.Text = &responseText{Format: responseTextFormat{
			Type: "json_schema", Name: request.SchemaName, Strict: true,
			Schema: request.JSONSchema,
		}}
	case OutputModeJSONObject:
		body.Input[0].Content = jsonObjectInstruction + string(request.JSONSchema)
		body.Text = &responseText{Format: responseTextFormat{Type: "json_object"}}
	case OutputModePromptJSON:
		body.Input[0].Content = promptJSONInstruction + string(request.JSONSchema)
	default:
		return PreparedStructuredRequest{}, errors.New("OpenAI Responses profile has unsupported output mode")
	}
	if profile.ReasoningEffort != "" {
		body.Reasoning = &responseReasoning{Effort: profile.ReasoningEffort}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return PreparedStructuredRequest{}, errors.New("encode inference provider request")
	}
	return NewPreparedStructuredRequest(request, payload)
}

type openAIResponsesEnvelope struct {
	Model  string `json:"model"`
	Output []struct {
		Type    string `json:"type"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"output"`
	Usage *struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
}

func (d *OpenAIResponsesDriver) GeneratePrepared(
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

	target := strings.TrimRight(profile.Endpoint, "/") + "/responses"
	httpResponse, err := d.http.post(ctx, target, profile, credential, prepared.WireRequest())
	if err != nil {
		return DriverResponse{}, err
	}
	var envelope openAIResponsesEnvelope
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
	if !safeProviderMetadata(envelope.Model) {
		return result, errors.Join(ErrInvalidStructuredOutput, errors.New("provider response is missing model version"))
	}
	result.ModelVersion = envelope.Model

	var candidate string
	for _, item := range envelope.Output {
		for _, block := range item.Content {
			if block.Type != "output_text" {
				continue
			}
			if item.Type != "message" {
				return result, errors.Join(ErrInvalidStructuredOutput, errors.New("provider response has output text outside a message"))
			}
			if strings.TrimSpace(block.Text) == "" {
				return result, errors.Join(ErrInvalidStructuredOutput, errors.New("provider response has empty structured content"))
			}
			if candidate != "" {
				return result, errors.Join(ErrInvalidStructuredOutput, errors.New("provider response has multiple structured content candidates"))
			}
			candidate = block.Text
		}
	}
	if candidate == "" {
		return result, errors.Join(ErrInvalidStructuredOutput, errors.New("provider response has no structured content"))
	}
	var decoded any
	if err := decodeSingleJSONUseNumber([]byte(candidate), &decoded); err != nil {
		return result, errors.Join(ErrInvalidStructuredOutput, errors.New("provider returned invalid structured JSON"))
	}
	result.CandidateJSON = append(json.RawMessage(nil), candidate...)
	return result, nil
}

var _ StructuredDriver = (*OpenAIResponsesDriver)(nil)
