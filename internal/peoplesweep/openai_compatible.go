package peoplesweep

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
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
	Model   string `json:"model"`
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

// PrepareJSON deterministically encodes the complete Chat Completions body.
func (t *OpenAICompatibleTransport) PrepareJSON(
	profile ProviderProfile,
	request StructuredRequest,
) (PreparedStructuredRequest, error) {
	if err := profile.Validate(); err != nil {
		return PreparedStructuredRequest{}, err
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
		return PreparedStructuredRequest{}, errors.New("encode inference provider request")
	}
	return NewPreparedStructuredRequest(request, payload)
}

// GeneratePreparedJSON performs one request without retries using only the
// bytes prepared before budget reservation.
func (t *OpenAICompatibleTransport) GeneratePreparedJSON(
	ctx context.Context,
	profile ProviderProfile,
	credential string,
	prepared PreparedStructuredRequest,
) (StructuredResponse, error) {
	if err := profile.Validate(); err != nil {
		return StructuredResponse{}, err
	}
	if profile.APIKeyEnv != "" && credential == "" {
		return StructuredResponse{}, errors.New("inference provider credential is empty")
	}
	if err := prepared.validateWireHash(); err != nil {
		return StructuredResponse{}, err
	}
	expected, err := t.PrepareJSON(profile, prepared.Request())
	if err != nil {
		return StructuredResponse{}, fmt.Errorf("re-encode prepared inference provider request: %w", err)
	}
	if !bytes.Equal(prepared.WireRequest(), expected.WireRequest()) {
		return StructuredResponse{}, errors.New("prepared structured request does not match deterministic provider encoding")
	}
	target := strings.TrimRight(profile.Endpoint, "/") + "/chat/completions"
	httpRequest, err := http.NewRequestWithContext( // #nosec G704 -- exact operator-configured endpoint validated by ProviderProfile.
		ctx, http.MethodPost, target, bytes.NewReader(prepared.WireRequest()))
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
	if !safeProviderMetadata(requestID) {
		requestID = ""
	}
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 32<<10))
		retryAfter := time.Duration(0)
		if retryableProviderStatus(response.StatusCode) {
			retryAfter = parseRetryAfter(response.Header.Get("Retry-After"), time.Now())
		}
		return StructuredResponse{}, &ProviderError{
			StatusCode: response.StatusCode,
			RequestID:  requestID,
			RetryAfter: retryAfter,
		}
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, maxProviderResponseBytes+1))
	if err != nil {
		return StructuredResponse{}, fmt.Errorf("read inference provider response: %w", err)
	}
	// A client deadline can arrive after response headers and make the body
	// appear as an empty EOF. Preserve cancellation rather than reclassifying
	// that transport failure as provider-completed invalid JSON.
	if requestErr := httpRequest.Context().Err(); requestErr != nil {
		return StructuredResponse{}, fmt.Errorf("read inference provider response: %w", requestErr)
	}
	if len(body) > maxProviderResponseBytes {
		return StructuredResponse{}, fmt.Errorf("%w: provider response is too large", ErrInvalidStructuredOutput)
	}
	var envelope chatCompletionsResponse
	if err := decodeSingleJSON(body, &envelope); err != nil {
		return StructuredResponse{}, fmt.Errorf("%w: decode provider response", ErrInvalidStructuredOutput)
	}
	result := StructuredResponse{
		ProviderRequestID: requestID,
		ProviderVersion:   OpenAICompatibleProviderVersion,
		Usage: TokenUsage{
			InputTokens:  envelope.Usage.PromptTokens,
			OutputTokens: envelope.Usage.CompletionTokens,
		},
	}
	if envelope.Usage.PromptTokens < 0 || envelope.Usage.CompletionTokens < 0 {
		return result, fmt.Errorf("%w: provider returned invalid token usage", ErrInvalidStructuredOutput)
	}
	if len(envelope.Choices) == 0 || strings.TrimSpace(envelope.Choices[0].Message.Content) == "" {
		return result, fmt.Errorf("%w: provider response has no structured content", ErrInvalidStructuredOutput)
	}
	if !safeProviderMetadata(envelope.Model) {
		return result, fmt.Errorf("%w: provider response is missing model version", ErrInvalidStructuredOutput)
	}
	result.ModelVersion = envelope.Model
	content := []byte(envelope.Choices[0].Message.Content)
	var decoded any
	if err := decodeSingleJSONUseNumber(content, &decoded); err != nil {
		return result, fmt.Errorf("%w: provider returned invalid structured JSON", ErrInvalidStructuredOutput)
	}
	result.Output = json.RawMessage(append([]byte(nil), content...))
	return result, nil
}

func retryableProviderStatus(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests ||
		(status >= http.StatusInternalServerError && status <= 599)
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	deltaSeconds := true
	for _, char := range value {
		if char < '0' || char > '9' {
			deltaSeconds = false
			break
		}
	}
	if deltaSeconds {
		seconds, err := strconv.ParseUint(value, 10, 64)
		if err != nil || seconds > uint64(math.MaxInt64/int64(time.Second)) {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return 0
	}
	delay := when.Sub(now)
	if delay <= 0 {
		return 0
	}
	return delay
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
