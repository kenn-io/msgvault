package personenrichment_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personenrichment"
	"go.kenn.io/msgvault/internal/personfacts"
)

func TestLiveProductionProviderContracts(t *testing.T) {
	if os.Getenv("MSGVAULT_LIVE_PROVIDER_TEST") != "1" {
		t.Skip("set MSGVAULT_LIVE_PROVIDER_TEST=1 for authenticated production contract probes")
	}
	requirements := require.New(t)
	name := strings.TrimSpace(os.Getenv("MSGVAULT_LIVE_PERSON_NAME"))
	company := strings.TrimSpace(os.Getenv("MSGVAULT_LIVE_PERSON_COMPANY"))
	requirements.NotEmpty(name, "MSGVAULT_LIVE_PERSON_NAME must identify a public, consenting test subject")
	requirements.NotEmpty(company, "MSGVAULT_LIVE_PERSON_COMPANY is required for provider-compatible identity")

	target := personfacts.TargetDescriptor{
		Kind: personfacts.TargetAttribute, Key: "attribute:name", UniversalID: "attribute:name",
		Slug: "name", Description: "Current public full name",
		ValueType: personfacts.ValueText, Cardinality: personfacts.CardinalitySingle,
	}
	target.Revision = liveTargetRevision(t, target)
	request := personenrichment.Request{
		RequestHash: strings.Repeat("a", 64),
		Identity:    personenrichment.Identity{Name: name, CurrentCompany: company},
		Targets:     []personfacts.TargetDescriptor{target},
	}

	t.Run("exa", func(t *testing.T) {
		checks := assert.New(t)
		requirements := require.New(t)
		credential := os.Getenv("EXA_API_KEY")
		requirements.NotEmpty(credential)
		cfg := liveProviderConfig(personenrichment.ProviderExa, target.Key)
		provider, err := personenrichment.NewExaProvider(
			cfg, credential, liveShapeClient(t, "Exa", request.Identity))
		requirements.NoError(err)
		attempt, err := provider.Start(t.Context(), request)
		requirements.NoError(err)
		requirements.NoError(attempt.Validate())
		requirements.Equal(personenrichment.AttemptComplete, attempt.State)
		requirements.NotNil(attempt.Result)
		checks.NotEmpty(attempt.Result.ProviderVersion)
		checks.NotEmpty(attempt.Result.ProviderPersonIDs)
		for _, providerPersonID := range attempt.Result.ProviderPersonIDs {
			checks.Positive(providerPersonID.Confidence)
		}
		checks.NotEmpty(attempt.Result.Claims)
		t.Logf("Exa production contract accepted: claims=%d citations=%d provider_person_ids=%d cost_micros=%d provider_version=%s",
			len(attempt.Result.Claims), len(attempt.Result.Citations),
			len(attempt.Result.ProviderPersonIDs), attempt.Result.Cost.AmountMicros,
			attempt.Result.ProviderVersion)
	})

	t.Run("sixtyfour", func(t *testing.T) {
		checks := assert.New(t)
		requirements := require.New(t)
		credential := os.Getenv("SIXTYFOUR_API_KEY")
		requirements.NotEmpty(credential)
		cfg := liveProviderConfig(personenrichment.ProviderSixtyfour, target.Key)
		provider, err := personenrichment.NewSixtyfourProvider(
			cfg, credential, liveShapeClient(t, "Sixtyfour", request.Identity))
		requirements.NoError(err)
		attempt, err := provider.Start(t.Context(), request)
		requirements.NoError(err)
		requirements.NoError(attempt.Validate())
		requirements.Equal(personenrichment.AttemptPending, attempt.State)

		ctx, cancel := context.WithTimeout(t.Context(), cfg.MaxJobAge)
		defer cancel()
		for {
			result, pollErr := provider.Poll(ctx, attempt)
			requirements.NoError(pollErr)
			requirements.NoError(result.Validate())
			if result.State == personenrichment.ResultComplete {
				checks.NotEmpty(result.ProviderVersion)
				checks.NotEmpty(result.Claims)
				t.Logf("Sixtyfour production contract accepted: claims=%d cost_micros=%d provider_version=%s",
					len(result.Claims), result.Cost.AmountMicros, result.ProviderVersion)
				return
			}
			select {
			case <-ctx.Done():
				requirements.NoError(ctx.Err())
			case <-time.After(result.PollAfter):
			}
		}
	})
}

func liveProviderConfig(kind, targetKey string) personenrichment.ProviderConfig {
	cfg := personenrichment.ProviderConfig{
		Name: kind + "-live-contract", Kind: kind, Enabled: true,
		AllowedIdentifiers: []personenrichment.IdentifierClass{
			personenrichment.IdentifierName, personenrichment.IdentifierCurrentCompany,
		},
		TargetKeys: []string{targetKey}, RetentionPosture: "zero_retention",
		TrainingPosture: "no_training", RefreshInterval: 24 * time.Hour,
		RequestTimeout: time.Minute, PollInterval: 2 * time.Second,
		MaxJobAge: 5 * time.Minute, MaxRetries: 0,
		MaxRequestsPerRun: 2, MaxRequestsPerDay: 2,
	}
	switch kind {
	case personenrichment.ProviderExa:
		cfg.Endpoint = "https://api.exa.ai/search"
		cfg.APIKeyEnv = "EXA_API_KEY"
		cfg.Mode = "people"
		cfg.NumResults = 1
	case personenrichment.ProviderSixtyfour:
		cfg.Endpoint = "https://api.sixtyfour.ai/people-intelligence-async"
		cfg.PollEndpoint = "https://api.sixtyfour.ai/job-status"
		cfg.APIKeyEnv = "SIXTYFOUR_API_KEY"
		cfg.Tier = "medium"
	}
	return cfg
}

func liveTargetRevision(t *testing.T, target personfacts.TargetDescriptor) string {
	t.Helper()
	revision, err := personfacts.DescriptorRevision(target)
	require.NoError(t, err)
	return revision
}

type liveShapeTransport struct {
	t        *testing.T
	provider string
	identity personenrichment.Identity
	seen     map[string]struct{}
}

func (transport *liveShapeTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := http.DefaultTransport.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		return nil, err
	}
	response.Body = io.NopCloser(bytes.NewReader(body))
	var decoded any
	if json.Unmarshal(body, &decoded) == nil {
		shape := make([]string, 0)
		appendLiveJSONShape(&shape, "$", decoded)
		slices.Sort(shape)
		summary := strings.Join(shape, ", ")
		if _, logged := transport.seen[summary]; !logged {
			transport.seen[summary] = struct{}{}
			transport.t.Logf("%s production response shape (HTTP %d): %s",
				transport.provider, response.StatusCode, summary)
			if transport.provider == "Exa" {
				transport.t.Logf("Exa production contract checks: %s",
					strings.Join(liveExaContractChecks(decoded, transport.identity), ", "))
			}
		}
	}
	return response, nil
}

func liveShapeClient(
	t *testing.T,
	provider string,
	identity personenrichment.Identity,
) *http.Client {
	t.Helper()
	return &http.Client{Transport: &liveShapeTransport{
		t: t, provider: provider, identity: identity, seen: make(map[string]struct{}),
	}}
}

func liveExaContractChecks(value any, identity personenrichment.Identity) []string {
	checks := map[string]bool{
		"one_result": false, "one_entity": false, "entity_type_person": false,
		"entity_id_present": false, "entity_version_positive": false,
		"public_https_url": false, "freshness_valid": false,
		"name_present": false, "name_exact": false, "company_exact": false,
	}
	root, ok := value.(map[string]any)
	if !ok {
		return renderLiveChecks(checks)
	}
	results, ok := root["results"].([]any)
	checks["one_result"] = ok && len(results) == 1
	if !checks["one_result"] {
		return renderLiveChecks(checks)
	}
	row, ok := results[0].(map[string]any)
	if !ok {
		return renderLiveChecks(checks)
	}
	if rawURL, ok := row["url"].(string); ok {
		parsed, err := url.Parse(rawURL)
		checks["public_https_url"] = err == nil && parsed.Scheme == "https" && parsed.Hostname() != ""
	}
	freshness, _ := row["publishedDate"].(string)
	_, freshnessErr := time.Parse(time.RFC3339Nano, freshness)
	checks["freshness_valid"] = freshness == "" || freshnessErr == nil
	entities, ok := row["entities"].([]any)
	checks["one_entity"] = ok && len(entities) == 1
	if !checks["one_entity"] {
		return renderLiveChecks(checks)
	}
	entity, ok := entities[0].(map[string]any)
	if !ok {
		return renderLiveChecks(checks)
	}
	checks["entity_type_person"] = entity["type"] == "person"
	id, _ := entity["id"].(string)
	checks["entity_id_present"] = strings.TrimSpace(id) != ""
	version, _ := entity["version"].(float64)
	checks["entity_version_positive"] = version > 0
	properties, ok := entity["properties"].(map[string]any)
	if !ok {
		return renderLiveChecks(checks)
	}
	name, _ := properties["name"].(string)
	checks["name_present"] = strings.TrimSpace(name) != ""
	checks["name_exact"] = liveExactText(identity.Name, name)
	workHistory, _ := properties["workHistory"].([]any)
	for _, item := range workHistory {
		work, _ := item.(map[string]any)
		company, _ := work["company"].(map[string]any)
		companyName, _ := company["name"].(string)
		if liveExactText(identity.CurrentCompany, companyName) {
			checks["company_exact"] = true
			break
		}
	}
	return renderLiveChecks(checks)
}

func liveExactText(left, right string) bool {
	return strings.EqualFold(strings.TrimSpace(left), strings.TrimSpace(right))
}

func renderLiveChecks(checks map[string]bool) []string {
	keys := make([]string, 0, len(checks))
	for key := range checks {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, fmt.Sprintf("%s=%t", key, checks[key]))
	}
	return result
}

func appendLiveJSONShape(shape *[]string, path string, value any) {
	switch typed := value.(type) {
	case map[string]any:
		*shape = append(*shape, fmt.Sprintf("%s=object[%d]", path, len(typed)))
		for key, child := range typed {
			if safeLiveJSONField(key) {
				appendLiveJSONShape(shape, path+"."+key, child)
			}
		}
	case []any:
		*shape = append(*shape, fmt.Sprintf("%s=array[%d]", path, len(typed)))
		if len(typed) > 0 {
			appendLiveJSONShape(shape, path+"[]", typed[0])
		}
	case string:
		if strings.HasSuffix(path, ".status") && safeLiveEnum(typed) {
			*shape = append(*shape, path+"=string("+typed+")")
		} else {
			*shape = append(*shape, path+"=string")
		}
	case float64:
		*shape = append(*shape, path+"=number")
	case bool:
		*shape = append(*shape, path+"=boolean")
	case nil:
		*shape = append(*shape, path+"=null")
	}
}

func safeLiveJSONField(key string) bool {
	switch key {
	case "requestId", "results", "title", "url", "publishedDate", "author", "id", "image",
		"entities", "type", "version", "properties", "name", "firstName", "lastName", "location",
		"research", "workHistory", "educationHistory", "dates", "from", "to", "company", "output",
		"resolvedSearchType", "searchTime", "costDollars", "total", "search", "contents", "run_id",
		"start_time", "close_time", "status", "result", "structured_data", "confidence_score", "findings",
		"notes", "references", "charge_amount", "task_type", "task_id", "error":
		return true
	default:
		return false
	}
}

func safeLiveEnum(value string) bool {
	if value == "" || len(value) > 32 {
		return false
	}
	for _, char := range value {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && char != '-' && char != '_' {
			return false
		}
	}
	return true
}
