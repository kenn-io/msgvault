package personenrichment_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personenrichment"
	"go.kenn.io/msgvault/internal/personfacts"
)

func TestExaBuildOutputSchemaUsesOnlyExactTargetDescriptors(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	targets := exaDeepTargets(t)

	schema, err := personenrichment.BuildExaOutputSchema(targets)
	requirements.NoError(err)
	checks.JSONEq(`{
	  "type":"object",
	  "properties":{
	    "attribute:summary":{
	      "type":"string",
	      "description":"One-sentence public profile summary",
	      "maxLength":240
	    },
	    "system:employment":{
	      "type":"array",
	      "description":"Current and historical employment, including organization, title, role, department, location, and partial start and end dates",
	      "items":{
	        "type":"object",
	        "properties":{
	          "organization":{
	            "type":"object",
	            "properties":{
	              "name":{"type":"string"},
	              "domain":{"type":"string"}
	            },
	            "required":["name"],
	            "additionalProperties":false
	          },
	          "title":{"type":"string"},
	          "role":{"type":"string"},
	          "department":{"type":"string"},
	          "location":{"type":"string"},
	          "start_date":{
	            "type":"object",
	            "properties":{
	              "year":{"type":"integer"},
	              "month":{"type":"integer"},
	              "day":{"type":"integer"}
	            },
	            "required":["year"],
	            "additionalProperties":false
	          },
	          "end_date":{
	            "type":"object",
	            "properties":{
	              "year":{"type":"integer"},
	              "month":{"type":"integer"},
	              "day":{"type":"integer"}
	            },
	            "required":["year"],
	            "additionalProperties":false
	          }
	        },
	        "required":["organization"],
	        "additionalProperties":false
	      }
	    }
	  },
	  "required":["attribute:summary","system:employment"],
	  "additionalProperties":false
	}`, string(schema))
	checks.NotContains(string(schema), "current_value")
	checks.NotContains(string(schema), "database_id")
	checks.NotContains(string(schema), "display_label")
	checks.NotContains(string(schema), "unrelated")
}

func TestExaBuildOutputSchemaMapsSupportedTypesAndRejectsUnsupported(t *testing.T) {
	types := []struct {
		valueType personfacts.ValueType
		want      string
	}{
		{personfacts.ValueText, `"type":"string"`},
		{personfacts.ValueInteger, `"type":"integer"`},
		{personfacts.ValueReal, `"type":"number"`},
		{personfacts.ValueBoolean, `"type":"boolean"`},
		{personfacts.ValueDate, `"format":"date"`},
		{personfacts.ValueTimestamp, `"format":"date-time"`},
	}
	for _, test := range types {
		t.Run(string(test.valueType), func(t *testing.T) {
			target := exaAttributeTarget(t, "attribute:test", test.valueType, personfacts.CardinalitySingle)
			schema, err := personenrichment.BuildExaOutputSchema([]personfacts.TargetDescriptor{target})
			require.NoError(t, err)
			assert.Contains(t, string(schema), test.want)
		})
	}

	multi := exaAttributeTarget(t, "attribute:multi", personfacts.ValueInteger, personfacts.CardinalityMulti)
	schema, err := personenrichment.BuildExaOutputSchema([]personfacts.TargetDescriptor{multi})
	require.NoError(t, err)
	assert.JSONEq(t, `{"type":"object","properties":{"attribute:multi":{"type":"array","description":"Synthetic attribute:multi","items":{"type":"integer"}}},"required":["attribute:multi"],"additionalProperties":false}`, string(schema))

	unsupported := exaAttributeTarget(t, "attribute:unsupported", personfacts.ValueText, personfacts.CardinalitySingle)
	unsupported.ValueType = "json"
	unsupported.Revision = exaTargetRevision(t, unsupported)
	_, err = personenrichment.BuildExaOutputSchema([]personfacts.TargetDescriptor{unsupported})
	require.ErrorContains(t, err, "unsupported")
}

func TestExaFactConfidenceMappings(t *testing.T) {
	for _, test := range []struct {
		label    string
		grounded bool
		want     int
	}{
		{"low", true, personenrichment.ExaGroundingLowScore},
		{" Medium ", true, personenrichment.ExaGroundingMediumScore},
		{"HIGH", true, personenrichment.ExaGroundingHighScore},
		{"", false, personenrichment.ExaTypedUngroundedScore},
	} {
		score, err := personenrichment.ExaFactConfidence(test.label, test.grounded)
		require.NoError(t, err)
		assert.Equal(t, test.want, score)
	}
	for _, label := range []string{"", "unknown"} {
		_, err := personenrichment.ExaFactConfidence(label, true)
		require.Error(t, err)
	}
}

func TestExaTypedPeopleRequestAndResponse(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	fixture := exaFixture(t, "exa_people_success.json")
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		checks.Equal(http.MethodPost, r.Method)
		checks.Equal("/search", r.URL.Path)
		checks.Equal("Bearer test-key", r.Header.Get("Authorization"))
		checks.Equal("application/json", r.Header.Get("Content-Type"))
		body, err := io.ReadAll(r.Body)
		if !checks.NoError(err) {
			return
		}
		checks.NotContains(string(body), "test-key")
		var request map[string]any
		if !checks.NoError(json.Unmarshal(body, &request)) {
			return
		}
		checks.Equal(map[string]any{
			"query":      "name: Test User; company: Example Labs",
			"category":   "people",
			"type":       "auto",
			"numResults": float64(1),
		}, request)
		for _, forbidden := range []string{"startPublishedDate", "endPublishedDate", "includeDomains", "excludeDomains"} {
			checks.NotContains(request, forbidden)
		}
		w.Header().Set("Content-Type", "application/json")
		_, err = w.Write(fixture)
		checks.NoError(err)
	}))
	defer server.Close()

	provider, err := personenrichment.NewExaProvider(
		exaConfig(server.URL+"/search", "people", 1), "test-key", server.Client(),
	)
	requirements.NoError(err)
	checks.Zero(calls.Load(), "constructing the adapter must not send credentials")
	attempt, err := provider.Start(t.Context(), personenrichment.Request{
		RequestHash: strings.Repeat("a", 64),
		Identity:    personenrichment.Identity{Name: "Test User", CurrentCompany: "Example Labs"},
		Targets:     exaTypedTargets(t),
	})
	requirements.NoError(err)
	requirements.NoError(attempt.Validate())
	checks.Equal(int32(1), calls.Load())
	checks.Equal(personenrichment.AttemptComplete, attempt.State)
	checks.Equal("request_test_people_42", attempt.RequestID)
	checks.Equal(personenrichment.ExaAdapterVersionV1, attempt.AdapterVersion)
	checks.Equal(personenrichment.ExaSearchWireSchemaV1, attempt.SchemaVersion)
	checks.False(attempt.GeneratedSchema)
	checks.Empty(attempt.GeneratedSchemaHash)
	checks.Len(attempt.Result.Claims, 2)
	for _, claim := range attempt.Result.Claims {
		checks.Equal(personenrichment.ExaTypedUngroundedScore, claim.Confidence.ReportedScore)
	}
	checks.Equal([]personenrichment.ProviderPersonID{{ID: "person_test_42", Confidence: 900}}, attempt.Result.ProviderPersonIDs)
	checks.Equal([]string{"https://profiles.example.test/test-user"}, attempt.Result.CanonicalPublicURLs)
	checks.Equal("1", attempt.Result.ProviderVersion)
	checks.Equal(personenrichment.Cost{Currency: "USD", AmountMicros: 7000, Estimated: true}, attempt.Result.Cost)
	checks.Equal(time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC), attempt.Result.FreshAsOf)
	checks.NotZero(attempt.Result.Citations[0].RetrievedAt)
	checks.Equal("cited", attempt.Result.SourceAttempts[0].Outcome)
	_, err = provider.Poll(t.Context(), attempt)
	requirements.ErrorContains(err, "synchronous")
}

func TestExaRejectsHistoricalEmploymentAsCurrentCompanyMatch(t *testing.T) {
	requirements := require.New(t)
	fixture := exaFixture(t, "exa_people_success.json")
	requirements.Contains(string(fixture), `"to": null`)
	fixture = bytes.Replace(fixture, []byte(`"to": null`), []byte(`"to": "2024-01-01"`), 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	}))
	defer server.Close()

	provider, err := personenrichment.NewExaProvider(
		exaConfig(server.URL+"/search", "people", 1), "test-key", server.Client(),
	)
	requirements.NoError(err)
	_, err = provider.Start(t.Context(), personenrichment.Request{
		Identity: personenrichment.Identity{Name: "Test User", CurrentCompany: "Example Labs"},
		Targets:  exaTypedTargets(t),
	})
	requirements.Error(err)
	var providerErr *personenrichment.ProviderError
	requirements.ErrorAs(err, &providerErr)
	assert.Equal(t, personenrichment.FailureInvalidOutput, providerErr.Class)
}

func TestExaDeepModesPreserveGroundingAndBindGeneratedSchema(t *testing.T) {
	fixture := exaFixture(t, "exa_deep_success.json")
	for _, mode := range []string{"deep", "deep-reasoning"} {
		t.Run(mode, func(t *testing.T) {
			checks := assert.New(t)
			requirements := require.New(t)
			var requestBody []byte
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var err error
				requestBody, err = io.ReadAll(r.Body)
				if !checks.NoError(err) {
					return
				}
				var request map[string]json.RawMessage
				if !checks.NoError(json.Unmarshal(requestBody, &request)) {
					return
				}
				checks.Equal([]string{"category", "numResults", "outputSchema", "query", "type"}, sortedMapKeys(request))
				checks.JSONEq(`"`+mode+`"`, string(request["type"]))
				checks.NotContains(string(requestBody), "test-key")
				w.Header().Set("Content-Type", "application/json")
				_, err = w.Write(fixture)
				checks.NoError(err)
			}))
			defer server.Close()

			provider, err := personenrichment.NewExaProvider(
				exaConfig(server.URL+"/search", mode, 1), "test-key", server.Client(),
			)
			requirements.NoError(err)
			targets := exaDeepTargets(t)
			attempt, err := provider.Start(t.Context(), personenrichment.Request{
				RequestHash: strings.Repeat("b", 64),
				Identity: personenrichment.Identity{
					Name: "Test User", PublicProfileURLs: []string{"https://sources.example.test/test-user"},
				},
				Targets: targets,
			})
			requirements.NoError(err)
			requirements.NoError(attempt.Validate())
			var request map[string]json.RawMessage
			requirements.NoError(json.Unmarshal(requestBody, &request))
			digest := sha256.Sum256(request["outputSchema"])
			wantHash := hex.EncodeToString(digest[:])
			checks.True(attempt.GeneratedSchema)
			checks.Equal(wantHash, attempt.GeneratedSchemaHash)
			checks.Equal(wantHash, attempt.Result.GeneratedSchemaHash)
			checks.Len(attempt.Result.Claims, 2)
			checks.Equal(personenrichment.ExaGroundingHighScore, attempt.Result.Claims[0].Confidence.ReportedScore)
			checks.Equal(personenrichment.ExaGroundingMediumScore, attempt.Result.Claims[1].Confidence.ReportedScore)
			checks.Equal("search-api-unversioned", attempt.Result.ProviderVersion)
			checks.Len(attempt.Result.Citations, 2)
			checks.Len(attempt.Result.SourceAttempts, 2)
			checks.Equal(personenrichment.Cost{Currency: "USD", AmountMicros: 12000, Estimated: true}, attempt.Result.Cost)
			checks.Equal(1000, attempt.Result.IdentityConfidence)

			firstFingerprint, err := personenrichment.ProgramFingerprint(personenrichment.ProgramDescriptor{
				HostMappingVersion:  personenrichment.HostClaimMappingVersion,
				AdapterVersion:      attempt.AdapterVersion,
				WireSchemaVersion:   attempt.SchemaVersion,
				GeneratedSchema:     true,
				GeneratedSchemaHash: attempt.GeneratedSchemaHash,
			})
			requirements.NoError(err)
			changed := targets
			changed[0].Description = "A changed public profile summary"
			changed[0].Revision = exaTargetRevision(t, changed[0])
			changedSchema, err := personenrichment.BuildExaOutputSchema(changed)
			requirements.NoError(err)
			changedDigest := sha256.Sum256(changedSchema)
			changedHash := hex.EncodeToString(changedDigest[:])
			checks.NotEqual(wantHash, changedHash)
			changedFingerprint, err := personenrichment.ProgramFingerprint(personenrichment.ProgramDescriptor{
				HostMappingVersion:  personenrichment.HostClaimMappingVersion,
				AdapterVersion:      attempt.AdapterVersion,
				WireSchemaVersion:   attempt.SchemaVersion,
				GeneratedSchema:     true,
				GeneratedSchemaHash: changedHash,
			})
			requirements.NoError(err)
			checks.NotEqual(firstFingerprint, changedFingerprint)
		})
	}
}

func TestExaRejectsAdverseHTTPResponsesWithoutLeakingPrivateData(t *testing.T) {
	const responseSentinel = "private-response-sentinel"
	const requestIdentity = "private-identity-sentinel"
	tests := []struct {
		name        string
		status      int
		contentType string
		body        []byte
		retryAfter  string
		wantClass   personenrichment.FailureClass
	}{
		{"rate limited", http.StatusTooManyRequests, "application/json", exaFixture(t, "exa_people_error.json"), "17", personenrichment.FailureRateLimited},
		{"server error", http.StatusBadGateway, "application/json", []byte(`{"requestId":"request_5xx","error":{"message":"` + responseSentinel + `"}}`), "", personenrichment.FailureTransient},
		{"client error", http.StatusBadRequest, "application/json", []byte(`{"requestId":"request_4xx","error":{"message":"` + responseSentinel + `"}}`), "", personenrichment.FailureTerminal},
		{"wrong content type", http.StatusOK, "text/plain", []byte(responseSentinel), "", personenrichment.FailureInvalidOutput},
		{"malformed JSON", http.StatusOK, "application/json", []byte(`{"requestId":` + responseSentinel), "", personenrichment.FailureInvalidOutput},
		{"trailing JSON", http.StatusOK, "application/json", append(exaFixture(t, "exa_people_success.json"), []byte(` {"sentinel":"`+responseSentinel+`"}`)...), "", personenrichment.FailureInvalidOutput},
		{"oversized body", http.StatusOK, "application/json", bytes.Repeat([]byte("x"), (1<<20)+1), "", personenrichment.FailureInvalidOutput},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			checks := assert.New(t)
			requirements := require.New(t)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", test.contentType)
				if test.retryAfter != "" {
					w.Header().Set("Retry-After", test.retryAfter)
				}
				w.WriteHeader(test.status)
				_, _ = w.Write(test.body)
			}))
			defer server.Close()
			provider, err := personenrichment.NewExaProvider(exaConfig(server.URL+"/search", "people", 1), "test-key", server.Client())
			requirements.NoError(err)
			_, err = provider.Start(t.Context(), personenrichment.Request{
				RequestHash: strings.Repeat("c", 64), Identity: personenrichment.Identity{Name: requestIdentity},
				Targets: exaTypedTargets(t),
			})
			requirements.Error(err)
			checks.NotContains(err.Error(), "test-key")
			checks.NotContains(err.Error(), requestIdentity)
			checks.NotContains(err.Error(), responseSentinel)
			var providerErr *personenrichment.ProviderError
			requirements.ErrorAs(err, &providerErr)
			checks.Equal(test.wantClass, providerErr.Class)
			checks.Equal(test.retryAfter, providerErr.RetryAfter)
		})
	}
	t.Run("malformed retry-after is discarded", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", responseSentinel)
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write(exaFixture(t, "exa_people_error.json"))
		}))
		defer server.Close()
		provider, err := personenrichment.NewExaProvider(exaConfig(server.URL+"/search", "people", 1), "test-key", server.Client())
		require.NoError(t, err)
		_, err = provider.Start(t.Context(), personenrichment.Request{
			Identity: personenrichment.Identity{Name: requestIdentity}, Targets: exaTypedTargets(t),
		})
		var providerErr *personenrichment.ProviderError
		require.ErrorAs(t, err, &providerErr)
		assert.Empty(t, providerErr.RetryAfter)
	})
}

func TestExaRejectsRedirects(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	redirectTarget := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		checks.Fail("redirect target must not be called")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer redirectTarget.Close()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{}, redirectTarget.URL+"/private-identity-sentinel", http.StatusFound)
	}))
	defer server.Close()
	provider, err := personenrichment.NewExaProvider(exaConfig(server.URL+"/search", "people", 1), "test-key", server.Client())
	requirements.NoError(err)
	_, err = provider.Start(t.Context(), personenrichment.Request{Identity: personenrichment.Identity{Name: "Test User"}, Targets: exaTypedTargets(t)})
	requirements.Error(err)
	checks.NotContains(err.Error(), "private-identity-sentinel")
	var providerErr *personenrichment.ProviderError
	requirements.ErrorAs(err, &providerErr)
	checks.Equal(personenrichment.FailureTerminal, providerErr.Class)
}

func TestExaEnforcesConfiguredRequestTimeout(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	fixture := exaFixture(t, "exa_people_success.json")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-time.After(100 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	}))
	defer server.Close()
	config := exaConfig(server.URL+"/search", "people", 1)
	config.RequestTimeout = 10 * time.Millisecond
	provider, err := personenrichment.NewExaProvider(config, "test-key", server.Client())
	requirements.NoError(err)
	_, err = provider.Start(t.Context(), personenrichment.Request{
		Identity: personenrichment.Identity{Name: "Test User"}, Targets: exaTypedTargets(t),
	})
	requirements.Error(err)
	var providerErr *personenrichment.ProviderError
	requirements.ErrorAs(err, &providerErr)
	checks.Equal(personenrichment.FailureTransient, providerErr.Class)
}

func TestExaRejectsInvalidTypedEntitiesAndStructuredOutput(t *testing.T) {
	typedFixture := exaFixture(t, "exa_people_success.json")
	deepFixture := exaFixture(t, "exa_deep_success.json")
	tests := []struct {
		name    string
		mode    string
		fixture []byte
	}{
		{"missing entity", "people", exaEmptyTypedEntities(t, typedFixture)},
		{"duplicate entity ID", "people", duplicateTypedEntity(t, typedFixture)},
		{"non-person entity", "people", bytes.Replace(typedFixture, []byte(`"type": "person"`), []byte(`"type": "company"`), 1)},
		{"unknown typed property", "people", bytes.Replace(typedFixture, []byte(`"location": "Test City",`), []byte(`"location": "Test City", "response_sentinel": "private",`), 1)},
		{"unknown structured target", "deep", bytes.Replace(deepFixture, []byte(`"attribute:summary": "Synthetic profile summary",`), []byte(`"attribute:summary": "Synthetic profile summary", "attribute:unknown": "private",`), 1)},
		{"missing grounding", "deep", exaEmptyDeepGrounding(t, deepFixture)},
		{"unknown confidence", "deep", bytes.Replace(deepFixture, []byte(`"confidence": "high"`), []byte(`"confidence": "unknown"`), 1)},
		{"unsafe citation", "deep", bytes.Replace(deepFixture, []byte(`https://sources.example.test/test-user/work"`), []byte(`http://127.0.0.1/private"`), 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			checks := assert.New(t)
			requirements := require.New(t)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(test.fixture)
			}))
			defer server.Close()
			provider, err := personenrichment.NewExaProvider(exaConfig(server.URL+"/search", test.mode, 1), "test-key", server.Client())
			requirements.NoError(err)
			targets := exaTypedTargets(t)
			if test.mode != "people" {
				targets = exaDeepTargets(t)
			}
			_, err = provider.Start(t.Context(), personenrichment.Request{Identity: personenrichment.Identity{Name: "Test User"}, Targets: targets})
			requirements.Error(err)
			checks.NotContains(err.Error(), "private")
			var providerErr *personenrichment.ProviderError
			requirements.ErrorAs(err, &providerErr)
			checks.Equal(personenrichment.FailureInvalidOutput, providerErr.Class)
		})
	}
}

func TestExaRejectsDeepOutputWithoutReturnedIdentityMatch(t *testing.T) {
	requirements := require.New(t)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(exaFixture(t, "exa_deep_success.json"))
	}))
	defer server.Close()
	provider, err := personenrichment.NewExaProvider(
		exaConfig(server.URL+"/search", "deep", 1), "test-key", server.Client(),
	)
	requirements.NoError(err)
	_, err = provider.Start(t.Context(), personenrichment.Request{
		Identity: personenrichment.Identity{
			Name: "Test User", PublicProfileURLs: []string{"https://profiles.example.test/different-person"},
		},
		Targets: exaDeepTargets(t),
	})
	requirements.Error(err)
	var providerErr *personenrichment.ProviderError
	requirements.ErrorAs(err, &providerErr)
	assert.Equal(t, personenrichment.FailureInvalidOutput, providerErr.Class)
}

func TestExaRejectsDeepOutputAcrossAmbiguousResultRows(t *testing.T) {
	requirements := require.New(t)
	fixture := appendDeepResult(t, exaFixture(t, "exa_deep_success.json"), map[string]any{
		"title":         "Different synthetic person",
		"url":           "https://sources.example.test/different-person",
		"publishedDate": "2026-08-22T12:00:00Z",
	})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	}))
	defer server.Close()
	provider, err := personenrichment.NewExaProvider(
		exaConfig(server.URL+"/search", "deep", 1), "test-key", server.Client(),
	)
	requirements.NoError(err)
	_, err = provider.Start(t.Context(), personenrichment.Request{
		Identity: personenrichment.Identity{
			Name: "Test User", PublicProfileURLs: []string{"https://sources.example.test/test-user"},
		},
		Targets: exaDeepTargets(t),
	})
	requirements.Error(err)
	var providerErr *personenrichment.ProviderError
	requirements.ErrorAs(err, &providerErr)
	assert.Equal(t, personenrichment.FailureInvalidOutput, providerErr.Class)
}

func TestExaRejectsTypedOutputWithoutReturnedIdentityMatch(t *testing.T) {
	requirements := require.New(t)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(exaFixture(t, "exa_people_success.json"))
	}))
	defer server.Close()
	provider, err := personenrichment.NewExaProvider(
		exaConfig(server.URL+"/search", "people", 1), "test-key", server.Client(),
	)
	requirements.NoError(err)
	_, err = provider.Start(t.Context(), personenrichment.Request{
		Identity: personenrichment.Identity{Name: "Different Synthetic Person"},
		Targets:  exaTypedTargets(t),
	})
	requirements.Error(err)
	var providerErr *personenrichment.ProviderError
	requirements.ErrorAs(err, &providerErr)
	assert.Equal(t, personenrichment.FailureInvalidOutput, providerErr.Class)
}

func TestExaRejectsDuplicateDeepContentMembers(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	fixture := bytes.Replace(
		exaFixture(t, "exa_deep_success.json"),
		[]byte(`"attribute:summary": "Synthetic profile summary",`),
		[]byte(`"attribute:summary": "Grounded synthetic profile", "attribute:summary": "private-response-sentinel",`),
		1,
	)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	}))
	defer server.Close()
	provider, err := personenrichment.NewExaProvider(
		exaConfig(server.URL+"/search", "deep", 1), "test-key", server.Client(),
	)
	requirements.NoError(err)
	_, err = provider.Start(t.Context(), personenrichment.Request{
		Identity: personenrichment.Identity{
			Name: "Test User", PublicProfileURLs: []string{"https://sources.example.test/test-user"},
		},
		Targets: exaDeepTargets(t),
	})
	requirements.Error(err)
	checks.NotContains(err.Error(), "private-response-sentinel")
	var providerErr *personenrichment.ProviderError
	requirements.ErrorAs(err, &providerErr)
	checks.Equal(personenrichment.FailureInvalidOutput, providerErr.Class)
}

func TestExaRejectsUnsupportedPeopleTargetsAndResultCounts(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(exaFixture(t, "exa_people_success.json"))
	}))
	defer server.Close()
	for _, numResults := range []int{0, 101} {
		_, err := personenrichment.NewExaProvider(exaConfig(server.URL+"/search", "people", numResults), "test-key", server.Client())
		require.ErrorContains(t, err, "num_results")
	}
	provider, err := personenrichment.NewExaProvider(exaConfig(server.URL+"/search", "people", 1), "test-key", server.Client())
	require.NoError(t, err)
	unsupported := exaAttributeTarget(t, "attribute:arbitrary", personfacts.ValueText, personfacts.CardinalitySingle)
	_, err = provider.Start(t.Context(), personenrichment.Request{Identity: personenrichment.Identity{Name: "Test User"}, Targets: []personfacts.TargetDescriptor{unsupported}})
	require.ErrorContains(t, err, "unsupported")
}

func TestExaPeopleAcceptsProductionNullResearchField(t *testing.T) {
	fixture := bytes.Replace(exaFixture(t, "exa_people_success.json"),
		[]byte(`"location": "Test City",`),
		[]byte(`"location": "Test City", "research": null,`), 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	}))
	defer server.Close()
	provider, err := personenrichment.NewExaProvider(
		exaConfig(server.URL+"/search", "people", 1), "test-key", server.Client())
	require.NoError(t, err)
	attempt, err := provider.Start(t.Context(), personenrichment.Request{
		Identity: personenrichment.Identity{Name: "Test User", CurrentCompany: "Example Labs"},
		Targets:  exaTypedTargets(t),
	})
	require.NoError(t, err)
	require.NoError(t, attempt.Validate())
}

func exaConfig(endpoint, mode string, numResults int) personenrichment.ProviderConfig {
	return personenrichment.ProviderConfig{
		Name: "exa-test", Kind: personenrichment.ProviderExa, Enabled: true,
		Endpoint: endpoint, APIKeyEnv: "EXA_API_KEY", Mode: mode, NumResults: numResults,
		AllowedIdentifiers: []personenrichment.IdentifierClass{personenrichment.IdentifierName},
		TargetKeys:         []string{"attribute:summary"}, RetentionPosture: "zero_retention",
		TrainingPosture: "no_training", RefreshInterval: 24 * time.Hour,
		RequestTimeout: time.Second, PollInterval: time.Second, MaxJobAge: 15 * time.Minute,
		MaxRetries: 2, MaxRequestsPerRun: 10, MaxRequestsPerDay: 100,
	}
}

func exaTypedTargets(t *testing.T) []personfacts.TargetDescriptor {
	t.Helper()
	location := exaAttributeTarget(t, "location", personfacts.ValueText, personfacts.CardinalitySingle)
	employment := exaEmploymentTarget(t)
	return []personfacts.TargetDescriptor{location, employment}
}

func exaDeepTargets(t *testing.T) []personfacts.TargetDescriptor {
	t.Helper()
	summary := exaAttributeTarget(t, "attribute:summary", personfacts.ValueText, personfacts.CardinalitySingle)
	summary.Description = "One-sentence public profile summary"
	summary.MaxLength = 240
	summary.Revision = exaTargetRevision(t, summary)
	return []personfacts.TargetDescriptor{summary, exaEmploymentTarget(t)}
}

func exaAttributeTarget(t *testing.T, key string, valueType personfacts.ValueType, cardinality personfacts.Cardinality) personfacts.TargetDescriptor {
	t.Helper()
	target := personfacts.TargetDescriptor{
		Kind: personfacts.TargetAttribute, Key: key, UniversalID: key,
		Slug: strings.TrimPrefix(key, "attribute:"), Description: "Synthetic " + key,
		ValueType: valueType, Cardinality: cardinality, Sensitive: false,
	}
	target.Revision = exaTargetRevision(t, target)
	return target
}

func exaEmploymentTarget(t *testing.T) personfacts.TargetDescriptor {
	t.Helper()
	target := personfacts.TargetDescriptor{
		Kind: personfacts.TargetEmployment, Key: "system:employment",
		UniversalID: "system:employment", Slug: "employment",
		Description: "Current and historical employment, including organization, title, role, department, location, and partial start and end dates",
		ValueType:   personfacts.ValueEmployment, Cardinality: personfacts.CardinalityMulti,
		Fields: []personfacts.FieldDescriptor{
			{Name: "organization", ValueType: personfacts.ValueOrganization, Cardinality: personfacts.CardinalitySingle, Required: true},
			{Name: "title", ValueType: personfacts.ValueText, Cardinality: personfacts.CardinalitySingle},
			{Name: "role", ValueType: personfacts.ValueText, Cardinality: personfacts.CardinalitySingle},
			{Name: "department", ValueType: personfacts.ValueText, Cardinality: personfacts.CardinalitySingle},
			{Name: "location", ValueType: personfacts.ValueText, Cardinality: personfacts.CardinalitySingle},
			{Name: "start_date", ValueType: personfacts.ValuePartialDate, Cardinality: personfacts.CardinalitySingle},
			{Name: "end_date", ValueType: personfacts.ValuePartialDate, Cardinality: personfacts.CardinalitySingle},
		},
	}
	target.Revision = exaTargetRevision(t, target)
	return target
}

func exaTargetRevision(t *testing.T, target personfacts.TargetDescriptor) string {
	t.Helper()
	revision, err := personfacts.DescriptorRevision(target)
	require.NoError(t, err)
	return revision
}

func exaFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	require.NoError(t, err)
	return data
}

func TestExaFixturesAreJSONOnlySyntheticAndOffline(t *testing.T) {
	for _, name := range []string{
		"exa_people_success.json", "exa_deep_success.json", "exa_people_error.json",
	} {
		data := exaFixture(t, name)
		lower := strings.ToLower(string(data))
		assert.True(t, json.Valid(data), name)
		for _, forbidden := range []string{
			"api_key", "test-key",
			"chat_sentinel", "private_attribute_sentinel", "person@example.com", "sk-",
		} {
			assert.NotContains(t, lower, forbidden, name)
		}
	}
}

func sortedMapKeys(input map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(input))
	for key := range input {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func duplicateTypedEntity(t *testing.T, fixture []byte) []byte {
	t.Helper()
	requirements := require.New(t)
	var response map[string]any
	requirements.NoError(json.Unmarshal(fixture, &response))
	results, ok := response["results"].([]any)
	requirements.True(ok)
	requirements.NotEmpty(results)
	row, ok := results[0].(map[string]any)
	requirements.True(ok)
	entities, ok := row["entities"].([]any)
	requirements.True(ok)
	requirements.NotEmpty(entities)
	row["entities"] = append(entities, entities[0])
	encoded, err := json.Marshal(response)
	requirements.NoError(err)
	return encoded
}

func exaEmptyTypedEntities(t *testing.T, fixture []byte) []byte {
	t.Helper()
	requirements := require.New(t)
	var response map[string]any
	requirements.NoError(json.Unmarshal(fixture, &response))
	results, ok := response["results"].([]any)
	requirements.True(ok)
	requirements.NotEmpty(results)
	row, ok := results[0].(map[string]any)
	requirements.True(ok)
	row["entities"] = []any{}
	encoded, err := json.Marshal(response)
	requirements.NoError(err)
	return encoded
}

func exaEmptyDeepGrounding(t *testing.T, fixture []byte) []byte {
	t.Helper()
	requirements := require.New(t)
	var response map[string]any
	requirements.NoError(json.Unmarshal(fixture, &response))
	output, ok := response["output"].(map[string]any)
	requirements.True(ok)
	output["grounding"] = []any{}
	encoded, err := json.Marshal(response)
	requirements.NoError(err)
	return encoded
}

func appendDeepResult(t *testing.T, fixture []byte, result map[string]any) []byte {
	t.Helper()
	requirements := require.New(t)
	var response map[string]any
	requirements.NoError(json.Unmarshal(fixture, &response))
	results, ok := response["results"].([]any)
	requirements.True(ok)
	response["results"] = append(results, result)
	encoded, err := json.Marshal(response)
	requirements.NoError(err)
	return encoded
}
