package cmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

const testPersonNotesAttributesJSON = `{
	"person_id":7,
	"attributes":[{
		"definition":{
			"id":9,"universal_id":"b72b3cf7-509f-4286-a0f0-bb039c85ff40","object_type":"person","slug":"notes_system","label":"Notes",
			"value_type":"text","field_type":"textarea","cardinality":"single","ownership":"system",
			"created_at":"2026-08-20T12:30:00Z","updated_at":"2026-08-20T12:30:00Z"
		},
		"current":[{
			"id":71,"person_id":7,"definition_id":9,"definition_slug":"notes_system","ordinal":0,
			"value":{"type":"text","text":"Private\ncontext"},"source":"user","actor":"cli",
			"active_from":"2026-08-20T12:30:00Z","created_at":"2026-08-20T12:30:00Z"
		}]
	}]
}`

const testPersonNotesWriteJSON = `{
	"dry_run":false,
	"value":{
		"id":72,"person_id":7,"definition_id":9,"definition_slug":"notes_system","ordinal":0,
		"value":{"type":"text","text":"Replacement"},"source":"user",
		"active_from":"2026-08-21T12:30:00Z","created_at":"2026-08-21T12:30:00Z"
	},
	"superseded":{
		"id":71,"person_id":7,"definition_id":9,"definition_slug":"notes_system","ordinal":0,
		"value":{"type":"text","text":"Private\ncontext"},"source":"user",
		"active_from":"2026-08-20T12:30:00Z","active_until":"2026-08-21T12:30:00Z",
		"created_at":"2026-08-20T12:30:00Z","superseded_at":"2026-08-21T12:30:00Z"
	}
}`

func executePersonNotesCommand(
	t *testing.T, input string, args ...string,
) (string, error) {
	t.Helper()
	root := &cobra.Command{Use: "person"}
	notes := &cobra.Command{
		Use: personNotesCmd.Use, Short: personNotesCmd.Short, Long: personNotesCmd.Long,
	}
	root.AddCommand(notes)
	for _, template := range []*cobra.Command{
		personNotesGetCmd, personNotesSetCmd, personNotesAppendCmd,
	} {
		leaf := &cobra.Command{
			Use: template.Use, Short: template.Short, Long: template.Long,
			Args: template.Args, RunE: template.RunE,
		}
		leaf.Flags().AddFlagSet(template.Flags())
		leaf.Flags().VisitAll(func(flag *pflag.Flag) {
			require.NoError(t, flag.Value.Set(flag.DefValue))
			flag.Changed = false
		})
		notes.AddCommand(leaf)
	}
	var output bytes.Buffer
	root.SetIn(strings.NewReader(input))
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs(append([]string{"notes"}, args...))
	err := root.Execute()
	return output.String(), err
}

func TestPersonNotesGetRoutesThroughDaemonAndPrintsOnlyText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/api/v1/people/7/attributes", r.URL.Path)
		assert.Empty(t, r.URL.Query().Get("slug"))
		assert.Equal(t, "b72b3cf7-509f-4286-a0f0-bb039c85ff40",
			r.URL.Query().Get("universal_id"))
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(testPersonNotesAttributesJSON))
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{
		Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})

	output, err := executePersonNotesCommand(t, "", "get", "7")
	require.NoError(t, err)
	assert.Equal(t, "Private\ncontext\n", output)
}

func TestPersonNotesGetJSONEmitsFullCurrentValue(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(testPersonNotesAttributesJSON))
		assert.NoError(err)
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{
		Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})

	output, err := executePersonNotesCommand(t, "", "get", "7", "--json")
	require.NoError(err)
	var value map[string]any
	require.NoError(json.Unmarshal([]byte(output), &value))
	assert.InDelta(float64(71), value["id"], 0)
	assert.Equal("notes_system", value["definition_slug"])
	assert.Equal("cli", value["actor"])
	assert.Equal(map[string]any{"type": "text", "text": "Private\ncontext"}, value["value"])
}

func TestPersonNotesSetForwardsTypedTextAndOptionalCAS(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var response string
		switch r.Method {
		case http.MethodGet:
			assert.Equal("/api/v1/people/7/attributes", r.URL.Path)
			response = testPersonNotesAttributesJSON
		case http.MethodPut:
			assert.Equal("/api/v1/people/7/attributes/notes_system", r.URL.Path)
			if !assert.NoError(json.NewDecoder(r.Body).Decode(&body)) {
				return
			}
			response = testPersonNotesWriteJSON
		default:
			assert.Fail("unexpected method", r.Method)
		}
		_, err := w.Write([]byte(response))
		assert.NoError(err)
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{
		Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})

	output, err := executePersonNotesCommand(t, "", "set", "7",
		"--text", "Replacement", "--expected-value-id", "71", "--json")
	require.NoError(err)
	assert.Equal(map[string]any{"text": "Replacement", "type": "text"}, body["value"])
	assert.InDelta(float64(71), body["expected_value_id"], 0)
	assert.Equal("user", body["source"])
	var write map[string]any
	require.NoError(json.Unmarshal([]byte(output), &write))
	assert.Equal(false, write["dry_run"])
	writeValue, ok := write["value"].(map[string]any)
	require.True(ok)
	supersededValue, ok := write["superseded"].(map[string]any)
	require.True(ok)
	assert.InDelta(float64(72), writeValue["id"], 0)
	assert.InDelta(float64(71), supersededValue["id"], 0)
}

func TestPersonNotesSetReadsFileWithoutDiscardingMultilineText(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "notes.txt")
	require.NoError(os.WriteFile(path, []byte("Line one\nLine two\n"), 0o600))
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		response := testPersonNotesAttributesJSON
		if r.Method == http.MethodPut {
			if !assert.NoError(json.NewDecoder(r.Body).Decode(&body)) {
				return
			}
			response = testPersonNotesWriteJSON
		}
		_, err := w.Write([]byte(response))
		assert.NoError(err)
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{
		Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})

	_, err := executePersonNotesCommand(t, "", "set", "7", "--text", "@"+path)
	require.NoError(err)
	value, ok := body["value"].(map[string]any)
	require.True(ok)
	assert.Equal("Line one\nLine two\n", value["text"])
	assert.NotContains(body, "expected_value_id")
}

func TestPersonNotesAppendPreservesMultilineStdinAndUsesAtomicRoute(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/people/7/notes/append", r.URL.Path)
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(testPersonNotesWriteJSON))
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{
		Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})

	_, err := executePersonNotesCommand(t, "First line\nSeñor 🌍\n", "append", "7", "--text", "-")
	require.NoError(t, err)
	assert.Equal(t, "First line\nSeñor 🌍\n", body["text"])
	assert.Equal(t, "user", body["source"])
}

func TestPersonNotesRejectsBlankTextBeforeDaemonRequest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{
		Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})

	for _, test := range []struct {
		name  string
		input string
		args  []string
	}{
		{name: "literal", args: []string{"set", "7", "--text", "  \t  "}},
		{name: "stdin", input: " \n\t\n", args: []string{"append", "7", "--text", "-"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := executePersonNotesCommand(t, test.input, test.args...)
			require.Error(t, err)
			assert.ErrorContains(t, err, "notes text must not be blank")
		})
	}
	assert.Zero(t, requests.Load())
}
