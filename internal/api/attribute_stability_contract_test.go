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
			srv, _ := newIdentityLinkTestServer(t)
			body, err := json.Marshal(map[string]any{
				"object_type": "person", "label": "Synthetic field", "value_type": "text", "field_type": "text", "stability": stability,
			})
			require.NoError(t, err)
			response := attributeRequest(t, srv, http.MethodPost, "/api/v1/attribute-definitions", body, "")
			require.Equal(t, http.StatusCreated, response.Code, response.Body.String())
			var got store.AttributeDefinition
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &got))
			assert.Equal(t, stability, got.Stability)
		})
	}
}

func TestOpenAPIAttributeStabilityContract(t *testing.T) {
	schemas := OpenAPIDocument().Components.Schemas.Map()
	for _, name := range []string{"CreateAttributeDefinitionRequest", "PatchAttributeDefinitionRequest", "AttributeDefinition"} {
		schema := schemas[name]
		require.NotNil(t, schema, name)
		field := schema.Properties["stability"]
		require.NotNil(t, field, name)
		assert.False(t, field.Nullable, name)
		assert.Equal(t, []any{"fixed", "durable", "volatile"}, field.Enum, name)
	}
}
