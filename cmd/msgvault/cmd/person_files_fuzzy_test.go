package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
)

func TestPersonFilesFuzzyValidation(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"40", "--fuzzy-filename"}, "--filename"},
		{[]string{"40", "--fuzzy-filename", "--filename", "report", "--lane", "documents", "--query", "report"}, "metadata"},
		{[]string{"40", "--fuzzy-filename", "--filename", "report", "--lane", "visual", "--query", "report"}, "metadata"},
		{[]string{"40", "--fuzzy-filename", "--filename", "report", "--lane", "all", "--query", "report"}, "metadata"},
		{[]string{"40", "--fuzzy-filename", "--filename", "report", "--cursor", "opaque"}, "cursor"},
		{[]string{"40", "--fuzzy-filename", "--filename", "report", "--query", "semantic"}, "query"},
		{[]string{"40", "--fuzzy-filename", "--filename", "report", "--query="}, "query"},
		{[]string{"40", "--fuzzy-filename", "--filename", "report", "--cursor="}, "cursor"},
		{[]string{"40", "--fuzzy-filename", "--filename", "report", "--limit", "0"}, "limit"},
		{[]string{"40", "--fuzzy-filename", "--filename", "!!!"}, "filename"},
		{[]string{"40", "--fuzzy-filename", "--filename", "report", "--mime-family", strings.Repeat("pdf,", 8) + "pdf"}, "mime-family"},
	} {
		t.Run(tc.want+"/"+tc.args[len(tc.args)-1], func(t *testing.T) {
			opened := false
			command := newPersonFilesCommand(personFilesCommandDeps{openClient: func(context.Context) (personFilesClient, func(), error) { opened = true; return nil, func() {}, nil }})
			command.SetOut(&bytes.Buffer{})
			command.SetErr(&bytes.Buffer{})
			command.SetArgs(tc.args)
			err := command.ExecuteContext(t.Context())
			require.Error(t, err)
			require.ErrorContains(t, err, tc.want)
			assert.False(t, opened, "validate before daemon access")
		})
	}
}

func TestPersonFilesFuzzyOutput(t *testing.T) {
	const envelope = `{"suggestions":[{"id":3,"message_id":2,"conversation_id":4,"source_id":1,"occurred_at":null,"filename":"report.pdf","mime_type":"application/pdf","mime_family":"pdf","filename_match":"typo","size_bytes":12,"content_state":"metadata_only","content_available":false,"person_provenance":{"participant_ids":[8],"roles":["to"],"directions":["to_person"]}}],"candidate_limit_reached":true,"index_revision":"g1:r1"}`
	for _, tc := range []struct {
		json  bool
		limit string
	}{{true, ""}, {false, "7"}} {
		t.Run(tc.limit, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(http.MethodPost, r.Method)
				assert.Equal("/api/v1/people/40/files/suggest", r.URL.Path)
				var request map[string]any
				decoder := json.NewDecoder(r.Body)
				decoder.UseNumber()
				if !assert.NoError(decoder.Decode(&request)) {
					return
				}
				assert.Equal("reprot.pdf", request["filename_query"])
				assert.Equal([]any{"to_person"}, request["directions"])
				assert.Equal([]any{"pdf"}, request["mime_families"])
				assert.Equal("2026-08-01T00:00:00Z", request["after"])
				assert.Equal("2026-08-20T00:00:00Z", request["before"])
				if tc.limit == "" {
					assert.NotContains(request, "limit")
				} else {
					assert.Equal(json.Number("7"), request["limit"])
				}
				for _, key := range []string{"cursor", "query", "predicate", "sort", "source_ids", "account_scopes"} {
					assert.NotContains(request, key)
				}
				w.Header().Set("Content-Type", "application/json")
				_, err := w.Write([]byte(envelope))
				assert.NoError(err)
			}))
			t.Cleanup(server.Close)
			client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
			require.NoError(err)
			t.Cleanup(func() { require.NoError(client.Close()) })
			command := newPersonFilesCommand(personFilesCommandDeps{openClient: func(context.Context) (personFilesClient, func(), error) { return client, func() {}, nil }})
			var output bytes.Buffer
			command.SetOut(&output)
			command.SetErr(&bytes.Buffer{})
			args := []string{"40", "--filename", "reprot.pdf", "--fuzzy-filename", "--direction", "to_person", "--mime-family", "pdf", "--after", "2026-08-01", "--before", "2026-08-20"}
			if tc.json {
				args = append(args, "--json")
			}
			if tc.limit != "" {
				args = append(args, "--limit", tc.limit)
			}
			command.SetArgs(args)
			require.NoError(command.ExecuteContext(t.Context()))
			if tc.json {
				assert.JSONEq(envelope, output.String())
			} else {
				assert.Contains(output.String(), "MATCH")
				assert.Contains(output.String(), "typo")
				assert.Contains(output.String(), "Candidate limit reached; suggestions may be incomplete.")
			}
		})
	}
}
