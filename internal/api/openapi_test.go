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

func TestOpenAPISchemaVersionAsyncImportsIs2160(t *testing.T) {
	assert.Equal(t, "2.16.0", APISchemaVersion)
}

func TestOpenAPIImportJobContract(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	doc := OpenAPIDocument()

	createPath := doc.Paths["/api/v1/imports"]
	require.NotNil(createPath, "import collection path")
	require.NotNil(createPath.Post, "create import operation")
	assert.Equal("createImportJob", createPath.Post.OperationID)
	require.Len(createPath.Post.Security, 1)
	_, secured := createPath.Post.Security[0][apiKeySecurityScheme]
	assert.True(secured, "create import requires API-key security")
	require.NotNil(createPath.Post.RequestBody)
	requestMedia := createPath.Post.RequestBody.Content[applicationJSONMediaType]
	require.NotNil(requestMedia)
	assert.Equal("#/components/schemas/ImportJobRequest", requestMedia.Schema.Ref)
	accepted := createPath.Post.Responses["202"]
	require.NotNil(accepted, "create import documents 202")
	acceptedMedia := accepted.Content[applicationJSONMediaType]
	require.NotNil(acceptedMedia)
	assert.Equal("#/components/schemas/ImportJobResponse", acceptedMedia.Schema.Ref)
	assert.Contains(createPath.Post.Responses, "503", "operation-gate contention is documented")

	statusPath := doc.Paths["/api/v1/imports/{job_id}"]
	require.NotNil(statusPath, "import status path")
	require.NotNil(statusPath.Get, "get import operation")
	assert.Equal("getImportJob", statusPath.Get.OperationID)
	require.Len(statusPath.Get.Parameters, 1)
	assert.Equal("job_id", statusPath.Get.Parameters[0].Name)
	assert.Equal("path", statusPath.Get.Parameters[0].In)
	assert.True(statusPath.Get.Parameters[0].Required)
}

func TestOpenAPIClientImportJobStatusEnumNamesPreserveExistingConstants(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	schema := openAPIClientDocument().Components.Schemas.Map()["ImportJobResponse"]
	requirements.NotNil(schema)
	requirements.NotNil(schema.Properties["status"])
	assertions.Equal([]any{
		"ImportJobResponseStatusPending",
		"ImportJobResponseStatusRunning",
		"ImportJobResponseStatusDone",
		"ImportJobResponseStatusFailed",
	}, schema.Properties["status"].Extensions["x-enum-names"])
}

func TestCLISearchOpenAPIDocumentsDeletionScope(t *testing.T) {
	assertions := assert.New(t)
	operation := OpenAPIDocument().Paths["/api/v1/cli/search"].Get
	require.NotNil(t, operation)
	for _, parameter := range operation.Parameters {
		if parameter.Name == "deletion_scope" {
			assertions.Equal("query", parameter.In)
			assertions.Contains(parameter.Description, "active")
			assertions.Contains(parameter.Description, "deleted")
			assertions.Contains(parameter.Description, "any")
			return
		}
	}
	assertions.Fail("deletion_scope query parameter is not documented")
}

func TestOpenAPIDeepSearchDocumentsUnsupportedListIDParameter(t *testing.T) {
	operation := OpenAPIDocument().Paths["/api/v1/search/deep"].Get
	require.NotNil(t, operation)
	for _, parameter := range operation.Parameters {
		if parameter.Name == "list_id" {
			assert.Contains(t, parameter.Description, "not supported by deep search")
			return
		}
	}
	require.Fail(t, "list_id query parameter is not documented")
}

func TestPersonFactOpenAPIOperationsContainNoReviewMutation(t *testing.T) {
	want := map[string]map[string]string{
		"/api/v1/person-fact-targets": {
			http.MethodGet: "listPersonFactTargets",
		},
		"/api/v1/people/{id}/fact-evidence": {
			http.MethodGet: "listPersonFactEvidence",
		},
		"/api/v1/people/{id}/fact-evidence-status-events": {
			http.MethodGet: "listPersonFactEvidenceStatusEvents",
		},
		"/api/v1/people/{id}/fact-claims": {
			http.MethodGet: "listPersonFactClaims",
		},
		"/api/v1/people/{id}/fact-decisions": {
			http.MethodGet: "listPersonFactDecisions",
		},
		"/api/v1/people/{id}/fact-pins": {
			http.MethodGet: "listPersonFactPins",
		},
		"/api/v1/people/{id}/fact-pins/{kind}/{key}": {
			http.MethodPut: "setPersonFactPin",
		},
	}
	got := make(map[string]map[string]string)
	for path, item := range OpenAPIDocument().Paths {
		if path != "/api/v1/person-fact-targets" &&
			!strings.HasPrefix(path, "/api/v1/people/{id}/fact-") {
			continue
		}
		operations := make(map[string]string)
		for method, operation := range map[string]*huma.Operation{
			http.MethodGet: item.Get, http.MethodPut: item.Put, http.MethodPost: item.Post,
			http.MethodDelete: item.Delete, http.MethodOptions: item.Options,
			http.MethodHead: item.Head, http.MethodPatch: item.Patch, http.MethodTrace: item.Trace,
		} {
			if operation != nil {
				operations[method] = operation.OperationID
			}
		}
		got[path] = operations
	}
	assert.Equal(t, want, got)
}

func TestPersonFactOpenAPIHistoryCollectionsAreNonNullable(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	for _, document := range []*huma.OpenAPI{OpenAPIDocument(), openAPIClientDocument()} {
		for schemaName, propertyName := range map[string]string{
			"PersonFactEvidenceResponse":             "evidence",
			"PersonFactEvidenceStatusEventsResponse": "events",
			"PersonFactClaimsResponse":               "claims",
			"PersonFactDecisionsResponse":            "decisions",
		} {
			schema := document.Components.Schemas.Map()[schemaName]
			requirements.NotNil(schema, schemaName)
			property := schema.Properties[propertyName]
			requirements.NotNil(property, schemaName+"."+propertyName)
			assertions.False(property.Nullable, schemaName+"."+propertyName)
		}
	}
}

func TestPersonFactPinsOpenAPIDeclaresBadPersonIDResponse(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	for _, document := range []*huma.OpenAPI{OpenAPIDocument(), openAPIClientDocument()} {
		path := document.Paths["/api/v1/people/{id}/fact-pins"]
		requirements.NotNil(path)
		requirements.NotNil(path.Get)
		assertions.Contains(path.Get.Responses, "400")
	}
}

func TestOpenAPISeparatesParticipantAnalyticsFromDurablePeople(t *testing.T) {
	assert := assert.New(t)
	doc := OpenAPIDocument()

	assert.Equal("2.16.0", APISchemaVersion)
	for _, path := range []string{
		"/api/v1/participants/search",
		"/api/v1/participants/{id}",
		"/api/v1/participants/{id}/summary",
		"/api/v1/participants/{id}/timeline",
		"/api/v1/participants/{id}/files/search",
		"/api/v1/people/{id}/files/search",
		"/api/v1/people",
		"/api/v1/people/{id}",
		"/api/v1/people/{id}/profile",
		"/api/v1/people/{id}/contact-state",
	} {
		assert.Contains(doc.Paths, path)
	}
	assert.NotContains(doc.Paths, "/api/v1/persons")
	assert.NotContains(doc.Paths, "/api/v1/persons/{id}")
	assert.NotNil(doc.Paths["/api/v1/people/search"])
	assert.Nil(doc.Paths["/api/v1/people/{id}/summary"])
}

func TestAnalyticsCacheReadinessUsesAdditiveSchemaVersion(t *testing.T) {
	assert.Equal(t, "2.16.0", APISchemaVersion)
}

func TestPersonFilesUseAdditiveSchemaVersion(t *testing.T) {
	assert.Equal(t, "2.16.0", APISchemaVersion)
}

func TestPersonFileRoutesPublishTypedPathIDs(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	for _, path := range []string{
		"/api/v1/participants/{id}/files/search",
		"/api/v1/people/{id}/files/search",
	} {
		operation := OpenAPIDocument().Paths[path].Post
		require.NotNil(operation)
		require.Len(operation.Parameters, 1)
		parameter := operation.Parameters[0]
		assert.Equal("id", parameter.Name)
		assert.Equal("path", parameter.In)
		assert.True(parameter.Required)
		assert.Equal(huma.TypeInteger, parameter.Schema.Type)
		assert.Equal(formatInt64, parameter.Schema.Format)
	}
}

func TestOrganizationCreateOpenAPIDocumentsLocationHeader(t *testing.T) {
	require := require.New(t)
	assert.Equal(t, "2.16.0", APISchemaVersion,
		"document and person-file search preserve the organization and employment contract")
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

func TestOpenAPISemanticPersonSearchReturnsOnlyDurableRootsAndScores(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	for _, document := range []*huma.OpenAPI{OpenAPIDocument(), openAPIClientDocument()} {
		path := document.Paths["/api/v1/people/search"]
		requirements.NotNil(path)
		requirements.NotNil(path.Post)
		assertions.Equal("searchPeople", path.Post.OperationID)
		requirements.Len(path.Post.Security, 1)
		_, secured := path.Post.Security[0]["apiKey"]
		assertions.True(secured)

		request := operationBodySchema(t, document, path.Post)
		assertions.Contains(request.Required, "query")
		assertions.InDelta(100, *request.Properties["limit"].Maximum, 0)
		assertions.Equal(defaultPersonSearchLimit, request.Properties["limit"].Default)

		schemas := document.Components.Schemas.Map()
		response := schemas["PersonSearchResponse"]
		requirements.NotNil(response)
		requirements.Contains(response.Properties, "results")
		assertions.False(response.Properties["results"].Nullable,
			"an empty search is an empty array, never null")
		result := schemas["PersonSearchResult"]
		requirements.NotNil(result)
		assertions.ElementsMatch([]string{"person", "score"}, result.Required)
		assertions.NotContains(result.Properties, "text")
		assertions.NotContains(result.Properties, "revision")
		assertions.Nil(schemas["PersonSemanticDocument"])
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
	require := require.New(t)
	require.NotNil(operation)
	require.NotNil(operation.RequestBody)
	schema := operation.RequestBody.Content["application/json"].Schema
	require.NotNil(schema)
	if schema.Ref == "" {
		return schema
	}
	const prefix = "#/components/schemas/"
	require.True(strings.HasPrefix(schema.Ref, prefix), schema.Ref)
	resolved := document.Components.Schemas.Map()[strings.TrimPrefix(schema.Ref, prefix)]
	require.NotNil(resolved)
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
	for _, schemaName := range []string{"FileSearchRow", "FileMetadataResponse", "PersonFileSearchRow"} {
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
	for _, schemaName := range []string{"FileSearchRow", "FileMetadataResponse", "PersonFileSearchRow"} {
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

func TestOpenAPISearchDocumentsConversationID(t *testing.T) {
	operation := OpenAPIDocument().Paths["/api/v1/search"].Get
	require.NotNil(t, operation, "search operation")

	for _, parameter := range operation.Parameters {
		if parameter.Name == "conversation_id" {
			assert.Equal(t, "query", parameter.In)
			return
		}
	}
	require.Fail(t, "conversation_id must be documented on /api/v1/search")
}

func TestOpenAPIPersonAttributeContract(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	assert.Equal("2.16.0", APISchemaVersion,
		"activity, identity match review, document search, and person files preserve the structured profile contract")

	doc := OpenAPIDocument()
	definitions := doc.Paths["/api/v1/attribute-definitions"]
	require.NotNil(definitions, "attribute definitions path")
	assert.NotNil(definitions.Get, "attribute definitions list operation")
	assert.NotNil(definitions.Post, "attribute definitions create operation")
	definition := doc.Paths["/api/v1/attribute-definitions/{id}"]
	require.NotNil(definition, "attribute definition path")
	assert.NotNil(definition.Patch, "attribute definition patch operation")
	assert.NotNil(definition.Delete, "attribute definition delete operation")
	values := doc.Paths["/api/v1/people/{id}/attributes"]
	require.NotNil(values, "person attributes path")
	assert.NotNil(values.Get, "person attributes list operation")
	value := doc.Paths["/api/v1/people/{id}/attributes/{slug}"]
	require.NotNil(value, "person attribute value path")
	assert.NotNil(value.Put, "person attribute set operation")
	assert.NotNil(value.Delete, "person attribute clear operation")

	createSchema := operationBodySchema(t, doc, definitions.Post)
	assert.Contains(createSchema.Properties, "slug")
	assert.NotContains(createSchema.Required, "slug",
		"the server generates a slug when the client omits it")
}

func TestOpenAPIParticipantInboxAndTextScopeContracts(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	doc := OpenAPIDocument()

	inboxes := doc.Paths["/api/v1/participants/{id}/inboxes"]
	requirements.NotNil(inboxes)
	requirements.NotNil(inboxes.Get)
	requirements.Len(inboxes.Get.Parameters, 1)
	assertions.Equal("id", inboxes.Get.Parameters[0].Name)
	assertions.Equal("path", inboxes.Get.Parameters[0].In)
	assertions.True(inboxes.Get.Parameters[0].Required)

	for _, path := range []string{
		"/api/v1/text/conversations",
		"/api/v1/text/conversations/{id}/messages",
	} {
		operation := doc.Paths[path].Get
		requirements.NotNil(operation, path)
		var participantIDs *huma.Param
		for _, parameter := range operation.Parameters {
			if parameter.Name == "participant_id" {
				participantIDs = parameter
				break
			}
		}
		requirements.NotNil(participantIDs, path)
		assertions.Equal("query", participantIDs.In)
		requirements.NotNil(participantIDs.Schema)
		assertions.Equal(huma.TypeArray, participantIDs.Schema.Type)
		requirements.NotNil(participantIDs.Schema.Items)
		assertions.Equal(huma.TypeInteger, participantIDs.Schema.Items.Type)
		assertions.Equal("int64", participantIDs.Schema.Items.Format)
	}
}

func TestOpenAPIPersonProfilePatchUsesWritableEnvelopeShape(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	doc := OpenAPIDocument()

	path := doc.Paths["/api/v1/people/{id}/profile"]
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

func TestOpenAPIOrganizationProfilePutDocumentsLimits(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	assertions.Equal("2.16.0", APISchemaVersion,
		"organization profile write limits advance the published contract")
	doc := OpenAPIDocument()
	path := doc.Paths["/api/v1/organizations/{id}/profile"]
	requirements.NotNil(path)
	requirements.NotNil(path.Put)
	assertions.Contains(path.Put.Description, "200")

	response := path.Put.Responses[httpStatusKey(http.StatusRequestEntityTooLarge)]
	requirements.NotNil(response)
	media := response.Content["application/json"]
	requirements.NotNil(media)
	requirements.NotNil(media.Schema)
	assertions.Equal("#/components/schemas/ErrorResponse", media.Schema.Ref)
}

func TestOpenAPIPersonProfileMediaContentContract(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	assert.Equal("2.16.0", APISchemaVersion,
		"activity, identity match review, document search, and person files preserve the raw profile media contract")
	doc := OpenAPIDocument()
	path := doc.Paths["/api/v1/people/{id}/profile/media/{media_id}/content"]
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

	assertions.Equal("2.16.0", APISchemaVersion,
		"document and person-file search preserve the identity match review contract")

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
	// routes added in 1.42.0, cache-readiness responses added in 1.43.0,
	// document search added in 1.44.0, participant/people separation added in
	// 2.0.0, tracking added in 2.1.0, and participant-scoped files added in
	// 2.5.0. Person search in 2.6.0, structured filters in 2.7.0, CardDAV routes
	// in 2.8.0, person merge/split operations in 2.9.0, and relationship
	// calendars in 2.10.0, person fact diagnostics in 2.11.0, lexical deletion
	// scope in 2.12.0, Directory people and deduplicate planning in 2.13.0,
	// CardDAV status and run history plus List-ID filtering in 2.14.0, Gmail
	// repair in 2.15.0, and historical import jobs in 2.16.0 did not touch it.
	assert.Equal("2.16.0", APISchemaVersion, "meeting import is an additive schema release")

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
		[]any{"source", "participant", "domain", "mailing_list", "message_type", "after", "before", "deletion", "identity"},
		filter.Properties["dimension"].Enum,
	)
	clientFilter := openAPIClientDocument().Components.Schemas.Map()["ExploreFilter"]
	requirements.NotNil(clientFilter)
	requirements.NotNil(clientFilter.Properties["dimension"])
	assertions.Equal([]any{
		"ExploreFilterDimensionSource", "ExploreFilterDimensionParticipant", "ExploreFilterDimensionDomain", "ExploreFilterDimensionMessageType",
		"ExploreFilterDimensionMailingList", "ExploreFilterDimensionAfter", "ExploreFilterDimensionBefore",
		"ExploreFilterDimensionDeletion", "ExploreFilterDimensionIdentity",
	}, clientFilter.Properties["dimension"].Extensions["x-enum-names"])
	for schemaName, properties := range map[string][]string{
		"DirectoryPeopleResponse":    {"people"},
		"DirectoryPersonSummary":     {"categories", "organizations"},
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
			assertions.Contains(schema.Required, property, "%s.%s must be required", schemaName, property)
			assertions.False(schema.Properties[property].Nullable, "%s.%s must not be nullable", schemaName, property)
		}
	}
}

func TestOpenAPIDirectoryLastContactParametersAreTyped(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	doc := OpenAPIDocument()
	operation := doc.Paths["/api/v1/people/directory"].Get
	require.NotNil(operation)
	parameters := make(map[string]*huma.Param, len(operation.Parameters))
	for _, parameter := range operation.Parameters {
		parameters[parameter.Name] = parameter
	}
	require.Contains(parameters, "last_contact_after")
	require.Contains(parameters, "last_contact_before")
	require.Contains(parameters, "sort")
	assert.Equal("date-time", parameters["last_contact_after"].Schema.Format)
	assert.Equal("date-time", parameters["last_contact_before"].Schema.Format)
	assert.ElementsMatch([]any{"name", "last_contact_desc", "last_contact_asc"}, parameters["sort"].Schema.Enum)
}

func TestOpenAPICardDAVConflictArraysAreRequiredAndNonNull(t *testing.T) {
	tests := []struct {
		schema   string
		property string
	}{
		{schema: "CardDAVContactSummaryResponse", property: "emails"},
		{schema: "CardDAVContactSummaryResponse", property: "phones"},
		{schema: "CardDAVConflictResponse", property: "allowed_resolutions"},
		{schema: "CardDAVConflictDetailResponse", property: "allowed_resolutions"},
		{schema: "CardDAVConflictsResponse", property: "conflicts"},
	}
	for documentName, document := range map[string]*huma.OpenAPI{
		"server": OpenAPIDocument(),
		"client": openAPIClientDocument(),
	} {
		t.Run(documentName, func(t *testing.T) {
			for _, tt := range tests {
				t.Run(tt.schema+"/"+tt.property, func(t *testing.T) {
					require := require.New(t)
					assert := assert.New(t)
					schema := document.Components.Schemas.Map()[tt.schema]
					require.NotNil(schema)
					property := schema.Properties[tt.property]
					require.NotNil(property)
					assert.Contains(schema.Required, tt.property)
					assert.False(property.Nullable)
				})
			}
		})
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

func TestOpenAPIClientAppendNoteSourceEnumNamesAvoidExistingConstants(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	schema := openAPIClientDocument().Components.Schemas.Map()["AppendPersonNoteRequest"]
	requirements.NotNil(schema)
	requirements.NotNil(schema.Properties["source"])
	assertions.Equal([]any{
		"AppendPersonNoteRequestSourceUser",
		"AppendPersonNoteRequestSourceCarddavImport",
		"AppendPersonNoteRequestSourceVcardImport",
		"AppendPersonNoteRequestSourceArchiveObservation",
		"AppendPersonNoteRequestSourceExtraction",
		"AppendPersonNoteRequestSourceEnrichment",
		"AppendPersonNoteRequestSourceSystem",
	}, schema.Properties["source"].Extensions["x-enum-names"])
}

func TestOpenAPIClientCardDAVEnumsDoNotRenameExistingConstants(t *testing.T) {
	schemas := openAPIClientDocument().Components.Schemas.Map()
	tests := []struct {
		schema, property string
		want             []any
	}{
		{schema: "CardDAVPublicationResponse", property: "state", want: []any{
			"CardDAVPublicationResponseStateUnpublished", "CardDAVPublicationResponseStatePublished",
			"CardDAVPublicationResponseStatePending", "CardDAVPublicationResponseStateConflict",
		}},
		{schema: "CardDAVPublicationResponse", property: "pending_operation", want: []any{
			"CardDAVPublicationResponsePendingOperationCreate", "CardDAVPublicationResponsePendingOperationUpdate",
			"CardDAVPublicationResponsePendingOperationDelete",
		}},
		{schema: "CardDAVContactSummaryResponse", property: "state", want: []any{
			"CardDAVContactSummaryResponseStatePresent", "CardDAVContactSummaryResponseStateDeleted",
			"CardDAVContactSummaryResponseStateUnavailable",
		}},
	}
	for _, tt := range tests {
		schema := schemas[tt.schema]
		require.NotNil(t, schema, tt.schema)
		property := schema.Properties[tt.property]
		require.NotNil(t, property, tt.schema+"."+tt.property)
		assert.Equal(t, tt.want, property.Extensions["x-enum-names"], tt.schema+"."+tt.property)
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
		for _, path := range []string{"/api/v1/participants/{id}", "/api/v1/domains/{domain}"} {
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
		"get participant": reflect.TypeFor[generated.GetParticipantResp](),
		"get domain":      reflect.TypeFor[generated.GetDomainResp](),
	} {
		_, ok := responseType.FieldByName("JSON503")
		assert.True(t, ok, "%s response must expose JSON503", name)
	}
}

func TestGeneratedPersonTrackingPreservesRequiredNullTimestamp(t *testing.T) {
	state := generated.PersonTracking{
		PersonID:  7,
		Tracked:   false,
		TrackedAt: nil,
	}

	require.NoError(t, state.Validate())
	encoded, err := json.Marshal(state)
	require.NoError(t, err)
	assert.JSONEq(t, `{"person_id":7,"tracked":false,"tracked_at":null}`, string(encoded))
}

func TestOpenAPIExplorationFiniteRequiredFieldsAreNonNull(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	doc := OpenAPIDocument()
	schemas := doc.Components.Schemas.Map()
	dimension := schemas["ExploreGroupDimension"]
	requirements.NotNil(dimension)
	assertions.ElementsMatch([]any{"source", "participant", "domain", "message_type", "mailing_list", "kind", "year", "month"}, dimension.Enum)

	filter := schemas["ExploreFilter"]
	requirements.NotNil(filter)
	assertions.Contains(filter.Properties["dimension"].Enum, any("mailing_list"))

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

func TestOpenAPIPersonMergeSnapshotUsesLosslessGoType(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	snapshot := openAPIClientDocument().Components.Schemas.Map()["PersonMergeSnapshotResponse"]
	require.NotNil(snapshot)
	property := snapshot.Properties["snapshot"]
	require.NotNil(property)
	assert.Equal("json.RawMessage", property.Extensions["x-go-type"])
	assert.Equal(map[string]any{"path": "encoding/json"},
		property.Extensions["x-go-type-import"])
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

func TestOpenAPIDirectoryArraysAreRequiredAndNonNullInRenderedDocuments(t *testing.T) {
	for _, version := range []string{"3.1", "3.0"} {
		t.Run(version, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			raw, err := OpenAPIJSONVersion(version)
			require.NoError(err)
			var document struct {
				Components struct {
					Schemas map[string]struct {
						Required   []string `json:"required"`
						Properties map[string]struct {
							Type     any  `json:"type"`
							Nullable bool `json:"nullable"`
						} `json:"properties"`
					} `json:"schemas"`
				} `json:"components"`
			}
			require.NoError(json.Unmarshal(raw, &document))
			for schemaName, properties := range map[string][]string{
				"DirectoryPeopleResponse": {"people"},
				"DirectoryPersonSummary":  {"categories", "organizations"},
			} {
				schema, ok := document.Components.Schemas[schemaName]
				require.True(ok, schemaName)
				for _, propertyName := range properties {
					property, ok := schema.Properties[propertyName]
					require.True(ok, "%s.%s", schemaName, propertyName)
					assert.Contains(schema.Required, propertyName)
					assert.Equal("array", property.Type, "%s.%s", schemaName, propertyName)
					assert.False(property.Nullable, "%s.%s", schemaName, propertyName)
				}
			}
		})
	}
}

func TestCardDAVOpenAPIDocumentsPositiveIDsAndOperationalErrors(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	doc := OpenAPIDocument()
	for _, tc := range []struct {
		path, method, parameter string
		statuses                []string
	}{
		{path: "/api/v1/carddav/books/{id}", method: http.MethodPatch, parameter: "id", statuses: []string{"400", "404", "409", "500", "503"}},
		{path: "/api/v1/carddav/conflicts/{id}", method: http.MethodGet, parameter: "id", statuses: []string{"400", "404", "500", "503"}},
		{path: "/api/v1/carddav/conflicts/{id}/resolve", method: http.MethodPost, parameter: "id", statuses: []string{"400", "404", "409", "500", "502", "503"}},
		{path: "/api/v1/carddav/publications/{person_id}", method: http.MethodGet, parameter: "person_id", statuses: []string{"400", "404", "500", "503"}},
	} {
		operation := pathOperation(doc.Paths[tc.path], tc.method)
		require.NotNil(operation, tc.path)
		require.NotEmpty(operation.Parameters, tc.path)
		parameter := operation.Parameters[0]
		assert.Equal(tc.parameter, parameter.Name)
		require.NotNil(parameter.Schema.Minimum, tc.path)
		assert.InDelta(float64(1), *parameter.Schema.Minimum, 0, tc.path)
		for _, status := range tc.statuses {
			assert.Contains(operation.Responses, status, "%s %s", tc.method, tc.path)
		}
	}

	for _, tc := range []struct {
		path, method string
		statuses     []string
	}{
		{path: "/api/v1/carddav/account/test", method: http.MethodPost, statuses: []string{"400", "500", "502", "503"}},
		{path: "/api/v1/carddav/account", method: http.MethodPut, statuses: []string{"400", "500", "502", "503"}},
		{path: "/api/v1/carddav/sync", method: http.MethodPost, statuses: []string{"400", "409", "500", "502", "503"}},
	} {
		operation := pathOperation(doc.Paths[tc.path], tc.method)
		require.NotNil(operation, tc.path)
		for _, status := range tc.statuses {
			assert.Contains(operation.Responses, status, "%s %s", tc.method, tc.path)
		}
	}
}

func TestCardDAVStatusAndRunHistoryOpenAPIContract(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	doc := OpenAPIDocument()
	status := doc.Paths["/api/v1/carddav/status"]
	require.NotNil(status)
	require.NotNil(status.Get)
	assert.Equal("getCardDAVStatus", status.Get.OperationID)
	runs := doc.Paths["/api/v1/carddav/runs"]
	require.NotNil(runs)
	require.NotNil(runs.Get)
	assert.Equal("listCardDAVRuns", runs.Get.OperationID)
	require.Len(runs.Get.Parameters, 2)
	assert.Equal("limit", runs.Get.Parameters[0].Name)
	require.NotNil(runs.Get.Parameters[0].Schema.Minimum)
	require.NotNil(runs.Get.Parameters[0].Schema.Maximum)
	assert.InDelta(1, *runs.Get.Parameters[0].Schema.Minimum, 0)
	assert.InDelta(100, *runs.Get.Parameters[0].Schema.Maximum, 0)
	assert.Equal("before_id", runs.Get.Parameters[1].Name)
	require.NotNil(runs.Get.Parameters[1].Schema.Minimum)
	assert.InDelta(1, *runs.Get.Parameters[1].Schema.Minimum, 0)

	run := doc.Components.Schemas.Map()["CardDAVRunResponse"]
	require.NotNil(run)
	assert.Equal([]any{"manual", "scheduled"}, run.Properties["trigger"].Enum)
	assert.Equal([]any{"running", "succeeded", "failed", "cancelled", "partial"}, run.Properties["state"].Enum)
	assert.Equal([]any{"cancelled", "retry_after", "authentication_failed", "upstream_failed", "safety_limit", "sync_failed", "unsafe_error_redacted", "daemon_restarted"}, run.Properties["error_code"].Enum)
	page := doc.Components.Schemas.Map()["CardDAVRunsResponse"]
	require.NotNil(page)
	assert.Contains(page.Required, "runs")
	assert.False(page.Properties["runs"].Nullable)
	statusSchema := doc.Components.Schemas.Map()["CardDAVStatusResponse"]
	require.NotNil(statusSchema)
	assert.NotContains(statusSchema.Required, "repair_reason")
	assert.NotContains(statusSchema.Required, "next_scheduled_at")
	assert.NotContains(statusSchema.Required, "active")
	assert.Equal([]any{"account_missing", "credential_missing", "credential_mismatch", "credential_unavailable", "runtime_unavailable"}, statusSchema.Properties["repair_reason"].Enum)
}

func TestOpenAPIOperationRoutesParametersAndFailures(t *testing.T) {
	for documentName, document := range map[string]*huma.OpenAPI{
		"server": OpenAPIDocument(),
		"client": openAPIClientDocument(),
	} {
		t.Run(documentName, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			list := document.Paths["/api/v1/operations/runs"]
			require.NotNil(list)
			require.NotNil(list.Get)
			assert.Equal("listOperationRuns", list.Get.OperationID)
			for _, status := range []string{"200", "400", "500", "503", "default"} {
				assert.Contains(list.Get.Responses, status)
			}
			require.Len(list.Get.Parameters, 5)
			parameters := make(map[string]*huma.Param, len(list.Get.Parameters))
			for _, parameter := range list.Get.Parameters {
				parameters[parameter.Name] = parameter
			}
			assert.ElementsMatch(operationKindValues(), anyToStrings(t, parameters["kind"].Schema.Enum))
			assert.ElementsMatch(operationLaneValues(), anyToStrings(t, parameters["lane"].Schema.Enum))
			assert.ElementsMatch(operationStateValues(), anyToStrings(t, parameters["state"].Schema.Enum))
			require.NotNil(parameters["limit"].Schema.Minimum)
			require.NotNil(parameters["limit"].Schema.Maximum)
			assert.InDelta(1, *parameters["limit"].Schema.Minimum, 0)
			assert.InDelta(100, *parameters["limit"].Schema.Maximum, 0)
			assert.Contains(parameters["limit"].Description, "default 25")
			assert.Contains(parameters["cursor"].Description, "Opaque")
			assert.Contains(parameters["cursor"].Description, "archive")
			assert.Contains(parameters["cursor"].Description, "exact kind, lane, and state filters")

			detail := document.Paths["/api/v1/operations/runs/{id}"]
			require.NotNil(detail)
			require.NotNil(detail.Get)
			assert.Equal("getOperationRun", detail.Get.OperationID)
			for _, status := range []string{"200", "400", "404", "500", "503", "default"} {
				assert.Contains(detail.Get.Responses, status)
			}
			require.Len(detail.Get.Parameters, 1)
			assert.Equal("id", detail.Get.Parameters[0].Name)
			assert.Contains(detail.Get.Parameters[0].Description, "Opaque")
			assert.Contains(detail.Get.Parameters[0].Description, "archive-bound")

			status := document.Paths["/api/v1/operations/status"]
			require.NotNil(status)
			require.NotNil(status.Get)
			assert.Equal("getOperationStatus", status.Get.OperationID)
			assert.Contains(status.Get.Responses, "200")
			assert.Contains(status.Get.Responses, "default")
		})
	}
}

func TestOpenAPIOperationEnumsAndNonNullCollections(t *testing.T) {
	enums := map[string]map[string][]string{
		"OperationPublicCounter": {
			"name": {"processed", "added", "updated", "item_errors", "attempted", "succeeded", "failed", "projected_writes", "books", "created", "removed"},
			"unit": {"messages", "people", "writes", "books", "contacts"},
		},
		"OperationPublicError": {
			"code": {
				"source_sync_failed", "person_sweep_failed", "policy", "budget", "lease_lost",
				"rate_limited", "timeout", "provider_http", "invalid_output", "archive_gap",
				"internal", "cancelled", "retry_after", "authentication_failed", "upstream_failed",
				"safety_limit", "sync_failed", "unsafe_error_redacted", "daemon_restarted", "carddav_sync_failed",
			},
		},
		"OperationRunSummary": {
			"kind":    operationKindValues(),
			"lane":    operationLaneValues(),
			"state":   operationStateValues(),
			"trigger": {"manual", "scheduled"},
		},
		"OperationUnavailableKind": {
			"kind": operationKindValues(),
			"lane": operationLaneValues(),
		},
		"OperationLaneStatus": {
			"kind":                 operationKindValues(),
			"lane":                 operationLaneValues(),
			"history_availability": {"available", "unavailable"},
			"related_status": {
				"listSourceStatus", "getDocumentIndexStatus", "getDocumentVectorStatus",
				"getVisualAttachmentStatus", "getCardDAVStatus",
			},
			"supported_actions": {"carddav_sync", "visual_build", "visual_resume"},
		},
	}
	collections := map[string][]string{
		"OperationRunSummary":     {"counters"},
		"OperationRunDetail":      {"counters"},
		"OperationRunsResponse":   {"runs", "unavailable_kinds"},
		"OperationLaneStatus":     {"supported_actions"},
		"OperationStatusResponse": {"lanes"},
	}
	for documentName, document := range map[string]*huma.OpenAPI{
		"server": OpenAPIDocument(),
		"client": openAPIClientDocument(),
	} {
		t.Run(documentName, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			schemas := document.Components.Schemas.Map()
			for schemaName, properties := range enums {
				schema := schemas[schemaName]
				require.NotNil(schema, schemaName)
				for propertyName, want := range properties {
					property := schema.Properties[propertyName]
					require.NotNil(property, schemaName+"."+propertyName)
					enumSchema := property
					if len(enumSchema.Enum) == 0 && property.Items != nil {
						enumSchema = property.Items
					}
					assert.ElementsMatch(want, anyToStrings(t, enumSchema.Enum), schemaName+"."+propertyName)
				}
			}
			for schemaName, properties := range collections {
				schema := schemas[schemaName]
				require.NotNil(schema, schemaName)
				for _, propertyName := range properties {
					property := schema.Properties[propertyName]
					require.NotNil(property, schemaName+"."+propertyName)
					assert.Contains(schema.Required, propertyName, schemaName+"."+propertyName)
					assert.False(property.Nullable, schemaName+"."+propertyName)
				}
			}
		})
	}
}

func TestOpenAPIOperationServerAndClientSchemasMatch(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	server := OpenAPIDocument().Components.Schemas.Map()
	client := openAPIClientDocument().Components.Schemas.Map()
	wantProperties := map[string][]string{
		"OperationPublicCounter":   {"name", "unit", "value"},
		"OperationPublicError":     {"code", "message"},
		"OperationRunSummary":      {"id", "kind", "lane", "state", "trigger", "started_at", "finished_at", "counters", "error"},
		"OperationRunDetail":       {"id", "kind", "lane", "state", "trigger", "started_at", "finished_at", "counters", "error"},
		"OperationUnavailableKind": {"kind", "lane", "unavailable_code"},
		"OperationRunsResponse":    {"runs", "next_cursor", "unavailable_kinds"},
		"OperationLaneStatus": {
			"kind", "lane", "configured", "history_availability", "unavailable_code",
			"active", "latest", "latest_successful", "related_status", "supported_actions",
		},
		"OperationStatusResponse": {"lanes"},
	}
	for schemaName, want := range wantProperties {
		require.NotNil(server[schemaName], schemaName)
		require.NotNil(client[schemaName], schemaName)
		assert.ElementsMatch(want, operationSchemaPropertyNames(server[schemaName].Properties), "server "+schemaName)
		assert.ElementsMatch(want, operationSchemaPropertyNames(client[schemaName].Properties), "client "+schemaName)
		assert.ElementsMatch(server[schemaName].Required, client[schemaName].Required, schemaName)
	}
}

func anyToStrings(t *testing.T, values []any) []string {
	t.Helper()
	result := make([]string, 0, len(values))
	for _, value := range values {
		text, ok := value.(string)
		require.True(t, ok, "OpenAPI enum value must be a string")
		result = append(result, text)
	}
	return result
}

func operationSchemaPropertyNames(values map[string]*huma.Schema) []string {
	result := make([]string, 0, len(values))
	for key := range values {
		result = append(result, key)
	}
	return result
}

func TestCardDAVServiceUnavailableResponsesDocumentRetryAfter(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	routes := []struct{ path, method string }{
		{path: "/api/v1/carddav/account/test", method: http.MethodPost},
		{path: "/api/v1/carddav/account", method: http.MethodPut},
		{path: "/api/v1/carddav/books", method: http.MethodGet},
		{path: "/api/v1/carddav/books/{id}", method: http.MethodPatch},
		{path: "/api/v1/carddav/publications/{person_id}", method: http.MethodGet},
		{path: "/api/v1/carddav/publications/{person_id}", method: http.MethodPost},
		{path: "/api/v1/carddav/publications/{person_id}", method: http.MethodDelete},
		{path: "/api/v1/carddav/conflicts", method: http.MethodGet},
		{path: "/api/v1/carddav/conflicts/{id}", method: http.MethodGet},
		{path: "/api/v1/carddav/conflicts/{id}/resolve", method: http.MethodPost},
		{path: "/api/v1/carddav/sync", method: http.MethodPost},
	}
	for _, document := range []*huma.OpenAPI{OpenAPIDocument(), openAPIClientDocument()} {
		for _, route := range routes {
			operation := pathOperation(document.Paths[route.path], route.method)
			require.NotNil(operation, "%s %s", route.method, route.path)
			response := operation.Responses[httpStatusKey(http.StatusServiceUnavailable)]
			require.NotNil(response, "%s %s", route.method, route.path)
			header := response.Headers["Retry-After"]
			require.NotNil(header, "%s %s", route.method, route.path)
			require.NotNil(header.Schema, "%s %s", route.method, route.path)
			assert.Equal(huma.TypeInteger, header.Schema.Type, "%s %s", route.method, route.path)
			assert.Equal(formatInt64, header.Schema.Format, "%s %s", route.method, route.path)
		}
	}

	syncResponse := generated.SyncCardDAVResp{
		Headers503: &generated.SyncCardDAVResp503Headers{RetryAfter: "17"},
	}
	publishResponse := generated.PublishCardDAVPersonResp{
		Headers503: &generated.PublishCardDAVPersonResp503Headers{RetryAfter: "23"},
	}
	require.NotNil(syncResponse.Headers503)
	require.NotNil(publishResponse.Headers503)
	assert.Equal("17", syncResponse.Headers503.RetryAfter)
	assert.Equal("23", publishResponse.Headers503.RetryAfter)
}

func pathOperation(item *huma.PathItem, method string) *huma.Operation {
	if item == nil {
		return nil
	}
	switch method {
	case http.MethodGet:
		return item.Get
	case http.MethodPost:
		return item.Post
	case http.MethodPut:
		return item.Put
	case http.MethodPatch:
		return item.Patch
	case http.MethodDelete:
		return item.Delete
	default:
		return nil
	}
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
