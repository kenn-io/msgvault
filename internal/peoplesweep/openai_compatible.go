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
	maxProviderResponseBytes    = 1 << 20
)

// OpenAICompatibleTransport implements strict JSON Schema output over the
// OpenAI-compatible Chat Completions protocol.
type OpenAICompatibleTransport struct {
	httpClient *http.Client
}

// NewOpenAICompatibleTransport constructs a network adapter. A nil client uses
// http.DefaultClient; the gated runner supplies the request timeout via context.
func NewOpenAICompatibleTransport(client *http.Client) *OpenAICompatibleTransport {
	if client == nil {
		client = http.DefaultClient
	}
	isolated := *client
	isolated.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &OpenAICompatibleTransport{httpClient: &isolated}
}

type chatCompletionsRequest struct {
	Model               string                  `json:"model"`
	Messages            []chatCompletionMessage `json:"messages"`
	ResponseFormat      chatCompletionFormat    `json:"response_format"`
	MaxCompletionTokens int                     `json:"max_completion_tokens"`
}

type chatCompletionMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatCompletionFormat struct {
	Type       string                   `json:"type"`
	JSONSchema chatCompletionJSONSchema `json:"json_schema"`
}

type chatCompletionJSONSchema struct {
	Name   string          `json:"name"`
	Strict bool            `json:"strict"`
	Schema json.RawMessage `json:"schema"`
}

type chatCompletionsResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
	} `json:"usage"`
}

// GenerateJSON performs one request without retries and returns parsed JSON.
// Consent, source policy, credential lookup, timeout, and schema validation are
// owned by Runner.
func (t *OpenAICompatibleTransport) GenerateJSON(
	ctx context.Context,
	profile ProviderProfile,
	credential string,
	request StructuredRequest,
) (StructuredResponse, error) {
	if err := profile.Validate(); err != nil {
		return StructuredResponse{}, err
	}
	if profile.APIKeyEnv != "" && credential == "" {
		return StructuredResponse{}, errors.New("inference provider credential is empty")
	}
	payload, err := json.Marshal(chatCompletionsRequest{
		Model: profile.Model,
		Messages: []chatCompletionMessage{
			{Role: "system", Content: structuredSystemInstruction},
			{Role: "user", Content: request.InputText},
		},
		ResponseFormat: chatCompletionFormat{
			Type: "json_schema",
			JSONSchema: chatCompletionJSONSchema{
				Name: request.SchemaName, Strict: true, Schema: request.JSONSchema,
			},
		},
		MaxCompletionTokens: request.MaxOutputTokens,
	})
	if err != nil {
		return StructuredResponse{}, errors.New("encode inference provider request")
	}
	target := strings.TrimRight(profile.Endpoint, "/") + "/chat/completions"
	httpRequest, err := http.NewRequestWithContext( // #nosec G704 -- exact operator-configured endpoint validated by ProviderProfile.
		ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return StructuredResponse{}, fmt.Errorf("create inference provider request: %w", err)
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	if credential != "" {
		httpRequest.Header.Set("Authorization", "Bearer "+credential)
	}

	response, err := t.httpClient.Do(httpRequest)
	if err != nil {
		return StructuredResponse{}, fmt.Errorf("call inference provider: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	requestID := response.Header.Get("x-request-id")
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 32<<10))
		return StructuredResponse{}, &ProviderError{
			StatusCode: response.StatusCode,
			RequestID:  requestID,
		}
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, maxProviderResponseBytes+1))
	if err != nil {
		return StructuredResponse{}, fmt.Errorf("read inference provider response: %w", err)
	}
	if len(body) > maxProviderResponseBytes {
		return StructuredResponse{}, errors.New("inference provider response is too large")
	}
	var envelope chatCompletionsResponse
	if err := decodeSingleJSON(body, &envelope); err != nil {
		return StructuredResponse{}, errors.New("decode inference provider response")
	}
	if len(envelope.Choices) == 0 || strings.TrimSpace(envelope.Choices[0].Message.Content) == "" {
		return StructuredResponse{}, errors.New("inference provider response has no structured content")
	}
	content := []byte(envelope.Choices[0].Message.Content)
	var decoded any
	if err := decodeSingleJSONUseNumber(content, &decoded); err != nil {
		return StructuredResponse{}, errors.New("inference provider returned invalid structured JSON")
	}
	return StructuredResponse{
		Output:            json.RawMessage(append([]byte(nil), content...)),
		ProviderRequestID: requestID,
		Usage: TokenUsage{
			InputTokens:  envelope.Usage.PromptTokens,
			OutputTokens: envelope.Usage.CompletionTokens,
		},
	}, nil
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
