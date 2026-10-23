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

func TestAttributeDefinitionsHTTPPatchInferenceEligible(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	srv, st := newIdentityLinkTestServer(t)
	definition, err := st.GetAttributeDefinitionBySlugContext(t.Context(), store.AttributeObjectPerson, store.AttributeSlugPersonality)
	requirements.NoError(err)
	requirements.False(definition.InferenceEligible)
	path := fmt.Sprintf("/api/v1/attribute-definitions/%d", definition.ID)
	etag := fmt.Sprintf(`"attribute-definition-%d-r%d"`, definition.ID, definition.Revision)
	response := attributeRequest(t, srv, http.MethodPatch, path, []byte(`{"inference_eligible":true}`), etag)
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	var got store.AttributeDefinition
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &got))
	assertions.True(got.InferenceEligible)
	assertions.Equal(definition.Revision+1, got.Revision)
	assertions.Equal(fmt.Sprintf(`"attribute-definition-%d-r%d"`, definition.ID, got.Revision), response.Header().Get("ETag"))
}

func TestAttributeDefinitionsHTTPCreateInferenceEligible(t *testing.T) {
	for _, tc := range []struct {
		name    string
		setting *bool
		want    bool
	}{
		{name: "default", want: true},
		{name: "disabled", setting: new(false), want: false},
		{name: "enabled", setting: new(true), want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			srv, _ := newIdentityLinkTestServer(t)
			body := map[string]any{"object_type": "person", "slug": "custom_eligibility", "label": "Custom eligibility", "description": "A synthetic custom field", "value_type": "text", "field_type": "text"}
			if tc.setting != nil {
				body["inference_eligible"] = *tc.setting
			}
			encoded, err := json.Marshal(body)
			requirements.NoError(err)
			response := attributeRequest(t, srv, http.MethodPost, "/api/v1/attribute-definitions", encoded, "")
			requirements.Equal(http.StatusCreated, response.Code, response.Body.String())
			var got store.AttributeDefinition
			requirements.NoError(json.Unmarshal(response.Body.Bytes(), &got))
			assertions.Equal(tc.want, got.InferenceEligible)
		})
	}
}
