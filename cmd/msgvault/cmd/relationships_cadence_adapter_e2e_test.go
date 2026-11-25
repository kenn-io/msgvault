package cmd

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/store"
)

func TestRelationshipCadenceHydratesAndFiltersThroughProductionAdapter(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := newRelationshipIndexHTTPFixture(t)
	person, _, err := fixture.store.CreatePersonFromParticipantContext(t.Context(), fixture.personID)
	require.NoError(err)
	days := int64(21)
	_, err = fixture.store.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{
		PersonID: person.ID, DefinitionSlug: store.AttributeSlugContactFrequency,
		Value: store.AttributeValue{Type: store.AttributeValueInteger, Integer: &days}, Source: store.ProvenanceUser,
	})
	require.NoError(err)

	handler := fixture.newServer(t, false).Router()
	allResponse := relationshipIndexHTTPJSON(t, handler, http.MethodPost, "/api/v1/relationships", map[string]any{
		"show_all": true,
	})
	allPage := decodeRelationshipIndexHTTP[api.RelationshipsHTTPResponse](t, allResponse)
	require.Len(allPage.Rows, 1)
	require.NotNil(allPage.Rows[0].PersonID)
	assert.Equal(person.ID, *allPage.Rows[0].PersonID)
	require.NotNil(allPage.Rows[0].Cadence)
	require.NotNil(allPage.Rows[0].Cadence.FrequencyDays)
	assert.Equal(days, *allPage.Rows[0].Cadence.FrequencyDays)
	assert.Equal(store.CadenceUnknown, allPage.Rows[0].Cadence.Status)

	unknownResponse := relationshipIndexHTTPJSON(t, handler, http.MethodPost, "/api/v1/relationships", map[string]any{
		"show_all": true, "cadence": "unknown",
	})
	unknownPage := decodeRelationshipIndexHTTP[api.RelationshipsHTTPResponse](t, unknownResponse)
	require.Len(unknownPage.Rows, 1)
	require.NotNil(unknownPage.Rows[0].PersonID)
	assert.Equal(person.ID, *unknownPage.Rows[0].PersonID)
	assert.Equal(int64(1), unknownPage.TotalCount)

	noListResponse := relationshipIndexHTTPJSON(t, handler, http.MethodPost, "/api/v1/relationships", map[string]any{
		"show_all": true, "cadence": "no_list",
	})
	noListPage := decodeRelationshipIndexHTTP[api.RelationshipsHTTPResponse](t, noListResponse)
	assert.Empty(noListPage.Rows)
	assert.Zero(noListPage.TotalCount)
}
