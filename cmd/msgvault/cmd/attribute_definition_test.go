package cmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

const testAttributeDefinitionJSON = `{
	"id":3,
	"universal_id":"93c658a1-2346-4a6e-98c2-abfa29209334",
	"object_type":"person",
	"slug":"ask_me_about",
	"label":"Ask me about",
	"value_type":"text",
	"field_type":"text",
	"cardinality":"multi",
	"display_order":30,
	"is_required":false,
	"ownership":"system",
	"ui_creatable":true,
	"ui_editable":true,
	"api_mutable":true,
	"is_searchable":true,
	"is_sensitive":true,
	"is_audited":true,
	"is_deletable":false,
	"history_exempt":false,
	"is_active":true,
	"revision":7,
	"created_at":"2026-07-30T12:00:00Z",
	"updated_at":"2026-07-30T12:00:00Z"
}`

func runAttributeCommand(
	t *testing.T, template *cobra.Command, args ...string,
) (string, error) {
	t.Helper()
	var output bytes.Buffer
	command := &cobra.Command{Use: template.Use, Args: template.Args, RunE: template.RunE}
	command.Flags().AddFlagSet(template.Flags())
	command.Flags().VisitAll(func(flag *pflag.Flag) {
		// Reset the bound package variable too: a previous test's flag value
		// (e.g. --dry-run) must not leak into this run's request.
		require.NoError(t, flag.Value.Set(flag.DefValue))
		flag.Changed = false
	})
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs(args)
	err := command.Execute()
	return output.String(), err
}

func TestAttributeDefinitionListPrintsRegistryAndForwardsFilter(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var query string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodGet, r.Method)
		assert.Equal("/api/v1/attribute-definitions", r.URL.Path)
		query = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"definitions":[` + testAttributeDefinitionJSON + `]}`))
		assert.NoError(err)
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{
		Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})

	output, err := runAttributeCommand(t, attributeDefinitionListCmd,
		"--object-type", "person")
	require.NoError(err)
	assert.Contains(query, "object_type=person")
	assert.Contains(output, "ask_me_about")
	assert.Contains(output, "93c658a1-2346-4a6e-98c2-abfa29209334")
	assert.Contains(output, "SENSITIVE")
}

func TestAttributeDefinitionCreateDryRunValidatesLocally(t *testing.T) {
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

	output, err := runAttributeCommand(t, attributeDefinitionCreateCmd,
		"--definition", `{"object_type":"person","slug":"scratch_note",
			"label":"Scratch note","value_type":"text","field_type":"text",
			"is_sensitive":true}`,
		"--dry-run")
	require.NoError(err)
	assert.Zero(requests)
	assert.Contains(output, "Would create")
	assert.Contains(output, "scratch_note")
	assert.Contains(output, `"is_sensitive": true`)
}

func TestAttributeDefinitionCreateDryRunAllowsOmittedSlug(t *testing.T) {
	output, err := runAttributeCommand(t, attributeDefinitionCreateCmd,
		"--definition", `{"object_type":"person","label":"Favorite color",
			"value_type":"text","field_type":"text","cardinality":"single"}`,
		"--dry-run")
	require.NoError(t, err)
	assert.Contains(t, output, "Would create attribute definition")
	assert.Contains(t, output, "Favorite color")
}

func TestAttributeDefinitionCreateDryRunAppliesServerValidationLocally(t *testing.T) {
	tests := []struct {
		name     string
		document string
		wantErr  string
	}{
		{
			name: "invalid slug",
			document: `{"object_type":"person","slug":"Bad Slug","label":"Bad",
				"value_type":"text","field_type":"text"}`,
			wantErr: "slug",
		},
		{
			name: "select without choices",
			document: `{"object_type":"person","slug":"favorite_tea","label":"Favorite tea",
				"value_type":"text","field_type":"select"}`,
			wantErr: "choices",
		},
		{
			name: "multiselect on single cardinality",
			document: `{"object_type":"person","slug":"tags","label":"Tags",
				"value_type":"text","field_type":"multiselect","cardinality":"single",
				"options":{"choices":[{"value":"a","label":"A"}]}}`,
			wantErr: "multi",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := runAttributeCommand(t, attributeDefinitionCreateCmd,
				"--definition", test.document, "--dry-run")
			require.Error(t, err, "dry run must reject what the server would reject")
			assert.Contains(t, err.Error(), test.wantErr)
		})
	}
}

func TestAttributeDefinitionCreateRejectsUnsupportedUniqueness(t *testing.T) {
	_, err := runAttributeCommand(t, attributeDefinitionCreateCmd,
		"--definition", `{"object_type":"person","slug":"employee_number",
			"label":"Employee number","value_type":"text","field_type":"text",
			"is_unique":true}`,
		"--dry-run")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "uniqueness is not supported")
}

func TestAttributeDefinitionRenameUsesFreshRevisionETag(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var patchIfMatch string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPatch {
			patchIfMatch = r.Header.Get("If-Match")
			var body map[string]json.RawMessage
			assert.NoError(json.NewDecoder(r.Body).Decode(&body))
			assert.Equal(`"Conversation starters"`, string(body["label"]))
		}
		_, err := w.Write([]byte(testAttributeDefinitionJSON))
		assert.NoError(err)
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{
		Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})

	_, err := runAttributeCommand(t, attributeDefinitionRenameCmd,
		"3", "--label", "Conversation starters")
	require.NoError(err)
	assert.Equal(`"attribute-definition-3-r7"`, patchIfMatch)
}

func TestAttributeDefinitionClearDescriptionSendsEmptyString(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var description json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPatch {
			var body map[string]json.RawMessage
			assert.NoError(json.NewDecoder(r.Body).Decode(&body))
			description = body["description"]
		}
		_, err := w.Write([]byte(testAttributeDefinitionJSON))
		assert.NoError(err)
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{
		Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})

	_, err := runAttributeCommand(t, attributeDefinitionRenameCmd,
		"3", "--clear-description")
	require.NoError(err)
	assert.Equal(`""`, string(description))
}
