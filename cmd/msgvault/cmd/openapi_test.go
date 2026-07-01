package cmd

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
)

func TestRenderOpenAPIJSONUsesAPISchemaVersion(t *testing.T) {
	out, err := renderOpenAPI("3.1", "json")
	require.NoError(t, err, "render openapi")

	var doc struct {
		OpenAPI string `json:"openapi"`
		Info    struct {
			Version string `json:"version"`
		} `json:"info"`
	}
	require.NoError(t, json.Unmarshal(out, &doc), "decode openapi")
	assert.Equal(t, "3.1.0", doc.OpenAPI)
	assert.Equal(t, api.APISchemaVersion, doc.Info.Version)
}

func TestOpenAPICommandWritesOnlyStdout(t *testing.T) {
	cmd := newOpenAPICmd()
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	cmd.SetArgs([]string{"--format", "json"})

	require.NoError(t, cmd.Execute(), "execute openapi command")
	assert.Contains(t, stdout.String(), `"openapi": "3.1.0"`)
	assert.Contains(t, stdout.String(), `"version": "`+api.APISchemaVersion+`"`)
	assert.Empty(t, stderr.String())
}

func TestOpenAPIDeleteDedupedExecuteCountsAreNotNullable(t *testing.T) {
	out, err := renderOpenAPI("3.1", "json")
	require.NoError(t, err, "render openapi")

	var doc map[string]any
	require.NoError(t, json.Unmarshal(out, &doc), "decode openapi")

	components, ok := doc["components"].(map[string]any)
	require.True(t, ok, "components object")
	schemas, ok := components["schemas"].(map[string]any)
	require.True(t, ok, "schemas object")
	schema, ok := schemas["CliDeleteDedupedExecuteRequest"].(map[string]any)
	require.True(t, ok, "execute request schema")
	properties, ok := schema["properties"].(map[string]any)
	require.True(t, ok, "execute request properties")

	assertOpenAPIPropertyRejectsNull(t, properties, "expected_total")
	assertOpenAPIPropertyRejectsNull(t, properties, "expected_batch_count")
}

func assertOpenAPIPropertyRejectsNull(t *testing.T, properties map[string]any, name string) {
	t.Helper()

	property, ok := properties[name].(map[string]any)
	require.True(t, ok, "%s property", name)
	assert.NotEqual(t, true, property["nullable"], "%s nullable", name)
	if types, ok := property["type"].([]any); ok {
		assert.NotContains(t, types, "null", "%s type", name)
	}
}
