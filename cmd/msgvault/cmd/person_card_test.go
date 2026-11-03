package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/personcard"
)

func personCardCLIDeps(t *testing.T, version string, check func(*http.Request), override ...personcard.PersonCardResult) personCardCommandDeps {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/health" {
			assert.NoError(t, json.MarshalWrite(w, map[string]any{"status": "ok", "api_schema_version": version}))
			return
		}
		ref := "person:7"
		if len(override) > 0 && override[0].Ref != "" {
			ref = override[0].Ref
		}
		assert.Equal(t, "/api/v1/people/"+ref+"/card", r.URL.Path)
		if check != nil {
			check(r)
		}
		format := personcard.Format(r.URL.Query().Get("format"))
		response := personcard.PersonCardResult{SchemaVersion: 1, Status: "ok", Ref: "person:7", Format: format}
		if format == personcard.FormatMarkdown {
			response.Content = new("# Alex Example\n")
			response.ContentBytes = len(*response.Content)
		} else {
			response.Card = &personcard.Card{Person: personcard.Person{PersonID: 7}}
		}
		if len(override) > 0 {
			response = override[0]
		}
		assert.NoError(t, json.MarshalWrite(w, response))
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, client.Close()) })
	return personCardCommandDeps{open: func(context.Context) (personCardCommandClient, func(), error) { return client, func() {}, nil }}
}

func TestPersonCardCLIFormatsAndOptions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   []string
		format string
		json   bool
	}{
		{"markdown default", nil, "markdown", false},
		{"json flag", []string{"--json"}, "json", true},
		{"json format", []string{"--format", "json"}, "json", true},
		{"json wins", []string{"--json", "--format", "markdown"}, "json", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			deps := personCardCLIDeps(t, "3.26.0", func(r *http.Request) {
				assert.Equal(t, tc.format, r.URL.Query().Get("format"))
				assert.Equal(t, "2026-10-17T15:00:00Z", r.URL.Query().Get("as_of"))
				assert.Equal(t, "4", r.URL.Query().Get("max_items"))
				assert.Equal(t, "8192", r.URL.Query().Get("max_bytes"))
				assert.Equal(t, "external", r.URL.Query().Get("affiliation"))
			})
			command := newPersonCardCommand(deps)
			var output bytes.Buffer
			command.SetOut(&output)
			command.SetErr(io.Discard)
			command.SetArgs(append([]string{"person:7", "--as-of", "2026-10-17T15:00:00Z", "--max-items", "4", "--max-bytes", "8192", "--affiliation", "external"}, tc.args...))
			requirements.NoError(command.ExecuteContext(t.Context()))
			if tc.json {
				var got personcard.PersonCardResult
				requirements.NoError(json.Unmarshal(output.Bytes(), &got))
				assertions.Equal("ok", got.Status)
				requirements.NotNil(got.Card)
				assertions.Equal(int64(7), got.Card.Person.PersonID)
			} else {
				assertions.Equal("# Alex Example\n", output.String())
			}
		})
	}
}

func TestPersonCardCLIValidatesBeforeOpeningDaemon(t *testing.T) {
	for _, args := range [][]string{
		{"person:7", "--max-items", "0"}, {"person:7", "--max-items", "11"},
		{"person:7", "--max-bytes", "2047"}, {"person:7", "--max-bytes", "65537"},
		{"person:7", "--format", "xml"}, {"person:7", "--affiliation", "friend"},
		{"person:7", "--as-of", "yesterday"}, {"invalid-ref"},
	} {
		t.Run(args[len(args)-1], func(t *testing.T) {
			command := newPersonCardCommand(personCardCommandDeps{open: func(context.Context) (personCardCommandClient, func(), error) {
				assert.Fail(t, "invalid arguments must not open the daemon")
				return nil, nil, nil
			}})
			command.SetOut(io.Discard)
			command.SetErr(io.Discard)
			command.SetArgs(args)
			require.Error(t, command.ExecuteContext(t.Context()))
		})
	}
}

func TestPersonCardCLISchemaGate(t *testing.T) {
	deps := personCardCLIDeps(t, "3.25.0", func(*http.Request) { assert.Fail(t, "old daemon must not receive a card read") })
	command := newPersonCardCommand(deps)
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs([]string{"person:7"})
	require.ErrorContains(t, command.ExecuteContext(t.Context()), "3.26.0 or newer")
}

func TestPersonCardCLIUnresolvedEnvelope(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	deps := personCardCLIDeps(t, "3.26.0", nil, personcard.PersonCardResult{SchemaVersion: 1, Status: "unknown", Ref: "email:missing@example.com", Format: personcard.FormatMarkdown, Gaps: []string{"not_in_archive"}})
	command := newPersonCardCommand(deps)
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(io.Discard)
	command.SetArgs([]string{"email:missing@example.com"})
	requirements.NoError(command.ExecuteContext(t.Context()))
	var got personcard.PersonCardResult
	requirements.NoError(json.Unmarshal(output.Bytes(), &got))
	assertions.Equal("unknown", got.Status)
	assertions.Equal([]string{"not_in_archive"}, got.Gaps)
}
