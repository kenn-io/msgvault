package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/explorecatalog"
	"go.kenn.io/msgvault/pkg/client/generated"
)

const (
	openAPIArtifactPath       = "../../api/openapi.yaml"
	openAPIClientArtifactPath = "../../pkg/client/openapi.yaml"
	openAPIClientGeneratedDir = "../../pkg/client/generated"
)

func TestOpenAPIDocumentUsesAPISchemaVersion(t *testing.T) {
	doc := OpenAPIDocument()

	require.NotNil(t, doc.Info, "openapi info")
	assert.Equal(t, APISchemaVersion, doc.Info.Version, "info.version tracks API schema, not binary version")
	assert.NotEmpty(t, doc.Paths, "paths")
}

func TestAnalyticsCacheReadinessUsesAdditiveSchemaVersion(t *testing.T) {
	assert.Equal(t, "1.43.0", APISchemaVersion)
}

func TestOrganizationCreateOpenAPIDocumentsLocationHeader(t *testing.T) {
	require := require.New(t)
	assert.Equal(t, "1.43.0", APISchemaVersion,
		"organization and employment routes are an additive schema release")
	for _, document := range []*huma.OpenAPI{
		OpenAPIDocument(),
		openAPIClientDocument(),
	} {
		operation := document.Paths[organizationsPath].Post
		require.NotNil(operation)
		response := operation.Responses[httpStatusKey(http.StatusCreated)]
		require.NotNil(response)
		require.Contains(response.Headers, "Location")
		require.Equal(huma.TypeString, response.Headers["Location"].Schema.Type)
	}
}

func TestOrganizationCreateSchemaOmitsPatchOnlyRetiredState(t *testing.T) {
	require := require.New(t)
	for _, document := range []*huma.OpenAPI{
		OpenAPIDocument(),
		openAPIClientDocument(),
	} {
		createSchema := operationBodySchema(
			t, document, document.Paths[organizationsPath].Post)
		patchSchema := operationBodySchema(
			t, document, document.Paths[organizationsPath+"/{id}"].Patch)
		require.NotContains(createSchema.Properties, "retired")
		require.Contains(patchSchema.Properties, "retired")
	}
}

func operationBodySchema(
	t *testing.T, document *huma.OpenAPI, operation *huma.Operation,
) *huma.Schema {
	t.Helper()
	require.NotNil(t, operation)
	require.NotNil(t, operation.RequestBody)
	schema := operation.RequestBody.Content["application/json"].Schema
	require.NotNil(t, schema)
	if schema.Ref == "" {
		return schema
	}
	const prefix = "#/components/schemas/"
	require.True(t, strings.HasPrefix(schema.Ref, prefix), schema.Ref)
	resolved := document.Components.Schemas.Map()[strings.TrimPrefix(schema.Ref, prefix)]
	require.NotNil(t, resolved)
	return resolved
}

func TestSourceStatusRunReferencesAreNullable(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	doc := OpenAPIDocument()
	schema := doc.Components.Schemas.Map()["SourceStatus"]
	requirements.NotNil(schema)
	for _, name := range []string{"active_sync", "latest_sync", "last_successful_sync"} {
		property := schema.Properties[name]
		requirements.NotNil(property, name)
		requirements.Len(property.OneOf, 2, name)
		assertions.Equal("#/components/schemas/SyncRunStatus", property.OneOf[0].Ref, name)
		assertions.Equal("null", property.OneOf[1].Type, name)
	}
}

func TestExploreServiceUnavailableResponseUsesNonExclusiveAlternatives(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	doc := OpenAPIDocument()
	operation := doc.Paths["/api/v1/explore"].Post
	requirements.NotNil(operation)
	response := operation.Responses["503"]
	requirements.NotNil(response)
	schema := response.Content["application/json"].Schema
	requirements.NotNil(schema)
	assertions.Empty(schema.OneOf)
	assertions.Len(schema.AnyOf, 2)
}

func TestOpenAPIFileNamesAndMIMETypesAreRequiredButMayBeEmpty(t *testing.T) {
	doc := OpenAPIDocument()
	for _, schemaName := range []string{"FileSearchRow", "FileMetadataResponse"} {
		t.Run(schemaName, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			schema := doc.Components.Schemas.Map()[schemaName]
			requirements.NotNil(schema)
			for _, property := range []string{"filename", "mime_type"} {
				assertions.Contains(schema.Required, property)
				field := schema.Properties[property]
				requirements.NotNil(field)
				assertions.Equal("string", field.Type)
				assertions.Nil(field.MinLength, "empty %s is legitimate archive metadata", property)
			}
		})
	}
}

func TestOpenAPIClientUsesPresenceAwareFileMetadataStrings(t *testing.T) {
	publicSchemas := OpenAPIDocument().Components.Schemas.Map()
	clientSchemas := openAPIClientDocument().Components.Schemas.Map()
	for _, schemaName := range []string{"FileSearchRow", "FileMetadataResponse"} {
		t.Run(schemaName, func(t *testing.T) {
			assertions := assert.New(t)
			for _, property := range []string{"filename", "mime_type"} {
				assertions.Contains(publicSchemas[schemaName].Required, property)
				assertions.False(publicSchemas[schemaName].Properties[property].Nullable)
				assertions.Contains(clientSchemas[schemaName].Required, property)
				assertions.True(clientSchemas[schemaName].Properties[property].Nullable,
					"client generation needs a pointer to distinguish missing from present empty")
			}
		})
	}
}

func TestOpenAPIJSONVersionPrettyPrintsSchema(t *testing.T) {
	assert := assert.New(t)
	require :=
		require.New(t)

	doc, err := OpenAPIJSONVersion("3.1")
	require.NoError(
		err, "render OpenAPI JSON")

	assert.True(bytes.HasSuffix(doc, []byte("\n")), "json output should end with newline")

	var decoded struct {
		OpenAPI string `json:"openapi"`
		Info    struct {
			Version string `json:"version"`
		} `json:"info"`
	}
	require.NoError(
		json.Unmarshal(doc, &decoded), "decode OpenAPI JSON")

	assert.Equal("3.1.0", decoded.OpenAPI)
	assert.Equal(APISchemaVersion, decoded.Info.Version)
}

func TestOpenAPIYAMLDeterministic(t *testing.T) {
	first, err := OpenAPIYAML()
	require.NoError(t, err, "first render")
	second, err := OpenAPIYAML()
	require.NoError(t, err, "second render")

	assert.Equal(t, string(first), string(second), "OpenAPI YAML should be deterministic")
}

func TestOpenAPIIdentityDiscoveryApplyIsOptional(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	for _, document := range []*huma.OpenAPI{OpenAPIDocument(), openAPIClientDocument()} {
		schema := document.Components.Schemas.Map()["DiscoverRequest"]
		requirements.NotNil(schema)
		assertions.NotContains(schema.Required, "apply", "omitting apply previews discovery")
		requirements.NotNil(schema.Properties["apply"])
		assertions.Equal("boolean", schema.Properties["apply"].Type)
	}
}

func TestOpenAPIIdentityCandidateRequiresProviderStates(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	for _, document := range []*huma.OpenAPI{OpenAPIDocument(), openAPIClientDocument()} {
		schema := document.Components.Schemas.Map()["Candidate"]
		requirements.NotNil(schema)
		assertions.Contains(schema.Required, "provider_states")
		property := schema.Properties["provider_states"]
		requirements.NotNil(property)
		assertions.Equal("array", property.Type)
		assertions.False(property.Nullable, "provider_states must always be an array, never null")
		requirements.NotNil(property.Items)
		assertions.Equal("string", property.Items.Type)
	}
	field, ok := reflect.TypeFor[generated.Candidate]().FieldByName("ProviderStates")
	requirements.True(ok)
	assertions.Equal(reflect.Slice, field.Type.Kind())
	assertions.Equal("provider_states", field.Tag.Get("json"),
		"generated clients must always serialize provider_states without omitempty")
}

func TestOpenAPIIdentityImportUsesParsedEntryContract(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	for _, document := range []*huma.OpenAPI{OpenAPIDocument(), openAPIClientDocument()} {
		path := document.Paths["/api/v1/cli/identities/import"]
		requirements.NotNil(path)
		operation := path.Post
		requirements.NotNil(operation)
		request := document.Components.Schemas.Map()["ImportRequest"]
		requirements.NotNil(request)
		assertions.Contains(request.Required, "entries")
		assertions.NotContains(request.Properties, "file")
		assertions.NotContains(request.Properties, "data")
		entries := request.Properties["entries"]
		requirements.NotNil(entries)
		assertions.Equal("array", entries.Type)
		assertions.False(entries.Nullable, "import entries must never be null")

		result := document.Components.Schemas.Map()["ImportResult"]
		requirements.NotNil(result)
		for _, propertyName := range []string{"candidates", "applied"} {
			assertions.Contains(result.Required, propertyName)
			property := result.Properties[propertyName]
			requirements.NotNil(property)
			assertions.Equal("array", property.Type)
			assertions.False(property.Nullable, "import result %s must never be null", propertyName)
		}
	}
	for _, test := range []struct {
		typ       reflect.Type
		fieldName string
		jsonTag   string
	}{
		{typ: reflect.TypeFor[generated.ImportRequest](), fieldName: "Entries", jsonTag: "entries"},
		{typ: reflect.TypeFor[generated.ImportResult](), fieldName: "Candidates", jsonTag: "candidates"},
		{typ: reflect.TypeFor[generated.ImportResult](), fieldName: "Applied", jsonTag: "applied"},
	} {
		field, ok := test.typ.FieldByName(test.fieldName)
		requirements.True(ok)
		assertions.Equal(reflect.Slice, field.Type.Kind())
		assertions.Equal(test.jsonTag, field.Tag.Get("json"),
			"generated import arrays must serialize without omitempty")
	}
}

func TestOpenAPITotalStatsDocumentsSearchScope(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	doc := OpenAPIDocument()
	op := doc.Paths["/api/v1/stats/total"].Get
	require.NotNil(op, "getTotalStats operation")

	foundSearchScope := false
	foundSourceIDs := false
	for _, param := range op.Parameters {
		switch param.Name {
		case "search_scope":
			assert.Equal("query", param.In, "search_scope location")
			require.NotNil(param.Schema, "search_scope schema")
			assert.Equal("boolean", param.Schema.Type, "search_scope type")
			foundSearchScope = true
		case "source_ids":
			assert.Equal("query", param.In, "source_ids location")
			require.NotNil(param.Schema, "source_ids schema")
			assert.Equal("array", param.Schema.Type, "source_ids type")
			require.NotNil(param.Schema.Items, "source_ids item schema")
			assert.Equal("integer", param.Schema.Items.Type, "source_ids item type")
			foundSourceIDs = true
		}
	}
	assert.True(foundSearchScope, "search_scope query parameter documented")
	assert.True(foundSourceIDs, "source_ids query parameter documented")
}

func TestOpenAPIFastSearchDocumentsSourceIDs(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	doc := OpenAPIDocument()
	op := doc.Paths["/api/v1/search/fast"].Get
	require.NotNil(op, "fastSearch operation")
	for _, param := range op.Parameters {
		if param.Name != "source_ids" {
			continue
		}
		assert.Equal("query", param.In)
		require.NotNil(param.Schema)
		assert.Equal("array", param.Schema.Type)
		require.NotNil(param.Schema.Items)
		assert.Equal("integer", param.Schema.Items.Type)
		return
	}
	assert.Fail("source_ids query parameter is not documented for fastSearch")
}

func TestOpenAPIPersonAttributeContract(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	assert.Equal("1.43.0", APISchemaVersion,
		"activity and identity match review preserve the structured profile contract")

	doc := OpenAPIDocument()
	definitions := doc.Paths["/api/v1/attribute-definitions"]
	require.NotNil(definitions, "attribute definitions path")
	assert.NotNil(definitions.Get, "attribute definitions list operation")
	assert.NotNil(definitions.Post, "attribute definitions create operation")
	definition := doc.Paths["/api/v1/attribute-definitions/{id}"]
	require.NotNil(definition, "attribute definition path")
	assert.NotNil(definition.Patch, "attribute definition patch operation")
	assert.NotNil(definition.Delete, "attribute definition delete operation")
	values := doc.Paths["/api/v1/persons/{id}/attributes"]
	require.NotNil(values, "person attributes path")
	assert.NotNil(values.Get, "person attributes list operation")
	value := doc.Paths["/api/v1/persons/{id}/attributes/{slug}"]
	require.NotNil(value, "person attribute value path")
	assert.NotNil(value.Put, "person attribute set operation")
	assert.NotNil(value.Delete, "person attribute clear operation")
}

func TestOpenAPIPersonProfilePatchUsesWritableEnvelopeShape(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	doc := OpenAPIDocument()

	path := doc.Paths["/api/v1/persons/{id}/profile"]
	require.NotNil(path)
	require.NotNil(path.Patch)
	require.NotNil(path.Patch.RequestBody)
	media := path.Patch.RequestBody.Content["application/json"]
	require.NotNil(media)
	require.NotNil(media.Schema)
	assert.Equal("#/components/schemas/PersonProfilePatchRequest", media.Schema.Ref)

	schemas := doc.Components.Schemas.Map()
	request := schemas["PersonProfilePatchRequest"]
	require.NotNil(request)
	for _, patchName := range []string{
		"PersonNamePatchRequest", "PersonContactPointPatchRequest",
		"PersonAddressPatchRequest", "PersonDatePatchRequest",
		"PersonCategoryPatchRequest", "PersonMediaPatchRequest",
	} {
		assert.NotNil(schemas[patchName], patchName)
	}
	envelope := schemas["ValueEnvelopeInput"]
	require.NotNil(envelope)
	for _, serverOwned := range []string{"id", "created_at", "updated_at", "superseded_at"} {
		assert.NotContains(envelope.Properties, serverOwned)
		assert.NotContains(envelope.Required, serverOwned)
	}
	assert.Contains(envelope.Required, "source")
	require.NotNil(envelope.Properties["ordinal"])
	require.NotNil(envelope.Properties["ordinal"].Minimum)
	assert.Zero(*envelope.Properties["ordinal"].Minimum)

	for schemaName, optionalFields := range map[string][]string{
		"PersonNameInputRequest":    {"original_value"},
		"PersonAddressInputRequest": {"original_value"},
		"PersonDateInputRequest":    {"date", "original_value"},
		"PersonMediaInputRequest":   {"original_value"},
	} {
		input := schemas[schemaName]
		require.NotNil(input, schemaName)
		for _, field := range optionalFields {
			assert.NotContains(input.Required, field, "%s.%s", schemaName, field)
		}
	}
}

func TestOpenAPIPersonProfileMediaContentContract(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	assert.Equal("1.43.0", APISchemaVersion,
		"activity and identity match review preserve the raw profile media contract")
	doc := OpenAPIDocument()
	path := doc.Paths["/api/v1/persons/{id}/profile/media/{media_id}/content"]
	require.NotNil(path)
	require.NotNil(path.Get)
	assert.Equal("getPersonProfileMediaContent", path.Get.OperationID)
	require.Len(path.Get.Security, 1)
	_, secured := path.Get.Security[0]["apiKey"]
	assert.True(secured)
	require.Len(path.Get.Parameters, 2)
	assert.Equal("id", path.Get.Parameters[0].Name)
	assert.Equal("media_id", path.Get.Parameters[1].Name)
	response := path.Get.Responses["200"]
	require.NotNil(response)
	binary := response.Content["*/*"]
	require.NotNil(binary)
	require.NotNil(binary.Schema)
	assert.Equal("binary", binary.Schema.Format)
	for _, status := range []string{"400", "401", "404", "500", "503"} {
		assert.NotNil(path.Get.Responses[status], status)
	}
}

func TestOpenAPIIdentityMatchReviewContract(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	assertions.Equal("1.43.0", APISchemaVersion,
		"activity routes follow the additive identity match review schema release")

	doc := OpenAPIDocument()
	list := doc.Paths["/api/v1/identity/match-candidates"]
	requirements.NotNil(list, "identity match candidate list path")
	requirements.NotNil(list.Get, "identity match candidate list operation")
	assertions.Equal("listIdentityMatchCandidates", list.Get.OperationID)

	for path, operationID := range map[string]string{
		"/api/v1/identity/match-candidates/{id}/accept": "acceptIdentityMatchCandidate",
		"/api/v1/identity/match-candidates/{id}/reject": "rejectIdentityMatchCandidate",
	} {
		item := doc.Paths[path]
		requirements.NotNil(item, path)
		requirements.NotNil(item.Post, path)
		assertions.Equal(operationID, item.Post.OperationID, path)
		requirements.NotNil(item.Post.RequestBody, path)
		assertions.False(item.Post.RequestBody.Required,
			path+" decision notes are optional and the runtime accepts an empty request body")
	}

	// The release is additive. Keep the source-identity route that shipped
	// before identity match review.
	sourceIdentities := doc.Paths["/api/v1/sources/{source_id}/identities"]
	requirements.NotNil(sourceIdentities, "source identity path")
	requirements.NotNil(sourceIdentities.Get, "source identity list operation")
	assertions.Equal("listSourceIdentities", sourceIdentities.Get.OperationID)
}

func TestOpenAPIMeetingImportContract(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	// Pinned so that anyone bumping the schema version has to come here and
	// confirm the meeting-import contract below still holds. Meeting import
	// shipped in 1.33.0; the feed added in 1.34.0, the attributes added in
	// 1.35.0, source-scoped identities added in 1.36.0, and structured profiles
	// added in 1.37.0, raw profile media added in 1.38.0, typed temporal
	// person relationships added in 1.39.0, organizations and employments
	// added in 1.40.0, identity match review added in 1.41.0, and dated activity
	// routes added in 1.42.0 and cache-readiness responses added in 1.43.0 did
	// not touch it.
	assert.Equal("1.43.0", APISchemaVersion, "meeting import is an additive schema release")

	doc := OpenAPIDocument()
	path := doc.Paths["/api/v1/import/meeting"]
	require.NotNil(path, "meeting import path")
	op := path.Post
	require.NotNil(op, "meeting import operation")
	assert.Equal("importMeeting", op.OperationID)
	require.Len(op.Security, 1, "API-key security requirement")
	_, secured := op.Security[0]["apiKey"]
	assert.True(secured, "apiKey security requirement")

	require.NotNil(op.RequestBody, "request body")
	assert.True(op.RequestBody.Required, "request body is required")
	requestMedia := op.RequestBody.Content["application/json"]
	require.NotNil(requestMedia, "JSON request media type")
	require.NotNil(requestMedia.Schema, "request schema")
	assert.Equal("#/components/schemas/MeetingImportRequest", requestMedia.Schema.Ref)

	schemas := doc.Components.Schemas.Map()
	requestSchema := schemas["MeetingImportRequest"]
	require.NotNil(requestSchema, "request component")
	requestAdditionalProperties, ok := requestSchema.AdditionalProperties.(bool)
	require.True(ok, "request additionalProperties is boolean")
	assert.False(requestAdditionalProperties, "request rejects unknown fields")
	assert.ElementsMatch([]string{"source", "meeting"}, requestSchema.Required)

	for _, name := range []string{"Source", "Meeting", "MeetingPerson", "TranscriptSegment"} {
		schema := schemas[name]
		require.NotNil(schema, "%s component", name)
		additionalProperties, ok := schema.AdditionalProperties.(bool)
		require.True(ok, "%s additionalProperties is boolean", name)
		assert.False(additionalProperties, "%s rejects unknown fields", name)
	}
	assert.ElementsMatch(
		[]string{"external_id", "started_at"},
		schemas["Meeting"].Required,
	)
	source := schemas["Source"]
	require.NotNil(source.Properties["identifier"].MaxLength)
	assert.Equal(128, *source.Properties["identifier"].MaxLength)
	require.NotNil(source.Properties["display_name"].MaxLength)
	assert.Equal(256, *source.Properties["display_name"].MaxLength)
	assert.Equal("email", source.Properties["account_email"].Format)

	meeting := schemas["Meeting"]
	require.NotNil(meeting.Properties["external_id"].MaxLength)
	assert.Equal(256, *meeting.Properties["external_id"].MaxLength)
	require.NotNil(meeting.Properties["title"].MaxLength)
	assert.Equal(4096, *meeting.Properties["title"].MaxLength)
	assert.Equal("date-time", meeting.Properties["started_at"].Format)
	assert.Equal("date-time", meeting.Properties["ended_at"].Format)
	require.Len(meeting.AllOf, 2, "meeting cross-field constraints")
	require.Len(meeting.AllOf[0].AnyOf, 4, "meeting requires a non-empty summary or transcript")
	assert.ElementsMatch(
		[]string{"summary_markdown", "summary_text", "transcript", "transcript_segments"},
		[]string{
			meeting.AllOf[0].AnyOf[0].Required[0],
			meeting.AllOf[0].AnyOf[1].Required[0],
			meeting.AllOf[0].AnyOf[2].Required[0],
			meeting.AllOf[0].AnyOf[3].Required[0],
		},
	)
	for idx, property := range []string{"summary_markdown", "summary_text", "transcript"} {
		require.NotNil(meeting.AllOf[0].AnyOf[idx].Properties[property].MinLength)
		assert.Equal(1, *meeting.AllOf[0].AnyOf[idx].Properties[property].MinLength)
	}
	require.NotNil(meeting.AllOf[0].AnyOf[3].Properties["transcript_segments"].MinItems)
	assert.Equal(1, *meeting.AllOf[0].AnyOf[3].Properties["transcript_segments"].MinItems)
	require.NotNil(meeting.AllOf[1].Not, "plain and segmented transcripts are mutually exclusive")
	assert.ElementsMatch(
		[]string{"transcript", "transcript_segments"},
		meeting.AllOf[1].Not.Required,
	)

	assert.Equal("email", schemas["MeetingPerson"].Properties["email"].Format)
	offset := schemas["TranscriptSegment"].Properties["offset_seconds"]
	require.NotNil(offset.Minimum)
	assert.Zero(*offset.Minimum)

	metadata := schemas["Meeting"].Properties["metadata"]
	require.NotNil(metadata, "metadata schema")
	_, extensible := metadata.AdditionalProperties.(*huma.Schema)
	assert.True(extensible, "metadata accepts provider-specific values")

	responseSchema := schemas["MeetingImportResponse"]
	require.NotNil(responseSchema, "meeting import response component")
	assert.Equal([]any{"created", "updated"}, responseSchema.Properties["status"].Enum)

	for _, status := range []string{"200", "201"} {
		response := op.Responses[status]
		require.NotNil(response, "response %s", status)
		media := response.Content["application/json"]
		require.NotNil(media, "response %s JSON media type", status)
		require.NotNil(media.Schema, "response %s schema", status)
		assert.Equal("#/components/schemas/MeetingImportResponse", media.Schema.Ref)
	}
}

func TestOpenAPIBinaryRoutesDocumentJSONErrors(t *testing.T) {
	doc := OpenAPIDocument()
	routes := map[string]struct {
		operationID string
		statuses    []int
	}{
		"/api/v1/cli/message/raw": {
			operationID: "getCLIMessageRaw",
			statuses:    []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound, http.StatusInternalServerError, http.StatusServiceUnavailable},
		},
		"/api/v1/cli/attachment": {
			operationID: "getCLIAttachment",
			statuses:    []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound, http.StatusInternalServerError, http.StatusServiceUnavailable},
		},
		"/api/v1/messages/{id}/inline": {
			operationID: "getMessageInlinePart",
			statuses: []int{
				http.StatusBadRequest,
				http.StatusUnauthorized,
				http.StatusNotFound,
				http.StatusUnsupportedMediaType,
				http.StatusInternalServerError,
				http.StatusNotImplemented,
				http.StatusServiceUnavailable,
			},
		},
	}

	for path, route := range routes {
		t.Run(route.operationID, func(t *testing.T) {
			assert := assert.New(t)
			require :=
				require.New(t)

			op := doc.Paths[path].Get
			require.NotNil(op, "operation")
			defaultResp := op.Responses["default"]
			require.NotNil(defaultResp, "default response")
			jsonError := defaultResp.Content["application/json"]
			require.NotNil(jsonError, "json error media type")
			require.NotNil(jsonError.Schema, "json error schema")
			assert.Equal("#/components/schemas/ErrorResponse", jsonError.Schema.Ref, "json error schema ref")
			for _, status := range route.statuses {
				resp := op.Responses[strconv.Itoa(status)]
				require.NotNil(resp, "response %d", status)
				jsonError := resp.Content["application/json"]
				require.NotNil(jsonError, "response %d json error media type", status)
				require.NotNil(jsonError.Schema, "response %d json error schema", status)
				assert.Equal("#/components/schemas/ErrorResponse", jsonError.Schema.Ref, "response %d json error schema ref", status)
			}
		})
	}
}

func TestOpenAPISavedViewMutationsDocumentBadRequests(t *testing.T) {
	doc := OpenAPIDocument()

	for name, operation := range map[string]*huma.Operation{
		"create": doc.Paths["/api/v1/saved-views"].Post,
		"patch":  doc.Paths["/api/v1/saved-views/{id}"].Patch,
	} {
		t.Run(name, func(t *testing.T) {
			requirements := require.New(t)
			requirements.NotNil(operation)
			response := operation.Responses[strconv.Itoa(http.StatusBadRequest)]
			requirements.NotNil(response)
			media := response.Content["application/json"]
			requirements.NotNil(media)
			requirements.NotNil(media.Schema)
			assert.Equal(t, "#/components/schemas/ErrorResponse", media.Schema.Ref)
		})
	}
}

func TestOpenAPIDocumentsAllExplorationOperations(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	doc := OpenAPIDocument()
	operations := map[string]string{
		"/api/v1/explore":              "explore",
		"/api/v1/explore/groups":       "exploreGroups",
		"/api/v1/explore/preflight":    "preflightExploreSelection",
		"/api/v1/explore/match-counts": "countExploreMatches",
		"/api/v1/explore/files":        "listExploreFiles",
	}
	for path, operationID := range operations {
		t.Run(operationID, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			op := doc.Paths[path].Post
			requirements.NotNil(op)
			assertions.Equal(operationID, op.OperationID)
			requirements.NotNil(op.RequestBody)
			requirements.NotNil(op.Responses["200"])
			requirements.NotNil(op.Responses["400"])
			requirements.NotNil(op.Responses["409"])
			requirements.NotNil(op.Responses["503"])
		})
	}
	filter := doc.Components.Schemas.Map()["ExploreFilter"]
	requirements.NotNil(filter)
	requirements.NotNil(filter.Properties["dimension"])
	assertions.ElementsMatch(
		[]any{"source", "participant", "domain", "message_type", "after", "before", "deletion", "identity"},
		filter.Properties["dimension"].Enum,
	)
	clientFilter := openAPIClientDocument().Components.Schemas.Map()["ExploreFilter"]
	requirements.NotNil(clientFilter)
	requirements.NotNil(clientFilter.Properties["dimension"])
	assertions.Equal([]any{
		"ExploreFilterDimensionSource", "ExploreFilterDimensionParticipant", "ExploreFilterDimensionDomain",
		"ExploreFilterDimensionMessageType", "ExploreFilterDimensionAfter", "ExploreFilterDimensionBefore",
		"ExploreFilterDimensionDeletion", "ExploreFilterDimensionIdentity",
	}, clientFilter.Properties["dimension"].Extensions["x-enum-names"])
	for schemaName, properties := range map[string][]string{
		"ExploreFilter":              {"values"},
		"ExploreHTTPResponse":        {"rows"},
		"EntryRow":                   {"matched_sender_identities", "matched_recipient_identities"},
		"ExploreGroupsHTTPResponse":  {"rows"},
		"ExploreFilesHTTPResponse":   {"files"},
		"ExploreMatchCountsResponse": {"counts"},
		"ExplorePreflightResponse":   {"unavailable_actions"},
	} {
		schema := doc.Components.Schemas.Map()[schemaName]
		requirements.NotNil(schema, schemaName)
		for _, property := range properties {
			requirements.NotNil(schema.Properties[property], "%s.%s", schemaName, property)
			assertions.False(schema.Properties[property].Nullable, "%s.%s must not be nullable", schemaName, property)
		}
	}
}

func TestOpenAPIClientServiceEnumsPreserveExistingGoNames(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	schema := openAPIClientDocument().Components.Schemas.Map()["CreateCommunicationServiceRequest"]
	requirements.NotNil(schema)

	for property, want := range map[string][]any{
		"normalization": {
			"CreateCommunicationServiceRequestNormalizationNone",
			"CreateCommunicationServiceRequestNormalizationLower",
			"CreateCommunicationServiceRequestNormalizationEmail",
			"CreateCommunicationServiceRequestNormalizationPhoneE164",
			"CreateCommunicationServiceRequestNormalizationStripAtLower",
			"CreateCommunicationServiceRequestNormalizationByAddressKind",
		},
		"scope_policy": {
			"CreateCommunicationServiceRequestScopePolicyNone",
			"CreateCommunicationServiceRequestScopePolicyOptional",
			"CreateCommunicationServiceRequestScopePolicyRequired",
		},
	} {
		requirements.NotNil(schema.Properties[property], property)
		assertions.Equal(want, schema.Properties[property].Extensions["x-enum-names"], property)
	}
}

func TestOpenAPIExplorationUsesStructuredUnavailableUnion(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	doc := OpenAPIDocument()
	for _, path := range []string{
		"/api/v1/explore", "/api/v1/explore/groups", "/api/v1/explore/preflight",
		"/api/v1/explore/match-counts", "/api/v1/explore/files",
	} {
		response := doc.Paths[path].Post.Responses["503"]
		requirements.NotNil(response, path)
		media := response.Content["application/json"]
		requirements.NotNil(media, path)
		requirements.NotNil(media.Schema, path)
		requirements.Len(media.Schema.AnyOf, 2, path)
		assertions.ElementsMatch([]string{
			"#/components/schemas/ExploreCacheUnavailableResponse",
			"#/components/schemas/ErrorResponse",
		}, []string{media.Schema.AnyOf[0].Ref, media.Schema.AnyOf[1].Ref}, path)
	}
	schema := doc.Components.Schemas.Map()["ExploreCacheUnavailableResponse"]
	requirements.NotNil(schema)
	readiness := schema.Properties["readiness"]
	requirements.NotNil(readiness)
	assertions.ElementsMatch([]any{"absent", "building", "interrupted", "stale_schema", "drifted"}, readiness.Enum)
}

func TestOpenAPIPersonAndDomainDetailsUseStructuredUnavailableUnion(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	for _, document := range []*huma.OpenAPI{OpenAPIDocument(), openAPIClientDocument()} {
		for _, path := range []string{"/api/v1/people/{id}", "/api/v1/domains/{domain}"} {
			response := document.Paths[path].Get.Responses[httpStatusKey(http.StatusServiceUnavailable)]
			requirements.NotNil(response, path)
			media := response.Content[applicationJSONMediaType]
			requirements.NotNil(media, path)
			requirements.NotNil(media.Schema, path)
			requirements.Len(media.Schema.AnyOf, 2, path)
			assertions.ElementsMatch([]string{
				"#/components/schemas/ExploreCacheUnavailableResponse",
				"#/components/schemas/ErrorResponse",
			}, []string{media.Schema.AnyOf[0].Ref, media.Schema.AnyOf[1].Ref}, path)
		}
	}
}

func TestGeneratedPersonAndDomainDetailsExposeServiceUnavailable(t *testing.T) {
	for name, responseType := range map[string]reflect.Type{
		"get person": reflect.TypeFor[generated.GetPersonResp](),
		"get domain": reflect.TypeFor[generated.GetDomainResp](),
	} {
		_, ok := responseType.FieldByName("JSON503")
		assert.True(t, ok, "%s response must expose JSON503", name)
	}
}

func TestOpenAPIExplorationFiniteRequiredFieldsAreNonNull(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	doc := OpenAPIDocument()
	schemas := doc.Components.Schemas.Map()
	dimension := schemas["ExploreGroupDimension"]
	requirements.NotNil(dimension)
	assertions.ElementsMatch([]any{"source", "participant", "domain", "message_type", "kind", "year", "month"}, dimension.Enum)

	for schemaName, properties := range map[string][]string{
		"ExploreGroupsHTTPRequest":  {"grouping"},
		"ExploreMatchCountsRequest": {"predicate", "row_keys"},
		"ExplorePreflightRequest":   {"selection"},
		"ExploreSelection":          {"predicate", "cache_revision"},
		"ExploreFilesHTTPRequest":   {"predicate"},
	} {
		schema := schemas[schemaName]
		requirements.NotNil(schema, schemaName)
		for _, propertyName := range properties {
			property := schema.Properties[propertyName]
			requirements.NotNil(property, "%s.%s", schemaName, propertyName)
			assertions.Contains(schema.Required, propertyName, "%s.%s", schemaName, propertyName)
			assertions.False(property.Nullable, "%s.%s", schemaName, propertyName)
		}
	}
	grouping := schemas["ExploreGroupsHTTPRequest"].Properties["grouping"]
	requirements.NotNil(grouping.Items)
	assertions.Equal("#/components/schemas/ExploreGroupDimension", grouping.Items.Ref)
	assertions.Equal(1, *grouping.MinItems)
	assertions.Equal(1, *grouping.MaxItems)
	clientGrouping := openAPIClientDocument().Components.Schemas.Map()["ExploreGroupsHTTPRequest"].Properties["grouping"]
	requirements.NotNil(clientGrouping.Extensions)
	assertions.Equal(map[string]any{"validate": "required,min=1,max=1"}, clientGrouping.Extensions["x-oapi-codegen-extra-tags"])
}

func TestOpenAPIExploreGroupingEnumUsesServerCatalog(t *testing.T) {
	dimensions := explorecatalog.GroupingDimensions()
	want := make([]any, len(dimensions))
	for index, dimension := range dimensions {
		want[index] = dimension
	}

	assert.Equal(t, want, exploreGroupingEnum())
	dimension := OpenAPIDocument().Components.Schemas.Map()["ExploreGroupDimension"]
	require.NotNil(t, dimension)
	assert.Equal(t, want, dimension.Enum)
}

func TestOpenAPIArtifactUpToDate(t *testing.T) {
	got, err := OpenAPIYAML()
	require.NoError(t, err, "render OpenAPI YAML")

	want, err := os.ReadFile(openAPIArtifactPath)
	require.NoError(t, err, "read api/openapi.yaml; run `make api-generate` to regenerate")
	assert.Equal(t, normalizeGeneratedArtifact(want), normalizeGeneratedArtifact(got), "api/openapi.yaml is stale; run `make api-generate`")
}

func TestOpenAPIClientSpecArtifactUpToDate(t *testing.T) {
	got, err := OpenAPIYAMLVersion("3.0")
	require.NoError(t, err, "render OpenAPI 3.0 YAML")

	want, err := os.ReadFile(openAPIClientArtifactPath)
	require.NoError(t, err, "read pkg/client/openapi.yaml; run `make api-generate` to regenerate")
	assert.Equal(t, normalizeGeneratedArtifact(want), normalizeGeneratedArtifact(got), "pkg/client/openapi.yaml is stale; run `make api-generate`")
}

func TestOpenAPIClientArtifactUpToDate(t *testing.T) {
	require :=
		require.New(t)

	tmpRoot := t.TempDir()
	tmpGenerated := filepath.Join(tmpRoot, "generated")
	require.NoError(
		os.Mkdir(tmpGenerated, 0o700), "mkdir generated temp dir")

	config, err := os.ReadFile(filepath.Join(openAPIClientGeneratedDir, "config.yaml"))
	require.NoError(
		err, "read generated config")

	require.NoError(
		os.WriteFile(filepath.Join(tmpGenerated, "config.yaml"), config, 0o600), "write generated config")

	spec, err := os.ReadFile(openAPIClientArtifactPath)
	require.NoError(
		err, "read pkg/client/openapi.yaml; run `make api-generate` to regenerate")

	require.NoError(
		os.WriteFile(filepath.Join(tmpRoot, "openapi.yaml"), spec, 0o600), "write generated spec")

	cmd := exec.Command(
		"go",
		"run",
		"github.com/doordash-oss/oapi-codegen-dd/v3/cmd/oapi-codegen@v3.75.5",
		"-config",
		"config.yaml",
		"../openapi.yaml",
	)
	cmd.Dir = tmpGenerated
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.CombinedOutput()
	require.NoError(err, "generate client:\n%s", out)
	fixer, err := filepath.Abs("../codegenfix/cmd")
	require.NoError(err, "resolve generated-client validator fixup")
	cmd = exec.Command("go", "run", fixer, filepath.Join(tmpGenerated, "types.go"))
	out, err = cmd.CombinedOutput()
	require.NoError(err, "apply generated-client validator fixup:\n%s", out)

	gotFiles, err := generatedGoFiles(tmpGenerated)
	require.NoError(
		err, "list generated temp files")

	wantFiles, err := generatedGoFiles(openAPIClientGeneratedDir)
	require.NoError(
		err, "list checked-in generated files")

	require.Equal(wantFiles, gotFiles, "generated file list is stale; run `make api-generate`")

	for _, name := range wantFiles {
		got, err := os.ReadFile(filepath.Join(tmpGenerated, name))
		require.NoError(err, "read generated temp file %s", name)
		want, err := os.ReadFile(filepath.Join(openAPIClientGeneratedDir, name))
		require.NoError(err, "read checked-in generated file %s", name)
		assert.Equal(t,
			normalizeGeneratedArtifact(want),
			normalizeGeneratedArtifact(got),
			"%s is stale; run `make api-generate`", filepath.Join(openAPIClientGeneratedDir, name))
	}
}

func TestOpenAPIGeneratedMeetingImportClient(t *testing.T) {
	assertGeneratedFileContains(t, "client.go",
		"ImportMeeting(ctx context.Context, options *ImportMeetingRequestOptions")
	assertGeneratedFileContains(t, "client_options.go",
		"type ImportMeetingRequestOptions struct")
	assertGeneratedFileContains(t, "payloads.go",
		"type ImportMeetingBody = MeetingImportRequest")
	assertGeneratedFileContains(t, "responses.go",
		"type ImportMeetingResp struct")
	assertGeneratedFileContains(t, "types.go",
		"type MeetingImportResponse struct")
	assertGeneratedFileContains(t, "types.go",
		"Metadata           map[string]any")
	assertGeneratedFileContains(t, "enums.go",
		`MeetingImportResponseStatusUpdated MeetingImportResponseStatus = "updated"`)
}

func assertGeneratedFileContains(t *testing.T, name, expected string) {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(openAPIClientGeneratedDir, name))
	require.NoError(t, err, "read generated client file %s", name)
	assert.Contains(t, string(content), expected,
		"%s is missing the meeting import contract; run `make api-generate`", name)
}

func normalizeGeneratedArtifact(src []byte) string {
	return strings.ReplaceAll(string(src), "\r\n", "\n")
}

func generatedGoFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	files := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".go" || entry.Name() == "generate.go" {
			continue
		}
		files = append(files, entry.Name())
	}
	sort.Strings(files)
	return files, nil
}
