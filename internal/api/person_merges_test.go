package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type corruptPersonMergeSnapshotStore struct {
	*stubIdentityCacheStore
}

type atomicCandidateDecisionStore struct {
	*stubIdentityCacheStore

	result         *store.PersonMergeCandidateDecisionResult
	getPersonCalls int
}

func (s *atomicCandidateDecisionStore) DecidePersonMergeCandidateContext(
	_ context.Context, _ store.PersonMergeCandidateDecisionRequest,
) (*store.PersonMergeCandidateDecisionResult, error) {
	return s.result, nil
}

func (s *atomicCandidateDecisionStore) GetPersonContext(
	_ context.Context, _ int64,
) (*store.Person, error) {
	s.getPersonCalls++
	return nil, store.ErrPersonNotFound
}

func (s *corruptPersonMergeSnapshotStore) GetPersonMergeSnapshotContext(
	_ context.Context, _ int64,
) (*store.PersonMergeSnapshotResponse, error) {
	return &store.PersonMergeSnapshotResponse{
		Version: 1,
		SHA256:  strings.Repeat("0", 64),
		JSON:    json.RawMessage(`{"version":1}`),
	}, nil
}

func TestPersonMergeHTTPMergeInspectDecideAndSplit(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	srv, st := newIdentityLinkTestServer(t)
	survivorParticipant := st.mustParticipant(t,
		"merge-api-survivor@example.com", "Merge Survivor", "example.com")
	absorbedParticipant := st.mustParticipant(t,
		"merge-api-absorbed@example.com", "Merge Absorbed", "example.com")
	survivor, _, err := st.CreatePersonFromParticipant(survivorParticipant)
	require.NoError(err)
	absorbed, _, err := st.CreatePersonFromParticipant(absorbedParticipant)
	require.NoError(err)
	for personID, value := range map[int64]string{
		survivor.ID: "email", absorbed.ID: "chat",
	} {
		_, err = st.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{
			PersonID: personID, DefinitionSlug: store.AttributeSlugPrimaryChannel,
			Value:  store.AttributeValue{Type: store.AttributeValueText, Text: &value},
			Source: store.ProvenanceUser,
		})
		require.NoError(err)
	}
	survivor, err = st.GetPersonContext(ctx, survivor.ID)
	require.NoError(err)
	absorbed, err = st.GetPersonContext(ctx, absorbed.ID)
	require.NoError(err)

	mergedResponse := personMergeAPIRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/people/%d/merge", survivor.ID),
		fmt.Appendf(nil, `{"absorbed_person_id":%d}`, absorbed.ID),
		map[string]string{
			"If-Match":        fmt.Sprintf(`%s, %s`, personETag(*survivor), personETag(*absorbed)),
			"Idempotency-Key": "merge-api-operation",
		})
	require.Equal(http.StatusOK, mergedResponse.Code, mergedResponse.Body.String())
	var merged struct {
		store.PersonMergeResult

		IdentityRevision int64  `json:"identity_revision"`
		CacheState       string `json:"cache_state"`
	}
	require.NoError(json.Unmarshal(mergedResponse.Body.Bytes(), &merged))
	assert.Equal(survivor.ID, merged.Person.ID)
	require.Len(merged.ReviewCandidates, 1)
	assert.Positive(merged.IdentityRevision)
	assert.Equal(identityCacheStateReady, merged.CacheState)
	assert.Equal(1, st.refreshCalls)
	assert.Equal(personETag(merged.Person), mergedResponse.Header().Get("ETag"))
	assert.Equal("no-store", mergedResponse.Header().Get("Cache-Control"))

	listResponse := personMergeAPIRequest(t, srv, http.MethodGet,
		fmt.Sprintf("/api/v1/people/%d/merges", survivor.ID), nil, nil)
	require.Equal(http.StatusOK, listResponse.Code, listResponse.Body.String())
	var listed struct {
		Merges []store.PersonMergeSummary `json:"merges"`
	}
	require.NoError(json.Unmarshal(listResponse.Body.Bytes(), &listed))
	require.Len(listed.Merges, 1)
	assert.Equal(merged.Merge.ID, listed.Merges[0].Merge.ID)

	detailResponse := personMergeAPIRequest(t, srv, http.MethodGet,
		fmt.Sprintf("/api/v1/person-merges/%d", merged.Merge.ID), nil, nil)
	require.Equal(http.StatusOK, detailResponse.Code, detailResponse.Body.String())
	var detail store.PersonMergeDetail
	require.NoError(json.Unmarshal(detailResponse.Body.Bytes(), &detail))
	assert.Equal(merged.Merge.ID, detail.Merge.ID)
	assert.NotEmpty(detail.Rows)

	snapshotResponse := personMergeAPIRequest(t, srv, http.MethodGet,
		fmt.Sprintf("/api/v1/person-merges/%d/snapshot", merged.Merge.ID), nil, nil)
	require.Equal(http.StatusOK, snapshotResponse.Code, snapshotResponse.Body.String())
	assert.Equal("no-store", snapshotResponse.Header().Get("Cache-Control"))
	assert.Equal("application/json", snapshotResponse.Header().Get("Content-Type"))
	var snapshot store.PersonMergeSnapshotResponse
	require.NoError(json.Unmarshal(snapshotResponse.Body.Bytes(), &snapshot))
	assert.Equal(merged.Merge.SnapshotSHA256, snapshot.SHA256)
	assert.NotEmpty(snapshot.JSON)

	decisionResponse := personMergeAPIRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/person-merge-candidates/%d/decision", merged.ReviewCandidates[0].ID),
		fmt.Appendf(nil, `{"person_id":%d,"decision":"reject"}`, merged.Person.ID),
		map[string]string{"If-Match": personETag(merged.Person)})
	require.Equal(http.StatusOK, decisionResponse.Code, decisionResponse.Body.String())
	var decided store.PersonMergeReviewCandidate
	require.NoError(json.Unmarshal(decisionResponse.Body.Bytes(), &decided))
	assert.Equal("rejected", decided.State)
	assert.NotEqual(personETag(merged.Person), decisionResponse.Header().Get("ETag"))

	current, err := st.GetPersonContext(ctx, merged.Person.ID)
	require.NoError(err)
	st.refreshErr = errors.New("cache refresh failed")
	splitResponse := personMergeAPIRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/people/%d/split", current.ID),
		fmt.Appendf(nil, `{"merge_id":%d,"participant_ids":[%d]}`,
			merged.Merge.ID, absorbedParticipant),
		map[string]string{
			"If-Match": personETag(*current), "Idempotency-Key": "split-api-operation",
		})
	require.Equal(http.StatusOK, splitResponse.Code, splitResponse.Body.String())
	var split struct {
		store.PersonSplitResult

		IdentityRevision int64  `json:"identity_revision"`
		CacheState       string `json:"cache_state"`
	}
	require.NoError(json.Unmarshal(splitResponse.Body.Bytes(), &split))
	assert.True(split.ExactReversal)
	assert.Greater(split.IdentityRevision, merged.IdentityRevision)
	assert.Equal(identityCacheStateStale, split.CacheState)
	assert.Equal(2, st.refreshCalls)
	assert.Equal(personETag(split.SourcePerson), splitResponse.Header().Get("ETag"))
	assert.Equal(personETag(split.NewPerson), splitResponse.Header().Get(newPersonETagHeaderName))
}

func TestPersonMergeHTTPCandidateDecisionUsesAtomicRevision(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	wrapped := &atomicCandidateDecisionStore{
		stubIdentityCacheStore: &stubIdentityCacheStore{Store: testutil.NewTestStore(t)},
		result: &store.PersonMergeCandidateDecisionResult{
			ID:             17,
			PersonID:       23,
			State:          "rejected",
			PersonRevision: 42,
		},
	}
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}},
		wrapped, nil, testLogger())
	response := personMergeAPIRequest(t, srv, http.MethodPost,
		"/api/v1/person-merge-candidates/17/decision",
		[]byte(`{"person_id":23,"decision":"reject"}`),
		map[string]string{"If-Match": `"person-23-r41"`})

	require.Equal(http.StatusOK, response.Code, response.Body.String())
	assert.Equal(`"person-23-r42"`, response.Header().Get("ETag"))
	assert.Zero(wrapped.getPersonCalls)
	var candidate store.PersonMergeReviewCandidate
	require.NoError(json.Unmarshal(response.Body.Bytes(), &candidate))
	assert.Equal(int64(17), candidate.ID)
	assert.Equal("rejected", candidate.State)
}

func TestPersonMergeHTTPPreconditionsAndTypedErrors(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, st := newIdentityLinkTestServer(t)
	firstParticipant := st.mustParticipant(t,
		"merge-precondition-a@example.com", "Merge A", "example.com")
	secondParticipant := st.mustParticipant(t,
		"merge-precondition-b@example.com", "Merge B", "example.com")
	first, _, err := st.CreatePersonFromParticipant(firstParticipant)
	require.NoError(err)
	second, _, err := st.CreatePersonFromParticipant(secondParticipant)
	require.NoError(err)
	path := fmt.Sprintf("/api/v1/people/%d/merge", first.ID)
	body := fmt.Appendf(nil, `{"absorbed_person_id":%d}`, second.ID)
	validTags := fmt.Sprintf(`%s, %s`, personETag(*first), personETag(*second))

	tests := []struct {
		name       string
		headers    map[string]string
		wantStatus int
		wantCode   string
	}{
		{name: "missing If-Match", headers: map[string]string{"Idempotency-Key": "missing-tags"},
			wantStatus: http.StatusPreconditionRequired, wantCode: "if_match_required"},
		{name: "missing idempotency", headers: map[string]string{"If-Match": validTags},
			wantStatus: http.StatusPreconditionRequired, wantCode: "idempotency_key_required"},
		{name: "one tag", headers: map[string]string{
			"If-Match": personETag(*first), "Idempotency-Key": "one-tag"},
			wantStatus: http.StatusBadRequest, wantCode: "invalid_if_match"},
		{name: "weak tag", headers: map[string]string{
			"If-Match": "W/" + validTags, "Idempotency-Key": "weak-tag"},
			wantStatus: http.StatusBadRequest, wantCode: "invalid_if_match"},
		{name: "wildcard", headers: map[string]string{
			"If-Match": "*, " + personETag(*second), "Idempotency-Key": "wildcard"},
			wantStatus: http.StatusBadRequest, wantCode: "invalid_if_match"},
		{name: "duplicate tag", headers: map[string]string{
			"If-Match":        personETag(*first) + ", " + personETag(*first),
			"Idempotency-Key": "duplicate-tag"},
			wantStatus: http.StatusBadRequest, wantCode: "invalid_if_match"},
		{name: "unrelated tag", headers: map[string]string{
			"If-Match":        personETag(*first) + `, "person-999-r1"`,
			"Idempotency-Key": "unrelated-tag"},
			wantStatus: http.StatusBadRequest, wantCode: "invalid_if_match"},
		{name: "oversized idempotency", headers: map[string]string{
			"If-Match": validTags, "Idempotency-Key": strings.Repeat("x", 129)},
			wantStatus: http.StatusBadRequest, wantCode: "invalid_idempotency_key"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := personMergeAPIRequest(t, srv, http.MethodPost, path, body, test.headers)
			assert.Equal(test.wantStatus, response.Code, response.Body.String())
			var apiErr ErrorResponse
			require.NoError(json.Unmarshal(response.Body.Bytes(), &apiErr))
			assert.Equal(test.wantCode, apiErr.Error)
		})
	}

	stale := personMergeAPIRequest(t, srv, http.MethodPost, path, body,
		map[string]string{
			"If-Match":        fmt.Sprintf(`"person-%d-r%d", %s`, first.ID, first.Revision+1, personETag(*second)),
			"Idempotency-Key": "stale-merge",
		})
	assert.Equal(http.StatusConflict, stale.Code, stale.Body.String())
	var apiErr ErrorResponse
	require.NoError(json.Unmarshal(stale.Body.Bytes(), &apiErr))
	assert.Equal("person_merge_revision_conflict", apiErr.Error)

	missingMerge := personMergeAPIRequest(t, srv, http.MethodGet,
		"/api/v1/person-merges/999999", nil, nil)
	assert.Equal(http.StatusNotFound, missingMerge.Code)
}

func TestPersonMergeOpenAPIContract(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	document := OpenAPIDocument()
	paths := map[string]string{
		"/api/v1/people/{id}/merge":                               http.MethodPost,
		"/api/v1/people/{id}/split":                               http.MethodPost,
		"/api/v1/people/{id}/merges":                              http.MethodGet,
		"/api/v1/person-merges/{merge_id}":                        http.MethodGet,
		"/api/v1/person-merges/{merge_id}/snapshot":               http.MethodGet,
		"/api/v1/person-merge-candidates/{candidate_id}/decision": http.MethodPost,
	}
	for path, method := range paths {
		item := document.Paths[path]
		require.NotNil(item, path)
		var operation *huma.Operation
		switch method {
		case http.MethodGet:
			operation = item.Get
		case http.MethodPost:
			operation = item.Post
		}
		require.NotNil(operation, path)
	}

	merge := document.Paths["/api/v1/people/{id}/merge"].Post
	assert.True(requiredHeaderParameter(merge, "If-Match"))
	assert.True(requiredHeaderParameter(merge, "Idempotency-Key"))
	assert.Contains(merge.Parameters[1].Description, "Exactly two")
	assert.Equal("#/components/schemas/MergePersonRequest",
		merge.RequestBody.Content[applicationJSONMediaType].Schema.Ref)
	assert.Equal("#/components/schemas/PersonMergeResult",
		merge.Responses[httpStatusKey(http.StatusOK)].Content[applicationJSONMediaType].Schema.Ref)
	require.Contains(merge.Responses[httpStatusKey(http.StatusOK)].Headers, etagHeaderName)
	split := document.Paths["/api/v1/people/{id}/split"].Post
	assert.True(requiredHeaderParameter(split, "If-Match"))
	assert.True(requiredHeaderParameter(split, "Idempotency-Key"))
	assert.Equal("#/components/schemas/SplitPersonRequest",
		split.RequestBody.Content[applicationJSONMediaType].Schema.Ref)
	assert.Equal("#/components/schemas/PersonSplitResult",
		split.Responses[httpStatusKey(http.StatusOK)].Content[applicationJSONMediaType].Schema.Ref)
	require.Contains(split.Responses[httpStatusKey(http.StatusOK)].Headers, etagHeaderName)
	require.Contains(split.Responses[httpStatusKey(http.StatusOK)].Headers,
		newPersonETagOpenAPIHeaderName)
	clientDocument := openAPIClientDocument()
	for schemaName, properties := range map[string][]string{
		"PersonMergeDetail": {"participants", "review_candidates", "rows", "splits"},
		"PersonMergeResult": {"review_candidates"},
		"PersonSplitResult": {"ambiguous_rows"},
	} {
		for _, propertyName := range properties {
			property := clientDocument.Components.Schemas.Map()[schemaName].Properties[propertyName]
			require.NotNil(property, schemaName+"."+propertyName)
			assert.Equal(false, property.Extensions["x-omitempty"], schemaName+"."+propertyName)
		}
	}

	expectedResponses := map[string]string{
		"/api/v1/people/{id}/merges":                "#/components/schemas/PersonMergesResponse",
		"/api/v1/person-merges/{merge_id}":          "#/components/schemas/PersonMergeDetail",
		"/api/v1/person-merges/{merge_id}/snapshot": "#/components/schemas/PersonMergeSnapshotResponse",
	}
	for path, wantRef := range expectedResponses {
		operation := document.Paths[path].Get
		assert.Equal(wantRef,
			operation.Responses[httpStatusKey(http.StatusOK)].Content[applicationJSONMediaType].Schema.Ref)
	}
	snapshot := document.Paths["/api/v1/person-merges/{merge_id}/snapshot"].Get
	require.Contains(snapshot.Responses[httpStatusKey(http.StatusOK)].Headers, "Cache-Control")
	decision := document.Paths["/api/v1/person-merge-candidates/{candidate_id}/decision"].Post
	assert.Equal("#/components/schemas/DecidePersonMergeCandidateRequest",
		decision.RequestBody.Content[applicationJSONMediaType].Schema.Ref)
	assert.Equal("#/components/schemas/PersonMergeReviewCandidate",
		decision.Responses[httpStatusKey(http.StatusOK)].Content[applicationJSONMediaType].Schema.Ref)
}

func TestPersonMergeRequiredOpenAPIContract(t *testing.T) {
	require := require.New(t)
	document := OpenAPIDocument()
	operations := []*huma.Operation{
		document.Paths["/api/v1/identity/links"].Post,
		document.Paths["/api/v1/identity/match-candidates/{id}/accept"].Post,
	}
	for _, operation := range operations {
		require.NotNil(operation)
		conflict := operation.Responses[httpStatusKey(http.StatusConflict)]
		require.NotNil(conflict)
		media := conflict.Content[applicationJSONMediaType]
		require.NotNil(media)
		require.Empty(media.Schema.OneOf)
		require.Len(media.Schema.AnyOf, 2)
		assert.Equal(t, "#/components/schemas/PersonMergeRequiredError", media.Schema.AnyOf[0].Ref)
		assert.Equal(t, "#/components/schemas/ErrorResponse", media.Schema.AnyOf[1].Ref)
	}
}

func TestPersonMergeHTTPErrorMapping(t *testing.T) {
	srv, _ := newIdentityLinkTestServer(t)
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{name: "invalid merge", err: store.ErrPersonMergeInvalid,
			wantStatus: http.StatusBadRequest, wantCode: "person_merge_invalid"},
		{name: "missing merge", err: store.ErrPersonMergeNotFound,
			wantStatus: http.StatusNotFound, wantCode: "person_merge_not_found"},
		{name: "merge revision", err: store.ErrPersonRevisionConflict,
			wantStatus: http.StatusConflict, wantCode: "person_merge_revision_conflict"},
		{name: "split revision", err: store.ErrPersonSplitRevision,
			wantStatus: http.StatusConflict, wantCode: "person_merge_revision_conflict"},
		{name: "merge retry", err: store.ErrPersonMergeIdempotency,
			wantStatus: http.StatusConflict, wantCode: "person_merge_idempotency_conflict"},
		{name: "split retry", err: store.ErrPersonSplitIdempotency,
			wantStatus: http.StatusConflict, wantCode: "person_split_idempotency_conflict"},
		{name: "split reviewed", err: store.ErrPersonSplitReviewed,
			wantStatus: http.StatusConflict, wantCode: "person_split_reviewed_candidates"},
		{name: "split participants", err: store.ErrPersonSplitParticipants,
			wantStatus: http.StatusBadRequest, wantCode: "person_split_invalid_participants"},
		{name: "candidate state", err: store.ErrPersonMergeCandidateState,
			wantStatus: http.StatusConflict, wantCode: "person_merge_candidate_state_changed"},
		{name: "missing candidate", err: store.ErrPersonMergeCandidateNotFound,
			wantStatus: http.StatusNotFound, wantCode: "person_merge_candidate_not_found"},
		{name: "snapshot", err: store.ErrPersonMergeSnapshotCorrupt,
			wantStatus: http.StatusInternalServerError, wantCode: "person_merge_snapshot_corrupt"},
		{name: "internal", err: errors.New("private driver detail"),
			wantStatus: http.StatusInternalServerError, wantCode: "person_merge_failed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			response := httptest.NewRecorder()
			srv.writePersonMergeError(response, test.err)
			assert.Equal(test.wantStatus, response.Code)
			var apiErr ErrorResponse
			require.NoError(json.Unmarshal(response.Body.Bytes(), &apiErr))
			assert.Equal(test.wantCode, apiErr.Error)
			assert.NotContains(response.Body.String(), "private driver detail")
		})
	}
}

func TestPersonMergeHTTPSnapshotRejectsHashMismatch(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	base := &stubIdentityCacheStore{Store: testutil.NewTestStore(t)}
	srv := NewServer(
		&config.Config{Server: config.ServerConfig{APIPort: 8080}},
		&corruptPersonMergeSnapshotStore{stubIdentityCacheStore: base},
		nil,
		testLogger(),
	)

	response := personMergeAPIRequest(t, srv, http.MethodGet,
		"/api/v1/person-merges/1/snapshot", nil, nil)
	assert.Equal(http.StatusInternalServerError, response.Code)
	var apiErr ErrorResponse
	require.NoError(json.Unmarshal(response.Body.Bytes(), &apiErr))
	assert.Equal("person_merge_snapshot_corrupt", apiErr.Error)
	assert.NotContains(response.Body.String(), `{"version":1}`)
}

func requiredHeaderParameter(operation *huma.Operation, name string) bool {
	for _, parameter := range operation.Parameters {
		if parameter.In == "header" && parameter.Name == name {
			return parameter.Required
		}
	}
	return false
}

func personMergeAPIRequest(
	t *testing.T,
	srv *Server,
	method, path string,
	body []byte,
	headers map[string]string,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, request)
	return response
}
