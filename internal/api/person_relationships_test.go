package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestPersonRelationshipHTTPRendersBothEndpointsFromOneRow(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, st := newIdentityLinkTestServer(t)
	alice, bob := mustHTTPPersons(t, st)

	created := personRequest(t, srv, http.MethodPost, personRelationshipsPath,
		fmt.Appendf(nil, `{"source_person_id":%d,"target_person_id":%d,"relationship_type_slug":"parent","start_date":"1994"}`, alice, bob), "")
	require.Equal(http.StatusCreated, created.Code)
	var edge store.PersonRelationship
	require.NoError(json.Unmarshal(created.Body.Bytes(), &edge))
	assert.Equal(alice, edge.SourcePersonID)
	assert.Equal(bob, edge.TargetPersonID)
	assert.Equal("parent", edge.TypeSlug)
	assert.Equal(fmt.Sprintf(`"person-relationship-%d-r%d"`, edge.ID, edge.Revision), created.Header().Get("ETag"))
	assert.Equal(fmt.Sprintf("%s/%d", personRelationshipsPath, edge.ID), created.Header().Get("Location"))

	fromAlice := personRequest(t, srv, http.MethodGet, fmt.Sprintf("%s/%d/relationships", personsPath, alice), nil, "")
	require.Equal(http.StatusOK, fromAlice.Code)
	var aliceView PersonRelationshipsResponse
	require.NoError(json.Unmarshal(fromAlice.Body.Bytes(), &aliceView))
	require.Len(aliceView.Relationships, 1)
	assert.Equal(store.RelationshipDirectionOutgoing, aliceView.Relationships[0].Direction)
	assert.Equal("child", aliceView.Relationships[0].CounterpartLabel)
	assert.Equal(bob, aliceView.Relationships[0].CounterpartPersonID)

	fromBob := personRequest(t, srv, http.MethodGet, fmt.Sprintf("%s/%d/relationships", personsPath, bob), nil, "")
	require.Equal(http.StatusOK, fromBob.Code)
	var bobView PersonRelationshipsResponse
	require.NoError(json.Unmarshal(fromBob.Body.Bytes(), &bobView))
	require.Len(bobView.Relationships, 1)
	assert.Equal(edge.ID, bobView.Relationships[0].Relationship.ID)
	assert.Equal(store.RelationshipDirectionIncoming, bobView.Relationships[0].Direction)
	assert.Equal("parent", bobView.Relationships[0].CounterpartLabel)
}

func TestPersonRelationshipHTTPPatchIsAtomic(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, st := newIdentityLinkTestServer(t)
	alice, bob := mustHTTPPersons(t, st)
	created := personRequest(t, srv, http.MethodPost, personRelationshipsPath,
		fmt.Appendf(nil, `{"source_person_id":%d,"target_person_id":%d,"relationship_type_slug":"spouse"}`, alice, bob), "")
	require.Equal(http.StatusCreated, created.Code)
	var edge store.PersonRelationship
	require.NoError(json.Unmarshal(created.Body.Bytes(), &edge))
	path := fmt.Sprintf("%s/%d", personRelationshipsPath, edge.ID)

	invalidNotes := fmt.Sprintf(`{"end_date":"2023-05","notes":"%s"}`, strings.Repeat("x", 4097))
	bad := personRequest(t, srv, http.MethodPatch, path, []byte(invalidNotes), created.Header().Get("ETag"))
	assert.Equal(http.StatusBadRequest, bad.Code)
	unchanged := personRequest(t, srv, http.MethodGet, path, nil, "")
	require.Equal(http.StatusOK, unchanged.Code)
	var before store.PersonRelationship
	require.NoError(json.Unmarshal(unchanged.Body.Bytes(), &before))
	assert.Nil(before.EndDate)
	assert.Equal(edge.Revision, before.Revision)

	patched := personRequest(t, srv, http.MethodPatch, path,
		[]byte(`{"end_date":"2023-05","notes":"met at the block party"}`), created.Header().Get("ETag"))
	require.Equal(http.StatusOK, patched.Code)
	var after store.PersonRelationship
	require.NoError(json.Unmarshal(patched.Body.Bytes(), &after))
	require.NotNil(after.EndDate)
	require.NotNil(after.Notes)
	assert.Equal("2023-05", after.EndDate.String())
	assert.Equal("met at the block party", *after.Notes)
	assert.Equal(edge.Revision+1, after.Revision)
	assert.Equal(fmt.Sprintf(`"person-relationship-%d-r%d"`, edge.ID, edge.Revision+1), patched.Header().Get("ETag"))

	assert.Equal(http.StatusBadRequest, personRequest(t, srv, http.MethodPatch, path, []byte(`{"end_date":null}`), patched.Header().Get("ETag")).Code)
	assert.Equal(http.StatusBadRequest, personRequest(t, srv, http.MethodPatch, path, []byte(`{}`), patched.Header().Get("ETag")).Code)
}

func TestPersonRelationshipHTTPEndPreconditionsAndConflicts(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, st := newIdentityLinkTestServer(t)
	alice, bob := mustHTTPPersons(t, st)
	created := personRequest(t, srv, http.MethodPost, personRelationshipsPath,
		fmt.Appendf(nil, `{"source_person_id":%d,"target_person_id":%d,"relationship_type_slug":"spouse"}`, alice, bob), "")
	require.Equal(http.StatusCreated, created.Code)
	var edge store.PersonRelationship
	require.NoError(json.Unmarshal(created.Body.Bytes(), &edge))
	path := fmt.Sprintf("%s/%d", personRelationshipsPath, edge.ID)

	assert.Equal(http.StatusConflict, personRequest(t, srv, http.MethodPost, personRelationshipsPath,
		fmt.Appendf(nil, `{"source_person_id":%d,"target_person_id":%d,"relationship_type_slug":"spouse"}`, bob, alice), "").Code)
	assert.Equal(http.StatusPreconditionRequired, personRequest(t, srv, http.MethodPatch, path, []byte(`{"end_date":"2023-05"}`), "").Code)
	ended := personRequest(t, srv, http.MethodPatch, path, []byte(`{"end_date":"2023-05"}`), created.Header().Get("ETag"))
	require.Equal(http.StatusOK, ended.Code)
	assert.Equal(http.StatusConflict, personRequest(t, srv, http.MethodPatch, path, []byte(`{"end_date":"2024"}`), created.Header().Get("ETag")).Code)
	active := personRequest(t, srv, http.MethodGet, fmt.Sprintf("%s/%d/relationships", personsPath, alice), nil, "")
	var activeView PersonRelationshipsResponse
	require.NoError(json.Unmarshal(active.Body.Bytes(), &activeView))
	assert.Empty(activeView.Relationships)
	all := personRequest(t, srv, http.MethodGet, fmt.Sprintf("%s/%d/relationships?include_ended=true", personsPath, alice), nil, "")
	var allView PersonRelationshipsResponse
	require.NoError(json.Unmarshal(all.Body.Bytes(), &allView))
	require.Len(allView.Relationships, 1)
	assert.Equal(http.StatusNoContent, personRequest(t, srv, http.MethodDelete, path, nil, ended.Header().Get("ETag")).Code)
}

func TestPersonRelationshipHTTPRejectsInvalidRequests(t *testing.T) {
	srv, st := newIdentityLinkTestServer(t)
	alice, bob := mustHTTPPersons(t, st)
	for _, test := range []struct {
		name, body string
		want       int
	}{
		{"self", fmt.Sprintf(`{"source_person_id":%d,"target_person_id":%d,"relationship_type_slug":"friend"}`, alice, alice), http.StatusBadRequest},
		{"unknown type", fmt.Sprintf(`{"source_person_id":%d,"target_person_id":%d,"relationship_type_slug":"nemesis"}`, alice, bob), http.StatusBadRequest},
		{"unknown person", fmt.Sprintf(`{"source_person_id":%d,"target_person_id":999999,"relationship_type_slug":"friend"}`, alice), http.StatusNotFound},
		{"malformed date", fmt.Sprintf(`{"source_person_id":%d,"target_person_id":%d,"relationship_type_slug":"friend","start_date":"19th of May"}`, alice, bob), http.StatusBadRequest},
		{"unknown field", fmt.Sprintf(`{"source_person_id":%d,"target_person_id":%d,"relationship_type_slug":"friend","confidence":0.9}`, alice, bob), http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, personRequest(t, srv, http.MethodPost, personRelationshipsPath, []byte(test.body), "").Code)
		})
	}
	assert.Equal(t, http.StatusBadRequest, personRequest(t, srv, http.MethodGet, personsPath+"/0/relationships", nil, "").Code)
}

func TestRelationshipTypeHTTPCRUDAndSystemProtection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, _ := newIdentityLinkTestServer(t)
	listed := personRequest(t, srv, http.MethodGet, relationshipTypesPath, nil, "")
	require.Equal(http.StatusOK, listed.Code)
	var types RelationshipTypesResponse
	require.NoError(json.Unmarshal(listed.Body.Bytes(), &types))
	require.Len(types.RelationshipTypes, 19)
	created := personRequest(t, srv, http.MethodPost, relationshipTypesPath, []byte(`{"slug":"mentor","forward_label":"mentor","reverse_label":"mentee"}`), "")
	require.Equal(http.StatusCreated, created.Code)
	var mentor store.RelationshipType
	require.NoError(json.Unmarshal(created.Body.Bytes(), &mentor))
	patched := personRequest(t, srv, http.MethodPatch, fmt.Sprintf("%s/%d", relationshipTypesPath, mentor.ID), []byte(`{"forward_label":"coach","color":"#112233"}`), created.Header().Get("ETag"))
	require.Equal(http.StatusOK, patched.Code)
	assert.Equal(http.StatusConflict, personRequest(t, srv, http.MethodPost, relationshipTypesPath, []byte(`{"slug":"friend","forward_label":"friend","reverse_label":"friend","is_symmetric":true}`), "").Code)
	assert.Equal(http.StatusNoContent, personRequest(t, srv, http.MethodDelete, fmt.Sprintf("%s/%d", relationshipTypesPath, mentor.ID), nil, patched.Header().Get("ETag")).Code)
}

func TestRelationshipTypeHTTPRejectsEmptyAndNullPatchesWithoutRevisionBump(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, _ := newIdentityLinkTestServer(t)
	created := personRequest(t, srv, http.MethodPost, relationshipTypesPath,
		[]byte(`{"slug":"mentor","forward_label":"mentor","reverse_label":"mentee"}`), "")
	require.Equal(http.StatusCreated, created.Code)
	var mentor store.RelationshipType
	require.NoError(json.Unmarshal(created.Body.Bytes(), &mentor))
	path := fmt.Sprintf("%s/%d", relationshipTypesPath, mentor.ID)

	empty := personRequest(t, srv, http.MethodPatch, path, []byte(`{}`), created.Header().Get("ETag"))
	assert.Equal(http.StatusBadRequest, empty.Code)
	explicitNull := personRequest(t, srv, http.MethodPatch, path,
		[]byte(`{"forward_label":null}`), created.Header().Get("ETag"))
	assert.Equal(http.StatusBadRequest, explicitNull.Code)
	reloaded := personRequest(t, srv, http.MethodGet, path, nil, "")
	require.Equal(http.StatusOK, reloaded.Code)
	var unchanged store.RelationshipType
	require.NoError(json.Unmarshal(reloaded.Body.Bytes(), &unchanged))
	assert.Equal(mentor.Revision, unchanged.Revision)
	assert.Equal("mentor", unchanged.ForwardLabel)
}

func TestPersonRelationshipOpenAPIPatchContracts(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	schemas := OpenAPIDocument().Components.Schemas.Map()
	edgePatch := schemas["PatchPersonRelationshipRequest"]
	require.NotNil(edgePatch)
	require.NotNil(edgePatch.MinProperties)
	assert.Equal(1, *edgePatch.MinProperties)
	require.NotNil(edgePatch.Properties["end_date"])
	assert.False(edgePatch.Properties["end_date"].Nullable)

	typePatch := schemas["PatchRelationshipTypeRequest"]
	require.NotNil(typePatch)
	require.NotNil(typePatch.MinProperties)
	assert.Equal(1, *typePatch.MinProperties)
	for _, field := range []string{"forward_label", "reverse_label", "vcard_related_type", "color", "icon", "description"} {
		require.NotNil(typePatch.Properties[field], field)
		assert.False(typePatch.Properties[field].Nullable, field)
	}
}

func TestRelationshipReviewHTTPListsStagedValues(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, st := newIdentityLinkTestServer(t)
	alice, _ := mustHTTPPersons(t, st)
	_, err := st.ResolveRelatedValueContext(t.Context(), store.RelatedImport{PersonID: alice, RawValue: "Bob from the gym", RawType: "friend", ValueKind: store.RelatedValueKindText, Source: store.ProvenanceVCardImport, Actor: "system"})
	require.NoError(err)
	listed := personRequest(t, srv, http.MethodGet, relationshipReviewsPath+"?status=pending", nil, "")
	require.Equal(http.StatusOK, listed.Code)
	var reviews RelationshipReviewsResponse
	require.NoError(json.Unmarshal(listed.Body.Bytes(), &reviews))
	require.Len(reviews.Reviews, 1)
	assert.Equal("Bob from the gym", reviews.Reviews[0].RawRelatedValue)
	assert.Equal(http.StatusBadRequest, personRequest(t, srv, http.MethodGet, relationshipReviewsPath+"?status=maybe", nil, "").Code)
}

func mustHTTPPersons(t *testing.T, st *stubIdentityCacheStore) (int64, int64) {
	t.Helper()
	aliceParticipant := st.mustParticipant(t, "alice@example.com", "alice", "example.com")
	bobParticipant := st.mustParticipant(t, "bob@example.com", "bob", "example.com")
	alice, _, err := st.CreatePersonFromParticipant(aliceParticipant)
	require.NoError(t, err)
	bob, _, err := st.CreatePersonFromParticipant(bobParticipant)
	require.NoError(t, err)
	return alice.ID, bob.ID
}
