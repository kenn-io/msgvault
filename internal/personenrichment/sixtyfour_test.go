package personenrichment_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personenrichment"
	"go.kenn.io/msgvault/internal/personfacts"
)

func TestSixtyfourAsyncLifecycleUsesExactWireAndSurvivesRestart(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1, 4:
			assert.Equal(t, http.MethodPost, r.Method)
			assert.Equal(t, "/people-intelligence-async", r.URL.Path)
			assert.Equal(t, "test-key", r.Header.Get("X-Api-Key"))
			assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
			body, err := io.ReadAll(r.Body)
			if !assert.NoError(t, err) {
				return
			}
			if calls.Load() == 1 {
				assert.JSONEq(t, `{
				"lead_info": {
					"name": "Test User",
					"email": "user@example.test",
					"phone": "+12025550123",
					"company": "Example Labs"
				},
				"struct": {
					"attribute:score": {"description":"Synthetic public score","type":"int"},
					"attribute:summary": "Synthetic public profile summary"
				},
				"tier": "medium"
			}`, string(body))
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(sixtyfourFixture(t, "sixtyfour_start.json"))
		case 2:
			assertSixtyfourPollRequest(t, r, "/job-status/opaque-job-42")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(sixtyfourFixture(t, "sixtyfour_pending.json"))
		case 3:
			assertSixtyfourPollRequest(t, r, "/job-status/opaque-job-42")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(sixtyfourFixture(t, "sixtyfour_complete.json"))
		default:
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	config := sixtyfourConfig(server.URL+"/people-intelligence-async", server.URL+"/job-status")
	request := sixtyfourRequest(t)
	provider, err := personenrichment.NewSixtyfourProvider(config, "test-key", server.Client())
	requirements.NoError(err)
	attempt, err := provider.Start(t.Context(), request)
	requirements.NoError(err)
	checks.Equal(personenrichment.AttemptPending, attempt.State)
	checks.Equal("opaque-job-42", attempt.JobID)
	checks.Empty(attempt.RequestID)
	checks.Equal(time.Second, attempt.PollAfter)
	checks.Equal(personenrichment.SixtyfourAdapterVersionV1, attempt.AdapterVersion)
	checks.Equal(personenrichment.SixtyfourWireSchemaV1, attempt.SchemaVersion)
	checks.True(attempt.GeneratedSchema)
	checks.False(attempt.StartedAt.IsZero())
	request.Targets[0].Description = "caller mutation after start"
	checks.Equal("Synthetic public score", attempt.Targets[0].Description)

	wireStruct := []byte(`{"attribute:score":{"description":"Synthetic public score","type":"int"},"attribute:summary":"Synthetic public profile summary"}`)
	digest := sha256.Sum256(wireStruct)
	wantHash := hex.EncodeToString(digest[:])
	checks.Equal(wantHash, attempt.GeneratedSchemaHash)
	checks.Len(attempt.ProgramFingerprint, 64)

	pending, err := provider.Poll(t.Context(), attempt)
	requirements.NoError(err)
	checks.Equal(personenrichment.ResultPending, pending.State)
	checks.Equal(attempt.JobID, pending.JobID)
	checks.Equal(time.Second, pending.PollAfter)

	// A fresh provider instance can resume from the persisted attempt alone.
	restarted, err := personenrichment.NewSixtyfourProvider(config, "test-key", server.Client())
	requirements.NoError(err)
	complete, err := restarted.Poll(t.Context(), attempt)
	requirements.NoError(err)
	checks.Equal(personenrichment.ResultComplete, complete.State)
	checks.Equal(attempt.JobID, complete.JobID)
	checks.Equal(attempt.RequestID, complete.RequestID)
	checks.Equal(wantHash, complete.GeneratedSchemaHash)
	checks.Equal(int64(120_000), complete.Cost.AmountMicros)
	checks.Equal("USD", complete.Cost.Currency)
	checks.Len(complete.Claims, 2)
	checks.Empty(complete.Citations)
	checks.Empty(complete.ProviderPersonIDs)
	checks.Empty(complete.CanonicalPublicURLs)
	checks.ElementsMatch([]personenrichment.IdentityMatch{
		{Class: personenrichment.IdentifierName, Value: "Test User", Confidence: 900},
		{Class: personenrichment.IdentifierCurrentCompany, Value: "Example Labs", Confidence: 900},
	}, complete.IdentityMatches)
	checks.Equal(900, complete.IdentityConfidence)
	checks.True(complete.FreshAsOf.IsZero())
	checks.Equal("people-intelligence-api-unversioned", complete.ProviderVersion)

	claimByKey := make(map[string]personfacts.ProposedClaim, len(complete.Claims))
	for _, claim := range complete.Claims {
		claimByKey[claim.Target.Key] = claim
		checks.Equal(900, claim.Confidence.ReportedScore)
	}
	requirements.Len(claimByKey["attribute:summary"].Evidence, 1)
	checks.Equal(personfacts.EvidenceProviderAssertion, claimByKey["attribute:summary"].Evidence[0].SourceClass)
	checks.Empty(claimByKey["attribute:summary"].Evidence[0].SourceRef)
	requirements.Len(claimByKey["attribute:score"].Evidence, 1)
	checks.Equal(personfacts.EvidenceProviderAssertion, claimByKey["attribute:score"].Evidence[0].SourceClass)
	checks.Empty(claimByKey["attribute:score"].Evidence[0].SourceRef)
	checks.Empty(claimByKey["attribute:score"].Evidence[0].SourceURL)
	assessment := personenrichment.AssessIdentity(sixtyfourRequest(t), complete, nil)
	checks.True(assessment.Accepted)
	checks.Equal("name_company_match", assessment.Reason)

	changed := sixtyfourRequest(t)
	changed.Targets[1].Description = "Changed synthetic summary"
	changed.Targets[1].Revision = exaTargetRevision(t, changed.Targets[1])
	changedAttempt, err := provider.Start(t.Context(), changed)
	requirements.NoError(err)
	checks.NotEqual(attempt.GeneratedSchemaHash, changedAttempt.GeneratedSchemaHash)
	checks.NotEqual(attempt.ProgramFingerprint, changedAttempt.ProgramFingerprint)
}

func TestSixtyfourRejectsUndocumentedRequestID(t *testing.T) {
	t.Run("start", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"task_id":"opaque-job-42","status":"RUNNING","request_id":"invented"}`))
		}))
		defer server.Close()
		provider, err := personenrichment.NewSixtyfourProvider(
			sixtyfourConfig(server.URL, server.URL+"/job-status"), "test-key", server.Client())
		require.NoError(t, err)
		_, err = provider.Start(t.Context(), sixtyfourRequest(t))
		require.Error(t, err)
	})

	t.Run("poll", func(t *testing.T) {
		provider, attempt, closeServer := sixtyfourPollProvider(t, http.StatusOK, "application/json",
			[]byte(`{"task_id":"opaque-job-42","status":"running","request_id":"invented"}`), "")
		defer closeServer()
		_, err := provider.Poll(t.Context(), attempt)
		require.Error(t, err)
	})
}

func TestSixtyfourRejectsUndocumentedIdentityCitationAndVersionFields(t *testing.T) {
	fields := []string{
		`"provider_person_id":"synthetic-id"`,
		`"canonical_urls":[]`,
		`"identity_confidence":9`,
		`"freshness":"2026-08-22T12:00:00Z"`,
		`"sources":[]`,
		`"provider_version":"synthetic-version"`,
		`"model":"synthetic-model"`,
		`"model_version":"synthetic-model-version"`,
	}
	for _, field := range fields {
		t.Run(strings.SplitN(field, `"`, 3)[1], func(t *testing.T) {
			body := bytes.Replace(sixtyfourFixture(t, "sixtyfour_complete.json"),
				[]byte(`"findings": []`), []byte(field+`, "findings": []`), 1)
			provider, attempt, closeServer := sixtyfourPollProvider(
				t, http.StatusOK, "application/json", body, "")
			defer closeServer()
			_, err := provider.Poll(t.Context(), attempt)
			require.Error(t, err)
			var providerErr *personenrichment.ProviderError
			require.ErrorAs(t, err, &providerErr)
			assert.Equal(t, personenrichment.FailureInvalidOutput, providerErr.Class)
		})
	}
}

func TestSixtyfourRequiresDocumentedEmptyFindings(t *testing.T) {
	for _, test := range []struct {
		name        string
		replacement string
	}{
		{"missing", ""},
		{"null", `"findings": null`},
		{"nonempty", `"findings": [{"synthetic":"unsupported"}]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			checks := assert.New(t)
			requirements := require.New(t)
			body := sixtyfourFixture(t, "sixtyfour_complete.json")
			if test.name == "missing" {
				var envelope map[string]json.RawMessage
				requirements.NoError(json.Unmarshal(body, &envelope))
				var result map[string]json.RawMessage
				requirements.NoError(json.Unmarshal(envelope["result"], &result))
				delete(result, "findings")
				resultBody, err := json.Marshal(result)
				requirements.NoError(err)
				envelope["result"] = resultBody
				body, err = json.Marshal(envelope)
				requirements.NoError(err)
			} else {
				body = bytes.Replace(body, []byte(`"findings": []`), []byte(test.replacement), 1)
			}
			provider, attempt, closeServer := sixtyfourPollProvider(
				t, http.StatusOK, "application/json", body, "")
			defer closeServer()
			_, err := provider.Poll(t.Context(), attempt)
			requirements.Error(err)
			var providerErr *personenrichment.ProviderError
			requirements.ErrorAs(err, &providerErr)
			checks.Equal(personenrichment.FailureInvalidOutput, providerErr.Class)
		})
	}
}

func TestSixtyfourGeneratedStructUsesDocumentedTypes(t *testing.T) {
	request := sixtyfourRequest(t)
	request.Targets = nil
	for _, input := range []struct {
		key         string
		valueType   personfacts.ValueType
		cardinality personfacts.Cardinality
		description string
	}{
		{"attribute:text", personfacts.ValueText, personfacts.CardinalitySingle, "Text value"},
		{"attribute:integer", personfacts.ValueInteger, personfacts.CardinalitySingle, "Integer value"},
		{"attribute:real", personfacts.ValueReal, personfacts.CardinalitySingle, "Real value"},
		{"attribute:boolean", personfacts.ValueBoolean, personfacts.CardinalitySingle, "Boolean value"},
		{"attribute:list", personfacts.ValueText, personfacts.CardinalityMulti, "Text list"},
	} {
		target := exaAttributeTarget(t, input.key, input.valueType, input.cardinality)
		target.Description = input.description
		target.Revision = exaTargetRevision(t, target)
		request.Targets = append(request.Targets, target)
	}

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Struct json.RawMessage `json:"struct"`
		}
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
			return
		}
		assert.JSONEq(t, `{
			"attribute:text":"Text value",
			"attribute:integer":{"description":"Integer value","type":"int"},
			"attribute:real":{"description":"Real value","type":"float"},
			"attribute:boolean":{"description":"Boolean value","type":"bool"},
			"attribute:list":{"description":"Text list","type":"list[str]"}
		}`, string(body.Struct))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(sixtyfourFixture(t, "sixtyfour_start.json"))
	}))
	defer server.Close()
	config := sixtyfourConfig(server.URL, server.URL+"/job-status")
	config.TargetKeys = make([]string, len(request.Targets))
	for i := range request.Targets {
		config.TargetKeys[i] = request.Targets[i].Key
	}
	provider, err := personenrichment.NewSixtyfourProvider(config, "test-key", server.Client())
	require.NoError(t, err)
	attempt, err := provider.Start(t.Context(), request)
	require.NoError(t, err)
	assert.Len(t, attempt.Targets, len(request.Targets))
}

func TestSixtyfourFactConfidenceRoundsDocumentedScale(t *testing.T) {
	tests := []struct {
		value float64
		want  int
		valid bool
	}{
		{0, 0, true}, {8.99, 899, true}, {9, 900, true}, {10, 1000, true},
		{-0.01, 0, false}, {10.01, 0, false}, {math.NaN(), 0, false},
		{math.Inf(1), 0, false}, {math.Inf(-1), 0, false},
	}
	for _, test := range tests {
		got, err := personenrichment.SixtyfourFactConfidence(test.value)
		if test.valid {
			require.NoError(t, err)
			assert.Equal(t, test.want, got)
		} else {
			require.Error(t, err)
		}
	}
}

func TestSixtyfourPollStatusTransitionsAndTerminalFailures(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantState personenrichment.ResultState
		wantClass personenrichment.FailureClass
		wantError bool
	}{
		{"running", `{"task_id":"opaque-job-42","status":"running"}`, personenrichment.ResultPending, "", false},
		{"production running envelope", `{"id":"opaque-job-42","run_id":"opaque-run-7","start_time":"2026-08-24T07:00:00Z","status":"running"}`, personenrichment.ResultPending, "", false},
		{"production envelope wrong id", `{"id":"other-job","run_id":"opaque-run-7","start_time":"2026-08-24T07:00:00Z","status":"running"}`, "", personenrichment.FailureInvalidOutput, true},
		{"pending", `{"task_id":"opaque-job-42","status":"pending"}`, personenrichment.ResultPending, "", false},
		{"failed", string(sixtyfourFixture(t, "sixtyfour_error.json")), "", personenrichment.FailureTerminal, true},
		{"cancelled", `{"task_id":"opaque-job-42","status":"cancelled","error":"synthetic cancellation"}`, "", personenrichment.FailureTerminal, true},
		{"unknown", `{"task_id":"opaque-job-42","status":"queued"}`, "", personenrichment.FailureInvalidOutput, true},
		{"wrong-case terminal", `{"task_id":"opaque-job-42","status":"COMPLETED","result":{}}`, "", personenrichment.FailureInvalidOutput, true},
		{"missing completed result", `{"task_id":"opaque-job-42","status":"completed","charge_amount":1}`, "", personenrichment.FailureInvalidOutput, true},
		{"running with result", `{"task_id":"opaque-job-42","status":"running","result":{}}`, "", personenrichment.FailureInvalidOutput, true},
		{"running with charge", `{"task_id":"opaque-job-42","status":"running","charge_amount":1}`, "", personenrichment.FailureInvalidOutput, true},
		{"running with error", `{"task_id":"opaque-job-42","status":"running","error":"synthetic"}`, "", personenrichment.FailureInvalidOutput, true},
		{"failed with result", `{"task_id":"opaque-job-42","status":"failed","result":{}}`, "", personenrichment.FailureInvalidOutput, true},
		{"failed with charge", `{"task_id":"opaque-job-42","status":"failed","charge_amount":1}`, "", personenrichment.FailureInvalidOutput, true},
		{"completed with error", string(bytes.Replace(sixtyfourFixture(t, "sixtyfour_complete.json"),
			[]byte(`"charge_amount": 12`), []byte(`"charge_amount": 12, "error":"synthetic"`), 1)), "", personenrichment.FailureInvalidOutput, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			checks := assert.New(t)
			requirements := require.New(t)
			provider, attempt, closeServer := sixtyfourPollProvider(t, http.StatusOK, "application/json", []byte(test.body), "")
			defer closeServer()
			result, err := provider.Poll(t.Context(), attempt)
			if !test.wantError {
				requirements.NoError(err)
				checks.Equal(test.wantState, result.State)
				return
			}
			requirements.Error(err)
			var providerErr *personenrichment.ProviderError
			requirements.ErrorAs(err, &providerErr)
			checks.Equal(test.wantClass, providerErr.Class)
			checks.NotContains(err.Error(), "synthetic")
		})
	}
}

func TestSixtyfourCompletedResultRequiresEveryRequestedStructKey(t *testing.T) {
	body := bytes.Replace(sixtyfourFixture(t, "sixtyfour_complete.json"),
		[]byte(`"attribute:summary": "Synthetic public profile summary",`), nil, 1)
	provider, attempt, closeServer := sixtyfourPollProvider(
		t, http.StatusOK, "application/json", body, "")
	defer closeServer()
	_, err := provider.Poll(t.Context(), attempt)
	require.Error(t, err)
	var providerErr *personenrichment.ProviderError
	require.ErrorAs(t, err, &providerErr)
	assert.Equal(t, personenrichment.FailureInvalidOutput, providerErr.Class)
}

func TestSixtyfourAcceptsProductionCompletedEnvelopeAndIgnoresValidatedMetadata(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	body := []byte(`{
		"id":"opaque-job-42",
		"run_id":"opaque-run-7",
		"start_time":"2026-08-24T07:00:00Z",
		"close_time":"2026-08-24T07:00:30Z",
		"status":"completed",
		"result":{
			"structured_data":{
				"attribute:summary":"Synthetic public profile summary",
				"attribute:score":42,
				"name":"Test User",
				"company":"Example Labs"
			},
			"confidence_score":9,
			"findings":[],
			"notes":"Synthetic provider metadata",
			"references":{"https://example.com/profile":"Synthetic public source"}
		},
		"charge_amount":12,
		"task_type":"people-intelligence"
	}`)
	provider, attempt, closeServer := sixtyfourPollProvider(
		t, http.StatusOK, "application/json", body, "")
	defer closeServer()

	result, err := provider.Poll(t.Context(), attempt)
	requirements.NoError(err)
	requirements.NoError(result.Validate())
	checks.Equal(personenrichment.ResultComplete, result.State)
	checks.Len(result.Claims, 2)
	checks.Equal(900, result.IdentityConfidence)
	checks.ElementsMatch([]personenrichment.IdentityMatch{
		{Class: personenrichment.IdentifierName, Value: "Test User", Confidence: 900},
		{Class: personenrichment.IdentifierCurrentCompany, Value: "Example Labs", Confidence: 900},
	}, result.IdentityMatches)
	assessment := personenrichment.AssessIdentity(sixtyfourRequest(t), result, nil)
	checks.True(assessment.Accepted)
	checks.Equal("name_company_match", assessment.Reason)
}

func TestSixtyfourRejectsInvalidProductionCompletedMetadata(t *testing.T) {
	base := `{
		"id":"opaque-job-42",
		"run_id":"opaque-run-7",
		"start_time":"2026-08-24T07:00:00Z",
		"close_time":"2026-08-24T07:00:30Z",
		"status":"completed",
		"result":{
			"structured_data":{
				"attribute:summary":"Synthetic public profile summary",
				"attribute:score":42,
				"name":"Test User",
				"company":"Example Labs"
			},
			"confidence_score":9,
			"findings":[],
			"notes":"Synthetic provider metadata",
			"references":{"https://example.com/profile":"Synthetic public source"}
		},
		"charge_amount":12,
		"task_type":"people-intelligence"
	}`
	tests := []struct {
		name string
		body string
	}{
		{"invalid close time", strings.Replace(base, "2026-08-24T07:00:30Z", "not-a-time", 1)},
		{"unsafe reference URL", strings.Replace(base, "https://example.com/profile", "http://127.0.0.1/private", 1)},
		{"blank identity echo", strings.Replace(base, `"name":"Test User"`, `"name":" "`, 1)},
		{"missing identity echo", strings.Replace(base, ",\n\t\t\t\t\"company\":\"Example Labs\"", "", 1)},
		{"identity confidence below threshold", strings.Replace(base, `"confidence_score":9`, `"confidence_score":8.99`, 1)},
		{"unknown structured field", strings.Replace(base, `"company":"Example Labs"`, `"company":"Example Labs","private_extra":"value"`, 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider, attempt, closeServer := sixtyfourPollProvider(
				t, http.StatusOK, "application/json", []byte(test.body), "")
			defer closeServer()
			_, err := provider.Poll(t.Context(), attempt)
			require.Error(t, err)
			var providerErr *personenrichment.ProviderError
			require.ErrorAs(t, err, &providerErr)
			assert.Equal(t, personenrichment.FailureInvalidOutput, providerErr.Class)
		})
	}
}

func TestSixtyfourStartRequiresExactUppercaseRunning(t *testing.T) {
	for _, status := range []string{"running", "PENDING", "completed"} {
		t.Run(status, func(t *testing.T) {
			requirements := require.New(t)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"task_id":"opaque-job-42","status":"` + status + `"}`))
			}))
			defer server.Close()
			provider, err := personenrichment.NewSixtyfourProvider(
				sixtyfourConfig(server.URL, server.URL+"/job-status"), "test-key", server.Client())
			requirements.NoError(err)
			_, err = provider.Start(t.Context(), sixtyfourRequest(t))
			requirements.Error(err)
			var providerErr *personenrichment.ProviderError
			requirements.ErrorAs(err, &providerErr)
			assert.Equal(t, personenrichment.FailureInvalidOutput, providerErr.Class)
		})
	}
}

func TestSixtyfourPollRejectsExpiredJobBeforeNetwork(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer server.Close()
	config := sixtyfourConfig(server.URL+"/start", server.URL+"/job-status")
	provider, err := personenrichment.NewSixtyfourProvider(config, "test-key", server.Client())
	requirements.NoError(err)
	attempt := sixtyfourPendingAttempt(t)
	attempt.StartedAt = time.Now().UTC().Add(-config.MaxJobAge - time.Minute)
	_, err = provider.Poll(t.Context(), attempt)
	requirements.Error(err)
	var providerErr *personenrichment.ProviderError
	requirements.ErrorAs(err, &providerErr)
	checks.Equal(personenrichment.FailureTerminal, providerErr.Class)
	checks.Zero(calls.Load())
}

func TestSixtyfourPollRejectsFutureOrSchemaMismatchedAttemptBeforeNetwork(t *testing.T) {
	requirements := require.New(t)
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer server.Close()
	provider, err := personenrichment.NewSixtyfourProvider(
		sixtyfourConfig(server.URL+"/start", server.URL+"/job-status"), "test-key", server.Client())
	requirements.NoError(err)

	future := sixtyfourPendingAttempt(t)
	future.StartedAt = time.Now().UTC().Add(time.Minute)
	_, err = provider.Poll(t.Context(), future)
	requirements.Error(err)

	tampered := sixtyfourPendingAttempt(t)
	tampered.Targets[0].Description = "Tampered after persistence"
	tampered.Targets[0].Revision = exaTargetRevision(t, tampered.Targets[0])
	_, err = provider.Poll(t.Context(), tampered)
	requirements.Error(err)
	assert.Zero(t, calls.Load())
}

func TestSixtyfourPollEscapesOpaqueTaskIDAndSendsNoIdentity(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/job-status/opaque%2Fjob%3Fx=1%23fragment", r.URL.EscapedPath())
		assert.Empty(t, r.URL.RawQuery)
		body, err := io.ReadAll(r.Body)
		if !assert.NoError(t, err) {
			return
		}
		assert.Empty(t, body)
		assert.Equal(t, "test-key", r.Header.Get("X-Api-Key"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"task_id":"opaque/job?x=1#fragment","status":"running"}`))
	}))
	defer server.Close()
	provider, err := personenrichment.NewSixtyfourProvider(
		sixtyfourConfig(server.URL+"/start", server.URL+"/job-status"), "test-key", server.Client())
	require.NoError(t, err)
	attempt := sixtyfourPendingAttempt(t)
	attempt.JobID = "opaque/job?x=1#fragment"
	result, err := provider.Poll(t.Context(), attempt)
	require.NoError(t, err)
	assert.Equal(t, attempt.JobID, result.JobID)
}

func TestSixtyfourRejectsInvalidCharges(t *testing.T) {
	for _, charge := range []string{"-1", "1.5", "922337203685478"} {
		t.Run(charge, func(t *testing.T) {
			body := bytes.Replace(sixtyfourFixture(t, "sixtyfour_complete.json"), []byte(`"charge_amount": 12`), []byte(`"charge_amount": `+charge), 1)
			provider, attempt, closeServer := sixtyfourPollProvider(t, http.StatusOK, "application/json", body, "")
			defer closeServer()
			_, err := provider.Poll(t.Context(), attempt)
			require.Error(t, err)
			var providerErr *personenrichment.ProviderError
			require.ErrorAs(t, err, &providerErr)
			assert.Equal(t, personenrichment.FailureInvalidOutput, providerErr.Class)
		})
	}
}

func TestSixtyfourRejectsAdverseHTTPAndCodecResponsesSafely(t *testing.T) {
	privateSentinel := "private-person@example.test"
	tests := []struct {
		name        string
		status      int
		contentType string
		body        []byte
		retryAfter  string
		wantClass   personenrichment.FailureClass
	}{
		{"expired 404", http.StatusNotFound, "application/json", []byte(`{"detail":"` + privateSentinel + `"}`), "", personenrichment.FailureTerminal},
		{"rate limited", http.StatusTooManyRequests, "application/json", []byte(`{"detail":"` + privateSentinel + `"}`), "17", personenrichment.FailureRateLimited},
		{"unsafe retry header", http.StatusTooManyRequests, "application/json", []byte(`{"detail":"safe"}`), privateSentinel, personenrichment.FailureRateLimited},
		{"server error", http.StatusInternalServerError, "application/json", []byte(`{"detail":"` + privateSentinel + `"}`), "", personenrichment.FailureTransient},
		{"wrong content type", http.StatusOK, "text/plain", sixtyfourFixture(t, "sixtyfour_pending.json"), "", personenrichment.FailureInvalidOutput},
		{"malformed", http.StatusOK, "application/json", []byte(`{"status":` + privateSentinel), "", personenrichment.FailureInvalidOutput},
		{"trailing", http.StatusOK, "application/json", append(sixtyfourFixture(t, "sixtyfour_pending.json"), []byte(` {}`)...), "", personenrichment.FailureInvalidOutput},
		{"duplicate", http.StatusOK, "application/json", []byte(`{"task_id":"opaque-job-42","status":"running","status":"completed"}`), "", personenrichment.FailureInvalidOutput},
		{"oversized", http.StatusOK, "application/json", []byte(`{"private":"` + strings.Repeat("x", (1<<20)+1) + `"}`), "", personenrichment.FailureInvalidOutput},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			checks := assert.New(t)
			requirements := require.New(t)
			provider, attempt, closeServer := sixtyfourPollProvider(t, test.status, test.contentType, test.body, test.retryAfter)
			defer closeServer()
			_, err := provider.Poll(t.Context(), attempt)
			requirements.Error(err)
			checks.NotContains(err.Error(), privateSentinel)
			checks.NotContains(err.Error(), "test-key")
			var providerErr *personenrichment.ProviderError
			requirements.ErrorAs(err, &providerErr)
			checks.Equal(test.wantClass, providerErr.Class)
			if test.status == http.StatusTooManyRequests {
				if test.retryAfter == "17" {
					checks.Equal("17", providerErr.RetryAfter)
				} else {
					checks.Empty(providerErr.RetryAfter)
				}
			}
		})
	}
}

func TestSixtyfourRejectsRedirectsAndEnforcesTimeout(t *testing.T) {
	t.Run("redirect", func(t *testing.T) {
		var destinationCalls atomic.Int32
		destination := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			destinationCalls.Add(1)
		}))
		defer destination.Close()
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, destination.URL, http.StatusFound)
		}))
		defer server.Close()
		provider, err := personenrichment.NewSixtyfourProvider(
			sixtyfourConfig(server.URL+"/start", server.URL+"/job-status"), "test-key", server.Client())
		require.NoError(t, err)
		_, err = provider.Poll(t.Context(), sixtyfourPendingAttempt(t))
		require.Error(t, err)
		assert.Zero(t, destinationCalls.Load())
	})

	t.Run("timeout", func(t *testing.T) {
		requirements := require.New(t)
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(100 * time.Millisecond)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(sixtyfourFixture(t, "sixtyfour_pending.json"))
		}))
		defer server.Close()
		config := sixtyfourConfig(server.URL+"/start", server.URL+"/job-status")
		config.RequestTimeout = 10 * time.Millisecond
		provider, err := personenrichment.NewSixtyfourProvider(config, "test-key", server.Client())
		requirements.NoError(err)
		_, err = provider.Poll(t.Context(), sixtyfourPendingAttempt(t))
		requirements.Error(err)
		var providerErr *personenrichment.ProviderError
		requirements.ErrorAs(err, &providerErr)
		assert.Equal(t, personenrichment.FailureTransient, providerErr.Class)
	})
}

func TestSixtyfourStartTransportFailureIsUncertain(t *testing.T) {
	requirements := require.New(t)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		if !assert.True(t, ok) {
			return
		}
		conn, _, err := hijacker.Hijack()
		if !assert.NoError(t, err) {
			return
		}
		_ = conn.Close()
	}))
	defer server.Close()
	provider, err := personenrichment.NewSixtyfourProvider(
		sixtyfourConfig(server.URL+"/start", server.URL+"/job-status"), "test-key", server.Client())
	requirements.NoError(err)
	_, err = provider.Start(t.Context(), sixtyfourRequest(t))
	requirements.Error(err)
	var providerErr *personenrichment.ProviderError
	requirements.ErrorAs(err, &providerErr)
	assert.Equal(t, personenrichment.FailureUncertainStart, providerErr.Class)
}

func TestSixtyfourRejectsUndurableTargetsBeforeEgress(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write(exaFixture(t, "sixtyfour_start.json"))
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)
	provider, err := personenrichment.NewSixtyfourProvider(
		sixtyfourConfig(server.URL, server.URL), "test-key", server.Client(),
	)
	require.NoError(t, err)

	tooMany := make([]personfacts.TargetDescriptor, 101)
	for i := range tooMany {
		key := fmt.Sprintf("attribute:target-%03d", i)
		tooMany[i] = exaAttributeTarget(t, key, personfacts.ValueText, personfacts.CardinalitySingle)
	}
	oversized := exaAttributeTarget(t, "attribute:oversized", personfacts.ValueText, personfacts.CardinalitySingle)
	oversized.Description = strings.Repeat("x", 256<<10)
	oversized.Revision = exaTargetRevision(t, oversized)

	for name, targets := range map[string][]personfacts.TargetDescriptor{
		"too many": tooMany, "oversized": {oversized},
	} {
		t.Run(name, func(t *testing.T) {
			_, startErr := provider.Start(t.Context(), personenrichment.Request{
				Identity: personenrichment.Identity{Email: "bounded@example.test"}, Targets: targets,
			})
			assert.Error(t, startErr)
		})
	}
	assert.Zero(t, calls.Load())
}

func sixtyfourConfig(start, poll string) personenrichment.ProviderConfig {
	return personenrichment.ProviderConfig{
		Name: "sixtyfour-test", Kind: personenrichment.ProviderSixtyfour, Enabled: true,
		Endpoint: start, PollEndpoint: poll, APIKeyEnv: "SIXTYFOUR_API_KEY", Tier: "medium",
		AllowedIdentifiers: []personenrichment.IdentifierClass{
			personenrichment.IdentifierName, personenrichment.IdentifierEmail,
			personenrichment.IdentifierPhone, personenrichment.IdentifierCurrentCompany,
			personenrichment.IdentifierPublicProfileURL,
		},
		TargetKeys:       []string{"attribute:score", "attribute:summary"},
		RetentionPosture: "zero_retention", TrainingPosture: "no_training",
		RefreshInterval: 24 * time.Hour, RequestTimeout: time.Second,
		PollInterval: time.Second, MaxJobAge: 15 * time.Minute, MaxRetries: 2,
		MaxRequestsPerRun: 10, MaxRequestsPerDay: 100,
	}
}

func sixtyfourRequest(t *testing.T) personenrichment.Request {
	t.Helper()
	score := exaAttributeTarget(t, "attribute:score", personfacts.ValueInteger, personfacts.CardinalitySingle)
	score.Description = "Synthetic public score"
	score.Revision = exaTargetRevision(t, score)
	summary := exaAttributeTarget(t, "attribute:summary", personfacts.ValueText, personfacts.CardinalitySingle)
	summary.Description = "Synthetic public profile summary"
	summary.Revision = exaTargetRevision(t, summary)
	return personenrichment.Request{
		RequestHash: strings.Repeat("a", 64),
		Identity: personenrichment.Identity{
			Name: "Test User", Email: "user@example.test", Phone: "+12025550123",
			CurrentCompany:    "Example Labs",
			PublicProfileURLs: []string{"https://profiles.example.test/test-user"},
		},
		Targets: []personfacts.TargetDescriptor{score, summary},
	}
}

func sixtyfourPendingAttempt(t *testing.T) personenrichment.Attempt {
	t.Helper()
	wireStruct := []byte(`{"attribute:score":{"description":"Synthetic public score","type":"int"},"attribute:summary":"Synthetic public profile summary"}`)
	digest := sha256.Sum256(wireStruct)
	schemaHash := hex.EncodeToString(digest[:])
	program, err := personenrichment.ProgramFingerprint(personenrichment.ProgramDescriptor{
		HostMappingVersion: personenrichment.HostClaimMappingVersion,
		AdapterVersion:     personenrichment.SixtyfourAdapterVersionV1,
		WireSchemaVersion:  personenrichment.SixtyfourWireSchemaV1,
		GeneratedSchema:    true, GeneratedSchemaHash: schemaHash,
	})
	require.NoError(t, err)
	return personenrichment.Attempt{
		State: personenrichment.AttemptPending, JobID: "opaque-job-42",
		PollAfter: time.Second, AdapterVersion: personenrichment.SixtyfourAdapterVersionV1,
		SchemaVersion:   personenrichment.SixtyfourWireSchemaV1,
		GeneratedSchema: true, GeneratedSchemaHash: schemaHash,
		ProgramFingerprint: program, StartedAt: time.Now().UTC(),
		Targets: sixtyfourRequest(t).Targets,
	}
}

func sixtyfourPollProvider(
	t *testing.T, status int, contentType string, body []byte, retryAfter string,
) (personenrichment.Provider, personenrichment.Attempt, func()) {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	provider, err := personenrichment.NewSixtyfourProvider(
		sixtyfourConfig(server.URL+"/start", server.URL+"/job-status"), "test-key", server.Client())
	require.NoError(t, err)
	return provider, sixtyfourPendingAttempt(t), server.Close
}

func assertSixtyfourPollRequest(t *testing.T, r *http.Request, path string) {
	t.Helper()
	assert.Equal(t, http.MethodGet, r.Method)
	assert.Equal(t, path, r.URL.Path)
	assert.Equal(t, "test-key", r.Header.Get("X-Api-Key"))
	assert.Empty(t, r.URL.RawQuery)
	body, err := io.ReadAll(r.Body)
	require.NoError(t, err)
	assert.Empty(t, body)
}

func sixtyfourFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	require.NoError(t, err)
	return data
}

func TestSixtyfourFixturesAreJSONOnlyAndSynthetic(t *testing.T) {
	for _, name := range []string{
		"sixtyfour_start.json", "sixtyfour_pending.json",
		"sixtyfour_complete.json", "sixtyfour_error.json",
	} {
		data := sixtyfourFixture(t, name)
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
