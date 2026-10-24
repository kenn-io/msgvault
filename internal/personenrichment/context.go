package personenrichment

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/personfacts"
)

const contextAdapterVersion = "context-search-adapter-v1"
const contextSchemaVersion = "context-people-search-v1"

type contextProvider struct {
	config     ProviderConfig
	credential string
	client     *http.Client
}

// NewContextProvider constructs a synchronous People Search adapter. The caller
// supplies credentials only after the existing consent and egress checks.
func NewContextProvider(cfg ProviderConfig, credential string, client *http.Client) (Provider, error) {
	if cfg.Kind != ProviderContext {
		return nil, errors.New("context provider requires kind context")
	}
	if err := cfg.validatePolicy(true); err != nil {
		return nil, err
	}
	if strings.TrimSpace(credential) == "" || client == nil {
		return nil, errors.New("context credential and HTTP client are required")
	}
	cloned := *client
	cloned.Timeout = cfg.RequestTimeout
	cloned.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return &contextProvider{config: cfg, credential: credential, client: &cloned}, nil
}

func validateContextTargets(targets []personfacts.TargetDescriptor) error {
	if len(targets) == 0 {
		return errors.New("context requires at least one target")
	}
	seen := make(map[string]bool)
	for _, target := range targets {
		if err := validateExaTargetDescriptor(target); err != nil {
			return fmt.Errorf("context target: %w", err)
		}
		if seen[target.Key] {
			return errors.New("duplicate context target")
		}
		seen[target.Key] = true
		if target.Kind == personfacts.TargetEmployment {
			continue
		}
		if target.ValueType != personfacts.ValueText {
			return fmt.Errorf("unsupported context target %q", target.Key)
		}
		switch target.Slug {
		case "name", "full_name", "location":
			if target.Cardinality != personfacts.CardinalitySingle {
				return errors.New("context name and location targets must be single-valued")
			}
		case "education", "education_history":
			if target.Cardinality != personfacts.CardinalityMulti {
				return errors.New("context education targets must be multi-valued")
			}
		default:
			return fmt.Errorf("unsupported context target %q", target.Key)
		}
	}
	return nil
}

func contextSearchQuery(identity Identity) (string, error) {
	if strings.TrimSpace(identity.Name) == "" || strings.TrimSpace(identity.CurrentCompany) == "" {
		return "", errors.New("context requires name and current company")
	}
	if identity.Email != "" || identity.Phone != "" || len(identity.PublicProfileURLs) != 0 {
		return "", errors.New("context search supports name and current company only")
	}
	query := "name: " + identity.Name + "; company: " + identity.CurrentCompany
	if utf8.RuneCountInString(query) > 500 {
		return "", errors.New("context search query exceeds 500 characters")
	}
	return query, nil
}

func (p *contextProvider) Start(ctx context.Context, request Request) (Attempt, error) {
	query, err := contextSearchQuery(request.Identity)
	if err != nil {
		return Attempt{}, err
	}
	if err := validateContextTargets(request.Targets); err != nil {
		return Attempt{}, err
	}

	encoded, err := json.Marshal(struct {
		Query string `json:"query"`
	}{query})
	if err != nil {
		return Attempt{}, errors.New("encode context search")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.config.Endpoint, bytes.NewReader(encoded)) // #nosec G107 -- exact consented HTTPS endpoint validated at construction.
	if err != nil {
		return Attempt{}, errors.New("create context search")
	}
	req.Header.Set("Authorization", "Bearer "+p.credential)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	response, err := p.client.Do(req)
	if err != nil {
		return Attempt{}, contextFailure(0, FailureTransient, "", "")
	}
	defer func() { _ = response.Body.Close() }()
	requestID := safeExaErrorRequestID(response.Header.Get("X-Request-Id"))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		class := FailureTerminal
		if response.StatusCode == 429 {
			class = FailureRateLimited
		} else if response.StatusCode == 408 || response.StatusCode >= 500 {
			class = FailureTransient
		}
		return Attempt{}, contextFailure(response.StatusCode, class, requestID, response.Header.Get("Retry-After"))
	}
	body, oversized, err := readExaBody(response.Body)
	if err != nil || oversized || !exaJSONContentType(response.Header.Get("Content-Type")) {
		return Attempt{}, contextFailure(response.StatusCode, FailureInvalidOutput, requestID, "")
	}
	var wire contextSearchResponse
	if err := json.Unmarshal(body, &wire); err != nil || wire.Results == nil || len(wire.Results) > 10 || !safeExaOpaqueID(wire.RequestID) {
		return Attempt{}, contextFailure(response.StatusCode, FailureInvalidOutput, requestID, "")
	}
	result, err := contextSearchResult(wire, request, time.Now().UTC())
	if err != nil {
		return Attempt{}, err
	}
	fingerprint, err := ProgramFingerprint(ProgramDescriptor{HostMappingVersion: HostClaimMappingVersion, AdapterVersion: contextAdapterVersion, WireSchemaVersion: contextSchemaVersion})
	if err != nil {
		return Attempt{}, err
	}
	if err := result.Validate(); err != nil {
		return Attempt{}, contextFailure(200, FailureInvalidOutput, wire.RequestID, "")
	}
	return Attempt{State: AttemptComplete, RequestID: wire.RequestID, AdapterVersion: contextAdapterVersion, SchemaVersion: contextSchemaVersion, ProgramFingerprint: fingerprint, Result: &result}, nil
}

func (p *contextProvider) Poll(context.Context, Attempt) (Result, error) {
	return Result{}, errors.New("context search is synchronous and cannot be polled")
}

type contextSearchResponse struct {
	Results   []contextSearchPerson `json:"results"`
	RequestID string                `json:"request_id"`
}

type contextSearchPerson struct {
	Name     string   `json:"name"`
	Profiles []string `json:"profiles"`
	Location struct {
		Country string `json:"country_code"`
		Region  string `json:"region"`
		City    string `json:"city"`
	} `json:"location"`
	Role struct {
		Title     string  `json:"title"`
		StartedOn *string `json:"started_on"`
		Company   *struct {
			Name string `json:"name"`
		} `json:"company"`
	} `json:"role"`
	Schools []string `json:"schools"`
}

func contextSearchResult(wire contextSearchResponse, request Request, now time.Time) (Result, error) {
	var selected *contextSearchPerson
	for i := range wire.Results {
		person := &wire.Results[i]
		if person.Role.Company == nil || !exactExaIdentityMatch(IdentifierName, request.Identity.Name, person.Name) || !exactExaIdentityMatch(IdentifierCurrentCompany, request.Identity.CurrentCompany, person.Role.Company.Name) {
			continue
		}
		if selected != nil {
			return Result{}, contextFailure(200, FailureIdentityRejected, wire.RequestID, "")
		}
		selected = person
	}
	if selected == nil {
		return Result{}, contextFailure(200, FailureIdentityRejected, wire.RequestID, "")
	}
	result := Result{State: ResultComplete, RequestID: wire.RequestID, ProviderVersion: "api-v1", AdapterVersion: contextAdapterVersion, SchemaVersion: contextSchemaVersion, IdentityConfidence: 900,
		IdentityMatches: []IdentityMatch{{Class: IdentifierName, Value: selected.Name, Confidence: 900}, {Class: IdentifierCurrentCompany, Value: selected.Role.Company.Name, Confidence: 900}}}
	evidence := []personfacts.EvidenceInput{}
	seen := make(map[string]bool)
	for _, raw := range selected.Profiles {
		canonical, err := safeExaPublicURL(raw)
		if err != nil {
			continue
		}
		if seen[canonical] {
			continue
		}
		seen[canonical] = true
		citation := newProviderCitation(ProviderContext, canonical, selected.Name, time.Time{}, now)
		result.CanonicalPublicURLs = append(result.CanonicalPublicURLs, canonical)
		result.Citations = append(result.Citations, citation)
		result.SourceAttempts = append(result.SourceAttempts, SourceAttempt{URL: canonical, Outcome: "cited", ObservedAt: now})
		evidence = append(evidence, personfacts.EvidenceInput{SourceClass: personfacts.EvidencePublic, Directness: personfacts.Indirect, Authority: personfacts.AuthorityAggregator, SourceRef: citation.Key, SourceURL: canonical})
	}
	if len(evidence) == 0 {
		evidence = []personfacts.EvidenceInput{{SourceClass: personfacts.EvidenceProviderAssertion, Directness: personfacts.Indirect, Authority: personfacts.AuthorityAggregator}}
	}
	for _, target := range request.Targets {
		values, err := contextTargetValues(target, *selected)
		if err != nil {
			return Result{}, contextFailure(200, FailureInvalidOutput, wire.RequestID, "")
		}
		// Missing fields are unknown, never deletion claims. Search supplies no
		// per-field confidence or freshness; use the conservative aggregator score.
		claims, err := claimsForValues(target, values, 500, evidence)
		if err != nil {
			return Result{}, contextFailure(200, FailureInvalidOutput, wire.RequestID, "")
		}
		result.Claims = append(result.Claims, claims...)
	}
	// Credits are plan-dependent and cannot be reported as a known USD charge.
	return result, nil
}

func contextTargetValues(target personfacts.TargetDescriptor, person contextSearchPerson) ([]jsontext.Value, error) {
	var values []any
	if target.Kind == personfacts.TargetEmployment {
		value := personfacts.EmploymentValue{Organization: personfacts.OrganizationReference{Name: person.Role.Company.Name}, Title: person.Role.Title}
		date, present, err := exaPartialDate(person.Role.StartedOn)
		if err != nil {
			return nil, err
		}
		if present {
			value.StartDate = &date
		}
		values = append(values, value)
	} else {
		switch target.Slug {
		case "name", "full_name":
			values = append(values, person.Name)
		case "location":
			var parts []string
			for _, part := range []string{person.Location.City, person.Location.Region, person.Location.Country} {
				if strings.TrimSpace(part) != "" {
					parts = append(parts, part)
				}
			}
			if len(parts) > 0 {
				values = append(values, strings.Join(parts, ", "))
			}
		case "education", "education_history":
			for _, school := range person.Schools {
				if strings.TrimSpace(school) != "" {
					values = append(values, school)
				}
			}
		}
	}
	result := make([]jsontext.Value, 0, len(values))
	for _, value := range values {
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		result = append(result, encoded)
	}
	return result, nil
}

func contextFailure(status int, class FailureClass, requestID, retryAfter string) error {
	return &ProviderError{Provider: ProviderContext, Status: status, Class: class, RequestID: safeExaErrorRequestID(requestID), RetryAfter: safeExaRetryAfter(retryAfter)}
}
