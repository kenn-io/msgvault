package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/store"
)

var personFactAPINow = time.Date(2026, time.August, 22, 12, 0, 0, 0, time.UTC)

func TestPersonFactCatalogHTTPDefaultsSensitiveAndSupportsExplicitInclude(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	srv, st, _, _ := newPersonFactAPIFixture(t)
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`
		UPDATE attribute_definitions SET is_sensitive = TRUE WHERE slug = ?`),
		store.AttributeSlugAskMeAbout)
	requirements.NoError(err)

	response := doRequest(t, srv.Router(), http.MethodGet,
		"/api/v1/person-fact-targets", nil, nil)
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	assertions.Equal("no-store", response.Header().Get("Cache-Control"))
	var catalog personfacts.Catalog
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &catalog))
	for _, target := range catalog.Targets {
		assertions.False(target.Sensitive, "sensitive targets are opt-in")
	}

	response = doRequest(t, srv.Router(), http.MethodGet,
		"/api/v1/person-fact-targets?include_sensitive=true", nil, nil)
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &catalog))
	assertions.Condition(func() bool {
		for _, target := range catalog.Targets {
			if target.Slug == store.AttributeSlugAskMeAbout && target.Sensitive {
				return true
			}
		}
		return false
	}, "explicit sensitive catalog request includes the marked target")
}

func TestPersonFactHistoryHTTPIsBoundedNewestFirstNonNullAndNoStore(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	srv, st, personID, targets := newPersonFactAPIFixture(t)
	target := targets[store.AttributeSlugPrimaryChannel]
	applyPersonFactAPIGeneration(t, st, personID, target, "second", `"phone"`)
	targetQuery := personFactAPITargetQuery(target)

	tests := []struct {
		name  string
		path  string
		field string
	}{
		{name: "evidence", path: "fact-evidence", field: "evidence"},
		{name: "claims", path: "fact-claims", field: "claims"},
		{name: "decisions", path: "fact-decisions", field: "decisions"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			path := fmt.Sprintf("/api/v1/people/%d/%s?target=%s&limit=1&offset=0",
				personID, test.path, targetQuery)
			response := doRequest(t, srv.Router(), http.MethodGet, path, nil, nil)
			requirements.Equal(http.StatusOK, response.Code, response.Body.String())
			assertions.Equal("no-store", response.Header().Get("Cache-Control"))
			var body map[string][]map[string]any
			requirements.NoError(json.Unmarshal(response.Body.Bytes(), &body))
			requirements.Len(body[test.field], 1)
			assertions.NotZero(body[test.field][0]["id"])
			newestID, ok := body[test.field][0]["id"].(float64)
			requirements.True(ok, "history id must decode as a number")

			olderPath := fmt.Sprintf("/api/v1/people/%d/%s?target=%s&limit=1&offset=1",
				personID, test.path, targetQuery)
			older := doRequest(t, srv.Router(), http.MethodGet, olderPath, nil, nil)
			requirements.Equal(http.StatusOK, older.Code, older.Body.String())
			requirements.NoError(json.Unmarshal(older.Body.Bytes(), &body))
			requirements.Len(body[test.field], 1)
			olderID, ok := body[test.field][0]["id"].(float64)
			requirements.True(ok, "history id must decode as a number")
			assertions.Greater(newestID, olderID,
				"history must be newest-first")

			emptyPath := fmt.Sprintf("/api/v1/people/%d/%s?limit=1&offset=999",
				personID, test.path)
			empty := doRequest(t, srv.Router(), http.MethodGet, emptyPath, nil, nil)
			requirements.Equal(http.StatusOK, empty.Code, empty.Body.String())
			assertions.JSONEq(fmt.Sprintf(`{"%s":[]}`, test.field), empty.Body.String())
		})
	}

	claims := doRequest(t, srv.Router(), http.MethodGet,
		fmt.Sprintf("/api/v1/people/%d/fact-claims?limit=1", personID), nil, nil)
	requirements.Equal(http.StatusOK, claims.Code, claims.Body.String())
	var claimBody struct {
		Claims []map[string]any `json:"claims"`
	}
	requirements.NoError(json.Unmarshal(claims.Body.Bytes(), &claimBody))
	requirements.Len(claimBody.Claims, 1)
	assertions.Equal("fixture-program", claimBody.Claims[0]["program_id"])
	assertions.Equal("v1", claimBody.Claims[0]["program_version"])
	assertions.Equal(strings.Repeat("a", 64), claimBody.Claims[0]["program_fingerprint"])
}

func TestPersonFactHistoryHTTPFiltersRealEmploymentTargetWithColonKey(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	srv, st, personID, targets := newPersonFactAPIFixtureWithoutClaims(t)
	target := targets["employment"]
	requirements.Equal(personfacts.TargetEmployment, target.Kind)
	requirements.Equal("system:employment", target.Key)
	requirements.Regexp(`^sha256:[0-9a-f]{64}$`, target.Revision)
	applyPersonFactAPIGeneration(t, st, personID, target, "employment",
		`{"organization":{"name":"Codec Company"},"title":"Engineer"}`)

	encoded := "employment:system:employment:" + target.Revision
	response := doRequest(t, srv.Router(), http.MethodGet,
		fmt.Sprintf("/api/v1/people/%d/fact-evidence?target=%s", personID, url.QueryEscape(encoded)),
		nil, nil)
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	var body PersonFactEvidenceResponse
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &body))
	assertions.Len(body.Evidence, 1)
}

func TestPersonFactEvidenceStatusHTTPFiltersPaginatesAndExposesSafeReactivationHistory(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	srv, st, personID, _ := newPersonFactAPIFixture(t)
	evidence, err := st.ListPersonFactEvidenceContext(t.Context(), personID,
		personfacts.EvidenceFilter{})
	requirements.NoError(err)
	requirements.Len(evidence, 1)
	key := evidence[0].Key
	applyPersonFactAPIStatus(t, st, personID, key, "off", false,
		personfacts.EvidenceStatusSourceDeleted)
	applyPersonFactAPIStatus(t, st, personID, key, "on", true,
		personfacts.EvidenceStatusSourceReimported)

	path := fmt.Sprintf("/api/v1/people/%d/fact-evidence-status-events?evidence_key=%s&limit=1&offset=0",
		personID, key)
	response := doRequest(t, srv.Router(), http.MethodGet, path, nil, nil)
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	assertions.Equal("no-store", response.Header().Get("Cache-Control"))
	var body struct {
		Events []map[string]json.RawMessage `json:"events"`
	}
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &body))
	requirements.Len(body.Events, 1)
	assertions.JSONEq("true", string(body.Events[0]["supported"]))
	assertions.ElementsMatch([]string{
		"id", "generation_id", "evidence_key", "source_version", "supported", "reason", "created_at",
	}, personFactRawMessageKeys(body.Events[0]))
	assertions.NotContains(response.Body.String(), "fixture-provider-secret")
	assertions.NotContains(response.Body.String(), "Synthetic archive payload")

	for _, supported := range []bool{true, false} {
		filteredPath := fmt.Sprintf(
			"/api/v1/people/%d/fact-evidence-status-events?evidence_key=%s&supported=%t&limit=10&offset=0",
			personID, key, supported)
		filtered := doRequest(t, srv.Router(), http.MethodGet, filteredPath, nil, nil)
		requirements.Equal(http.StatusOK, filtered.Code, filtered.Body.String())
		requirements.NoError(json.Unmarshal(filtered.Body.Bytes(), &body))
		requirements.Len(body.Events, 1)
		assertions.JSONEq(strconv.FormatBool(supported), string(body.Events[0]["supported"]))
	}

	empty := doRequest(t, srv.Router(), http.MethodGet,
		fmt.Sprintf("/api/v1/people/%d/fact-evidence-status-events?evidence_key=missing", personID),
		nil, nil)
	requirements.Equal(http.StatusOK, empty.Code, empty.Body.String())
	assertions.JSONEq(`{"events":[]}`, empty.Body.String())
}

func TestPersonFactPinHTTPUsesAPIActorAndReturnsOnlyFinalProjectionReferences(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	srv, st, personID, targets := newPersonFactAPIFixtureWithoutClaims(t)
	target := targets[store.AttributeSlugPrimaryChannel]
	_, err := st.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{
		PersonID: personID, DefinitionSlug: store.AttributeSlugPrimaryChannel,
		Value:  store.AttributeValue{Type: store.AttributeValueText, Text: new("email")},
		Source: store.ProvenanceUser,
	})
	requirements.NoError(err)
	applyPersonFactAPIGeneration(t, st, personID, target, "pin-candidate", `"chat"`)

	path := fmt.Sprintf("/api/v1/people/%d/fact-pins/%s/%s",
		personID, target.Kind, target.Key)
	response := doRequest(t, srv.Router(), http.MethodPut, path,
		[]byte(`{"pinned":false}`), nil)
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	assertions.Equal("no-store", response.Header().Get("Cache-Control"))
	var body map[string]any
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &body))
	state, ok := body["state"].(map[string]any)
	requirements.True(ok, "pin state must decode as an object")
	assertions.Equal("api", state["actor"])
	projections, ok := body["projections"].([]any)
	requirements.True(ok, "projections must decode as an array")
	requirements.NotEmpty(projections)
	projection, ok := projections[0].(map[string]any)
	requirements.True(ok, "projection must decode as an object")
	assertions.ElementsMatch([]string{"kind", "row_id"},
		mapKeysAny(projection))
	for _, forbidden := range []string{"operation", "current_ref", "active_from", "active_until"} {
		assertions.NotContains(response.Body.String(), forbidden)
	}

	listed := doRequest(t, srv.Router(), http.MethodGet,
		fmt.Sprintf("/api/v1/people/%d/fact-pins", personID), nil, nil)
	requirements.Equal(http.StatusOK, listed.Code, listed.Body.String())
	assertions.Contains(listed.Body.String(), `"pinned":false`)

	unknownField := doRequest(t, srv.Router(), http.MethodPut, path,
		[]byte(`{"pinned":true,"actor":"client"}`), nil)
	assertions.Equal(http.StatusBadRequest, unknownField.Code, unknownField.Body.String())
}

func TestPersonFactHTTPMapsInvalidMissingConflictAndUnavailable(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	srv, _, personID, _ := newPersonFactAPIFixture(t)
	invalid := doRequest(t, srv.Router(), http.MethodGet,
		fmt.Sprintf("/api/v1/people/%d/fact-claims?limit=201", personID), nil, nil)
	assertions.Equal(http.StatusBadRequest, invalid.Code, invalid.Body.String())
	for _, target := range []string{
		"candidate:system:employment:sha256:" + strings.Repeat("a", 64),
		"employment:system:employment:revision-1",
	} {
		malformedTarget := doRequest(t, srv.Router(), http.MethodGet,
			fmt.Sprintf("/api/v1/people/%d/fact-claims?target=%s", personID, url.QueryEscape(target)),
			nil, nil)
		assertions.Equal(http.StatusBadRequest, malformedTarget.Code,
			malformedTarget.Body.String())
	}

	invalidPinsPerson := doRequest(t, srv.Router(), http.MethodGet,
		"/api/v1/people/not-a-person/fact-pins", nil, nil)
	assertions.Equal(http.StatusBadRequest, invalidPinsPerson.Code, invalidPinsPerson.Body.String())

	missing := doRequest(t, srv.Router(), http.MethodGet,
		"/api/v1/people/999999/fact-evidence", nil, nil)
	assertions.Equal(http.StatusNotFound, missing.Code, missing.Body.String())

	_, wrapped := newIdentityLinkTestServer(t)
	conflicting := &personFactFailingStore{stubIdentityCacheStore: wrapped,
		pinErr: store.ErrPersonFactKeyCollision}
	conflictServer := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}},
		conflicting, nil, testLogger())
	catalog, err := conflicting.BuildPersonFactCatalogContext(t.Context(), true)
	requirements.NoError(err)
	requirements.NotEmpty(catalog.Targets)
	conflictPerson := conflicting.mustParticipant(t, "conflict@example.test", "conflict", "example.test")
	person, _, err := conflicting.CreatePersonFromParticipant(conflictPerson)
	requirements.NoError(err)
	target := catalog.Targets[0]
	conflict := doRequest(t, conflictServer.Router(), http.MethodPut,
		fmt.Sprintf("/api/v1/people/%d/fact-pins/%s/%s", person.ID, target.Kind, target.Key),
		[]byte(`{"pinned":true}`), nil)
	assertions.Equal(http.StatusConflict, conflict.Code, conflict.Body.String())

	unavailable, _ := newTestServerWithMockStore(t)
	serviceUnavailable := doRequest(t, unavailable.Router(), http.MethodGet,
		"/api/v1/person-fact-targets", nil, nil)
	assertions.Equal(http.StatusServiceUnavailable, serviceUnavailable.Code,
		serviceUnavailable.Body.String())
}

func TestPersonFactPinHTTPDecodesTargetKeyExactlyOnce(t *testing.T) {
	_, wrapped := newIdentityLinkTestServer(t)
	participantID := wrapped.mustParticipant(t, "encoded@example.test", "encoded", "example.test")
	person, _, err := wrapped.CreatePersonFromParticipant(participantID)
	require.NoError(t, err)
	target := personfacts.TargetDescriptor{
		Kind: personfacts.TargetAttribute, Key: "encoded%2Fkey", Revision: "revision-1",
		UniversalID: "encoded%2Fkey", Slug: "encoded_key", Description: "Synthetic encoded key",
		ValueType: personfacts.ValueText, Cardinality: personfacts.CardinalitySingle,
		Choices: []personfacts.ChoiceDescriptor{}, Fields: []personfacts.FieldDescriptor{},
	}
	capturing := &personFactCapturingStore{stubIdentityCacheStore: wrapped,
		catalog: personfacts.Catalog{Version: "v1", Fingerprint: "fixture", Targets: []personfacts.TargetDescriptor{target}}}
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}},
		capturing, nil, testLogger())
	response := doRequest(t, srv.Router(), http.MethodPut,
		fmt.Sprintf("/api/v1/people/%d/fact-pins/attribute/encoded%%252Fkey", person.ID),
		[]byte(`{"pinned":true}`), nil)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Equal(t, "encoded%2Fkey", capturing.target.Key)
}

type personFactFailingStore struct {
	*stubIdentityCacheStore

	pinErr error
}

func (s *personFactFailingStore) SetPersonFactPinContext(
	_ context.Context, _ int64, _ personfacts.TargetRef, _ bool, _ string,
) (*personfacts.PinWrite, error) {
	return nil, s.pinErr
}

type personFactCapturingStore struct {
	*stubIdentityCacheStore

	catalog personfacts.Catalog
	target  personfacts.TargetRef
}

func (s *personFactCapturingStore) BuildPersonFactCatalogContext(
	_ context.Context, _ bool,
) (personfacts.Catalog, error) {
	return s.catalog, nil
}

func (s *personFactCapturingStore) SetPersonFactPinContext(
	_ context.Context, _ int64, target personfacts.TargetRef, pinned bool, actor string,
) (*personfacts.PinWrite, error) {
	s.target = target
	return &personfacts.PinWrite{
		State:       personfacts.PinState{Target: target, Pinned: pinned, Actor: actor},
		Resolutions: []personfacts.ResolutionResult{}, Projections: []personfacts.ProjectionRef{},
	}, nil
}

func newPersonFactAPIFixture(
	t *testing.T,
) (*Server, *store.Store, int64, map[string]personfacts.TargetDescriptor) {
	t.Helper()
	srv, st, personID, targets := newPersonFactAPIFixtureWithoutClaims(t)
	applyPersonFactAPIGeneration(t, st, personID,
		targets[store.AttributeSlugPrimaryChannel], "first", `"chat"`)
	return srv, st, personID, targets
}

func newPersonFactAPIFixtureWithoutClaims(
	t *testing.T,
) (*Server, *store.Store, int64, map[string]personfacts.TargetDescriptor) {
	t.Helper()
	srv, wrapped := newIdentityLinkTestServer(t)
	participantID := wrapped.mustParticipant(t, "facts@example.test", "facts", "example.test")
	person, _, err := wrapped.CreatePersonFromParticipant(participantID)
	require.NoError(t, err)
	_, err = wrapped.SetPersonTrackingContext(t.Context(), person.ID, true)
	require.NoError(t, err)
	catalog, err := wrapped.BuildPersonFactCatalogContext(t.Context(), true)
	require.NoError(t, err)
	targets := make(map[string]personfacts.TargetDescriptor)
	for _, target := range catalog.Targets {
		targets[target.Slug] = target
	}
	require.Contains(t, targets, store.AttributeSlugPrimaryChannel)
	return srv, wrapped.Store, person.ID, targets
}

func applyPersonFactAPIGeneration(
	t *testing.T, st *store.Store, personID int64, target personfacts.TargetDescriptor,
	suffix, submitted string,
) {
	t.Helper()
	subject := personID
	_, err := st.ApplyPersonFactGenerationContext(t.Context(), personfacts.GenerationInput{
		PersonID: personID,
		SourceCursors: []personfacts.SourceCursor{{
			Lane: "fixture", Start: suffix, End: suffix + "-end",
		}},
		ProgramID: "fixture-program", ProgramVersion: "v1",
		ProgramFingerprint: strings.Repeat("a", 64), CatalogFingerprint: "fixture-catalog",
		Provider: "fixture-provider-secret", ProviderVersion: "v1",
		Model: "fixture-model", ModelVersion: "v1", ResolvedAt: personFactAPINow,
		Policy: personfacts.PolicyContext{ProviderPolicyFingerprint: "fixture-policy"},
		Claims: []personfacts.ProposedClaim{{
			Target: target, Relation: personfacts.RelationSupport,
			SubmittedValue: json.RawMessage(submitted), Origin: personfacts.OriginExtraction,
			Confidence: personfacts.ConfidenceInputs{ReportedScore: 900},
			Evidence: []personfacts.EvidenceInput{{
				PersonID: personID, SourceClass: personfacts.EvidencePublic,
				Directness: personfacts.DirectSelf, Authority: personfacts.AuthorityAuthoritative,
				SourceURL: "https://example.test/" + suffix, SubjectPersonID: &subject,
				SubjectRef: "synthetic-person", Excerpt: "Synthetic archive payload " + suffix,
				SourceVersion: "source-v1", EventTime: personFactAPINow.Add(-time.Hour),
				RecordedTime: personFactAPINow, IdentityScore: 990,
			}},
		}},
	}, nil)
	require.NoError(t, err)
}

func applyPersonFactAPIStatus(
	t *testing.T, st *store.Store, personID int64, evidenceKey, suffix string,
	supported bool, reason personfacts.EvidenceStatusReason,
) {
	t.Helper()
	_, err := st.ApplyPersonFactGenerationContext(t.Context(), personfacts.GenerationInput{
		PersonID: personID,
		SourceCursors: []personfacts.SourceCursor{{
			Lane: "fixture-status", Start: suffix, End: suffix + "-end",
		}},
		ProgramID: "fixture-program", ProgramVersion: "v1",
		ProgramFingerprint: strings.Repeat("a", 64), CatalogFingerprint: "fixture-catalog",
		Provider: "fixture-provider-secret", ProviderVersion: "v1",
		ResolvedAt: personFactAPINow.Add(time.Duration(len(suffix)) * time.Minute),
		Policy:     personfacts.PolicyContext{ProviderPolicyFingerprint: "fixture-policy"},
		EvidenceStatusChanges: []personfacts.EvidenceStatusChange{{
			EvidenceKey: evidenceKey, SourceVersion: "source-v1",
			Supported: supported, Reason: reason,
		}},
	}, nil)
	require.NoError(t, err)
}

func personFactAPITargetQuery(target personfacts.TargetDescriptor) string {
	return fmt.Sprintf("%s:%s:%s", target.Kind, target.Key, target.Revision)
}

func personFactRawMessageKeys(values map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

func mapKeysAny(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}
