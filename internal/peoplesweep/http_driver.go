package peoplesweep

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/http/httpguts"
)

const maxProviderResponseBytes = 1 << 20

type httpDriver struct {
	client *http.Client
}

type httpDriverResponse struct {
	body      []byte
	requestID string
}

type safeTransportError struct {
	operation string
	cause     error
}

func (e *safeTransportError) Error() string { return e.operation }
func (e *safeTransportError) Unwrap() error { return e.cause }

func newHTTPDriver(client *http.Client) *httpDriver {
	if client == nil {
		client = http.DefaultClient
	}
	isolated := *client
	isolated.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &httpDriver{client: &isolated}
}

func (d *httpDriver) post(
	ctx context.Context,
	target string,
	profile ProviderProfile,
	credential Credential,
	body []byte,
) (httpDriverResponse, error) {
	return d.postWithHeaders(ctx, target, profile, credential, body, nil)
}

func (d *httpDriver) postWithHeaders(
	ctx context.Context,
	target string,
	profile ProviderProfile,
	credential Credential,
	body []byte,
	headers map[string]string,
) (httpDriverResponse, error) {
	fixedHeaders, err := validateFixedHTTPHeaders(headers)
	if err != nil {
		return httpDriverResponse{}, err
	}
	request, err := http.NewRequestWithContext( // #nosec G704 -- the exact operator-configured endpoint is validated by ProviderProfile.
		ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return httpDriverResponse{}, errors.New("create inference provider request")
	}
	request.Header.Set("Content-Type", "application/json")
	for name, value := range fixedHeaders {
		request.Header.Set(name, value)
	}
	if err := applyHTTPCredential(request, profile.Auth, credential); err != nil {
		return httpDriverResponse{}, err
	}

	response, err := d.client.Do(request) //nolint:bodyclose // Every successful response is drained and closed by disposeHTTPResponse below.
	if err != nil {
		return httpDriverResponse{}, &safeTransportError{
			operation: "call inference provider", cause: err,
		}
	}
	defer disposeHTTPResponse(response.Body)
	requestID := safeRequestID(response.Header)
	if response.StatusCode != http.StatusOK {
		capability := ProviderCapabilityError("")
		if response.StatusCode == http.StatusBadRequest || response.StatusCode == http.StatusNotFound ||
			response.StatusCode == http.StatusUnprocessableEntity {
			errorBody, readErr := io.ReadAll(io.LimitReader(response.Body, (32<<10)+1))
			if readErr == nil && len(errorBody) <= 32<<10 {
				capability = classifyProviderCapabilityError(profile, errorBody)
			}
		}
		retryAfter := time.Duration(0)
		if retryableProviderStatus(response.StatusCode) {
			retryAfter = parseRetryAfter(response.Header.Get("Retry-After"), time.Now())
		}
		return httpDriverResponse{}, &ProviderError{
			StatusCode: response.StatusCode, RequestID: requestID, RetryAfter: retryAfter,
			Capability: capability,
		}
	}

	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxProviderResponseBytes+1))
	if err != nil {
		if requestErr := request.Context().Err(); requestErr != nil {
			return httpDriverResponse{}, &safeTransportError{
				operation: "read inference provider response", cause: requestErr,
			}
		}
		return httpDriverResponse{}, &safeTransportError{
			operation: "read inference provider response", cause: err,
		}
	}
	if requestErr := request.Context().Err(); requestErr != nil {
		return httpDriverResponse{}, &safeTransportError{
			operation: "read inference provider response", cause: requestErr,
		}
	}
	if len(responseBody) > maxProviderResponseBytes {
		return httpDriverResponse{}, errors.Join(
			ErrInvalidStructuredOutput, errors.New("provider response is too large"))
	}
	return httpDriverResponse{body: responseBody, requestID: requestID}, nil
}

func classifyProviderCapabilityError(profile ProviderProfile, body []byte) ProviderCapabilityError {
	root, ok := decodeUniqueErrorObject(body)
	if !ok {
		return ""
	}
	switch profile.Protocol {
	case ProtocolOpenAIChat, ProtocolOpenAIResponses:
		errorObject, valid := decodeUniqueErrorObject(root["error"])
		parameter, parameterPresent, parameterValid := rawOptionalJSONString(errorObject, "param")
		if valid && parameterValid && rawJSONString(errorObject["type"]) == "invalid_request_error" &&
			capabilityCodeMatchesProfile(profile, rawJSONString(errorObject["code"]), parameter, parameterPresent) {
			return ProviderCapabilityUnsupportedRepresentation
		}
	case ProtocolAnthropicMessages:
		errorObject, valid := decodeUniqueErrorObject(root["error"])
		parameter, parameterPresent, parameterValid := rawOptionalJSONString(errorObject, "param")
		if rawJSONString(root["type"]) == "error" && valid &&
			parameterValid &&
			rawJSONString(errorObject["type"]) == "invalid_request_error" &&
			capabilityCodeMatchesProfile(profile, rawJSONString(errorObject["code"]), parameter, parameterPresent) {
			return ProviderCapabilityUnsupportedRepresentation
		}
	case ProtocolGoogleGenerateContent:
		errorObject, valid := decodeUniqueErrorObject(root["error"])
		if !valid || rawJSONString(errorObject["status"]) != "INVALID_ARGUMENT" {
			return ""
		}
		var details []json.RawMessage
		if err := json.Unmarshal(errorObject["details"], &details); err != nil || len(details) > 32 {
			return ""
		}
		relevantDetails := 0
		matched := false
		for _, raw := range details {
			detail, valid := decodeUniqueErrorObject(raw)
			if !valid {
				return ""
			}
			if rawJSONString(detail["@type"]) != "type.googleapis.com/google.rpc.ErrorInfo" ||
				rawJSONString(detail["domain"]) != "generativelanguage.googleapis.com" {
				continue
			}
			relevantDetails++
			parameter := ""
			parameterPresent := false
			if rawMetadata, present := detail["metadata"]; present {
				metadata, metadataValid := decodeUniqueErrorObject(rawMetadata)
				if !metadataValid {
					return ""
				}
				var parameterValid bool
				parameter, parameterPresent, parameterValid = rawOptionalJSONString(metadata, "parameter")
				if !parameterValid {
					return ""
				}
			}
			if capabilityCodeMatchesProfile(profile, rawJSONString(detail["reason"]), parameter, parameterPresent) {
				matched = true
			}
		}
		if relevantDetails == 1 && matched {
			return ProviderCapabilityUnsupportedRepresentation
		}
	case ProtocolCodexAppServer:
		return ""
	}
	return ""
}

func capabilityCodeMatchesProfile(
	profile ProviderProfile,
	code string,
	parameter string,
	parameterPresent bool,
) bool {
	genericCode := false
	representationCode := false
	switch profile.Protocol {
	case ProtocolOpenAIChat, ProtocolOpenAIResponses:
		genericCode = code == "unsupported_parameter" || code == "unsupported_value"
		representationCode = code == "unsupported_response_format" || code == "unsupported_json_schema"
	case ProtocolAnthropicMessages:
		genericCode = code == "unsupported_parameter" || code == "unsupported_value"
		representationCode = code == "unsupported_json_schema"
	case ProtocolGoogleGenerateContent:
		genericCode = code == "UNSUPPORTED_PARAMETER" || code == "UNSUPPORTED_VALUE"
		representationCode = code == "UNSUPPORTED_RESPONSE_FORMAT" || code == "UNSUPPORTED_JSON_SCHEMA"
	case ProtocolCodexAppServer:
		return false
	}
	if genericCode {
		return capabilityParameterMatchesProfile(profile, parameter)
	}
	if !representationCode || !capabilityRepresentationCodeMatchesProfile(profile, code) {
		return false
	}
	return !parameterPresent
}

func capabilityParameterMatchesProfile(profile ProviderProfile, parameter string) bool {
	switch profile.Protocol {
	case ProtocolOpenAIChat:
		if profile.TokenLimitParameter != "" && parameter == profile.TokenLimitParameter {
			return true
		}
		if profile.ReasoningEffort != "" && parameter == "reasoning_effort" {
			return true
		}
		if (profile.ReasoningMode == "enabled" || profile.ReasoningMode == "disabled") &&
			(parameter == "reasoning" || parameter == "reasoning.enabled") {
			return true
		}
		return capabilityRepresentationParameterMatchesProfile(profile, parameter)
	case ProtocolOpenAIResponses:
		if parameter == "max_output_tokens" {
			return true
		}
		if profile.ReasoningEffort != "" && (parameter == "reasoning" || parameter == "reasoning.effort") {
			return true
		}
		return capabilityRepresentationParameterMatchesProfile(profile, parameter)
	case ProtocolAnthropicMessages:
		if parameter == "max_tokens" {
			return true
		}
		return capabilityRepresentationParameterMatchesProfile(profile, parameter)
	case ProtocolGoogleGenerateContent:
		if parameter == "generationConfig.maxOutputTokens" {
			return true
		}
		return capabilityRepresentationParameterMatchesProfile(profile, parameter)
	default:
		return false
	}
}

func capabilityRepresentationCodeMatchesProfile(profile ProviderProfile, code string) bool {
	switch profile.Protocol {
	case ProtocolOpenAIChat, ProtocolOpenAIResponses:
		switch profile.OutputMode {
		case OutputModeNativeJSONSchema:
			return code == "unsupported_response_format" || code == "unsupported_json_schema"
		case OutputModeJSONObject:
			return code == "unsupported_response_format"
		case OutputModePromptJSON:
			return false
		}
	case ProtocolAnthropicMessages:
		return profile.OutputMode == OutputModeNativeJSONSchema && code == "unsupported_json_schema"
	case ProtocolGoogleGenerateContent:
		return profile.OutputMode == OutputModeNativeJSONSchema &&
			(code == "UNSUPPORTED_RESPONSE_FORMAT" || code == "UNSUPPORTED_JSON_SCHEMA")
	case ProtocolCodexAppServer:
		return false
	}
	return false
}

func capabilityRepresentationParameterMatchesProfile(profile ProviderProfile, parameter string) bool {
	switch profile.Protocol {
	case ProtocolOpenAIChat:
		switch profile.OutputMode {
		case OutputModeNativeJSONSchema:
			return parameter == "response_format" || parameter == "response_format.type" || parameter == "response_format.json_schema"
		case OutputModeJSONObject:
			return parameter == "response_format" || parameter == "response_format.type"
		case OutputModePromptJSON:
			return false
		}
	case ProtocolOpenAIResponses:
		switch profile.OutputMode {
		case OutputModeNativeJSONSchema:
			return parameter == "text.format" || parameter == "text.format.type" || parameter == "text.format.schema"
		case OutputModeJSONObject:
			return parameter == "text.format" || parameter == "text.format.type"
		case OutputModePromptJSON:
			return false
		}
	case ProtocolAnthropicMessages:
		return profile.OutputMode == OutputModeNativeJSONSchema &&
			(parameter == "tools" || parameter == "tool_choice")
	case ProtocolGoogleGenerateContent:
		return profile.OutputMode == OutputModeNativeJSONSchema &&
			(parameter == "generationConfig.responseMimeType" || parameter == "generationConfig.responseSchema")
	case ProtocolCodexAppServer:
		return false
	}
	return false
}

func decodeUniqueErrorObject(raw []byte) (map[string]json.RawMessage, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return nil, false
	}
	result := make(map[string]json.RawMessage)
	for decoder.More() {
		token, tokenErr := decoder.Token()
		key, valid := token.(string)
		if tokenErr != nil || !valid {
			return nil, false
		}
		if _, duplicate := result[key]; duplicate {
			return nil, false
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return nil, false
		}
		result[key] = value
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') {
		return nil, false
	}
	var trailing any
	return result, errors.Is(decoder.Decode(&trailing), io.EOF)
}

func rawJSONString(raw json.RawMessage) string {
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return value
}

func rawOptionalJSONString(object map[string]json.RawMessage, key string) (string, bool, bool) {
	raw, present := object[key]
	if !present {
		return "", false, true
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return "", true, false
	}
	return value, true, true
}

func validateFixedHTTPHeaders(headers map[string]string) (map[string]string, error) {
	validated := make(map[string]string, len(headers))
	for name, value := range headers {
		if !httpguts.ValidHeaderFieldName(name) || !safeHTTPHeaderValue(value) {
			return nil, errors.New("inference provider request header is invalid")
		}
		canonical := strings.ToLower(name)
		if canonical != "anthropic-version" || value != defaultAnthropicVersion {
			return nil, errors.New("inference provider request header is not allowed")
		}
		if _, duplicate := validated[canonical]; duplicate {
			return nil, errors.New("inference provider request header is duplicated")
		}
		validated[canonical] = value
	}
	return validated, nil
}

func applyHTTPCredential(request *http.Request, scheme AuthScheme, credential Credential) error {
	if scheme == AuthNone {
		if credential.hasValue() {
			return errors.New("unauthenticated inference provider does not accept a credential")
		}
		return nil
	}
	if credential.Scheme != scheme {
		return errors.New("people provider credential authentication scheme does not match profile")
	}
	value := credential.Value()
	if value == "" {
		return errors.New("inference provider credential is empty")
	}
	if !safeHTTPHeaderValue(value) {
		return errors.New("inference provider credential is not a valid HTTP header value")
	}
	switch scheme {
	case AuthBearer:
		request.Header.Set("Authorization", "Bearer "+value)
	case AuthXAPIKey:
		request.Header.Set("X-Api-Key", value)
	case AuthGoogleAPIKey:
		request.Header.Set("X-Goog-Api-Key", value)
	default:
		return errors.New("unsupported people provider HTTP authentication scheme")
	}
	return nil
}

func safeHTTPHeaderValue(value string) bool {
	for _, char := range []byte(value) {
		if char < 0x20 || char == 0x7f {
			return false
		}
	}
	return true
}

func safeRequestID(header http.Header) string {
	for _, name := range []string{"x-request-id", "request-id", "x-amzn-requestid"} {
		if value := header.Get(name); safeProviderMetadata(value) {
			return value
		}
	}
	return ""
}

func disposeHTTPResponse(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 32<<10))
	_ = body.Close()
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
	seconds, err := strconv.ParseUint(value, 10, 64)
	if err == nil {
		if seconds > uint64(math.MaxInt64/int64(time.Second)) {
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
