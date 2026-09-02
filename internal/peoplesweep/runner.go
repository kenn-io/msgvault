package peoplesweep

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
)

const (
	maxStructuredInputBytes       = 128 << 10
	maxStructuredSchemaBytes      = 64 << 10
	maxStructuredOutputTokens     = 32_768
	maxValidationCandidateBytes   = 1 << 20
	maxValidationMessages         = 32
	maxValidationMessageBytes     = 256
	maxRepairStructuredInputBytes = maxStructuredInputBytes + maxValidationCandidateBytes +
		maxValidationMessages*maxValidationMessageBytes + 16<<10
)

var (
	programComponentPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
	schemaNamePattern       = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	providerMetadataPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)
)

var syntheticCheckSchema = json.RawMessage(`{
	"type":"object",
	"properties":{"ok":{"type":"boolean","const":true}},
	"required":["ok"],
	"additionalProperties":false
}`)

// ProviderAuthority is the runner's complete store dependency. It cannot read
// messages, attachments, or any other archive content.
type ProviderAuthority interface {
	HasSuccessfulPersonInferenceCheck(ctx context.Context, fingerprint string) (bool, error)
	HasActivePersonInferenceConsent(ctx context.Context, fingerprint string) (bool, error)
}

// CredentialLookup resolves exactly one configured environment-variable name.
type CredentialLookup func(string) (string, bool)

// Runner enforces request, exact-consent, source, credential, and output-schema
// policy around one protocol-selected structured driver.
type Runner struct {
	config    Config
	authority ProviderAuthority
	driver    StructuredDriver
	resolver  CredentialResolver
}

type runnerExecutionSession struct {
	runner     *Runner
	profile    ProviderProfile
	provider   ProviderConfig
	credential Credential
	primary    PreparedStructuredRequest
	mu         sync.Mutex
	state      runnerExecutionState
	response   StructuredResponse
	failure    ValidationFailure
	repair     PreparedStructuredRequest
}

type runnerPreparedCall struct {
	session  *runnerExecutionSession
	prepared PreparedStructuredRequest
	kind     runnerCallKind
	ran      atomic.Bool
}

type runnerExecutionState uint8

const (
	runnerSessionPrimaryReady runnerExecutionState = iota
	runnerSessionPrimaryIssued
	runnerSessionPrimaryStarted
	runnerSessionPrimaryCompleted
	runnerSessionPrimaryRepairable
	runnerSessionRepairPrepared
	runnerSessionRepairIssued
	runnerSessionRepairStarted
	runnerSessionTerminal
)

type runnerCallKind uint8

const (
	runnerPrimaryCall runnerCallKind = iota
	runnerRepairCall
)

// NewRunner creates a gated runner without resolving a credential or touching
// the network.
func NewRunner(
	config Config,
	authority ProviderAuthority,
	registry *DriverRegistry,
	resolver CredentialResolver,
) (*Runner, error) {
	if authority == nil {
		return nil, errors.New("people inference runner requires provider authority")
	}
	if registry == nil {
		return nil, errors.New("people inference runner requires a driver registry")
	}
	if resolver == nil {
		return nil, errors.New("people inference runner requires a credential resolver")
	}
	_, provider, err := config.ActiveProviderConfig()
	if err != nil {
		return nil, err
	}
	driver, err := registry.Driver(provider.Protocol, provider)
	if err != nil {
		return nil, fmt.Errorf("select people inference driver: %w", err)
	}
	providers := make(map[string]ProviderConfig, len(config.Providers))
	for name, provider := range config.Providers {
		provider.AllowedSources = slices.Clone(provider.AllowedSources)
		providers[name] = provider
	}
	config.Providers = providers
	return &Runner{
		config: config, authority: authority, driver: driver, resolver: resolver,
	}, nil
}

// RunStructured performs one normal text-only request. Normal requests must
// carry at least one source descriptor.
func (r *Runner) RunStructured(
	ctx context.Context,
	request StructuredRequest,
) (StructuredResponse, error) {
	prepared, err := r.PrepareStructured(ctx, request)
	if err != nil {
		return StructuredResponse{}, err
	}
	return r.RunPreparedStructured(ctx, prepared)
}

// Check exercises the real provider boundary with package-owned synthetic
// input. It cannot accept archive text or source selectors.
func (r *Runner) Check(ctx context.Context) (StructuredResponse, error) {
	prepared, err := r.prepare(ctx, StructuredRequest{
		ProgramID: "provider-check", ProgramVersion: "1",
		InputText:  "Return an object with ok set to true.",
		SchemaName: "provider_check", JSONSchema: slices.Clone(syntheticCheckSchema),
		MaxOutputTokens: 16,
	}, true)
	if err != nil {
		return StructuredResponse{}, err
	}
	return r.runPrepared(ctx, prepared, true)
}

// PrepareStructured validates an ordinary request and deterministically creates
// the exact provider-input bytes without resolving credentials or launching I/O.
func (r *Runner) PrepareStructured(
	ctx context.Context,
	request StructuredRequest,
) (PreparedStructuredRequest, error) {
	return r.prepare(ctx, request, false)
}

// PrepareRepair deterministically prepares the only repair packet permitted
// for a locally invalid candidate. It reuses the exact active runner and
// original request; callers cannot add archive context through this API.
func (r *Runner) PrepareRepair(
	request StructuredRequest,
	failure ValidationFailure,
) (PreparedStructuredRequest, error) {
	if request.repair || failure.repair {
		return PreparedStructuredRequest{}, errors.New("structured inference request is already a repair")
	}
	if len(failure.Candidate) > maxValidationCandidateBytes {
		return PreparedStructuredRequest{}, errors.New("structured inference repair candidate exceeds its bounded limit")
	}
	if len(failure.Errors) == 0 || len(failure.Errors) > maxValidationMessages {
		return PreparedStructuredRequest{}, errors.New("structured inference repair validation messages exceed their bounded limit")
	}
	errorsCopy := make([]string, len(failure.Errors))
	for index, message := range failure.Errors {
		if message == "" || !utf8.ValidString(message) || len(message) > maxValidationMessageBytes {
			return PreparedStructuredRequest{}, errors.New("structured inference repair validation message is invalid")
		}
		errorsCopy[index] = message
	}
	envelope, err := json.Marshal(struct {
		OriginalRequest  StructuredRequest `json:"original_request"`
		InvalidCandidate string            `json:"invalid_candidate"`
		ValidationErrors []string          `json:"validation_errors"`
	}{
		OriginalRequest:  cloneStructuredRequest(request),
		InvalidCandidate: string(failure.Candidate), ValidationErrors: errorsCopy,
	})
	if err != nil {
		return PreparedStructuredRequest{}, errors.New("encode structured inference repair instruction")
	}
	repairRequest := cloneStructuredRequest(request)
	repairRequest.InputText = string(envelope)
	repairRequest.repair = true
	prepared, err := r.prepare(context.Background(), repairRequest, false)
	if err != nil {
		return PreparedStructuredRequest{}, fmt.Errorf("prepare structured inference repair: %w", err)
	}
	prepared.repair = true
	return prepared, nil
}

// BeginStructuredExecution performs every non-I/O identity, authority, and
// credential check needed to pin one execution identity. The returned session
// retains the credential only in memory and never exposes it to callers.
func (r *Runner) BeginStructuredExecution(
	ctx context.Context,
	primary PreparedStructuredRequest,
) (StructuredExecutionSession, error) {
	profile, err := r.config.Profile()
	if err != nil {
		return nil, err
	}
	if primary.preparedBy != r || primary.identity == nil || primary.repair || primary.Request().repair {
		return nil, errors.New("structured execution session requires its exact prepared primary")
	}
	if err := r.validatePrepared(primary, profile, false); err != nil {
		return nil, err
	}
	if err := r.requireVerifiedAndConsented(ctx, profile.Fingerprint); err != nil {
		return nil, err
	}
	profileName, provider, err := r.config.ActiveProviderConfig()
	if err != nil {
		return nil, err
	}
	credential, err := r.resolver.Resolve(profileName, cloneProviderProfile(profile))
	if err != nil {
		return nil, fmt.Errorf("resolve people provider credential: %w", err)
	}
	provider.AllowedSources = slices.Clone(provider.AllowedSources)
	return &runnerExecutionSession{runner: r, profile: cloneProviderProfile(profile), provider: provider,
		credential: credential, primary: clonePreparedStructuredRequest(primary),
		state: runnerSessionPrimaryReady}, nil
}

func (s *runnerExecutionSession) PrimaryCall(
	prepared PreparedStructuredRequest,
) (PreparedStructuredCall, error) {
	if s == nil || s.runner == nil {
		return nil, errors.New("structured execution session is invalid")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != runnerSessionPrimaryReady {
		return nil, errors.New("structured execution session primary call is unavailable")
	}
	if !samePreparedIdentity(prepared, s.primary) {
		return nil, errors.New("structured execution call does not match its bound primary")
	}
	s.state = runnerSessionPrimaryIssued
	return &runnerPreparedCall{session: s, prepared: clonePreparedStructuredRequest(prepared),
		kind: runnerPrimaryCall}, nil
}

func (s *runnerExecutionSession) SemanticValidationFailure(
	response StructuredResponse,
) (ValidationFailure, error) {
	if s == nil || s.runner == nil {
		return ValidationFailure{}, errors.New("structured execution session is invalid")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != runnerSessionPrimaryCompleted || !sameStructuredResponse(response, s.response) {
		return ValidationFailure{}, errors.New("structured execution semantic failure does not match its primary")
	}
	failure := newValidationFailure(s.response.Output, "candidate failed extraction semantics", false)
	failure.execution = s
	s.failure = cloneValidationFailure(*failure)
	s.state = runnerSessionPrimaryRepairable
	return *failure, nil
}

func (s *runnerExecutionSession) PrepareRepair(
	failure ValidationFailure,
) (PreparedStructuredRequest, error) {
	if s == nil || s.runner == nil {
		return PreparedStructuredRequest{}, errors.New("structured execution session is invalid")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == runnerSessionPrimaryReady || s.state == runnerSessionPrimaryIssued ||
		s.state == runnerSessionPrimaryStarted {
		return PreparedStructuredRequest{}, errors.New("structured execution session primary has no repairable validation failure")
	}
	if s.state != runnerSessionPrimaryRepairable || !sameValidationFailure(failure, s.failure) {
		return PreparedStructuredRequest{}, errors.New("structured execution repair does not match its primary validation failure")
	}
	if failure.repair {
		return PreparedStructuredRequest{}, errors.New("structured execution session repair is unavailable")
	}
	repair, err := s.runner.PrepareRepair(s.primary.Request(), s.failure)
	if err != nil {
		s.state = runnerSessionTerminal
		return PreparedStructuredRequest{}, err
	}
	repair.execution = s
	s.repair = clonePreparedStructuredRequest(repair)
	s.state = runnerSessionRepairPrepared
	return clonePreparedStructuredRequest(repair), nil
}

func (s *runnerExecutionSession) RepairCall(
	prepared PreparedStructuredRequest,
) (PreparedStructuredCall, error) {
	if s == nil || s.runner == nil {
		return nil, errors.New("structured execution session is invalid")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != runnerSessionRepairPrepared {
		return nil, errors.New("structured execution session repair call is unavailable")
	}
	if prepared.execution != s || !samePreparedIdentity(prepared, s.repair) {
		return nil, errors.New("structured execution call does not match its bound repair")
	}
	s.state = runnerSessionRepairIssued
	return &runnerPreparedCall{session: s, prepared: clonePreparedStructuredRequest(prepared),
		kind: runnerRepairCall}, nil
}

func samePreparedIdentity(left, right PreparedStructuredRequest) bool {
	return left.identity != nil && left.identity == right.identity && left.preparedBy == right.preparedBy &&
		left.execution == right.execution && reflect.DeepEqual(left.request, right.request) &&
		left.repair == right.repair && left.synthetic == right.synthetic &&
		left.wireSHA256 == right.wireSHA256 && bytes.Equal(left.wireRequest, right.wireRequest)
}

func clonePreparedStructuredRequest(prepared PreparedStructuredRequest) PreparedStructuredRequest {
	prepared.request = cloneStructuredRequest(prepared.request)
	prepared.wireRequest = slices.Clone(prepared.wireRequest)
	return prepared
}

func cloneStructuredResponse(response StructuredResponse) StructuredResponse {
	response.Output = slices.Clone(response.Output)
	return response
}

func cloneProviderProfile(profile ProviderProfile) ProviderProfile {
	profile.AllowedSources = slices.Clone(profile.AllowedSources)
	profile.DisclosedPacketFields = slices.Clone(profile.DisclosedPacketFields)
	profile.PolicyJSON = slices.Clone(profile.PolicyJSON)
	return profile
}

func sameStructuredResponse(left, right StructuredResponse) bool {
	return left.execution != nil && left.execution == right.execution &&
		bytes.Equal(left.Output, right.Output) &&
		left.ProviderRequestID == right.ProviderRequestID &&
		left.ProviderVersion == right.ProviderVersion &&
		left.ModelVersion == right.ModelVersion && left.Usage == right.Usage &&
		left.UsageKnown == right.UsageKnown
}

func (c *runnerPreparedCall) Execute(
	ctx context.Context,
	markStarted func(context.Context) error,
) (StructuredResponse, error) {
	if c == nil || c.session == nil || c.session.runner == nil {
		return StructuredResponse{}, errors.New("prepared structured call is invalid")
	}
	if markStarted == nil {
		return StructuredResponse{}, errors.New("prepared structured call requires a started marker")
	}
	if !c.ran.CompareAndSwap(false, true) {
		return StructuredResponse{}, errors.New("prepared structured call was already claimed")
	}
	if err := c.session.claim(ctx, c); err != nil {
		return StructuredResponse{}, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, c.session.provider.RequestTimeout)
	defer cancel()
	if err := markStarted(requestCtx); err != nil {
		c.session.terminate()
		return StructuredResponse{}, err
	}
	response, err := c.session.runner.generateAndValidateResolved(requestCtx, c.session.profile,
		c.session.credential, c.prepared)
	c.session.complete(c.kind, &response, err)
	return response, err
}

func (s *runnerExecutionSession) claim(ctx context.Context, call *runnerPreparedCall) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	want := runnerSessionPrimaryIssued
	next := runnerSessionPrimaryStarted
	if call.kind == runnerRepairCall {
		want = runnerSessionRepairIssued
		next = runnerSessionRepairStarted
	}
	bound := s.primary
	if call.kind == runnerRepairCall {
		bound = s.repair
	}
	if s.state != want || !samePreparedIdentity(call.prepared, bound) {
		s.state = runnerSessionTerminal
		return errors.New("prepared structured call does not match its execution session")
	}
	active, err := s.runner.config.Profile()
	if err != nil {
		s.state = runnerSessionTerminal
		return err
	}
	if active.Fingerprint != s.profile.Fingerprint {
		s.state = runnerSessionTerminal
		return errors.New("structured execution session provider profile changed")
	}
	if err := s.runner.requireVerifiedAndConsented(ctx, s.profile.Fingerprint); err != nil {
		s.state = runnerSessionTerminal
		return err
	}
	s.state = next
	return nil
}

func (s *runnerExecutionSession) terminate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = runnerSessionTerminal
}

func (s *runnerExecutionSession) complete(
	kind runnerCallKind,
	response *StructuredResponse,
	runErr error,
) {
	s.mu.Lock()
	defer s.mu.Unlock()
	response.execution = s
	if kind == runnerRepairCall {
		s.state = runnerSessionTerminal
		return
	}
	if failure, ok := errors.AsType[*ValidationFailure](runErr); ok {
		failure.execution = s
		s.failure = cloneValidationFailure(*failure)
		s.state = runnerSessionPrimaryRepairable
		return
	}
	if runErr != nil {
		s.state = runnerSessionTerminal
		return
	}
	s.response = cloneStructuredResponse(*response)
	s.state = runnerSessionPrimaryCompleted
}

func cloneValidationFailure(failure ValidationFailure) ValidationFailure {
	failure.Candidate = append(json.RawMessage(nil), failure.Candidate...)
	failure.Errors = slices.Clone(failure.Errors)
	return failure
}

func sameValidationFailure(left, right ValidationFailure) bool {
	return left.execution != nil && left.execution == right.execution &&
		left.repair == right.repair && left.summary == right.summary &&
		bytes.Equal(left.Candidate, right.Candidate) && slices.Equal(left.Errors, right.Errors)
}

func (r *Runner) prepare(
	_ context.Context,
	request StructuredRequest,
	synthetic bool,
) (PreparedStructuredRequest, error) {
	_, err := validateStructuredRequest(request, synthetic)
	if err != nil {
		return PreparedStructuredRequest{}, err
	}
	profile, err := r.config.Profile()
	if err != nil {
		return PreparedStructuredRequest{}, err
	}
	if err := validateRequestPolicy(request, profile, synthetic); err != nil {
		return PreparedStructuredRequest{}, err
	}
	prepared, err := r.driver.Prepare(cloneProviderProfile(profile), cloneStructuredRequest(request))
	if err != nil {
		return PreparedStructuredRequest{}, fmt.Errorf("prepare structured inference: %w", err)
	}
	prepared.synthetic = synthetic
	prepared.preparedBy = r
	prepared.identity = &preparedRequestIdentity{}
	return prepared, nil
}

// RunPreparedStructured is the single-call convenience path. Repairs require
// a pinned StructuredExecutionSession so they cannot resolve a new credential.
func (r *Runner) RunPreparedStructured(
	ctx context.Context,
	prepared PreparedStructuredRequest,
) (StructuredResponse, error) {
	if prepared.repair || prepared.Request().repair {
		return StructuredResponse{}, errors.New("structured inference repair requires a pinned execution session")
	}
	return r.runPrepared(ctx, prepared, prepared.preparedBy == r && prepared.synthetic)
}

func (r *Runner) runPrepared(
	ctx context.Context,
	prepared PreparedStructuredRequest,
	synthetic bool,
) (StructuredResponse, error) {
	profile, err := r.config.Profile()
	if err != nil {
		return StructuredResponse{}, err
	}
	if err := r.validatePrepared(prepared, profile, synthetic); err != nil {
		return StructuredResponse{}, err
	}
	if !synthetic {
		if err := r.requireVerifiedAndConsented(ctx, profile.Fingerprint); err != nil {
			return StructuredResponse{}, err
		}
	}
	return r.generateAndValidate(ctx, profile, prepared)
}

func (r *Runner) validatePrepared(
	prepared PreparedStructuredRequest,
	profile ProviderProfile,
	synthetic bool,
) error {
	if err := prepared.validateWireHash(); err != nil {
		return err
	}
	request := prepared.Request()
	_, err := validateStructuredRequest(request, synthetic)
	if err != nil {
		return err
	}
	expected, err := r.driver.Prepare(cloneProviderProfile(profile), cloneStructuredRequest(request))
	if err != nil {
		return fmt.Errorf("re-encode prepared structured inference: %w", err)
	}
	if err := expected.validateWireHash(); err != nil {
		return errors.New("prepared structured request deterministic provider encoding is invalid")
	}
	if !bytes.Equal(prepared.WireRequest(), expected.WireRequest()) {
		return errors.New("prepared structured request does not match deterministic provider encoding")
	}
	if err := validateRequestPolicy(request, profile, synthetic); err != nil {
		return err
	}
	return nil
}

func (r *Runner) requireVerifiedAndConsented(ctx context.Context, fingerprint string) error {
	verified, err := r.authority.HasSuccessfulPersonInferenceCheck(ctx, fingerprint)
	if err != nil {
		return fmt.Errorf("check exact people inference provider verification: %w", err)
	}
	if !verified {
		return errors.New("people inference requires a successful check for the exact provider profile")
	}
	active, err := r.authority.HasActivePersonInferenceConsent(ctx, fingerprint)
	if err != nil {
		return fmt.Errorf("check exact people inference consent: %w", err)
	}
	if !active {
		return fmt.Errorf("%w: people inference requires active exact consent",
			ErrPersonSweepConsentRevoked)
	}
	return nil
}

func (r *Runner) generateAndValidate(
	ctx context.Context,
	profile ProviderProfile,
	prepared PreparedStructuredRequest,
) (StructuredResponse, error) {
	profileName, provider, err := r.config.ActiveProviderConfig()
	if err != nil {
		return StructuredResponse{}, err
	}
	credential, err := r.resolver.Resolve(profileName, cloneProviderProfile(profile))
	if err != nil {
		return StructuredResponse{}, fmt.Errorf("resolve people provider credential: %w", err)
	}
	requestCtx, cancel := context.WithTimeout(ctx, provider.RequestTimeout)
	defer cancel()
	return r.generateAndValidateResolved(requestCtx, profile, credential, prepared)
}

func (r *Runner) generateAndValidateResolved(
	ctx context.Context,
	profile ProviderProfile,
	credential Credential,
	prepared PreparedStructuredRequest,
) (StructuredResponse, error) {
	driverResponse, err := r.driver.GeneratePrepared(ctx, cloneProviderProfile(profile), credential,
		clonePreparedStructuredRequest(prepared))
	if err != nil {
		if errors.Is(err, ErrInvalidStructuredOutput) {
			return structuredResponseFromDriver(driverResponse), fmt.Errorf("generate structured inference: %w", err)
		}
		return StructuredResponse{}, fmt.Errorf("generate structured inference: %w", err)
	}
	response, failure, validateErr := r.validate(prepared.Request(), driverResponse,
		prepared.synthetic)
	if validateErr != nil {
		return response, validateErr
	}
	if failure != nil {
		return response, failure
	}
	return response, nil
}

// Validate applies exact local JSON parsing and the request's resolved schema
// without performing provider I/O. A schema failure retains bounded repair
// inputs while its returned error remains safe for logs and durable history.
func (r *Runner) Validate(
	request StructuredRequest,
	driverResponse DriverResponse,
) (StructuredResponse, *ValidationFailure, error) {
	return r.validate(request, driverResponse, false)
}

func (r *Runner) validate(
	request StructuredRequest,
	driverResponse DriverResponse,
	synthetic bool,
) (StructuredResponse, *ValidationFailure, error) {
	response := structuredResponseFromDriver(driverResponse)
	resolvedSchema, err := validateStructuredRequest(request, synthetic)
	if err != nil {
		return response, nil, err
	}
	if !safeProviderMetadata(response.ProviderVersion) || !safeProviderMetadata(response.ModelVersion) {
		return response, nil, fmt.Errorf("%w: missing authoritative version metadata", ErrInvalidStructuredOutput)
	}
	if len(response.Output) > maxValidationCandidateBytes {
		return response, newValidationFailure(response.Output[:maxValidationCandidateBytes],
			"candidate exceeds the local validation limit", request.repair), nil
	}
	var output any
	if err := decodeJSONSchemaInstance(response.Output, &output); err != nil {
		return response, newValidationFailure(response.Output,
			"invalid structured JSON", request.repair), nil
	}
	if err := resolvedSchema.Validate(output); err != nil {
		return response, newValidationFailure(response.Output,
			"output does not match requested schema", request.repair), nil
	}
	return response, nil, nil
}

func newValidationFailure(candidate json.RawMessage, message string, repair bool) *ValidationFailure {
	if len(candidate) > maxValidationCandidateBytes {
		candidate = candidate[:maxValidationCandidateBytes]
	}
	if len(message) > maxValidationMessageBytes {
		message = message[:maxValidationMessageBytes]
	}
	return &ValidationFailure{Candidate: append(json.RawMessage(nil), candidate...),
		Errors: []string{message}, repair: repair, summary: message}
}

func structuredResponseFromDriver(response DriverResponse) StructuredResponse {
	return StructuredResponse{
		Output:            append(json.RawMessage(nil), response.CandidateJSON...),
		ProviderRequestID: response.ProviderRequestID,
		ProviderVersion:   response.ProviderVersion,
		ModelVersion:      response.ModelVersion,
		Usage:             response.Usage,
		UsageKnown:        response.UsageKnown,
	}
}

func decodeJSONSchemaInstance(data []byte, destination *any) error {
	var decoded any
	if err := decodeSingleJSONUseNumber(data, &decoded); err != nil {
		return err
	}
	normalized, err := normalizeJSONSchemaNumbers(decoded)
	if err != nil {
		return err
	}
	*destination = normalized
	return nil
}

func normalizeJSONSchemaNumbers(value any) (any, error) {
	switch typed := value.(type) {
	case json.Number:
		if !strings.ContainsAny(string(typed), ".eE") {
			integer, err := strconv.ParseInt(string(typed), 10, 64)
			if err == nil {
				return integer, nil
			}
		}
		number, err := strconv.ParseFloat(string(typed), 64)
		if err != nil {
			return nil, errors.New("JSON number is outside the supported schema range")
		}
		return number, nil
	case []any:
		for index := range typed {
			normalized, err := normalizeJSONSchemaNumbers(typed[index])
			if err != nil {
				return nil, err
			}
			typed[index] = normalized
		}
		return typed, nil
	case map[string]any:
		for key, item := range typed {
			normalized, err := normalizeJSONSchemaNumbers(item)
			if err != nil {
				return nil, err
			}
			typed[key] = normalized
		}
		return typed, nil
	default:
		return value, nil
	}
}

func safeProviderMetadata(value string) bool {
	return len(value) <= 128 && providerMetadataPattern.MatchString(value)
}

func validateStructuredRequest(
	request StructuredRequest,
	synthetic bool,
) (*jsonschema.Resolved, error) {
	if !programComponentPattern.MatchString(request.ProgramID) {
		return nil, errors.New("structured inference program_id is invalid")
	}
	if !programComponentPattern.MatchString(request.ProgramVersion) {
		return nil, errors.New("structured inference program_version is invalid")
	}
	if !schemaNamePattern.MatchString(request.SchemaName) {
		return nil, errors.New("structured inference schema_name is invalid")
	}
	inputLimit := maxStructuredInputBytes
	if request.repair {
		inputLimit = maxRepairStructuredInputBytes
	}
	if request.InputText == "" || !utf8.ValidString(request.InputText) ||
		len(request.InputText) > inputLimit {
		return nil, errors.New("structured inference input_text must be valid UTF-8 from 1 through 131072 bytes")
	}
	if len(request.JSONSchema) == 0 || len(request.JSONSchema) > maxStructuredSchemaBytes {
		return nil, errors.New("structured inference JSON Schema must be from 1 through 65536 bytes")
	}
	if request.MaxOutputTokens < 1 || request.MaxOutputTokens > maxStructuredOutputTokens {
		return nil, errors.New("structured inference max_output_tokens must be from 1 through 32768")
	}
	if !synthetic && len(request.Sources) == 0 {
		return nil, errors.New("structured inference requires at least one source")
	}
	for _, source := range request.Sources {
		if err := validateObservedOn(source.ObservedOn); err != nil {
			return nil, err
		}
	}
	var schema jsonschema.Schema
	if err := decodeSingleJSON(request.JSONSchema, &schema); err != nil {
		return nil, errors.New("structured inference JSON Schema is invalid")
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		return nil, errors.New("structured inference JSON Schema cannot be resolved")
	}
	return resolved, nil
}

func validateRequestPolicy(
	request StructuredRequest,
	profile ProviderProfile,
	synthetic bool,
) error {
	if synthetic {
		if len(request.Sources) != 0 || request.ContainsSensitive {
			return errors.New("synthetic inference check cannot include archive sources or sensitive content")
		}
		return nil
	}
	if request.ContainsSensitive && !profile.AllowSensitive {
		return errors.New("people inference profile does not allow sensitive input")
	}
	for _, source := range request.Sources {
		if !slices.Contains(profile.AllowedSources, source.Class) {
			return fmt.Errorf("people inference source class %q is not allowed", source.Class)
		}
		if source.ObservedOn < profile.SourceSince ||
			(profile.SourceUntil != "" && source.ObservedOn > profile.SourceUntil) {
			return fmt.Errorf("people inference source date %s is outside the consented date range",
				source.ObservedOn)
		}
	}
	return nil
}

func validateObservedOn(value string) error {
	parsed, err := time.Parse(time.DateOnly, value)
	if err != nil || parsed.Format(time.DateOnly) != value {
		return fmt.Errorf("structured inference source observed_on %q must be YYYY-MM-DD", value)
	}
	return nil
}

var _ StructuredRunner = (*Runner)(nil)
