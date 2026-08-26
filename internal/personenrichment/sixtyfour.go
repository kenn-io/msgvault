package personenrichment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/jsonexact"
	"go.kenn.io/msgvault/internal/personfacts"
)

const (
	SixtyfourAdapterVersionV1 = "sixtyfour-adapter-v1"
	SixtyfourWireSchemaV1     = "sixtyfour-people-intelligence-wire-v1"
	SixtyfourProviderVersion  = "people-intelligence-api-unversioned"

	sixtyfourMaxResponseBytes = 1 << 20
)

type sixtyfourProvider struct {
	config     ProviderConfig
	credential string
	client     *http.Client
}

var errSixtyfourInvalidResponse = errors.New("invalid Sixtyfour response")

type sixtyfourHTTPResponse struct {
	body       []byte
	status     int
	retryAfter string
}

// NewSixtyfourProvider constructs the asynchronous People Intelligence
// adapter. The caller must obtain credential through EgressGate.Authorize;
// construction performs no I/O.
func NewSixtyfourProvider(cfg ProviderConfig, credential string, client *http.Client) (Provider, error) {
	if cfg.Kind != ProviderSixtyfour {
		return nil, fmt.Errorf("sixtyfour provider requires kind %q", ProviderSixtyfour)
	}
	if err := cfg.validatePolicy(true); err != nil {
		return nil, fmt.Errorf("validate Sixtyfour provider configuration: %w", err)
	}
	if strings.TrimSpace(credential) == "" {
		return nil, errors.New("sixtyfour credential is required")
	}
	if client == nil {
		return nil, errors.New("sixtyfour HTTP client is required")
	}
	cloned := *client
	cloned.Timeout = cfg.RequestTimeout
	cloned.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &sixtyfourProvider{config: cfg, credential: credential, client: &cloned}, nil
}

func SixtyfourFactConfidence(value float64) (int, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 10 {
		return 0, errors.New("sixtyfour confidence score must be finite and in [0,10]")
	}
	return int(math.Floor(value*100 + 0.5)), nil
}

type sixtyfourRequest struct {
	LeadInfo map[string]any `json:"lead_info"`
	Struct   map[string]any `json:"struct"`
	Tier     string         `json:"tier"`
}

type sixtyfourStartResponse struct {
	TaskID string `json:"task_id"`
	Status string `json:"status"`
}

type sixtyfourPollResponse struct {
	TaskID string `json:"task_id"`
	// ID is the production job-status identifier and must equal TaskID from
	// Start. It is not a stable provider person identity.
	ID           string                    `json:"id"`
	RunID        string                    `json:"run_id"`
	StartTime    string                    `json:"start_time"`
	CloseTime    string                    `json:"close_time"`
	Status       string                    `json:"status"`
	Result       *sixtyfourCompletedResult `json:"result"`
	ChargeAmount json.Number               `json:"charge_amount"`
	TaskType     string                    `json:"task_type"`
	Error        string                    `json:"error"`
}

type sixtyfourCompletedResult struct {
	StructuredData  map[string]json.RawMessage `json:"structured_data"`
	ConfidenceScore json.Number                `json:"confidence_score"`
	Findings        *[]json.RawMessage         `json:"findings"`
	Notes           string                     `json:"notes"`
	References      map[string]string          `json:"references"`
}

type sixtyfourStructField struct {
	Description string `json:"description"`
	Type        string `json:"type"`
}

func (p *sixtyfourProvider) Start(ctx context.Context, request Request) (Attempt, error) {
	if p == nil || p.client == nil {
		return Attempt{}, errors.New("sixtyfour provider is not initialized")
	}
	_, durableTargets, err := EncodeDurableAttemptTargets(request.Targets)
	if err != nil {
		return Attempt{}, fmt.Errorf("validate durable Sixtyfour targets: %w", err)
	}
	generatedStruct, fields, err := buildSixtyfourStruct(durableTargets)
	if err != nil {
		return Attempt{}, err
	}
	digest := sha256.Sum256(generatedStruct)
	schemaHash := hex.EncodeToString(digest[:])
	programFingerprint, err := ProgramFingerprint(ProgramDescriptor{
		HostMappingVersion: HostClaimMappingVersion, AdapterVersion: SixtyfourAdapterVersionV1,
		WireSchemaVersion: SixtyfourWireSchemaV1, GeneratedSchema: true,
		GeneratedSchemaHash: schemaHash,
	})
	if err != nil {
		return Attempt{}, fmt.Errorf("fingerprint Sixtyfour adapter program: %w", err)
	}
	leadInfo, err := sixtyfourLeadInfo(request.Identity)
	if err != nil {
		return Attempt{}, err
	}
	payload, err := json.Marshal(sixtyfourRequest{LeadInfo: leadInfo, Struct: fields, Tier: p.config.Tier})
	if err != nil {
		return Attempt{}, errors.New("encode Sixtyfour request")
	}
	response, err := p.do(ctx, http.MethodPost, p.config.Endpoint, payload)
	if err != nil {
		if errors.Is(err, errSixtyfourInvalidResponse) {
			return Attempt{}, sixtyfourFailure(response.status, FailureInvalidOutput, "")
		}
		return Attempt{}, sixtyfourFailure(0, FailureUncertainStart, "")
	}
	if response.status < http.StatusOK || response.status >= http.StatusMultipleChoices {
		return Attempt{}, p.httpFailure(response, true)
	}
	var wire sixtyfourStartResponse
	if err := decodeSixtyfour(response.body, &wire); err != nil || wire.Status != "RUNNING" ||
		!safeExaOpaqueID(wire.TaskID) {
		return Attempt{}, sixtyfourFailure(response.status, FailureInvalidOutput, "")
	}
	startedAt := time.Now().UTC()
	return Attempt{
		State: AttemptPending, JobID: wire.TaskID,
		PollAfter: p.config.PollInterval, StartedAt: startedAt,
		AdapterVersion: SixtyfourAdapterVersionV1, SchemaVersion: SixtyfourWireSchemaV1,
		GeneratedSchema: true, GeneratedSchemaHash: schemaHash,
		ProgramFingerprint: programFingerprint,
		Targets:            durableTargets,
	}, nil
}

func (p *sixtyfourProvider) Poll(ctx context.Context, attempt Attempt) (Result, error) {
	if p == nil || p.client == nil {
		return Result{}, errors.New("sixtyfour provider is not initialized")
	}
	if err := validateSixtyfourPollAttempt(attempt); err != nil {
		return Result{}, err
	}
	now := time.Now().UTC()
	if attempt.StartedAt.IsZero() || attempt.StartedAt.After(now) ||
		now.Sub(attempt.StartedAt) > p.config.MaxJobAge {
		return Result{}, sixtyfourFailure(0, FailureTerminal, "")
	}
	pollURL := strings.TrimRight(p.config.PollEndpoint, "/") + "/" + url.PathEscape(attempt.JobID)
	response, err := p.do(ctx, http.MethodGet, pollURL, nil)
	if err != nil {
		if errors.Is(err, errSixtyfourInvalidResponse) {
			return Result{}, sixtyfourFailure(response.status, FailureInvalidOutput, "")
		}
		return Result{}, sixtyfourFailure(0, FailureTransient, "")
	}
	if response.status < http.StatusOK || response.status >= http.StatusMultipleChoices {
		return Result{}, p.httpFailure(response, false)
	}
	var wire sixtyfourPollResponse
	if err := decodeSixtyfour(response.body, &wire); err != nil ||
		!validSixtyfourPollEnvelope(wire, attempt.JobID) {
		return Result{}, sixtyfourFailure(response.status, FailureInvalidOutput, "")
	}
	switch wire.Status {
	case "running", "pending":
		if wire.Result != nil || wire.ChargeAmount != "" || wire.Error != "" {
			return Result{}, sixtyfourFailure(response.status, FailureInvalidOutput, "")
		}
		return Result{
			State: ResultPending, JobID: attempt.JobID,
			PollAfter: p.config.PollInterval, AdapterVersion: SixtyfourAdapterVersionV1,
			SchemaVersion: SixtyfourWireSchemaV1, GeneratedSchema: true,
			GeneratedSchemaHash: attempt.GeneratedSchemaHash,
		}, nil
	case "failed", "cancelled":
		if wire.Result != nil || wire.ChargeAmount != "" {
			return Result{}, sixtyfourFailure(response.status, FailureInvalidOutput, "")
		}
		return Result{}, sixtyfourFailure(response.status, FailureTerminal, "")
	case "completed":
		if wire.Result == nil || wire.Error != "" {
			return Result{}, sixtyfourFailure(response.status, FailureInvalidOutput, "")
		}
		result, decodeErr := decodeSixtyfourCompleted(
			*wire.Result, wire.ChargeAmount, attempt)
		if decodeErr != nil {
			return Result{}, sixtyfourFailure(response.status, FailureInvalidOutput, "")
		}
		if validateErr := result.Validate(); validateErr != nil {
			return Result{}, sixtyfourFailure(response.status, FailureInvalidOutput, "")
		}
		return result, nil
	default:
		return Result{}, sixtyfourFailure(response.status, FailureInvalidOutput, "")
	}
}

func validSixtyfourPollEnvelope(wire sixtyfourPollResponse, jobID string) bool {
	if wire.TaskID != "" && wire.ID != "" {
		return false
	}
	boundID := wire.TaskID
	if boundID == "" {
		boundID = wire.ID
	}
	if boundID != jobID || !safeExaOpaqueID(boundID) {
		return false
	}
	if wire.RunID != "" && !safeExaOpaqueID(wire.RunID) {
		return false
	}
	if wire.StartTime != "" {
		if _, err := time.Parse(time.RFC3339Nano, wire.StartTime); err != nil {
			return false
		}
	}
	if wire.CloseTime != "" {
		if _, err := time.Parse(time.RFC3339Nano, wire.CloseTime); err != nil {
			return false
		}
	}
	if wire.TaskType != "" && !safeExaOpaqueID(wire.TaskType) {
		return false
	}
	return true
}

func validateSixtyfourPollAttempt(attempt Attempt) error {
	if attempt.State != AttemptPending || !safeExaOpaqueID(attempt.JobID) ||
		attempt.RequestID != "" ||
		attempt.AdapterVersion != SixtyfourAdapterVersionV1 ||
		attempt.SchemaVersion != SixtyfourWireSchemaV1 || !attempt.GeneratedSchema ||
		!lowercaseSHA256Pattern.MatchString(attempt.GeneratedSchemaHash) {
		return errors.New("invalid persisted Sixtyfour attempt")
	}
	generatedStruct, _, err := buildSixtyfourStruct(attempt.Targets)
	if err != nil {
		return errors.New("invalid persisted Sixtyfour targets")
	}
	digest := sha256.Sum256(generatedStruct)
	if hex.EncodeToString(digest[:]) != attempt.GeneratedSchemaHash {
		return errors.New("persisted Sixtyfour targets do not match schema hash")
	}
	want, err := ProgramFingerprint(ProgramDescriptor{
		HostMappingVersion: HostClaimMappingVersion, AdapterVersion: attempt.AdapterVersion,
		WireSchemaVersion: attempt.SchemaVersion, GeneratedSchema: attempt.GeneratedSchema,
		GeneratedSchemaHash: attempt.GeneratedSchemaHash,
	})
	if err != nil || want != attempt.ProgramFingerprint {
		return errors.New("invalid persisted Sixtyfour program fingerprint")
	}
	return nil
}

func buildSixtyfourStruct(
	targets []personfacts.TargetDescriptor,
) (json.RawMessage, map[string]any, error) {
	if len(targets) == 0 {
		return nil, nil, errors.New("sixtyfour struct requires at least one target")
	}
	fields := make(map[string]any, len(targets))
	for i, target := range targets {
		if err := validateExaTargetDescriptor(target); err != nil {
			return nil, nil, fmt.Errorf("invalid Sixtyfour target %d: %w", i, err)
		}
		if _, duplicate := fields[target.Key]; duplicate {
			return nil, nil, fmt.Errorf("duplicate Sixtyfour target %q", target.Key)
		}
		value, err := sixtyfourStructValue(target)
		if err != nil {
			return nil, nil, fmt.Errorf("sixtyfour target %q: %w", target.Key, err)
		}
		fields[target.Key] = value
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return nil, nil, fmt.Errorf("encode Sixtyfour struct: %w", err)
	}
	return encoded, fields, nil
}

func sixtyfourStructValue(target personfacts.TargetDescriptor) (any, error) {
	if target.Kind != personfacts.TargetAttribute {
		return nil, errors.New("structured targets are unsupported by the Sixtyfour struct codec")
	}
	typeName := "str"
	switch target.ValueType {
	case personfacts.ValueText:
	case personfacts.ValueInteger:
		typeName = "int"
	case personfacts.ValueReal:
		typeName = "float"
	case personfacts.ValueBoolean:
		typeName = "bool"
	case personfacts.ValueDate, personfacts.ValueTimestamp:
		typeName = "str"
	default:
		return nil, fmt.Errorf("unsupported value type %q", target.ValueType)
	}
	if target.Cardinality == personfacts.CardinalityMulti {
		typeName = "list[" + typeName + "]"
	}
	if typeName == "str" && target.Cardinality == personfacts.CardinalitySingle {
		return target.Description, nil
	}
	return sixtyfourStructField{Description: target.Description, Type: typeName}, nil
}

func sixtyfourLeadInfo(identity Identity) (map[string]any, error) {
	lead := make(map[string]any, 4)
	appendString := func(key, value string) {
		if value = strings.TrimSpace(value); value != "" {
			lead[key] = value
		}
	}
	appendString("name", identity.Name)
	appendString("email", identity.Email)
	appendString("phone", identity.Phone)
	appendString("company", identity.CurrentCompany)
	if len(lead) == 0 {
		return nil, errors.New("sixtyfour request has no eligible identity")
	}
	return lead, nil
}

func decodeSixtyfourCompleted(
	wire sixtyfourCompletedResult,
	charge json.Number,
	attempt Attempt,
) (Result, error) {
	if len(wire.StructuredData) == 0 || wire.Findings == nil || len(*wire.Findings) != 0 ||
		!validSixtyfourReferences(wire.References) {
		return Result{}, errors.New("invalid Sixtyfour completed result")
	}
	factConfidence, err := parseSixtyfourConfidence(wire.ConfidenceScore)
	if err != nil {
		return Result{}, err
	}
	cost, err := sixtyfourCost(charge)
	if err != nil {
		return Result{}, err
	}

	keys := make([]string, 0, len(attempt.Targets))
	identityMatches := make([]IdentityMatch, 0, 2)
	for key := range wire.StructuredData {
		if _, ok := sixtyfourTargetByKey(attempt, key); ok {
			keys = append(keys, key)
			continue
		}
		match, ok := sixtyfourIdentityMatch(key, wire.StructuredData[key], factConfidence)
		if !ok {
			return Result{}, errors.New("sixtyfour output target is not in the persisted request")
		}
		identityMatches = append(identityMatches, match)
	}
	if len(keys) != len(attempt.Targets) {
		return Result{}, errors.New("sixtyfour output omitted a persisted request target")
	}
	if len(identityMatches) != 2 || factConfidence < 900 {
		return Result{}, errors.New("sixtyfour output did not provide a verifiable name and company binding")
	}
	slices.Sort(keys)
	slices.SortFunc(identityMatches, func(left, right IdentityMatch) int {
		return strings.Compare(string(left.Class), string(right.Class))
	})
	claims := make([]personfacts.ProposedClaim, 0, len(keys))
	for _, key := range keys {
		target, ok := sixtyfourTargetByKey(attempt, key)
		if !ok {
			return Result{}, errors.New("sixtyfour output target is not in the persisted request")
		}
		values, valueErr := exaSubmittedValues(target, wire.StructuredData[key])
		if valueErr != nil || len(values) == 0 {
			return Result{}, errors.New("unsupported Sixtyfour result value")
		}
		for _, value := range values {
			claim, claimErr := sixtyfourClaim(target, value, factConfidence)
			if claimErr != nil {
				return Result{}, claimErr
			}
			claims = append(claims, claim)
		}
	}

	return Result{
		State: ResultComplete, JobID: attempt.JobID,
		Claims: claims, Cost: cost, IdentityMatches: identityMatches,
		IdentityConfidence: factConfidence,
		AdapterVersion:     SixtyfourAdapterVersionV1, SchemaVersion: SixtyfourWireSchemaV1,
		GeneratedSchema: true, GeneratedSchemaHash: attempt.GeneratedSchemaHash,
		ProviderVersion: SixtyfourProviderVersion,
	}, nil
}

func sixtyfourIdentityMatch(key string, raw json.RawMessage, confidence int) (IdentityMatch, bool) {
	var class IdentifierClass
	switch key {
	case "name":
		class = IdentifierName
	case "company":
		class = IdentifierCurrentCompany
	default:
		return IdentityMatch{}, false
	}
	var value string
	if json.Unmarshal(raw, &value) != nil || strings.TrimSpace(value) == "" {
		return IdentityMatch{}, false
	}
	return IdentityMatch{Class: class, Value: value, Confidence: confidence}, true
}

func validSixtyfourReferences(references map[string]string) bool {
	for rawURL, title := range references {
		if _, err := safeExaPublicURL(rawURL); err != nil || strings.TrimSpace(title) == "" {
			return false
		}
	}
	return true
}

// Poll has no Request argument, so the immutable target descriptors are carried
// on the pending attempt for restart-safe decoding.
func sixtyfourTargetByKey(attempt Attempt, key string) (personfacts.TargetDescriptor, bool) {
	for _, target := range attempt.Targets {
		if target.Key == key {
			return target, true
		}
	}
	return personfacts.TargetDescriptor{}, false
}

func sixtyfourClaim(
	target personfacts.TargetDescriptor,
	value json.RawMessage,
	score int,
) (personfacts.ProposedClaim, error) {
	if normalized, failure, err := personfacts.NormalizeClaimValue(target, value); err != nil || failure != nil || normalized == nil {
		return personfacts.ProposedClaim{}, errors.New("sixtyfour returned an unsupported target value")
	}
	return personfacts.ProposedClaim{
		Target: target, Relation: personfacts.RelationSupport,
		SubmittedValue: append(json.RawMessage(nil), value...),
		Evidence: []personfacts.EvidenceInput{{
			SourceClass: personfacts.EvidenceProviderAssertion,
			Directness:  personfacts.Indirect, Authority: personfacts.AuthorityAggregator,
		}},
		Origin:     personfacts.OriginEnrichment,
		Confidence: personfacts.ConfidenceInputs{ReportedScore: score},
	}, nil
}

func parseSixtyfourConfidence(value json.Number) (int, error) {
	if value == "" {
		return 0, errors.New("missing Sixtyfour confidence score")
	}
	parsed, err := strconv.ParseFloat(value.String(), 64)
	if err != nil {
		return 0, errors.New("invalid Sixtyfour confidence score")
	}
	return SixtyfourFactConfidence(parsed)
}

func sixtyfourCost(value json.Number) (Cost, error) {
	if value == "" {
		return Cost{}, errors.New("missing Sixtyfour charge amount")
	}
	cents, err := value.Int64()
	if err != nil || cents < 0 || cents > math.MaxInt64/10_000 {
		return Cost{}, errors.New("invalid Sixtyfour charge amount")
	}
	return Cost{Currency: "USD", AmountMicros: cents * 10_000}, nil
}

func (p *sixtyfourProvider) do(
	ctx context.Context, method, endpoint string, body []byte,
) (sixtyfourHTTPResponse, error) {
	request, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body)) // #nosec G107 -- endpoint is exact validated consent.
	if err != nil {
		return sixtyfourHTTPResponse{}, err
	}
	request.Header.Set("X-Api-Key", p.credential)
	request.Header.Set("Accept", "application/json")
	if method == http.MethodPost {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := p.client.Do(request)
	if err != nil {
		return sixtyfourHTTPResponse{}, err
	}
	defer func() { _ = response.Body.Close() }()
	result := sixtyfourHTTPResponse{
		status: response.StatusCode, retryAfter: response.Header.Get("Retry-After"),
	}
	data, oversized, readErr := readSixtyfourBody(response.Body)
	if readErr != nil || oversized {
		return result, errSixtyfourInvalidResponse
	}
	result.body = data
	if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices &&
		!sixtyfourJSONContentType(response.Header.Get("Content-Type")) {
		return result, errSixtyfourInvalidResponse
	}
	return result, nil
}

func (p *sixtyfourProvider) httpFailure(
	response sixtyfourHTTPResponse, start bool,
) error {
	class := FailureTerminal
	if response.status == http.StatusTooManyRequests {
		class = FailureRateLimited
	} else if response.status >= http.StatusInternalServerError {
		if start {
			class = FailureUncertainStart
		} else {
			class = FailureTransient
		}
	}
	return sixtyfourFailure(response.status, class, response.retryAfter)
}

func decodeSixtyfour(data []byte, destination any) error {
	if err := jsonexact.Validate(data, destination); err != nil {
		return errors.New("invalid Sixtyfour response")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(destination); err != nil {
		return errors.New("invalid Sixtyfour response")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("invalid Sixtyfour response")
	}
	return nil
}

func sixtyfourJSONContentType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	return err == nil && mediaType == "application/json"
}

func readSixtyfourBody(body io.Reader) ([]byte, bool, error) {
	data, err := io.ReadAll(io.LimitReader(body, sixtyfourMaxResponseBytes+1))
	return data, len(data) > sixtyfourMaxResponseBytes, err
}

func sixtyfourFailure(status int, class FailureClass, retryAfter string) error {
	return &ProviderError{
		Provider: ProviderSixtyfour, Status: status, Class: class,
		RetryAfter: safeExaRetryAfter(retryAfter),
	}
}
