package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func personAttributesPath(personID int64) string {
	return fmt.Sprintf("%s/%d/attributes", personsPath, personID)
}

func newPersonAttributeFixture(t *testing.T) (*Server, int64) {
	t.Helper()
	srv, st := newIdentityLinkTestServer(t)
	participant := st.mustParticipant(t, "alice@example.com", "alice", "example.com")
	person, _, err := st.CreatePersonFromParticipant(participant)
	require.NoError(t, err)
	return srv, person.ID
}

func decodePersonAttributes(
	t *testing.T, body []byte,
) map[string]PersonAttributeGroup {
	t.Helper()
	var response PersonAttributesResponse
	require.NoError(t, json.Unmarshal(body, &response))
	grouped := make(map[string]PersonAttributeGroup, len(response.Attributes))
	for _, group := range response.Attributes {
		grouped[group.Definition.Slug] = group
	}
	return grouped
}

func TestPersonAttributesHTTPListsDefinitionsSetsHistoryAndClears(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, person := newPersonAttributeFixture(t)

	listed := attributeRequest(t, srv, http.MethodGet,
		personAttributesPath(person), nil, "")
	require.Equal(http.StatusOK, listed.Code, listed.Body.String())
	groups := decodePersonAttributes(t, listed.Body.Bytes())
	require.Len(groups, 4)
	assert.Empty(groups[store.AttributeSlugLastContacted].Current)

	path := personAttributesPath(person) + "/" + store.AttributeSlugPrimaryChannel
	first := attributeRequest(t, srv, http.MethodPut, path,
		[]byte(`{"value":{"type":"text","text":"email"},"source":"user"}`), "")
	require.Equal(http.StatusOK, first.Code, first.Body.String())
	var firstWrite store.PersonAttributeWrite
	require.NoError(json.Unmarshal(first.Body.Bytes(), &firstWrite))

	second := attributeRequest(t, srv, http.MethodPut, path,
		[]byte(`{"value":{"type":"text","text":"chat"},"source":"user"}`), "")
	require.Equal(http.StatusOK, second.Code, second.Body.String())
	var secondWrite store.PersonAttributeWrite
	require.NoError(json.Unmarshal(second.Body.Bytes(), &secondWrite))
	require.NotNil(secondWrite.Superseded)
	assert.Equal(firstWrite.Value.ID, secondWrite.Superseded.ID)

	history := attributeRequest(t, srv, http.MethodGet,
		personAttributesPath(person)+"?history=true", nil, "")
	require.Equal(http.StatusOK, history.Code)
	historyGroups := decodePersonAttributes(t, history.Body.Bytes())
	assert.Len(historyGroups[store.AttributeSlugPrimaryChannel].History, 2)

	cleared := attributeRequest(t, srv, http.MethodDelete, path, nil, "")
	require.Equal(http.StatusOK, cleared.Code, cleared.Body.String())
}

func TestPersonAttributesHTTPClearRejectsStaleExpectedValueID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, person := newPersonAttributeFixture(t)
	path := personAttributesPath(person) + "/" + store.AttributeSlugPrimaryChannel

	first := attributeRequest(t, srv, http.MethodPut, path,
		[]byte(`{"value":{"type":"text","text":"email"},"source":"user"}`), "")
	require.Equal(http.StatusOK, first.Code, first.Body.String())
	var firstWrite store.PersonAttributeWrite
	require.NoError(json.Unmarshal(first.Body.Bytes(), &firstWrite))

	second := attributeRequest(t, srv, http.MethodPut, path,
		[]byte(`{"value":{"type":"text","text":"chat"},"source":"user"}`), "")
	require.Equal(http.StatusOK, second.Code, second.Body.String())
	var secondWrite store.PersonAttributeWrite
	require.NoError(json.Unmarshal(second.Body.Bytes(), &secondWrite))

	staleClear := attributeRequest(t, srv, http.MethodDelete,
		fmt.Sprintf("%s?expected_value_id=%d", path, firstWrite.Value.ID), nil, "")
	require.Equal(http.StatusConflict, staleClear.Code, staleClear.Body.String())

	listed := attributeRequest(t, srv, http.MethodGet,
		personAttributesPath(person), nil, "")
	require.Equal(http.StatusOK, listed.Code, listed.Body.String())
	current := decodePersonAttributes(t, listed.Body.Bytes())[store.AttributeSlugPrimaryChannel].Current
	require.Len(current, 1)
	assert.Equal(secondWrite.Value.ID, current[0].ID)
}

func TestPersonAttributesHTTPRejectsDerivedInvalidUnknownAndSupportsDryRun(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, person := newPersonAttributeFixture(t)

	derived := attributeRequest(t, srv, http.MethodPut,
		personAttributesPath(person)+"/"+store.AttributeSlugLastContacted,
		[]byte(`{"value":{"type":"timestamp","timestamp":"2026-07-30T09:00:00Z"},
			"source":"system"}`), "")
	assert.Equal(http.StatusConflict, derived.Code)

	invalid := attributeRequest(t, srv, http.MethodPut,
		personAttributesPath(person)+"/"+store.AttributeSlugPrimaryChannel,
		[]byte(`{"value":{"type":"text","text":"carrier_pigeon"},"source":"user"}`), "")
	assert.Equal(http.StatusBadRequest, invalid.Code)

	for _, malformed := range [][]byte{
		[]byte(`{"value":{"type":"text","text":"email","record_type":"person"},"source":"user"}`),
		[]byte(`{"value":{"type":"text","text":"email","record_id":1},"source":"user"}`),
	} {
		response := attributeRequest(t, srv, http.MethodPut,
			personAttributesPath(person)+"/"+store.AttributeSlugPrimaryChannel,
			malformed, "")
		assert.Equal(http.StatusBadRequest, response.Code, response.Body.String())
	}

	unknown := attributeRequest(t, srv, http.MethodPut,
		personAttributesPath(person)+"/no_such_definition",
		[]byte(`{"value":{"type":"text","text":"x"},"source":"user"}`), "")
	assert.Equal(http.StatusNotFound, unknown.Code)

	preview := attributeRequest(t, srv, http.MethodPut,
		personAttributesPath(person)+"/"+store.AttributeSlugPrimaryChannel+"?dry_run=true",
		[]byte(`{"value":{"type":"text","text":"email"},"source":"user"}`), "")
	require.Equal(http.StatusOK, preview.Code, preview.Body.String())
	var write store.PersonAttributeWrite
	require.NoError(json.Unmarshal(preview.Body.Bytes(), &write))
	assert.True(write.DryRun)
	assert.Zero(write.Value.ID)

	missing := attributeRequest(t, srv, http.MethodGet,
		personAttributesPath(999_999), nil, "")
	assert.Equal(http.StatusNotFound, missing.Code)

	badExpected := attributeRequest(t, srv, http.MethodPut,
		personAttributesPath(person)+"/"+store.AttributeSlugPrimaryChannel,
		[]byte(`{"value":{"type":"text","text":"email"},"source":"user",
			"expected_value_id":-5}`), "")
	assert.Equal(http.StatusBadRequest, badExpected.Code, badExpected.Body.String())
}

func TestPersonAttributesHTTPKeepsInactiveDefinitionValuesVisibleAndClearable(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, st := newIdentityLinkTestServer(t)
	participant := st.mustParticipant(t, "alice@example.com", "alice", "example.com")
	person, _, err := st.CreatePersonFromParticipant(participant)
	require.NoError(err)
	ctx := t.Context()

	definition, err := st.CreateAttributeDefinitionContext(ctx,
		store.AttributeDefinitionInput{
			UniversalID: "test-retired-api-field", ObjectType: store.AttributeObjectPerson,
			Slug: "retired_api_field", Label: "Retired field",
			ValueType: store.AttributeValueText, FieldType: store.AttributeFieldText,
			APIMutable: true, IsDeletable: true,
		})
	require.NoError(err)
	path := personAttributesPath(person.ID) + "/" + definition.Slug
	set := attributeRequest(t, srv, http.MethodPut, path,
		[]byte(`{"value":{"type":"text","text":"stale"},"source":"user"}`), "")
	require.Equal(http.StatusOK, set.Code, set.Body.String())

	inactive := false
	_, err = st.UpdateAttributeDefinitionContext(ctx, definition.ID, definition.Revision,
		store.AttributeDefinitionUpdate{IsActive: &inactive})
	require.NoError(err)

	listed := attributeRequest(t, srv, http.MethodGet,
		personAttributesPath(person.ID), nil, "")
	require.Equal(http.StatusOK, listed.Code, listed.Body.String())
	groups := decodePersonAttributes(t, listed.Body.Bytes())
	group, ok := groups[definition.Slug]
	require.True(ok, "values under an inactive definition must stay listed")
	assert.False(group.Definition.IsActive)
	require.Len(group.Current, 1)

	cleared := attributeRequest(t, srv, http.MethodDelete, path, nil, "")
	require.Equal(http.StatusOK, cleared.Code, cleared.Body.String())

	relisted := attributeRequest(t, srv, http.MethodGet,
		personAttributesPath(person.ID), nil, "")
	require.Equal(http.StatusOK, relisted.Code)
	_, ok = decodePersonAttributes(t, relisted.Body.Bytes())[definition.Slug]
	assert.False(ok,
		"an inactive definition with no current values must drop out of the listing")
}
