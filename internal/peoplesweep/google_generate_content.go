package peoplesweep

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"unicode"
)

type googleGenerateContentRequest struct {
	Contents          []googleContent        `json:"contents"`
	SystemInstruction googleContent          `json:"systemInstruction"`
	GenerationConfig  googleGenerationConfig `json:"generationConfig"`
}

type googleContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []googlePart `json:"parts"`
}

type googlePart struct {
	Text string `json:"text"`
}

type googleGenerationConfig struct {
	MaxOutputTokens  int            `json:"maxOutputTokens"`
	ResponseMIMEType string         `json:"responseMimeType,omitempty"`
	ResponseSchema   jsontext.Value `json:"responseSchema,omitempty"`
}

// GoogleGenerateContentDriver implements one exact saved Gemini
// generateContent representation. It does not negotiate capabilities or retry.
type GoogleGenerateContentDriver struct {
	http *httpDriver
}

// NewGoogleGenerateContentDriver constructs a Gemini generateContent network driver.
func NewGoogleGenerateContentDriver(client *http.Client) *GoogleGenerateContentDriver {
	return &GoogleGenerateContentDriver{http: newHTTPDriver(client)}
}

func (d *GoogleGenerateContentDriver) Prepare(
	profile ProviderProfile,
	request StructuredRequest,
) (PreparedStructuredRequest, error) {
	if err := profile.Validate(); err != nil {
		return PreparedStructuredRequest{}, err
	}
	if profile.Protocol != ProtocolGoogleGenerateContent {
		return PreparedStructuredRequest{}, errors.New("google generateContent driver requires google_generate_content profile")
	}
	if _, err := googleGenerateContentTarget(profile.Endpoint, profile.Model); err != nil {
		return PreparedStructuredRequest{}, err
	}

	instruction := structuredSystemInstruction
	body := googleGenerateContentRequest{
		Contents: []googleContent{{
			Role: "user", Parts: []googlePart{{Text: request.InputText}},
		}},
		SystemInstruction: googleContent{Parts: []googlePart{{Text: instruction}}},
		GenerationConfig:  googleGenerationConfig{MaxOutputTokens: request.MaxOutputTokens},
	}
	switch profile.OutputMode {
	case OutputModeNativeJSONSchema:
		body.GenerationConfig.ResponseMIMEType = "application/json"
		body.GenerationConfig.ResponseSchema = request.JSONSchema
	case OutputModePromptJSON:
		body.SystemInstruction.Parts[0].Text = promptJSONInstruction + string(request.JSONSchema)
	default:
		return PreparedStructuredRequest{}, errors.New("google generateContent profile has unsupported output mode")
	}
	payload, err := json.Marshal(body, json.Deterministic(true))
	if err != nil {
		return PreparedStructuredRequest{}, errors.New("encode inference provider request")
	}
	return NewPreparedStructuredRequest(request, payload)
}

type googleGenerateContentEnvelope struct {
	Candidates     []jsontext.Value `json:"candidates"`
	PromptFeedback *struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
	UsageMetadata *googleUsageMetadata `json:"usageMetadata"`
	ModelVersion  string               `json:"modelVersion"`
	ResponseID    string               `json:"responseId"`
}

type googleUsageMetadata struct {
	PromptTokenCount     jsontext.Value `json:"promptTokenCount"`
	CandidatesTokenCount jsontext.Value `json:"candidatesTokenCount"`
}

type googleCandidate struct {
	Content      jsontext.Value `json:"content"`
	FinishReason string         `json:"finishReason"`
}

type googleCandidateContent struct {
	Role  string           `json:"role"`
	Parts []jsontext.Value `json:"parts"`
}

func (d *GoogleGenerateContentDriver) GeneratePrepared(
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
	target, err := googleGenerateContentTarget(profile.Endpoint, profile.Model)
	if err != nil {
		return DriverResponse{}, err
	}

	httpResponse, err := d.http.post(ctx, target, profile, credential, prepared.WireRequest())
	if err != nil {
		return DriverResponse{}, err
	}
	var envelope googleGenerateContentEnvelope
	if err := decodeSingleJSON(httpResponse.body, &envelope); err != nil {
		return DriverResponse{}, errors.Join(ErrInvalidStructuredOutput, errors.New("decode provider response"))
	}
	result := DriverResponse{
		ProviderRequestID: httpResponse.requestID,
		ProviderVersion:   profile.DriverVersion,
	}
	if safeProviderMetadata(envelope.ResponseID) {
		result.ProviderRequestID = envelope.ResponseID
	}
	if err := applyGoogleUsage(&result, envelope.UsageMetadata); err != nil {
		return result, err
	}
	if !safeProviderMetadata(envelope.ModelVersion) {
		return result, errors.Join(ErrInvalidStructuredOutput, errors.New("provider response is missing model version"))
	}
	result.ModelVersion = envelope.ModelVersion
	if envelope.PromptFeedback != nil && envelope.PromptFeedback.BlockReason != "" {
		return result, errors.Join(ErrInvalidStructuredOutput, errors.New("provider blocked the structured request"))
	}
	if len(envelope.Candidates) != 1 {
		return result, errors.Join(ErrInvalidStructuredOutput, errors.New("provider response must contain exactly one candidate"))
	}
	candidate, err := extractGoogleCandidate(envelope.Candidates[0])
	if err != nil {
		return result, err
	}
	result.CandidateJSON = candidate
	return result, nil
}

func googleGenerateContentTarget(endpoint, model string) (string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" || parsed.Opaque != "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.ForceQuery {
		return "", errors.New("google generateContent endpoint join is unsafe")
	}
	if parsed.RawPath != "" || unsafeGooglePath(parsed.Path) || containsUnicodeControl(parsed.Path) ||
		model == "." || model == ".." || containsUnicodeControl(model) {
		return "", errors.New("google generateContent endpoint join is unsafe")
	}
	basePath := strings.TrimRight(parsed.Path, "/")
	escapedBasePath := strings.TrimRight(parsed.EscapedPath(), "/")
	escapedModel := url.PathEscape(model)
	parsed.Path = basePath + "/models/" + model + ":generateContent"
	parsed.RawPath = escapedBasePath + "/models/" + escapedModel + ":generateContent"
	return parsed.String(), nil
}

func containsUnicodeControl(value string) bool {
	for _, char := range value {
		if unicode.IsControl(char) {
			return true
		}
	}
	return false
}

func unsafeGooglePath(path string) bool {
	segments := strings.Split(strings.Trim(path, "/"), "/")
	for index, segment := range segments {
		if segment == "." || segment == ".." || (segment == "" && index > 0) {
			return true
		}
	}
	return false
}

func applyGoogleUsage(result *DriverResponse, usage *googleUsageMetadata) error {
	if usage == nil || (len(usage.PromptTokenCount) == 0 && len(usage.CandidatesTokenCount) == 0) {
		return nil
	}
	if len(usage.PromptTokenCount) == 0 || len(usage.CandidatesTokenCount) == 0 {
		return errors.Join(ErrInvalidStructuredOutput, errors.New("provider returned invalid token usage"))
	}
	if bytes.Equal(bytes.TrimSpace(usage.PromptTokenCount), []byte("null")) ||
		bytes.Equal(bytes.TrimSpace(usage.CandidatesTokenCount), []byte("null")) {
		return errors.Join(ErrInvalidStructuredOutput, errors.New("provider returned invalid token usage"))
	}
	var promptTokens, candidateTokens int64
	if decodeSingleJSON(usage.PromptTokenCount, &promptTokens) != nil ||
		decodeSingleJSON(usage.CandidatesTokenCount, &candidateTokens) != nil ||
		promptTokens < 0 || candidateTokens < 0 {
		return errors.Join(ErrInvalidStructuredOutput, errors.New("provider returned invalid token usage"))
	}
	result.UsageKnown = true
	result.Usage = TokenUsage{
		InputTokens: promptTokens, OutputTokens: candidateTokens,
	}
	return nil
}

func extractGoogleCandidate(raw jsontext.Value) (jsontext.Value, error) {
	var candidate googleCandidate
	if err := decodeSingleJSON(raw, &candidate); err != nil {
		return nil, errors.Join(ErrInvalidStructuredOutput, errors.New("decode provider candidate"))
	}
	if candidate.FinishReason != "STOP" {
		return nil, errors.Join(ErrInvalidStructuredOutput, errors.New("provider candidate did not finish successfully"))
	}
	var content googleCandidateContent
	if err := decodeSingleJSON(candidate.Content, &content); err != nil || content.Role != "model" || len(content.Parts) == 0 {
		return nil, errors.Join(ErrInvalidStructuredOutput, errors.New("provider candidate content is invalid"))
	}
	var joined strings.Builder
	for _, part := range content.Parts {
		text, err := decodeGoogleTextPart(part)
		if err != nil || text == "" {
			return nil, errors.Join(ErrInvalidStructuredOutput, errors.New("provider candidate part is invalid"))
		}
		joined.WriteString(text)
	}
	candidateJSON := []byte(joined.String())
	var decoded any
	if err := decodeSingleJSONUseNumber(candidateJSON, &decoded); err != nil {
		return nil, errors.Join(ErrInvalidStructuredOutput, errors.New("provider returned invalid structured JSON"))
	}
	return append(jsontext.Value(nil), candidateJSON...), nil
}

func decodeGoogleTextPart(raw jsontext.Value) (string, error) {
	decoder := jsontext.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.ReadToken()
	if err != nil || token.Kind() != '{' || decoder.PeekKind() != '"' {
		return "", errors.New("invalid text part")
	}
	name, err := decoder.ReadToken()
	if err != nil || name.String() != "text" {
		return "", errors.New("invalid text part")
	}
	var text string
	if err := json.UnmarshalDecode(decoder, &text); err != nil || (decoder.PeekKind() != '}' && decoder.PeekKind() != ']' && decoder.PeekKind() != 0) {
		return "", errors.New("invalid text part")
	}
	closing, err := decoder.ReadToken()
	if err != nil || closing.Kind() != '}' {
		return "", errors.New("invalid text part")
	}
	if _, err := decoder.ReadToken(); !errors.Is(err, io.EOF) {
		return "", errors.New("invalid text part")
	}
	return text, nil
}

var _ StructuredDriver = (*GoogleGenerateContentDriver)(nil)
