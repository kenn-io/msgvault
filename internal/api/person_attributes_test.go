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
	return fmt.Sprintf("%s/%d/attributes", peoplePath, personID)
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
	require.Len(groups, len(store.SeededAttributeDefinitions()))
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

// This catches coupling the writable preferred-channel attribute to the
// Directory's observed last-contact channel and its filter membership.
func TestPersonAttributesHTTPPreferredChannelDoesNotChangeDirectoryObservedChannel(t *testing.T) {
	require := require.New(t)
	srv, st := newIdentityLinkTestServer(t)
	person := createDirectoryHTTPPerson(
		t, st, "Channel Fixture", "channel@example.test", "friend", "Example Org", true,
	)
	path := personAttributesPath(person.ID) + "/" + store.AttributeSlugPrimaryChannel

	assertObservedEmail := func() {
		email := personRequest(t, srv, http.MethodGet,
			peoplePath+"/directory?primary_channel=email", nil, "")
		require.Equal(http.StatusOK, email.Code, email.Body.String())
		var page DirectoryPeopleResponse
		require.NoError(json.Unmarshal(email.Body.Bytes(), &page))
		require.Len(page.People, 1)
		assert.Equal(t, person.ID, page.People[0].ID)
		assert.Equal(t, "email", page.People[0].PrimaryChannel)

		chat := personRequest(t, srv, http.MethodGet,
			peoplePath+"/directory?primary_channel=chat", nil, "")
		require.Equal(http.StatusOK, chat.Code, chat.Body.String())
		var chatPage DirectoryPeopleResponse
		require.NoError(json.Unmarshal(chat.Body.Bytes(), &chatPage))
		assert.Empty(t, chatPage.People)
	}

	assertObservedEmail()
	set := attributeRequest(t, srv, http.MethodPut, path,
		[]byte(`{"value":{"type":"text","text":"chat"},"source":"user"}`), "")
	require.Equal(http.StatusOK, set.Code, set.Body.String())
	var write store.PersonAttributeWrite
	require.NoError(json.Unmarshal(set.Body.Bytes(), &write))
	require.NotNil(write.Value)
	require.NotNil(write.Value.Value.Text)
	assert.Equal(t, "chat", *write.Value.Value.Text)
	assertObservedEmail()

	cleared := attributeRequest(t, srv, http.MethodDelete, path, nil, "")
	require.Equal(http.StatusOK, cleared.Code, cleared.Body.String())
	assertObservedEmail()
}

func TestHTTPAppendPersonNoteUsesAtomicStorePath(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, st := newIdentityLinkTestServer(t)
	participant := st.mustParticipant(
		t, "notes-append@example.test", "Notes Person", "example.test",
	)
	person, _, err := st.CreatePersonFromParticipant(participant)
	require.NoError(err)
	path := fmt.Sprintf("%s/%d/notes/append", peoplePath, person.ID)

	first := attributeRequest(t, srv, http.MethodPost, path,
		[]byte(`{"text":"first fact"}`), "")
	require.Equal(http.StatusOK, first.Code, first.Body.String())
	var firstWrite store.PersonAttributeWrite
	require.NoError(json.Unmarshal(first.Body.Bytes(), &firstWrite))
	require.NotNil(firstWrite.Value)
	assert.Equal(store.ProvenanceUser, firstWrite.Value.Source)
	require.NotNil(firstWrite.Value.Value.Text)
	assert.Equal("first fact", *firstWrite.Value.Value.Text)
	assert.Equal("no-store", first.Header().Get("Cache-Control"))

	second := attributeRequest(t, srv, http.MethodPost, path,
		[]byte(`{"text":"second fact"}`), "")
	require.Equal(http.StatusOK, second.Code, second.Body.String())
	var secondWrite store.PersonAttributeWrite
	require.NoError(json.Unmarshal(second.Body.Bytes(), &secondWrite))
	require.NotNil(secondWrite.Value)
	require.NotNil(secondWrite.Value.Value.Text)
	assert.Equal("first fact\nsecond fact", *secondWrite.Value.Value.Text)
	require.NotNil(secondWrite.Superseded)
	assert.Equal(firstWrite.Value.ID, secondWrite.Superseded.ID)

	history := attributeRequest(t, srv, http.MethodGet,
		personAttributesPath(person.ID)+"?history=true&slug="+store.AttributeSlugNotes,
		nil, "")
	require.Equal(http.StatusOK, history.Code, history.Body.String())
	groups := decodePersonAttributes(t, history.Body.Bytes())
	assert.Len(groups[store.AttributeSlugNotes].History, 2)

	blank := attributeRequest(t, srv, http.MethodPost, path,
		[]byte(`{"text":"  \n\t "}`), "")
	require.Equal(http.StatusBadRequest, blank.Code, blank.Body.String())
	assert.Contains(blank.Body.String(), `"error":"attribute_value_invalid"`)

	missing := attributeRequest(t, srv, http.MethodPost,
		fmt.Sprintf("%s/%d/notes/append", peoplePath, 999_999),
		[]byte(`{"text":"missing"}`), "")
	assert.Equal(http.StatusNotFound, missing.Code, missing.Body.String())

	preview := attributeRequest(t, srv, http.MethodPost, path+"?dry_run=true",
		[]byte(`{"text":"preview fact"}`), "")
	require.Equal(http.StatusOK, preview.Code, preview.Body.String())
	var previewWrite store.PersonAttributeWrite
	require.NoError(json.Unmarshal(preview.Body.Bytes(), &previewWrite))
	assert.True(previewWrite.DryRun)
	require.NotNil(previewWrite.Value)
	assert.Zero(previewWrite.Value.ID)
	require.NotNil(previewWrite.Value.Value.Text)
	assert.Equal("first fact\nsecond fact\npreview fact", *previewWrite.Value.Value.Text)

	operationPath := OpenAPIDocument().Paths["/api/v1/people/{id}/notes/append"]
	require.NotNil(operationPath)
	require.NotNil(operationPath.Post)
	assert.Equal("appendPersonNote", operationPath.Post.OperationID)
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
	var conflict PersonAttributeConflictResponse
	require.NoError(json.Unmarshal(staleClear.Body.Bytes(), &conflict))
	assert.Equal("attribute_value_conflict", conflict.Error)
	require.NotNil(conflict.CurrentValueID)
	assert.Equal(secondWrite.Value.ID, *conflict.CurrentValueID)
	require.NotNil(conflict.CurrentValue)
	assert.Equal(secondWrite.Value.ID, conflict.CurrentValue.ID)

	listed := attributeRequest(t, srv, http.MethodGet,
		personAttributesPath(person), nil, "")
	require.Equal(http.StatusOK, listed.Code, listed.Body.String())
	current := decodePersonAttributes(t, listed.Body.Bytes())[store.AttributeSlugPrimaryChannel].Current
	require.Len(current, 1)
	assert.Equal(secondWrite.Value.ID, current[0].ID)
}

func TestPersonAttributesHTTPPreservesLegacySlugAcrossSeedCollision(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, st := newIdentityLinkTestServer(t)
	participant := st.mustParticipant(t, "slug-collision@example.test", "slug-collision", "example.test")
	person, _, err := st.CreatePersonFromParticipant(participant)
	require.NoError(err)

	_, err = st.DB().Exec(st.Rebind(`
		DELETE FROM attribute_definitions WHERE universal_id = ?
	`), store.AttributeUniversalIDLocation)
	require.NoError(err)
	legacy := seededAttributeDefinitionInputBySlug(
		t, store.SeededAttributeDefinitions(), store.AttributeSlugLocation,
	)
	legacy.UniversalID = "994e8d78-4711-42ec-9801-e3348e6fd133"
	legacy.Ownership = store.AttributeOwnershipUser
	legacy.IsDeletable = true
	legacyDefinition, err := st.CreateAttributeDefinitionContext(t.Context(), legacy)
	require.NoError(err)
	require.NoError(st.InitSchema())

	path := personAttributesPath(person.ID) + "/" + store.AttributeSlugLocation
	written := attributeRequest(t, srv, http.MethodPut, path,
		[]byte(`{"value":{"type":"text","text":"Old town"},"source":"user"}`), "")
	require.Equal(http.StatusOK, written.Code, written.Body.String())
	var write store.PersonAttributeWrite
	require.NoError(json.Unmarshal(written.Body.Bytes(), &write))
	assert.Equal(legacyDefinition.ID, write.Value.DefinitionID)

	listed := attributeRequest(t, srv, http.MethodGet,
		personAttributesPath(person.ID), nil, "")
	require.Equal(http.StatusOK, listed.Code, listed.Body.String())
	group := decodePersonAttributes(t, listed.Body.Bytes())[store.AttributeSlugLocation]
	assert.Equal(legacy.UniversalID, group.Definition.UniversalID)
	require.Len(group.Current, 1)
	require.NotNil(group.Current[0].Value.Text)
	assert.Equal("Old town", *group.Current[0].Value.Text)
}

func seededAttributeDefinitionInputBySlug(
	t *testing.T, definitions []store.AttributeDefinitionInput, slug string,
) store.AttributeDefinitionInput {
	t.Helper()
	for _, definition := range definitions {
		if definition.Slug == slug {
			return definition
		}
	}
	require.FailNow(t, "seeded attribute definition not found", "slug=%s", slug)
	return store.AttributeDefinitionInput{}
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
