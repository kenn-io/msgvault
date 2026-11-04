package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestAttributeDefinitionsHTTPCreateStability(t *testing.T) {
	for _, stability := range []store.AttributeStability{store.AttributeStabilityFixed, store.AttributeStabilityDurable, store.AttributeStabilityVolatile} {
		t.Run(string(stability), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			srv, _ := newIdentityLinkTestServer(t)
			body, err := json.Marshal(map[string]any{
				"object_type": "person", "label": "Synthetic field", "value_type": "text", "field_type": "text", "stability": stability,
			})
			require.NoError(err)
			response := attributeRequest(t, srv, http.MethodPost, "/api/v1/attribute-definitions", body, "")
			require.Equal(http.StatusCreated, response.Code, response.Body.String())
			var got store.AttributeDefinition
			require.NoError(json.Unmarshal(response.Body.Bytes(), &got))
			assert.Equal(stability, got.Stability)
		})
	}
}

func TestOpenAPIAttributeStabilityContract(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	schemas := OpenAPIDocument().Components.Schemas.Map()
	for _, name := range []string{"CreateAttributeDefinitionRequest", "PatchAttributeDefinitionRequest", "AttributeDefinition"} {
		schema := schemas[name]
		require.NotNil(schema, name)
		field := schema.Properties["stability"]
		require.NotNil(field, name)
		assert.False(field.Nullable, name)
		assert.Equal([]any{"fixed", "durable", "volatile"}, field.Enum, name)
	}
}
