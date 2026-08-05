package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/pkg/client/generated"
)

const testPersonAttributesJSON = `{"person_id":7,"attributes":[{
	"definition":{
		"id":1,
		"universal_id":"59e9a7d3-4904-4d0e-97d1-d0680e1e9e55",
		"object_type":"person",
		"slug":"primary_channel",
		"label":"Primary channel",
		"value_type":"text",
		"field_type":"select",
		"cardinality":"single",
		"display_order":10,
		"is_required":false,
		"ownership":"system",
		"ui_creatable":true,
		"ui_editable":true,
		"api_mutable":true,
		"is_searchable":false,
		"is_audited":true,
		"is_deletable":false,
		"history_exempt":false,
		"is_active":true,
		"revision":1,
		"created_at":"2026-07-30T12:00:00Z",
		"updated_at":"2026-07-30T12:00:00Z"
	},
	"current":[]
}]}`

func TestApplyCLIScalarAttributeValueCoercesTypedScalars(t *testing.T) {
	cmd := &cobra.Command{Use: "test"}
	tests := []struct {
		valueType string
		raw       string
		assert    func(*testing.T, generated.AttributeValue)
	}{
		{"text", " sailing ", func(t *testing.T, value generated.AttributeValue) {
			t.Helper()
			require.NotNil(t, value.Text)
			assert.Equal(t, "sailing", *value.Text)
		}},
		{"integer", "30", func(t *testing.T, value generated.AttributeValue) {
			t.Helper()
			require.NotNil(t, value.Integer)
			assert.Equal(t, int64(30), *value.Integer)
		}},
		{"boolean", "true", func(t *testing.T, value generated.AttributeValue) {
			t.Helper()
			require.NotNil(t, value.Boolean)
			assert.True(t, *value.Boolean)
		}},
	}
	for _, test := range tests {
		t.Run(test.valueType, func(t *testing.T) {
			var value generated.AttributeValue
			require.NoError(t, applyCLIScalarAttributeValue(
				cmd, &value, test.valueType, test.raw))
			test.assert(t, value)
		})
	}
}

func TestApplyCLIScalarAttributeValueRequiresJSONForStructuredTypes(t *testing.T) {
	cmd := &cobra.Command{Use: "test"}
	var value generated.AttributeValue
	err := applyCLIScalarAttributeValue(cmd, &value, "json", `{"kind":"synthetic"}`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--value-json")
}

func TestPersonAttributesSetCoercesScalarAndForwardsMetadata(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var query string
	var received map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_, err := w.Write([]byte(testPersonAttributesJSON))
			assert.NoError(err)
		case http.MethodPut:
			assert.Equal("/api/v1/persons/7/attributes/primary_channel", r.URL.Path)
			query = r.URL.RawQuery
			assert.NoError(json.NewDecoder(r.Body).Decode(&received))
			_, err := w.Write([]byte(`{"dry_run":true,"value":{
				"id":12,"person_id":7,"definition_id":1,
				"definition_slug":"primary_channel","ordinal":0,
				"value":{"type":"text","text":"chat"},
				"active_from":"2026-07-30T13:00:00Z",
				"created_at":"2026-07-30T13:00:00Z",
				"source":"extraction","confidence":0.62}}`))
			assert.NoError(err)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{
		Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})

	output, err := runAttributeCommand(t, personAttributesSetCmd,
		"7", "primary_channel", "--value", "chat",
		"--source", "extraction", "--source-ref", "message:1234",
		"--confidence", "0.62", "--actor", "extractor",
		"--expected-value-id", "11", "--dry-run")
	require.NoError(err)
	assert.Contains(query, "dry_run=true")
	value, ok := received["value"].(map[string]any)
	require.True(ok)
	assert.Equal("text", value["type"])
	assert.Equal("chat", value["text"])
	assert.Equal("extraction", received["source"])
	assert.Equal("message:1234", received["source_ref"])
	assert.InDelta(0.62, received["confidence"], 0.0001)
	assert.Equal("extractor", received["actor"])
	assert.InDelta(11, received["expected_value_id"], 0)
	assert.Contains(output, "Dry run")
}

func TestPersonAttributesSetRejectsExplicitNonPositiveExpectedValueID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{
		Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})

	for _, expectedID := range []string{"0", "-1"} {
		t.Run(expectedID, func(t *testing.T) {
			_, err := runAttributeCommand(t, personAttributesSetCmd,
				"7", "primary_channel", "--value", "chat",
				"--expected-value-id", expectedID)
			require.Error(err)
			assert.ErrorContains(err, "--expected-value-id must be a positive integer")
		})
	}
	assert.Zero(requests, "invalid compare-and-swap IDs must be rejected before any API request")
}

func TestPersonAttributesClearForwardsOrdinalAndExpectedValueID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var query string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodDelete, r.Method)
		assert.Equal("/api/v1/persons/7/attributes/ask_me_about", r.URL.Path)
		query = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"dry_run":false,"superseded":{
			"id":11,"person_id":7,"definition_id":3,
			"definition_slug":"ask_me_about","ordinal":1,
			"value":{"type":"text","text":"sailing"},
			"active_from":"2026-07-30T12:00:00Z",
			"active_until":"2026-07-30T13:00:00Z",
			"created_at":"2026-07-30T12:00:00Z",
			"superseded_at":"2026-07-30T13:00:00Z",
			"source":"user"}}`))
		assert.NoError(err)
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{
		Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})

	output, err := runAttributeCommand(t, personAttributesClearCmd,
		"7", "ask_me_about", "--ordinal", "1", "--expected-value-id", "11")
	require.NoError(err)
	assert.Contains(query, "ordinal=1")
	assert.Contains(query, "expected_value_id=11")
	assert.NotContains(query, "dry_run",
		"a prior test's --dry-run must not leak into this run")
	assert.Contains(output, "Superseded")
}
