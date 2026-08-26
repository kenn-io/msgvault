package peoplesweep

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
)

const (
	maxStructuredInputBytes   = 128 << 10
	maxStructuredSchemaBytes  = 64 << 10
	maxStructuredOutputTokens = 32_768
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

// ConsentChecker is the runner's complete store dependency. It cannot read
// messages, attachments, or any other archive content.
type ConsentChecker interface {
	HasActivePersonInferenceConsent(ctx context.Context, fingerprint string) (bool, error)
}

// CredentialLookup resolves exactly one configured environment-variable name.
type CredentialLookup func(string) (string, bool)

// Runner enforces request, exact-consent, source, credential, and output-schema
// policy around one structured transport.
type Runner struct {
	config    Config
	consent   ConsentChecker
	transport StructuredTransport
	lookup    CredentialLookup
}

// NewRunner creates a gated runner without resolving a credential or touching
// the network.
func NewRunner(
	config Config,
	consent ConsentChecker,
	transport StructuredTransport,
	lookup CredentialLookup,
) (*Runner, error) {
	if consent == nil {
		return nil, errors.New("people inference runner requires a consent checker")
	}
	if transport == nil {
		return nil, errors.New("people inference runner requires a transport")
	}
	if lookup == nil {
		return nil, errors.New("people inference runner requires a credential lookup")
	}
	config.Provider.AllowedSources = slices.Clone(config.Provider.AllowedSources)
	return &Runner{
		config: config, consent: consent, transport: transport, lookup: lookup,
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

func (r *Runner) prepare(
	_ context.Context,
	request StructuredRequest,
	synthetic bool,
) (PreparedStructuredRequest, error) {
	resolvedSchema, err := validateStructuredRequest(request, synthetic)
	if err != nil {
		return PreparedStructuredRequest{}, err
	}
	_ = resolvedSchema
	profile, err := r.config.Profile()
	if err != nil {
		return PreparedStructuredRequest{}, err
	}
	if err := validateRequestPolicy(request, profile, synthetic); err != nil {
		return PreparedStructuredRequest{}, err
	}
	prepared, err := r.transport.PrepareJSON(profile, request)
	if err != nil {
		return PreparedStructuredRequest{}, fmt.Errorf("prepare structured inference: %w", err)
	}
	prepared.synthetic = synthetic
	prepared.preparedBy = r
	return prepared, nil
}

// RunPreparedStructured rechecks consent, resolves the credential, and sends
// only the opaque prepared packet.
func (r *Runner) RunPreparedStructured(
	ctx context.Context,
	prepared PreparedStructuredRequest,
) (StructuredResponse, error) {
	return r.runPrepared(ctx, prepared, prepared.preparedBy == r && prepared.synthetic)
}

func (r *Runner) runPrepared(
	ctx context.Context,
	prepared PreparedStructuredRequest,
	synthetic bool,
) (StructuredResponse, error) {
	if err := prepared.validateWireHash(); err != nil {
		return StructuredResponse{}, err
	}
	request := prepared.Request()
	resolvedSchema, err := validateStructuredRequest(request, synthetic)
	if err != nil {
		return StructuredResponse{}, err
	}
	profile, err := r.config.Profile()
	if err != nil {
		return StructuredResponse{}, err
	}
	expected, err := r.transport.PrepareJSON(profile, request)
	if err != nil {
		return StructuredResponse{}, fmt.Errorf("re-encode prepared structured inference: %w", err)
	}
	if err := expected.validateWireHash(); err != nil {
		return StructuredResponse{}, errors.New("prepared structured request deterministic provider encoding is invalid")
	}
	if !bytes.Equal(prepared.WireRequest(), expected.WireRequest()) {
		return StructuredResponse{}, errors.New("prepared structured request does not match deterministic provider encoding")
	}
	active, err := r.consent.HasActivePersonInferenceConsent(ctx, profile.Fingerprint)
	if err != nil {
		return StructuredResponse{}, fmt.Errorf("check exact people inference consent: %w", err)
	}
	if !active {
		return StructuredResponse{}, fmt.Errorf("%w: people inference requires active exact consent",
			ErrPersonSweepConsentRevoked)
	}
	if err := validateRequestPolicy(request, profile, synthetic); err != nil {
		return StructuredResponse{}, err
	}

	credential := ""
	if profile.APIKeyEnv != "" {
		value, ok := r.lookup(profile.APIKeyEnv)
		if !ok || value == "" {
			return StructuredResponse{}, fmt.Errorf(
				"inference credential environment variable %s is not set",
				profile.APIKeyEnv)
		}
		credential = value
	}

	requestCtx, cancel := context.WithTimeout(ctx, r.config.Provider.RequestTimeout)
	defer cancel()
	response, err := r.transport.GeneratePreparedJSON(requestCtx, profile, credential, prepared)
	if err != nil {
		if errors.Is(err, ErrInvalidStructuredOutput) {
			return response, fmt.Errorf("generate structured inference: %w", err)
		}
		return StructuredResponse{}, fmt.Errorf("generate structured inference: %w", err)
	}
	if !safeProviderMetadata(response.ProviderVersion) || !safeProviderMetadata(response.ModelVersion) {
		return response, fmt.Errorf("%w: missing authoritative version metadata", ErrInvalidStructuredOutput)
	}
	var output any
	if err := decodeJSONSchemaInstance(response.Output, &output); err != nil {
		return response, fmt.Errorf("%w: invalid structured JSON", ErrInvalidStructuredOutput)
	}
	if err := resolvedSchema.Validate(output); err != nil {
		return response, fmt.Errorf("%w: output does not match requested schema", ErrInvalidStructuredOutput)
	}
	return response, nil
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
	if request.InputText == "" || !utf8.ValidString(request.InputText) ||
		len(request.InputText) > maxStructuredInputBytes {
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
