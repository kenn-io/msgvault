package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/explorecatalog"
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
		[]any{"source", "participant", "domain", "message_type", "after", "before", "deletion"},
		filter.Properties["dimension"].Enum,
	)
	for schemaName, properties := range map[string][]string{
		"ExploreFilter":              {"values"},
		"ExploreHTTPResponse":        {"rows"},
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
	assertions.ElementsMatch([]any{"absent", "interrupted", "stale_schema", "drifted"}, readiness.Enum)
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
