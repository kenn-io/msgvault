package personenrichment_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personenrichment"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/store"
)

const contextPerson = `{"name":"Alex Example","profiles":["https://example.com/alex"],"location":{"city":"Vancouver","region":null,"country_code":"CA"},"role":{"title":"Engineer","started_on":"2024-01-01","company":{"name":"Example Labs","domain":"example.com"}},"schools":["Example University"],"skills":["Go"],"description":null,"is_founder":false,"years_of_experience":5}`

func contextConfig(endpoint string) personenrichment.ProviderConfig {
	cfg := exaConfig(endpoint, "", 0)
	cfg.Kind = personenrichment.ProviderContext
	cfg.AllowedIdentifiers = []personenrichment.IdentifierClass{personenrichment.IdentifierName, personenrichment.IdentifierCurrentCompany}
	cfg.TargetKeys = []string{"attribute:location"}
	return cfg
}

func TestContextSearchContract(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	calls := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		checks.Equal("/v1/people/search", r.URL.Path)
		checks.Equal(http.MethodPost, r.Method)
		checks.Equal("Bearer test-key", r.Header.Get("Authorization"))
		var payload map[string]any
		if !checks.NoError(json.NewDecoder(r.Body).Decode(&payload)) {
			return
		}
		assert.Equal(t, map[string]any{"query": "name: Alex Example"}, payload)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"request_id":"req-1","results":[` + strings.Replace(contextPerson, `"https://example.com/alex"`, `"https://example.com/alex","http://example.com/alex"`, 1) + `],"next_cursor":"next-page","key_metadata":{"credits_consumed":10,"credits_remaining":90}}`))
	}))
	defer server.Close()
	provider, err := personenrichment.NewContextProvider(contextConfig(server.URL+"/v1/people/search"), "test-key", server.Client())
	requirements.NoError(err)
	targets := exaTypedTargets(t)
	attempt, err := provider.Start(t.Context(), personenrichment.Request{Identity: personenrichment.Identity{Name: "Alex Example", CurrentCompany: "Example Labs"}, Targets: targets})
	requirements.NoError(err)
	requirements.NoError(attempt.Validate())
	requirements.NotNil(attempt.Result)
	checks.Equal(1, calls, "pagination must not silently spend additional requests")
	checks.Len(attempt.Result.Claims, 2)
	checks.Equal(900, attempt.Result.IdentityConfidence)
	checks.True(personenrichment.AssessIdentity(personenrichment.Request{Identity: personenrichment.Identity{Name: "Alex Example", CurrentCompany: "Example Labs"}}, *attempt.Result, nil).Accepted)
	checks.Equal(personenrichment.Cost{}, attempt.Result.Cost)
	checks.True(attempt.Result.FreshAsOf.IsZero())
	checks.Equal([]string{"https://example.com/alex"}, attempt.Result.CanonicalPublicURLs)
	checks.True(strings.HasPrefix(attempt.Result.Citations[0].Key, "context:"))
	_, err = provider.Poll(t.Context(), attempt)
	requirements.Error(err)
}

func TestContextLongCompanyStaysLocal(t *testing.T) {
	calls := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var payload map[string]any
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&payload)) {
			return
		}
		assert.Equal(t, map[string]any{"query": "name: Alex Example"}, payload)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"request_id":"r","results":[` + contextPerson + `]}`))
	}))
	defer server.Close()
	provider, err := personenrichment.NewContextProvider(contextConfig(server.URL), "test-key", server.Client())
	require.NoError(t, err)
	_, err = provider.Start(t.Context(), personenrichment.Request{
		Identity: personenrichment.Identity{Name: "Alex Example", CurrentCompany: strings.Repeat("x", 500)},
		Targets:  exaTypedTargets(t),
	})
	var failure *personenrichment.ProviderError
	require.ErrorAs(t, err, &failure)
	assert.Equal(t, personenrichment.FailureIdentityRejected, failure.Class)
	assert.Equal(t, 1, calls)
}

func TestContextSearchRejectsUnsafeResults(t *testing.T) {
	for _, test := range []struct {
		name, body string
		class      personenrichment.FailureClass
	}{
		{"empty", `{"request_id":"r","results":[]}`, personenrichment.FailureIdentityRejected},
		{"missing results", `{"request_id":"r"}`, personenrichment.FailureInvalidOutput},
		{"null results", `{"request_id":"r","results":null}`, personenrichment.FailureInvalidOutput},
		{"ambiguous", `{"request_id":"r","results":[` + contextPerson + `,` + contextPerson + `]}`, personenrichment.FailureIdentityRejected},
		{"wrong employer", `{"request_id":"r","results":[` + strings.ReplaceAll(contextPerson, "Example Labs", "Other Labs") + `]}`, personenrichment.FailureIdentityRejected},
		{"duplicate key", `{"request_id":"r","request_id":"s","results":[]}`, personenrichment.FailureInvalidOutput},
		{"missing request ID", `{"results":[]}`, personenrichment.FailureInvalidOutput},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			provider, err := personenrichment.NewContextProvider(contextConfig(server.URL), "test-key", server.Client())
			require.NoError(t, err)
			_, err = provider.Start(t.Context(), personenrichment.Request{Identity: personenrichment.Identity{Name: "Alex Example", CurrentCompany: "Example Labs"}, Targets: exaTypedTargets(t)})
			var failure *personenrichment.ProviderError
			require.ErrorAs(t, err, &failure)
			assert.Equal(t, test.class, failure.Class)
		})
	}
}

func TestContextHTTPFailures(t *testing.T) {
	for _, test := range []struct {
		status int
		class  personenrichment.FailureClass
	}{{401, personenrichment.FailureTerminal}, {422, personenrichment.FailureTerminal}, {408, personenrichment.FailureTransient}, {429, personenrichment.FailureRateLimited}, {503, personenrichment.FailureTransient}, {302, personenrichment.FailureTerminal}} {
		t.Run(http.StatusText(test.status), func(t *testing.T) {
			checks := assert.New(t)
			requirements := require.New(t)
			calls := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Retry-After", "30")
				w.Header().Set("Location", "/redirect")
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte("private provider body"))
			}))
			defer server.Close()
			provider, err := personenrichment.NewContextProvider(contextConfig(server.URL), "test-key", server.Client())
			requirements.NoError(err)
			_, err = provider.Start(t.Context(), personenrichment.Request{Identity: personenrichment.Identity{Name: "Alex Example", CurrentCompany: "Example Labs"}, Targets: exaTypedTargets(t)})
			var failure *personenrichment.ProviderError
			requirements.ErrorAs(err, &failure)
			checks.Equal(test.class, failure.Class)
			checks.Equal("30", failure.RetryAfter)
			checks.NotContains(err.Error(), "private")
			checks.Equal(1, calls)
		})
	}
}

func TestContextPolicyAndTargets(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	cfg := contextConfig("")
	cfg.APIKeyEnv = ""
	cfg.ApplyDefaults()
	checks.Equal("https://api.context.dev/v1/people/search", cfg.Endpoint)
	checks.Equal("CONTEXT_DEV_API_KEY", cfg.APIKeyEnv)
	requirements.NoError(cfg.Validate())
	location := exaAttributeTarget(t, "attribute:location", personfacts.ValueText, personfacts.CardinalitySingle)
	profile, err := cfg.Profile(personfacts.Catalog{Targets: []personfacts.TargetDescriptor{location}})
	requirements.NoError(err)
	requirements.NoError(profile.Validate())
	_, _, err = personenrichment.BuildRequest(personenrichment.RequestInput{
		PersonID: 1, Names: []personenrichment.IdentityCandidate{{StableID: 1, Value: strings.Repeat("界", 495)}},
		CurrentCompanies: []personenrichment.IdentityCandidate{{StableID: 1, Value: "Example Labs"}},
		Catalog:          personfacts.Catalog{Targets: []personfacts.TargetDescriptor{location}},
	}, profile)
	require.ErrorContains(t, err, "500 characters")
	cfg.AllowedIdentifiers = append(cfg.AllowedIdentifiers, personenrichment.IdentifierEmail)
	requirements.Error(cfg.Validate())
	cfg = contextConfig("https://api.context.dev/v1/people/search")
	cfg.MaxCostUSDMicrosPerDay = 100
	requirements.ErrorContains(cfg.Validate(), "hard cost caps unsupported")
	cfg = contextConfig("https://api.context.dev/v1/people/search")
	cfg.TargetKeys = []string{"attribute:summary"}
	_, err = cfg.Profile(personfacts.Catalog{Targets: exaDeepTargets(t)})
	requirements.ErrorContains(err, "unsupported context target")
}

func TestContextMissingFieldsDoNotClearFacts(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"request_id":"r","results":[{"name":"Alex Example","profiles":["https://example.com/alex"],"location":{"city":null,"region":null,"country_code":null},"role":{"title":null,"started_on":null,"company":{"name":"Example Labs"}},"schools":[]}]}`))
	}))
	defer server.Close()
	provider, err := personenrichment.NewContextProvider(contextConfig(server.URL), "test-key", server.Client())
	require.NoError(t, err)
	attempt, err := provider.Start(t.Context(), personenrichment.Request{Identity: personenrichment.Identity{Name: "Alex Example", CurrentCompany: "Example Labs"}, Targets: []personfacts.TargetDescriptor{exaAttributeTarget(t, "attribute:location", personfacts.ValueText, personfacts.CardinalitySingle)}})
	require.NoError(t, err)
	assert.Empty(t, attempt.Result.Claims)
}

func TestContextInvalidRequestsDoNotSend(t *testing.T) {
	calls := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(http.StatusInternalServerError) }))
	defer server.Close()
	provider, err := personenrichment.NewContextProvider(contextConfig(server.URL), "test-key", server.Client())
	require.NoError(t, err)
	for _, identity := range []personenrichment.Identity{
		{Name: "Alex Example"},
		{Name: strings.Repeat("界", 495), CurrentCompany: "Example Labs"},
		{Name: "Alex Example", CurrentCompany: "Example Labs", Email: "alex@example.com"},
	} {
		_, err := provider.Start(t.Context(), personenrichment.Request{Identity: identity, Targets: exaTypedTargets(t)})
		require.Error(t, err)
	}
	assert.Zero(t, calls)
}

func TestContextWorkerCommitsAndRefreshesThroughConsentPipeline(t *testing.T) {
	requirements := require.New(t)
	checks := assert.New(t)
	calls := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		name := "Worker Person"
		if calls > 1 {
			name = "WORKER PERSON"
		}
		body := `{"request_id":"worker-context","results":[` + strings.ReplaceAll(contextPerson, "Alex Example", name) + `]}`
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	f := newWorkerFixture(t, "context-worker", func(cfg *personenrichment.ProviderConfig) {
		cfg.Kind = personenrichment.ProviderContext
		cfg.Endpoint = server.URL
		cfg.Mode = ""
		cfg.NumResults = 0
		cfg.AllowedIdentifiers = []personenrichment.IdentifierClass{personenrichment.IdentifierName, personenrichment.IdentifierCurrentCompany}
		cfg.TargetKeys = []string{"system:employment"}
	})
	organization, err := f.store.CreateOrganizationContext(t.Context(), store.OrganizationInput{Name: "Example Labs"})
	requirements.NoError(err)
	_, err = f.store.AddEmploymentContext(t.Context(), store.EmploymentInput{PersonID: f.person.ID, OrganizationID: organization.ID, IsCurrent: new(true), Source: store.ProvenanceUser})
	requirements.NoError(err)
	worker := f.newWorker(t, map[string]personenrichment.ProviderFactory{f.config.Name: func(cfg personenrichment.ProviderConfig, key string) (personenrichment.Provider, error) {
		return personenrichment.NewContextProvider(cfg, key, server.Client())
	}}, map[string]personenrichment.ProviderConfig{f.config.Name: f.config}, func(string) (string, bool) { return "test-key", true })
	processed, err := worker.RunOnce(t.Context(), f.run.ID)
	requirements.NoError(err)
	requirements.True(processed)
	attempts, err := f.store.ListPersonEnrichmentAttemptsContext(t.Context(), store.PersonEnrichmentAttemptFilter{PersonID: f.person.ID, RunID: f.run.ID, Limit: 10})
	requirements.NoError(err)
	requirements.Len(attempts, 1)
	checks.Equal("succeeded", attempts[0].State)
	checks.Nil(attempts[0].FailureClass)
	var claims int
	requirements.NoError(f.store.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM person_fact_claims").Scan(&claims))
	checks.Equal(1, claims)
	// A changed display name on the same profile URL must not collide with an
	// earlier citation's immutable metadata when refreshing the same person.
	requirements.NoError(f.store.PutPersonEnrichmentWorkContext(t.Context(), store.PersonEnrichmentWorkInput{
		PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint,
		Trigger: personenrichment.Trigger{Kind: personenrichment.TriggerManual, Generation: "context-refresh"},
		DueAt:   time.Now().UTC().Add(-time.Second),
	}))
	processed, err = worker.RunOnce(t.Context(), f.run.ID)
	requirements.NoError(err)
	requirements.True(processed)
	attempts, err = f.store.ListPersonEnrichmentAttemptsContext(t.Context(), store.PersonEnrichmentAttemptFilter{PersonID: f.person.ID, Limit: 10})
	requirements.NoError(err)
	requirements.Len(attempts, 2)
	for _, attempt := range attempts {
		checks.Equal("succeeded", attempt.State)
	}
	checks.Equal(2, calls)
}

func TestContextWithoutSafeProfileLinksUsesProviderAssertions(t *testing.T) {
	for _, profiles := range []string{`[]`, `["http://example.com/alex"]`} {
		t.Run(profiles, func(t *testing.T) {
			checks := assert.New(t)
			requirements := require.New(t)
			body := `{"request_id":"r","results":[` + strings.Replace(contextPerson, `["https://example.com/alex"]`, profiles, 1) + `]}`
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(body))
			}))
			defer server.Close()
			provider, err := personenrichment.NewContextProvider(contextConfig(server.URL), "test-key", server.Client())
			requirements.NoError(err)
			attempt, err := provider.Start(t.Context(), personenrichment.Request{Identity: personenrichment.Identity{Name: "Alex Example", CurrentCompany: "Example Labs"}, Targets: exaTypedTargets(t)})
			requirements.NoError(err)
			checks.Empty(attempt.Result.CanonicalPublicURLs)
			checks.Empty(attempt.Result.Citations)
			requirements.Len(attempt.Result.Claims, 2)
			checks.Equal(personfacts.EvidenceProviderAssertion, attempt.Result.Claims[0].Evidence[0].SourceClass)
		})
	}
}
