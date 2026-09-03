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
	"time"
)

const capabilityProfileName = "provider-check"

// NegotiatedCapabilities is the exact synthetic representation accepted by a
// selected protocol, endpoint, and model. Callers decide whether to persist
// these values as part of an onboarding transaction.
type NegotiatedCapabilities struct {
	OutputMode          OutputMode
	TokenLimitParameter string
	ReasoningEffort     string
	ReasoningMode       string
	DriverVersion       string
	Response            StructuredResponse
}

// NegotiationStage identifies the bounded phase that produced a terminal
// capability-negotiation error.
type NegotiationStage string

const (
	NegotiationStageDriverUnavailable NegotiationStage = "driver_unavailable"
	NegotiationStageSettingsInvalid   NegotiationStage = "settings_invalid"
	NegotiationStageProbe             NegotiationStage = "probe"
	NegotiationStageReasoningProbe    NegotiationStage = "reasoning_probe"
	NegotiationStageExhausted         NegotiationStage = "exhausted"
)

// NegotiationError carries safe attempt context while keeping its underlying
// provider error available to errors.As.
type NegotiationError struct {
	Stage               NegotiationStage
	OutputMode          OutputMode
	TokenLimitParameter string
	Reasoning           bool
	StatusCode          int
	RequestID           string
	Capability          ProviderCapabilityError
	Diagnostics         ProviderDiagnostics

	cause error
}

func (e *NegotiationError) Error() string {
	if e == nil {
		return ""
	}
	parts := make([]string, 0, 9)
	if negotiationStageIsKnown(e.Stage) {
		parts = append(parts, "stage="+string(e.Stage))
	}
	if e.StatusCode != 0 {
		parts = append(parts, fmt.Sprintf("status=%d", e.StatusCode))
	}
	if outputModeIsKnown(e.OutputMode) {
		parts = append(parts, "output_mode="+string(e.OutputMode))
	}
	if tokenLimitParameterIsKnown(e.TokenLimitParameter) && e.TokenLimitParameter != "" {
		parts = append(parts, "token_limit="+e.TokenLimitParameter)
	}
	if e.Reasoning {
		parts = append(parts, "reasoning=true")
	}
	if safeProviderMetadata(e.RequestID) && e.RequestID != "" {
		parts = append(parts, "request_id="+e.RequestID)
	}
	if providerEnvelopeIsKnown(e.Diagnostics.Envelope) {
		parts = append(parts, "envelope="+string(e.Diagnostics.Envelope))
	}
	if providerDiagnosticCodeIsKnown(e.Diagnostics.Code) {
		parts = append(parts, "code="+string(e.Diagnostics.Code))
	}
	if providerDiagnosticFieldIsKnown(e.Diagnostics.Field) {
		parts = append(parts, "field="+string(e.Diagnostics.Field))
	}
	summary := negotiationSummary(e.Stage)
	if len(parts) == 0 {
		return summary
	}
	return summary + " (" + strings.Join(parts, " ") + ")"
}

func (e *NegotiationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func newNegotiationError(
	stage NegotiationStage,
	mode OutputMode,
	tokenParameter string,
	reasoning bool,
	cause error,
) *NegotiationError {
	result := &NegotiationError{
		Stage: stage, OutputMode: mode, TokenLimitParameter: tokenParameter,
		Reasoning: reasoning,
	}
	var providerErr *ProviderError
	if errors.As(cause, &providerErr) {
		result.StatusCode = providerErr.StatusCode
		result.RequestID = providerErr.RequestID
		result.Capability = providerErr.Capability
		result.Diagnostics = providerErr.Diagnostics
		result.cause = providerErr
	}
	return result
}

func negotiationSummary(stage NegotiationStage) string {
	switch stage {
	case NegotiationStageDriverUnavailable:
		return "provider capability negotiation is unavailable"
	case NegotiationStageSettingsInvalid:
		return "provider capability negotiation settings are invalid"
	case NegotiationStageProbe:
		return "provider capability negotiation failed"
	case NegotiationStageReasoningProbe:
		return "provider capability negotiation rejected requested reasoning settings"
	case NegotiationStageExhausted:
		return "provider capability negotiation found no supported structured output mode"
	default:
		return "provider capability negotiation failed"
	}
}

func negotiationStageIsKnown(stage NegotiationStage) bool {
	switch stage {
	case NegotiationStageDriverUnavailable, NegotiationStageSettingsInvalid,
		NegotiationStageProbe, NegotiationStageReasoningProbe, NegotiationStageExhausted:
		return true
	default:
		return false
	}
}

func outputModeIsKnown(mode OutputMode) bool {
	return mode == OutputModeNativeJSONSchema || mode == OutputModeJSONObject || mode == OutputModePromptJSON
}

func tokenLimitParameterIsKnown(parameter string) bool {
	return parameter == "" || parameter == "max_completion_tokens" || parameter == "max_tokens" || parameter == "max_output_tokens"
}

func providerEnvelopeIsKnown(envelope ProviderEnvelope) bool {
	return envelope == ProviderEnvelopeRecognized || envelope == ProviderEnvelopeOtherClass || envelope == ProviderEnvelopeUnreadable
}

func providerDiagnosticCodeIsKnown(code ProviderDiagnosticCode) bool {
	return code == ProviderDiagnosticCodeRejectedField || code == ProviderDiagnosticCodeRejectedRepresentation || code == ProviderDiagnosticCodeUnclassified
}

func providerDiagnosticFieldIsKnown(field ProviderDiagnosticField) bool {
	switch field {
	case ProviderDiagnosticFieldTokenLimit, ProviderDiagnosticFieldReasoning,
		ProviderDiagnosticFieldRepresentation, ProviderDiagnosticFieldForeign,
		ProviderDiagnosticFieldMalformed, ProviderDiagnosticFieldAbsent:
		return true
	default:
		return false
	}
}

// CapabilityChecker owns setup-only synthetic negotiation. It has no archive,
// authority, consent, history, credential-store, or persistence dependency.
type CapabilityChecker struct {
	registry *DriverRegistry
}

// NewCapabilityChecker constructs a caller-invoked setup checker.
func NewCapabilityChecker(registry *DriverRegistry) *CapabilityChecker {
	return &CapabilityChecker{registry: registry}
}

// Negotiate checks one selected protocol, endpoint, model, and credential with
// package-owned synthetic input. It never repairs, falls back, or changes the
// selected provider identity.
func (c *CapabilityChecker) Negotiate(
	ctx context.Context,
	candidate ProviderConfig,
	credential Credential,
) (NegotiatedCapabilities, error) {
	driver, err := c.registry.capabilityDriver(candidate.Protocol)
	if err != nil {
		return NegotiatedCapabilities{}, newNegotiationError(NegotiationStageDriverUnavailable, "", "", false, nil)
	}
	if err := validateCapabilityReasoning(candidate); err != nil {
		return NegotiatedCapabilities{}, newNegotiationError(NegotiationStageSettingsInvalid, "", "", false, nil)
	}

	// reasoningMissed records that at least one classified reasoning
	// follow-up miss actually occurred, so the terminal diagnosis names the
	// rejected reasoning settings instead of the generic no-supported-mode
	// outcome when every viable base attempt succeeded.
	reasoningMissed := false
	var lastCapabilityMiss error
	var lastCapabilityMode OutputMode
	var lastCapabilityTokenParameter string
	var lastReasoningMiss error
	var lastReasoningMode OutputMode
	var lastReasoningTokenParameter string
	for _, mode := range capabilityOutputModes(candidate.Protocol) {
		for _, tokenParameter := range capabilityTokenParameters(candidate.Protocol) {
			base, profileErr := capabilityProfile(candidate, mode, tokenParameter, false)
			if profileErr != nil {
				return NegotiatedCapabilities{}, newNegotiationError(NegotiationStageSettingsInvalid, "", "", false, nil)
			}
			response, attemptErr := runCapabilityAttempt(ctx, candidate.RequestTimeout,
				driver, base, credential)
			if attemptErr != nil {
				if capabilityMiss(attemptErr) {
					lastCapabilityMiss = attemptErr
					lastCapabilityMode = mode
					lastCapabilityTokenParameter = tokenParameter
					continue
				}
				return NegotiatedCapabilities{}, newNegotiationError(NegotiationStageProbe, mode, tokenParameter, false, attemptErr)
			}

			result := NegotiatedCapabilities{
				OutputMode: mode, TokenLimitParameter: tokenParameter,
				DriverVersion: base.DriverVersion, Response: response,
			}
			if !capabilityReasoningRequested(candidate) {
				result.ReasoningEffort = candidate.ReasoningEffort
				result.ReasoningMode = candidate.ReasoningMode
				return result, nil
			}

			reasoningProfile, profileErr := capabilityProfile(candidate, mode, tokenParameter, true)
			if profileErr != nil {
				return NegotiatedCapabilities{}, newNegotiationError(NegotiationStageSettingsInvalid, "", "", false, nil)
			}
			reasoningResponse, reasoningErr := runCapabilityAttempt(ctx, candidate.RequestTimeout,
				driver, reasoningProfile, credential)
			if reasoningErr != nil {
				if capabilityMiss(reasoningErr) {
					reasoningMissed = true
					lastReasoningMiss = reasoningErr
					lastReasoningMode = mode
					lastReasoningTokenParameter = tokenParameter
					continue
				}
				return NegotiatedCapabilities{}, newNegotiationError(NegotiationStageReasoningProbe, mode, tokenParameter, true, reasoningErr)
			}
			result.ReasoningEffort = candidate.ReasoningEffort
			result.ReasoningMode = candidate.ReasoningMode
			result.Response = reasoningResponse
			return result, nil
		}
	}
	if reasoningMissed {
		return NegotiatedCapabilities{}, newNegotiationError(NegotiationStageReasoningProbe,
			lastReasoningMode, lastReasoningTokenParameter, true, lastReasoningMiss)
	}
	return NegotiatedCapabilities{}, newNegotiationError(NegotiationStageExhausted,
		lastCapabilityMode, lastCapabilityTokenParameter, false, lastCapabilityMiss)
}

func capabilityOutputModes(protocol Protocol) []OutputMode {
	switch protocol {
	case ProtocolOpenAIChat, ProtocolOpenAIResponses:
		return []OutputMode{OutputModeNativeJSONSchema, OutputModeJSONObject, OutputModePromptJSON}
	case ProtocolAnthropicMessages, ProtocolGoogleGenerateContent:
		return []OutputMode{OutputModeNativeJSONSchema, OutputModePromptJSON}
	default:
		return nil
	}
}

func capabilityTokenParameters(protocol Protocol) []string {
	if protocol == ProtocolOpenAIChat {
		return []string{"max_completion_tokens", "max_tokens"}
	}
	return []string{""}
}

func validateCapabilityReasoning(candidate ProviderConfig) error {
	if err := validateReasoning(candidate); err != nil {
		return err
	}
	switch candidate.Protocol {
	case ProtocolOpenAIChat:
		return nil
	case ProtocolOpenAIResponses:
		if candidate.ReasoningMode == "" || candidate.ReasoningMode == reasoningModeProviderDefault {
			return nil
		}
	case ProtocolAnthropicMessages, ProtocolGoogleGenerateContent:
		if candidate.ReasoningEffort == "" &&
			(candidate.ReasoningMode == "" || candidate.ReasoningMode == reasoningModeProviderDefault) {
			return nil
		}
	case ProtocolCodexAppServer:
		return errors.New("reasoning capability negotiation is unavailable for codex app server")
	}
	return errors.New("reasoning settings are not represented by the selected protocol")
}

func capabilityReasoningRequested(candidate ProviderConfig) bool {
	return candidate.ReasoningEffort != "" ||
		(candidate.ReasoningMode != "" && candidate.ReasoningMode != reasoningModeProviderDefault)
}

func capabilityProfile(
	candidate ProviderConfig,
	mode OutputMode,
	tokenParameter string,
	includeReasoning bool,
) (ProviderProfile, error) {
	credentialSource := CredentialStored
	if candidate.Auth == AuthNone {
		credentialSource = CredentialNone
	}
	provider := ProviderConfig{
		Protocol: candidate.Protocol, Endpoint: candidate.Endpoint, Model: candidate.Model,
		Auth: candidate.Auth, Credential: credentialSource,
		OutputMode: mode, TokenLimitParameter: tokenParameter,
		DriverVersion:    defaultDriverVersion(candidate.Protocol),
		RetentionPosture: "synthetic-check-only", TrainingPosture: "synthetic-check-only",
		AllowedSources: []SourceClass{SourceConversationText}, SourceSince: "1970-01-01",
		RequestTimeout: candidate.RequestTimeout,
	}
	if includeReasoning || !capabilityReasoningRequested(candidate) {
		provider.ReasoningEffort = candidate.ReasoningEffort
		provider.ReasoningMode = candidate.ReasoningMode
	}
	config := Config{
		Enabled: true, Provider: ProviderSelection{Name: capabilityProfileName},
		Providers: map[string]ProviderConfig{capabilityProfileName: provider},
	}
	config.ApplyDefaults()
	return config.Profile()
}

func runCapabilityAttempt(
	ctx context.Context,
	timeout time.Duration,
	driver StructuredDriver,
	profile ProviderProfile,
	credential Credential,
) (StructuredResponse, error) {
	request := capabilitySyntheticRequest()
	prepared, err := driver.Prepare(profile, request)
	if err != nil {
		return StructuredResponse{}, err
	}
	if timeout <= 0 {
		timeout = time.Minute
	}
	attemptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	driverResponse, err := driver.GeneratePrepared(attemptCtx, profile, credential, prepared)
	if err != nil {
		return StructuredResponse{}, err
	}
	return validateCapabilityResponse(driverResponse)
}

// capabilitySyntheticRequest exercises the exact frozen extraction schema
// and schema name every real sweep sends verbatim, so a provider that only
// accepts trivial native schemas fails negotiation instead of persisting an
// output mode that rejects every actual sweep. The input stays package-owned
// synthetic text with no archive content, and the minimal valid instance of
// the extraction schema is an empty claims array.
func capabilitySyntheticRequest() StructuredRequest {
	return StructuredRequest{
		ProgramID: "provider-check", ProgramVersion: "1",
		InputText:  "Return an object with claims set to an empty array.",
		SchemaName: ExtractionSchemaName, JSONSchema: ExtractionJSONSchema(),
		MaxOutputTokens: 16,
	}
}

func validateCapabilityResponse(driverResponse DriverResponse) (StructuredResponse, error) {
	response := structuredResponseFromDriver(driverResponse)
	if !safeProviderMetadata(response.ProviderVersion) ||
		!safeProviderMetadata(response.ModelVersion) ||
		(response.ProviderRequestID != "" && !safeProviderMetadata(response.ProviderRequestID)) {
		return StructuredResponse{}, errors.New("provider capability response metadata is invalid")
	}
	// The capability attempt sends the frozen extraction schema, so the
	// response is held to the exact closed-schema validation real sweeps use.
	if err := validateExtractionOutput(response.Output); err != nil {
		return StructuredResponse{}, errors.New("provider capability response is invalid")
	}
	output, valid := decodeUniqueCapabilityObject(response.Output)
	if !valid || len(output) != 1 {
		return StructuredResponse{}, errors.New("provider capability response is invalid")
	}
	var claims []json.RawMessage
	if _, present := output["claims"]; !present ||
		json.Unmarshal(output["claims"], &claims) != nil || len(claims) != 0 {
		return StructuredResponse{}, errors.New("provider capability response is invalid")
	}
	response.Output = append(json.RawMessage(nil), response.Output...)
	return response, nil
}

func decodeUniqueCapabilityObject(raw []byte) (map[string]json.RawMessage, bool) {
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

func capabilityMiss(err error) bool {
	var providerErr *ProviderError
	if !errors.As(err, &providerErr) {
		return false
	}
	if providerErr.StatusCode != http.StatusBadRequest && providerErr.StatusCode != http.StatusNotFound &&
		providerErr.StatusCode != http.StatusUnprocessableEntity {
		return false
	}
	return providerErr.Capability == ProviderCapabilityUnsupportedRepresentation
}
