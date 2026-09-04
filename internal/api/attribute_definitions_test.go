package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func attributeRequest(
	t *testing.T, srv *Server, method, path string, body []byte, ifMatch string,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, req)
	return response
}

func TestAttributeDefinitionsHTTPListsSeedsAndRejectsUniqueness(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, _ := newIdentityLinkTestServer(t)

	response := attributeRequest(t, srv, http.MethodGet,
		"/api/v1/attribute-definitions?object_type=person", nil, "")
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	var listed AttributeDefinitionsResponse
	require.NoError(json.Unmarshal(response.Body.Bytes(), &listed))
	require.Len(listed.Definitions, len(store.SeededAttributeDefinitions()))
	assert.NotContains(response.Body.String(), "is_unique")

	rejected := attributeRequest(t, srv, http.MethodPost,
		"/api/v1/attribute-definitions", []byte(`{
			"object_type":"person","slug":"employee_number",
			"label":"Employee number","value_type":"text","field_type":"text",
			"is_unique":true
		}`), "")
	assert.Equal(http.StatusBadRequest, rejected.Code)
	assert.Contains(rejected.Body.String(), "attribute_uniqueness_unsupported")
}

func TestAttributeDefinitionsHTTPCreateRenameAndDelete(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, _ := newIdentityLinkTestServer(t)

	createdResponse := attributeRequest(t, srv, http.MethodPost,
		"/api/v1/attribute-definitions", []byte(`{
			"object_type":"person","slug":"favorite_tea","label":"Favorite tea",
			"value_type":"text","field_type":"select","cardinality":"multi",
			"is_sensitive":true,
			"options":{"choices":[{"value":"green","label":"Green"}]}
		}`), "")
	require.Equal(http.StatusCreated, createdResponse.Code, createdResponse.Body.String())
	var created store.AttributeDefinition
	require.NoError(json.Unmarshal(createdResponse.Body.Bytes(), &created))
	assert.NotEmpty(created.UniversalID)
	assert.Equal(store.AttributeOwnershipUser, created.Ownership)
	assert.True(created.IsSensitive)
	assert.Equal(fmt.Sprintf(`"attribute-definition-%d-r1"`, created.ID),
		createdResponse.Header().Get("ETag"))

	renamedResponse := attributeRequest(t, srv, http.MethodPatch,
		fmt.Sprintf("/api/v1/attribute-definitions/%d", created.ID),
		[]byte(`{"label":"Tea preferences","is_sensitive":false}`), createdResponse.Header().Get("ETag"))
	require.Equal(http.StatusOK, renamedResponse.Code, renamedResponse.Body.String())
	var renamed store.AttributeDefinition
	require.NoError(json.Unmarshal(renamedResponse.Body.Bytes(), &renamed))
	assert.Equal("Tea preferences", renamed.Label)
	assert.Equal(created.UniversalID, renamed.UniversalID)
	assert.Equal(created.Slug, renamed.Slug)
	assert.False(renamed.IsSensitive)

	stale := attributeRequest(t, srv, http.MethodPatch,
		fmt.Sprintf("/api/v1/attribute-definitions/%d", created.ID),
		[]byte(`{"label":"Stale"}`), createdResponse.Header().Get("ETag"))
	assert.Equal(http.StatusConflict, stale.Code)

	removed := attributeRequest(t, srv, http.MethodDelete,
		fmt.Sprintf("/api/v1/attribute-definitions/%d", created.ID), nil,
		renamedResponse.Header().Get("ETag"))
	assert.Equal(http.StatusNoContent, removed.Code, removed.Body.String())
}

func TestAttributeDefinitionsHTTPCreateDerivesSlug(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, _ := newIdentityLinkTestServer(t)

	body, err := json.Marshal(CreateAttributeDefinitionRequest{
		ObjectType: "person", Label: "Favorite color",
		ValueType: "text", FieldType: "text",
		Cardinality: "single",
	})
	require.NoError(err)
	assert.NotContains(string(body), `"slug"`)

	response := attributeRequest(t, srv, http.MethodPost,
		"/api/v1/attribute-definitions", body, "")
	require.Equal(http.StatusCreated, response.Code, response.Body.String())
	var created store.AttributeDefinition
	require.NoError(json.Unmarshal(response.Body.Bytes(), &created))
	assert.Equal("favorite_color", created.Slug)
}

func TestAttributeDefinitionsHTTPWebSafeChoicesRoundTripThroughPersonWrites(t *testing.T) {
	tests := []struct {
		name      string
		valueType string
		choices   []string
		canonical []string
		maxLength int
		values    []json.RawMessage
	}{
		{
			name: "text", valueType: "text",
			choices:   []string{"\u0085😀\uFEFF\u0085"},
			canonical: []string{"😀\uFEFF"},
			maxLength: 2,
			values: []json.RawMessage{
				[]byte(`{"type":"text","text":"😀\uFEFF"}`),
			},
		},
		{
			name: "integer", valueType: "integer",
			choices: []string{"-9007199254740991", "9007199254740991"},
			values: []json.RawMessage{
				[]byte(`{"type":"integer","integer":-9007199254740991}`),
				[]byte(`{"type":"integer","integer":9007199254740991}`),
			},
		},
		{
			name: "real", valueType: "real",
			choices: []string{"-999999.5", "0", "0.0001", "999999.5"},
			values: []json.RawMessage{
				[]byte(`{"type":"real","real":-999999.5}`),
				[]byte(`{"type":"real","real":0}`),
				[]byte(`{"type":"real","real":0.0001}`),
				[]byte(`{"type":"real","real":999999.5}`),
			},
		},
		{
			name: "timestamp", valueType: "timestamp",
			choices: []string{
				"0000-01-01T00:00:00Z",
				"2026-01-01T00:00:00.123456789Z",
				"9999-12-31T23:59:59Z",
			},
			values: []json.RawMessage{
				[]byte(`{"type":"timestamp","timestamp":"0000-01-01T00:00:00Z"}`),
				[]byte(`{"type":"timestamp","timestamp":"2026-01-01T00:00:00.123456789Z"}`),
				[]byte(`{"type":"timestamp","timestamp":"9999-12-31T23:59:59Z"}`),
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			srv, person := newPersonAttributeFixture(t)
			choices := make([]map[string]string, 0, len(test.choices))
			for _, value := range test.choices {
				choices = append(choices, map[string]string{"value": value, "label": value})
			}
			options := map[string]any{"choices": choices}
			if test.maxLength > 0 {
				options["max_length"] = test.maxLength
			}
			createBody, err := json.Marshal(map[string]any{
				"object_type": "person", "slug": "web_safe_" + test.name,
				"label": "Web safe " + test.name, "value_type": test.valueType,
				"field_type": "multiselect", "cardinality": "multi",
				"options": options,
			})
			require.NoError(err)
			createdResponse := attributeRequest(t, srv, http.MethodPost,
				"/api/v1/attribute-definitions", createBody, "")
			require.Equal(http.StatusCreated, createdResponse.Code, createdResponse.Body.String())
			var created store.AttributeDefinition
			require.NoError(json.Unmarshal(createdResponse.Body.Bytes(), &created))
			require.NotNil(created.Options)
			wantChoices := test.canonical
			if wantChoices == nil {
				wantChoices = test.choices
			}
			require.Len(created.Options.Choices, len(wantChoices))
			for i, choice := range created.Options.Choices {
				assert.Equal(wantChoices[i], choice.Value)
			}
			assert.Equal(test.maxLength, created.Options.MaxLength)

			for _, value := range test.values {
				writeBody, marshalErr := json.Marshal(map[string]any{
					"value": value, "source": "user",
				})
				require.NoError(marshalErr)
				written := attributeRequest(t, srv, http.MethodPut,
					personAttributesPath(person)+"/"+created.Slug, writeBody, "")
				require.Equal(http.StatusOK, written.Code, written.Body.String())
			}
		})
	}
}

func TestAttributeDefinitionsHTTPProtectsSeedAndRejectsUnknownFields(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, st := newIdentityLinkTestServer(t)

	seeded, err := st.GetAttributeDefinitionBySlugContext(t.Context(),
		store.AttributeObjectPerson, store.AttributeSlugPrimaryChannel)
	require.NoError(err)
	protected := attributeRequest(t, srv, http.MethodDelete,
		fmt.Sprintf("/api/v1/attribute-definitions/%d", seeded.ID), nil,
		attributeDefinitionETag(*seeded))
	assert.Equal(http.StatusConflict, protected.Code)

	religion, err := st.GetAttributeDefinitionBySlugContext(t.Context(),
		store.AttributeObjectPerson, store.AttributeSlugReligion)
	require.NoError(err)
	sensitivityPatch := attributeRequest(t, srv, http.MethodPatch,
		fmt.Sprintf("/api/v1/attribute-definitions/%d", religion.ID),
		[]byte(`{"is_sensitive":false}`), attributeDefinitionETag(*religion))
	assert.Equal(http.StatusBadRequest, sensitivityPatch.Code, sensitivityPatch.Body.String())
	assert.Contains(sensitivityPatch.Body.String(), "attribute_invalid")

	unknown := attributeRequest(t, srv, http.MethodPost,
		"/api/v1/attribute-definitions", []byte(`{
			"object_type":"person","slug":"scratch","label":"Scratch",
			"value_type":"text","field_type":"text","ownership":"system"
		}`), "")
	assert.Equal(http.StatusBadRequest, unknown.Code)
}
