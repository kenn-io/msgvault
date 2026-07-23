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

func TestOpenAPIMeetingImportContract(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	assert.Equal("1.6.0", APISchemaVersion, "meeting import is an additive schema release")

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

	for _, name := range []string{"Source", "Meeting", "Person", "TranscriptSegment"} {
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
	metadata := schemas["Meeting"].Properties["metadata"]
	require.NotNil(metadata, "metadata schema")
	_, extensible := metadata.AdditionalProperties.(*huma.Schema)
	assert.True(extensible, "metadata accepts provider-specific values")

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
