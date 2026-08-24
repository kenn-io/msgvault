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
	"math/big"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"go.kenn.io/msgvault/internal/jsonexact"
	"go.kenn.io/msgvault/internal/personfacts"
)

const (
	ExaGroundingLowScore    = 400
	ExaTypedUngroundedScore = 500
	ExaGroundingMediumScore = 700
	ExaGroundingHighScore   = 900
	ExaAdapterVersionV1     = "exa-adapter-v1"
	ExaSearchWireSchemaV1   = "exa-search-wire-v1"
	ExaDeepProviderVersion  = "search-api-unversioned"

	exaMaxResponseBytes = 1 << 20
)

var errExaSynchronous = errors.New("exa provider is synchronous and cannot be polled")

type exaProvider struct {
	config     ProviderConfig
	credential string
	client     *http.Client
}

// NewExaProvider builds an Exa adapter from a credential that the caller has
// already obtained through EgressGate.Authorize. Construction performs no I/O.
func NewExaProvider(cfg ProviderConfig, credential string, client *http.Client) (Provider, error) {
	if cfg.Kind != ProviderExa {
		return nil, fmt.Errorf("exa provider requires kind %q", ProviderExa)
	}
	if err := cfg.validatePolicy(true); err != nil {
		return nil, fmt.Errorf("validate Exa provider configuration: %w", err)
	}
	if cfg.NumResults != 1 {
		return nil, errors.New("exa num_results must be exactly 1")
	}
	if strings.TrimSpace(credential) == "" {
		return nil, errors.New("exa credential is required")
	}
	if client == nil {
		return nil, errors.New("exa HTTP client is required")
	}
	cloned := *client
	cloned.Timeout = cfg.RequestTimeout
	cloned.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &exaProvider{config: cfg, credential: credential, client: &cloned}, nil
}

func ExaFactConfidence(label string, grounded bool) (int, error) {
	if !grounded {
		return ExaTypedUngroundedScore, nil
	}
	switch strings.ToLower(strings.TrimSpace(label)) {
	case "low":
		return ExaGroundingLowScore, nil
	case "medium":
		return ExaGroundingMediumScore, nil
	case "high":
		return ExaGroundingHighScore, nil
	default:
		return 0, errors.New("exa grounding confidence label is invalid")
	}
}

func (p *exaProvider) Start(ctx context.Context, request Request) (Attempt, error) {
	if p == nil || p.client == nil {
		return Attempt{}, errors.New("exa provider is not initialized")
	}
	query, err := exaIdentityQuery(request.Identity)
	if err != nil {
		return Attempt{}, err
	}
	generated := p.config.Mode != "people"
	var outputSchema json.RawMessage
	if generated {
		outputSchema, err = BuildExaOutputSchema(request.Targets)
	} else {
		err = validateExaPeopleTargets(request.Targets)
	}
	if err != nil {
		return Attempt{}, err
	}

	schemaHash := ""
	if generated {
		digest := sha256.Sum256(outputSchema)
		schemaHash = hex.EncodeToString(digest[:])
	}
	programFingerprint, err := ProgramFingerprint(ProgramDescriptor{
		HostMappingVersion:  HostClaimMappingVersion,
		AdapterVersion:      ExaAdapterVersionV1,
		WireSchemaVersion:   ExaSearchWireSchemaV1,
		GeneratedSchema:     generated,
		GeneratedSchemaHash: schemaHash,
	})
	if err != nil {
		return Attempt{}, fmt.Errorf("fingerprint Exa adapter program: %w", err)
	}

	payload := exaSearchRequest{
		Query: query, Category: "people", Type: exaRequestType(p.config.Mode),
		NumResults: p.config.NumResults, OutputSchema: outputSchema,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return Attempt{}, errors.New("encode Exa request")
	}
	httpRequest, err := http.NewRequestWithContext( // #nosec G107 -- exact consented HTTPS endpoint validated at construction.
		ctx, http.MethodPost, p.config.Endpoint, bytes.NewReader(encoded))
	if err != nil {
		return Attempt{}, errors.New("create Exa request")
	}
	httpRequest.Header.Set("Authorization", "Bearer "+p.credential)
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "application/json")

	response, err := p.client.Do(httpRequest)
	if err != nil {
		return Attempt{}, exaFailure(0, FailureTransient, "", "")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return Attempt{}, exaHTTPFailure(response)
	}
	if !exaJSONContentType(response.Header.Get("Content-Type")) {
		return Attempt{}, exaFailure(response.StatusCode, FailureInvalidOutput, "", "")
	}
	body, oversized, err := readExaBody(response.Body)
	if err != nil || oversized {
		return Attempt{}, exaFailure(response.StatusCode, FailureInvalidOutput, "", "")
	}
	wire, err := decodeExaResponse(body)
	if err != nil {
		return Attempt{}, exaFailure(response.StatusCode, FailureInvalidOutput, "", "")
	}
	if !safeExaOpaqueID(wire.RequestID) {
		return Attempt{}, exaFailure(response.StatusCode, FailureInvalidOutput, "", "")
	}
	now := time.Now().UTC()
	var result Result
	if generated {
		result, err = decodeExaDeepResult(wire, request, now)
	} else {
		result, err = decodeExaPeopleResult(wire, request, now)
	}
	if err != nil {
		return Attempt{}, exaFailure(response.StatusCode, FailureInvalidOutput, wire.RequestID, "")
	}
	result.AdapterVersion = ExaAdapterVersionV1
	result.SchemaVersion = ExaSearchWireSchemaV1
	result.GeneratedSchema = generated
	result.GeneratedSchemaHash = schemaHash
	if err := result.Validate(); err != nil {
		return Attempt{}, exaFailure(response.StatusCode, FailureInvalidOutput, wire.RequestID, "")
	}
	return Attempt{
		State: AttemptComplete, RequestID: wire.RequestID,
		AdapterVersion: ExaAdapterVersionV1, SchemaVersion: ExaSearchWireSchemaV1,
		GeneratedSchema: generated, GeneratedSchemaHash: schemaHash,
		ProgramFingerprint: programFingerprint, Result: &result,
	}, nil
}

func (p *exaProvider) Poll(context.Context, Attempt) (Result, error) {
	return Result{}, errExaSynchronous
}

type exaSearchRequest struct {
	Query        string          `json:"query"`
	Category     string          `json:"category"`
	Type         string          `json:"type"`
	NumResults   int             `json:"numResults"`
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
}

type exaSearchResponse struct {
	RequestID          string            `json:"requestId"`
	Results            []exaSearchResult `json:"results"`
	Output             *exaOutput        `json:"output"`
	ResolvedSearchType string            `json:"resolvedSearchType"`
	SearchTime         json.Number       `json:"searchTime"`
	CostDollars        *exaCostDollars   `json:"costDollars"`
}

type exaCostDollars struct {
	Total    json.Number            `json:"total"`
	Search   map[string]json.Number `json:"search"`
	Contents map[string]json.Number `json:"contents"`
}

type exaSearchResult struct {
	Title           string      `json:"title"`
	URL             string      `json:"url"`
	PublishedDate   string      `json:"publishedDate"`
	Author          any         `json:"author"`
	ID              any         `json:"id"`
	Image           any         `json:"image"`
	Favicon         any         `json:"favicon"`
	Text            any         `json:"text"`
	Highlights      any         `json:"highlights"`
	HighlightScores any         `json:"highlightScores"`
	Summary         any         `json:"summary"`
	Subpages        any         `json:"subpages"`
	Extras          any         `json:"extras"`
	Entities        []exaEntity `json:"entities"`
}

type exaEntity struct {
	ID         string              `json:"id"`
	Type       string              `json:"type"`
	Version    int64               `json:"version"`
	Properties exaPersonProperties `json:"properties"`
}

type exaPersonProperties struct {
	Name             *string         `json:"name"`
	FirstName        *string         `json:"firstName"`
	LastName         *string         `json:"lastName"`
	Location         *string         `json:"location"`
	Research         json.RawMessage `json:"research"`
	WorkHistory      []exaWork       `json:"workHistory"`
	EducationHistory []exaEducation  `json:"educationHistory"`
}

type exaWork struct {
	Title    *string         `json:"title"`
	Location *string         `json:"location"`
	Dates    *exaDates       `json:"dates"`
	Company  *exaNamedEntity `json:"company"`
}

type exaEducation struct {
	Degree      *string         `json:"degree"`
	Dates       *exaDates       `json:"dates"`
	Institution *exaNamedEntity `json:"institution"`
}

type exaDates struct {
	From *string `json:"from"`
	To   *string `json:"to"`
}

type exaNamedEntity struct {
	ID   *string `json:"id"`
	Name *string `json:"name"`
}

type exaOutput struct {
	Content   map[string]json.RawMessage `json:"content"`
	Grounding []exaGrounding             `json:"grounding"`
}

type exaGrounding struct {
	Field      string        `json:"field"`
	Citations  []exaCitation `json:"citations"`
	Confidence string        `json:"confidence"`
}

type exaCitation struct {
	URL   string `json:"url"`
	Title string `json:"title"`
}

func exaRequestType(mode string) string {
	if mode == "people" {
		return "auto"
	}
	return mode
}

func exaIdentityQuery(identity Identity) (string, error) {
	parts := make([]string, 0, 5+len(identity.PublicProfileURLs))
	appendPart := func(label, value string) {
		if value = strings.TrimSpace(value); value != "" {
			parts = append(parts, label+": "+value)
		}
	}
	appendPart("name", identity.Name)
	appendPart("email", identity.Email)
	appendPart("phone", identity.Phone)
	appendPart("company", identity.CurrentCompany)
	for _, profileURL := range identity.PublicProfileURLs {
		appendPart("public profile", profileURL)
	}
	if len(parts) == 0 {
		return "", errors.New("exa request has no eligible identity")
	}
	return strings.Join(parts, "; "), nil
}

type exaPeopleCapability string

const (
	exaCapabilityName       exaPeopleCapability = "name"
	exaCapabilityFirstName  exaPeopleCapability = "first_name"
	exaCapabilityLastName   exaPeopleCapability = "last_name"
	exaCapabilityLocation   exaPeopleCapability = "location"
	exaCapabilityEmployment exaPeopleCapability = "employment"
	exaCapabilityEducation  exaPeopleCapability = "education"
)

func validateExaPeopleTargets(targets []personfacts.TargetDescriptor) error {
	if len(targets) == 0 {
		return errors.New("exa people mode requires at least one target")
	}
	seen := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		if err := validateExaTargetDescriptor(target); err != nil {
			return fmt.Errorf("invalid Exa people target %q: %w", target.Key, err)
		}
		if _, duplicate := seen[target.Key]; duplicate {
			return fmt.Errorf("duplicate Exa people target %q", target.Key)
		}
		seen[target.Key] = struct{}{}
		if _, err := exaPeopleCapabilityForTarget(target); err != nil {
			return err
		}
	}
	return nil
}

func exaPeopleCapabilityForTarget(target personfacts.TargetDescriptor) (exaPeopleCapability, error) {
	if target.Kind == personfacts.TargetEmployment && target.Key == "system:employment" {
		return exaCapabilityEmployment, nil
	}
	if target.Kind != personfacts.TargetAttribute || target.ValueType != personfacts.ValueText {
		return "", fmt.Errorf("exa people mode target %q is unsupported", target.Key)
	}
	key := target.Slug
	if key == "" {
		key = target.Key
	}
	switch key {
	case "name", "full_name":
		return exaCapabilityName, nil
	case "first_name":
		return exaCapabilityFirstName, nil
	case "last_name":
		return exaCapabilityLastName, nil
	case "location":
		return exaCapabilityLocation, nil
	case "education", "education_history":
		return exaCapabilityEducation, nil
	default:
		return "", fmt.Errorf("exa people mode target %q is unsupported by the fixed entity schema", target.Key)
	}
}

func decodeExaResponse(body []byte) (exaSearchResponse, error) {
	if err := jsonexact.Validate(body, exaSearchResponse{}); err != nil {
		return exaSearchResponse{}, errors.New("invalid Exa response")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	var response exaSearchResponse
	if err := decoder.Decode(&response); err != nil {
		return exaSearchResponse{}, errors.New("invalid Exa response")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return exaSearchResponse{}, errors.New("invalid Exa response")
	}
	return response, nil
}

func decodeExaPeopleResult(wire exaSearchResponse, request Request, now time.Time) (Result, error) {
	var selected *exaEntity
	var selectedRow *exaSearchResult
	for i := range wire.Results {
		for j := range wire.Results[i].Entities {
			entity := &wire.Results[i].Entities[j]
			if entity.Type != "person" {
				return Result{}, errors.New("non-person Exa entity")
			}
			if selected != nil {
				return Result{}, errors.New("ambiguous Exa person identity")
			}
			selected = entity
			selectedRow = &wire.Results[i]
		}
	}
	if selected == nil || selectedRow == nil || !safeExaOpaqueID(selected.ID) || selected.Version <= 0 {
		return Result{}, errors.New("missing Exa person identity")
	}
	if len(selected.Properties.Research) > 0 &&
		!bytes.Equal(bytes.TrimSpace(selected.Properties.Research), []byte("null")) {
		return Result{}, errors.New("unsupported Exa person research payload")
	}
	profileURL, err := safeExaPublicURL(selectedRow.URL)
	if err != nil {
		return Result{}, err
	}
	published, err := optionalExaTime(selectedRow.PublishedDate)
	if err != nil {
		return Result{}, err
	}
	citation := newExaCitation(profileURL, selectedRow.Title, published, now)
	claims := make([]personfacts.ProposedClaim, 0, len(request.Targets))
	for _, target := range request.Targets {
		capability, err := exaPeopleCapabilityForTarget(target)
		if err != nil {
			return Result{}, err
		}
		values, err := exaTypedValues(target, capability, selected.Properties)
		if err != nil || len(values) == 0 {
			return Result{}, errors.New("exa typed target value is missing or malformed")
		}
		for _, value := range values {
			claim, err := exaClaim(target, value, ExaTypedUngroundedScore, []Citation{citation})
			if err != nil {
				return Result{}, err
			}
			claims = append(claims, claim)
		}
	}
	matches, identityConfidence := exaTypedIdentityMatches(request.Identity, selected.Properties, profileURL)
	if len(matches) == 0 || identityConfidence == 0 {
		return Result{}, errors.New("missing Exa returned identity match")
	}
	cost, err := exaCost(wire.CostDollars)
	if err != nil {
		return Result{}, err
	}
	return Result{
		State: ResultComplete, RequestID: wire.RequestID, Claims: claims,
		Citations:           []Citation{citation},
		ProviderPersonIDs:   []ProviderPersonID{{ID: selected.ID, Confidence: identityConfidence}},
		CanonicalPublicURLs: []string{profileURL}, IdentityMatches: matches,
		IdentityConfidence: identityConfidence, FreshAsOf: published,
		SourceAttempts: []SourceAttempt{{URL: profileURL, Outcome: "cited", ObservedAt: now}},
		Cost:           cost, ProviderVersion: strconv.FormatInt(selected.Version, 10),
	}, nil
}

func decodeExaDeepResult(wire exaSearchResponse, request Request, now time.Time) (Result, error) {
	matches, identityConfidence, err := exaDeepResultIdentityMatch(request.Identity, wire.Results)
	if err != nil {
		return Result{}, err
	}
	if wire.Output == nil || len(wire.Output.Content) == 0 {
		return Result{}, errors.New("missing Exa synthesized output")
	}
	content := wire.Output.Content
	if len(content) != len(request.Targets) {
		return Result{}, errors.New("exa synthesized output target coverage mismatch")
	}
	byTarget := make(map[string]personfacts.TargetDescriptor, len(request.Targets))
	for _, target := range request.Targets {
		if _, duplicate := byTarget[target.Key]; duplicate {
			return Result{}, errors.New("duplicate requested Exa target")
		}
		byTarget[target.Key] = target
		if _, ok := content[target.Key]; !ok {
			return Result{}, errors.New("exa synthesized output omitted a requested target")
		}
	}
	grounding := make(map[string]exaGrounding, len(wire.Output.Grounding))
	for _, item := range wire.Output.Grounding {
		if _, ok := byTarget[item.Field]; !ok {
			return Result{}, errors.New("exa synthesized output contains unrequested grounding")
		}
		if _, duplicate := grounding[item.Field]; duplicate {
			return Result{}, errors.New("exa synthesized output contains duplicate grounding")
		}
		if len(item.Citations) == 0 {
			return Result{}, errors.New("exa synthesized output grounding has no citations")
		}
		grounding[item.Field] = item
	}
	if len(grounding) != len(request.Targets) {
		return Result{}, errors.New("exa synthesized output grounding coverage mismatch")
	}

	claims := make([]personfacts.ProposedClaim, 0, len(request.Targets))
	citationByKey := make(map[string]Citation)
	citationOrder := make([]string, 0)
	sourceByURL := make(map[string]SourceAttempt)
	for _, target := range request.Targets {
		item := grounding[target.Key]
		score, err := ExaFactConfidence(item.Confidence, true)
		if err != nil {
			return Result{}, err
		}
		citations := make([]Citation, 0, len(item.Citations))
		for _, rawCitation := range item.Citations {
			canonicalURL, err := safeExaPublicURL(rawCitation.URL)
			if err != nil || strings.TrimSpace(rawCitation.Title) == "" {
				return Result{}, errors.New("unsafe Exa citation")
			}
			citation := newExaCitation(canonicalURL, rawCitation.Title, time.Time{}, now)
			if existing, ok := citationByKey[citation.Key]; ok && existing != citation {
				return Result{}, errors.New("conflicting Exa citation")
			}
			if _, exists := citationByKey[citation.Key]; !exists {
				citationByKey[citation.Key] = citation
				citationOrder = append(citationOrder, citation.Key)
			}
			citations = append(citations, citation)
			sourceByURL[canonicalURL] = SourceAttempt{URL: canonicalURL, Outcome: "cited", ObservedAt: now}
		}
		values, err := exaSubmittedValues(target, content[target.Key])
		if err != nil || len(values) == 0 {
			return Result{}, errors.New("unsupported Exa synthesized value")
		}
		for _, value := range values {
			claim, err := exaClaim(target, value, score, citations)
			if err != nil {
				return Result{}, err
			}
			claims = append(claims, claim)
		}
	}
	citations := make([]Citation, 0, len(citationOrder))
	for _, key := range citationOrder {
		citations = append(citations, citationByKey[key])
	}
	sources := make([]SourceAttempt, 0, len(sourceByURL))
	for _, citation := range citations {
		if source, ok := sourceByURL[citation.URL]; ok {
			sources = append(sources, source)
			delete(sourceByURL, citation.URL)
		}
	}
	freshness, err := exaResponseFreshness(wire.Results)
	if err != nil {
		return Result{}, err
	}
	cost, err := exaCost(wire.CostDollars)
	if err != nil {
		return Result{}, err
	}
	return Result{
		State: ResultComplete, RequestID: wire.RequestID, Claims: claims,
		Citations: citations, SourceAttempts: sources, FreshAsOf: freshness,
		Cost: cost, IdentityMatches: matches, IdentityConfidence: identityConfidence,
		ProviderVersion: ExaDeepProviderVersion,
	}, nil
}

func exaTypedValues(
	target personfacts.TargetDescriptor,
	capability exaPeopleCapability,
	properties exaPersonProperties,
) ([]json.RawMessage, error) {
	stringValue := func(value *string) ([]json.RawMessage, error) {
		if value == nil || strings.TrimSpace(*value) == "" {
			return nil, errors.New("typed string is absent")
		}
		encoded, err := json.Marshal(*value)
		return []json.RawMessage{encoded}, err
	}
	switch capability {
	case exaCapabilityName:
		return stringValue(properties.Name)
	case exaCapabilityFirstName:
		return stringValue(properties.FirstName)
	case exaCapabilityLastName:
		return stringValue(properties.LastName)
	case exaCapabilityLocation:
		return stringValue(properties.Location)
	case exaCapabilityEmployment:
		values := make([]json.RawMessage, 0, len(properties.WorkHistory))
		for _, work := range properties.WorkHistory {
			if work.Company == nil || work.Company.Name == nil || strings.TrimSpace(*work.Company.Name) == "" {
				return nil, errors.New("typed employment has no organization")
			}
			value := personfacts.EmploymentValue{
				Organization: personfacts.OrganizationReference{Name: *work.Company.Name},
				Title:        valueOrEmpty(work.Title), Location: valueOrEmpty(work.Location),
			}
			var err error
			if work.Dates != nil {
				var start personfacts.PartialDateValue
				var present bool
				start, present, err = exaPartialDate(work.Dates.From)
				if present {
					value.StartDate = &start
				}
				if err == nil {
					var end personfacts.PartialDateValue
					end, present, err = exaPartialDate(work.Dates.To)
					if present {
						value.EndDate = &end
					}
				}
			}
			if err != nil {
				return nil, err
			}
			encoded, err := json.Marshal(value)
			if err != nil {
				return nil, err
			}
			values = append(values, encoded)
		}
		return values, nil
	case exaCapabilityEducation:
		values := make([]json.RawMessage, 0, len(properties.EducationHistory))
		for _, education := range properties.EducationHistory {
			parts := make([]string, 0, 2)
			if education.Degree != nil && strings.TrimSpace(*education.Degree) != "" {
				parts = append(parts, strings.TrimSpace(*education.Degree))
			}
			if education.Institution != nil && education.Institution.Name != nil &&
				strings.TrimSpace(*education.Institution.Name) != "" {
				parts = append(parts, strings.TrimSpace(*education.Institution.Name))
			}
			if len(parts) == 0 {
				return nil, errors.New("typed education is empty")
			}
			encoded, err := json.Marshal(strings.Join(parts, " — "))
			if err != nil {
				return nil, err
			}
			values = append(values, encoded)
		}
		return values, nil
	default:
		return nil, fmt.Errorf("unsupported typed Exa capability %q for %q", capability, target.Key)
	}
}

func exaSubmittedValues(target personfacts.TargetDescriptor, raw json.RawMessage) ([]json.RawMessage, error) {
	if target.Cardinality == personfacts.CardinalitySingle {
		return []json.RawMessage{append(json.RawMessage(nil), raw...)}, nil
	}
	var values []json.RawMessage
	if err := decodeOneExaValue(raw, &values); err != nil {
		return nil, err
	}
	return values, nil
}

func exaClaim(
	target personfacts.TargetDescriptor,
	value json.RawMessage,
	score int,
	citations []Citation,
) (personfacts.ProposedClaim, error) {
	if normalized, failure, err := personfacts.NormalizeClaimValue(target, value); err != nil || failure != nil || normalized == nil {
		return personfacts.ProposedClaim{}, errors.New("exa returned an unsupported target value")
	}
	evidence := make([]personfacts.EvidenceInput, len(citations))
	for i, citation := range citations {
		evidence[i] = personfacts.EvidenceInput{
			SourceClass: personfacts.EvidencePublic, Directness: personfacts.Indirect,
			Authority: personfacts.AuthorityAggregator, SourceRef: citation.Key,
			SourceURL: citation.URL,
		}
	}
	return personfacts.ProposedClaim{
		Target: target, Relation: personfacts.RelationSupport,
		SubmittedValue: append(json.RawMessage(nil), value...), Evidence: evidence,
		Origin:     personfacts.OriginEnrichment,
		Confidence: personfacts.ConfidenceInputs{ReportedScore: score},
	}, nil
}

func exaTypedIdentityMatches(
	identity Identity,
	properties exaPersonProperties,
	profileURL string,
) ([]IdentityMatch, int) {
	matches := make([]IdentityMatch, 0, 3)
	nameMatch := properties.Name != nil && exactExaIdentityMatch(IdentifierName, identity.Name, *properties.Name)
	companyMatch := false
	var companyValue string
	for _, work := range properties.WorkHistory {
		if work.Company != nil && work.Company.Name != nil &&
			exactExaIdentityMatch(IdentifierCurrentCompany, identity.CurrentCompany, *work.Company.Name) {
			companyMatch = true
			companyValue = *work.Company.Name
			break
		}
	}
	if nameMatch {
		matches = append(matches, IdentityMatch{Class: IdentifierName, Value: *properties.Name, Confidence: 900})
	}
	if companyMatch {
		matches = append(matches, IdentityMatch{Class: IdentifierCurrentCompany, Value: companyValue, Confidence: 900})
	}
	strong := false
	for _, requestURL := range identity.PublicProfileURLs {
		if exactExaIdentityMatch(IdentifierPublicProfileURL, requestURL, profileURL) {
			matches = append(matches, IdentityMatch{Class: IdentifierPublicProfileURL, Value: profileURL, Confidence: 1000})
			strong = true
			break
		}
	}
	if strong {
		return matches, 1000
	}
	if nameMatch && companyMatch {
		return matches, 900
	}
	return matches, 0
}

func exaDeepResultIdentityMatch(identity Identity, results []exaSearchResult) ([]IdentityMatch, int, error) {
	if len(results) != 1 {
		return nil, 0, errors.New("ambiguous Exa synthesized result identity")
	}
	canonical, err := safeExaPublicURL(results[0].URL)
	if err != nil {
		return nil, 0, err
	}
	for _, requestURL := range identity.PublicProfileURLs {
		if exactExaIdentityMatch(IdentifierPublicProfileURL, requestURL, canonical) {
			return []IdentityMatch{{Class: IdentifierPublicProfileURL, Value: canonical, Confidence: 1000}}, 1000, nil
		}
	}
	return nil, 0, errors.New("missing Exa returned identity match")
}

func exactExaIdentityMatch(class IdentifierClass, left, right string) bool {
	want, err := NormalizeIdentifier(class, left)
	if err != nil {
		return false
	}
	got, err := NormalizeIdentifier(class, right)
	return err == nil && want.Value == got.Value
}

func exaPartialDate(raw *string) (personfacts.PartialDateValue, bool, error) {
	if raw == nil {
		return personfacts.PartialDateValue{}, false, nil
	}
	value := strings.TrimSpace(*raw)
	for _, layout := range []string{"2006-01-02", "2006-01", "2006"} {
		parsed, err := time.Parse(layout, value)
		if err != nil {
			continue
		}
		partial := personfacts.PartialDateValue{Year: parsed.Year()}
		if len(value) >= 7 {
			partial.Month = int(parsed.Month())
		}
		if len(value) == 10 {
			partial.Day = parsed.Day()
		}
		return partial, true, nil
	}
	return personfacts.PartialDateValue{}, false, errors.New("exa partial date is invalid")
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func exaCost(value *exaCostDollars) (Cost, error) {
	if value == nil {
		return Cost{}, nil
	}
	if value.Total == "" {
		return Cost{}, errors.New("invalid Exa cost")
	}
	rational, ok := new(big.Rat).SetString(value.Total.String())
	if !ok || rational.Sign() < 0 {
		return Cost{}, errors.New("invalid Exa cost")
	}
	rational.Mul(rational, big.NewRat(1_000_000, 1))
	if !rational.IsInt() || !rational.Num().IsInt64() {
		return Cost{}, errors.New("invalid Exa cost")
	}
	return Cost{Currency: "USD", AmountMicros: rational.Num().Int64(), Estimated: true}, nil
}

func exaResponseFreshness(results []exaSearchResult) (time.Time, error) {
	var latest time.Time
	for _, result := range results {
		value, err := optionalExaTime(result.PublishedDate)
		if err != nil {
			return time.Time{}, err
		}
		if value.After(latest) {
			latest = value
		}
	}
	return latest, nil
}

func optionalExaTime(raw string) (time.Time, error) {
	if strings.TrimSpace(raw) == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, errors.New("invalid Exa freshness timestamp")
	}
	return parsed.UTC(), nil
}

func newExaCitation(rawURL, title string, published, retrieved time.Time) Citation {
	digest := sha256.Sum256([]byte(rawURL + "\x00" + title))
	return Citation{
		Key: "exa:" + hex.EncodeToString(digest[:]), URL: rawURL, Title: title,
		PublishedAt: published, RetrievedAt: retrieved,
	}
}

func safeExaPublicURL(raw string) (string, error) {
	canonical, err := CanonicalPublicURL(raw)
	if err != nil {
		return "", errors.New("unsafe Exa public URL")
	}
	parsed, err := url.Parse(canonical)
	if err != nil || parsed.Scheme != "https" {
		return "", errors.New("unsafe Exa public URL")
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return "", errors.New("unsafe Exa public URL")
	}
	if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsLinkLocalUnicast()) {
		return "", errors.New("unsafe Exa public URL")
	}
	return canonical, nil
}

func safeExaOpaqueID(value string) bool {
	if value == "" || value != strings.TrimSpace(value) || len(value) > 1024 {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func safeExaErrorRequestID(value string) string {
	if value == "" || value != strings.TrimSpace(value) || len(value) > 200 {
		return ""
	}
	for _, r := range value {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune("._:-", r) {
			return ""
		}
	}
	return value
}

func exaJSONContentType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	return err == nil && mediaType == "application/json"
}

func readExaBody(body io.Reader) ([]byte, bool, error) {
	data, err := io.ReadAll(io.LimitReader(body, exaMaxResponseBytes+1))
	return data, len(data) > exaMaxResponseBytes, err
}

func exaHTTPFailure(response *http.Response) error {
	body, oversized, _ := readExaBody(response.Body)
	requestID := ""
	if !oversized {
		var envelope struct {
			RequestID string `json:"requestId"`
		}
		if json.Unmarshal(body, &envelope) == nil {
			requestID = safeExaErrorRequestID(envelope.RequestID)
		}
	}
	class := FailureTerminal
	if response.StatusCode == http.StatusTooManyRequests {
		class = FailureRateLimited
	} else if response.StatusCode >= http.StatusInternalServerError {
		class = FailureTransient
	}
	return exaFailure(response.StatusCode, class, requestID, safeExaRetryAfter(response.Header.Get("Retry-After")))
}

func safeExaRetryAfter(value string) string {
	if value == "" || value != strings.TrimSpace(value) || len(value) > 64 {
		return ""
	}
	seconds, err := strconv.ParseUint(value, 10, 31)
	if err == nil && seconds <= 2_147_483_647 {
		return value
	}
	if _, err := http.ParseTime(value); err == nil {
		return value
	}
	return ""
}

func exaFailure(status int, class FailureClass, requestID, retryAfter string) error {
	return &ProviderError{
		Provider: ProviderExa, RequestID: safeExaErrorRequestID(requestID),
		Status: status, Class: class, RetryAfter: retryAfter,
	}
}

func decodeOneExaValue(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(destination); err != nil {
		return errors.New("invalid Exa JSON value")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("invalid Exa JSON value")
	}
	return nil
}
