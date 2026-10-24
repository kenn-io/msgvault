package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestMediaSearchCLIDaemonRoundTrip(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name             string
		args             []string
		status           int
		want             []string
		absent           []string
		recordingsError  string
		attachmentsError string
		terminal         bool
	}{
		{name: "table", args: []string{"search", "quarterly", "numbers", "--person", "7", "--direction", "from_person"}, status: 200, want: []string{"Coverage complete", "generated", "quarterly numbers", "report.pdf", "More may match"}},
		{name: "recordings unavailable", args: []string{"search", "quartz"}, status: 200, recordingsError: "media_search_unavailable", want: []string{"partial true", "Docbank transcript search is unavailable", "Attachments truncated true", "report.pdf", "More may match"}, absent: []string{"quarterly numbers", "generated"}},
		{name: "attachments unavailable", args: []string{"search", "quarterly numbers"}, status: 200, attachmentsError: "transport", want: []string{"Coverage complete", "generated", "quarterly numbers", "Attachments truncated false", "Attachment search is unavailable."}, absent: []string{"report.pdf", "More may match"}},
		{name: "terminal", args: []string{"search", "quarterly numbers"}, status: 200, terminal: true, want: []string{"Coverage complete", "generated", "quarterly numbers", "report.pdf", "More may match"}},
		{name: "json", args: []string{"search", "quarterly numbers", "--json"}, status: 200, want: []string{`"message_id":11`, "generated", "quarterly numbers", "report.pdf"}},
		{name: "mode delegated", args: []string{"search", "words", "--mode", "hybrid"}, status: 503, want: []string{"media_search_mode_unavailable"}},
		{name: "upper limit delegated", args: []string{"search", "words", "--limit", "101"}, status: 400, want: []string{"invalid_limit"}},
		{name: "person relation delegated", args: []string{"search", "words", "--direction", "from_person"}, status: 400, want: []string{"invalid_person"}},
		{name: "direction delegated", args: []string{"search", "words", "--person", "7", "--direction", "invalid"}, status: 400, want: []string{"invalid_direction"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal("/api/v1/media/search", r.URL.Path)
				switch test.name {
				case "table":
					assert.Equal("quarterly numbers", r.URL.Query().Get("q"))
					assert.Equal("7", r.URL.Query().Get("person_id"))
					assert.Equal("from_person", r.URL.Query().Get("direction"))
				case "mode delegated":
					assert.Equal("hybrid", r.URL.Query().Get("mode"))
				case "upper limit delegated":
					assert.Equal("101", r.URL.Query().Get("limit"))
				case "person relation delegated":
					assert.Empty(r.URL.Query().Get("person_id"))
					assert.Equal("from_person", r.URL.Query().Get("direction"))
				case "direction delegated":
					assert.Equal("invalid", r.URL.Query().Get("direction"))
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.status)
				if test.status != 200 {
					_ = json.NewEncoder(w).Encode(map[string]string{"error": test.want[0], "message": test.want[0]})
					return
				}
				state, excerpt := "complete", "quarterly numbers"
				if test.terminal {
					state, excerpt = "\x1b[31mcomplete\x1b[0m\a", "quarterly\x1b[31m numbers\x1b[0m\a"
				}
				response := generated.MediaSearchResponse{
					Coverage:    generated.SearchCoverage{State: state},
					Results:     []generated.MediaSearchResult{{MessageID: 11, ConversationID: 22, AttachmentID: 33, Origin: generated.MediaSearchResultOriginGenerated, Excerpt: excerpt}},
					Attachments: &generated.MediaAttachmentSearch{Results: []generated.MediaAttachmentResult{{MessageID: 44, ConversationID: 22, AttachmentID: 55, Filename: "report.pdf", Excerpt: "quartz"}}, Truncated: true},
				}
				if test.recordingsError != "" {
					response.Coverage.State = "unknown"
					response.Results = []generated.MediaSearchResult{}
					response.Partial = true
					response.RecordingsError = &generated.ErrorResponse{ErrorData: test.recordingsError, Message: new("Docbank transcript search is unavailable")}
				}
				if test.attachmentsError != "" {
					response.Attachments = &generated.MediaAttachmentSearch{Results: []generated.MediaAttachmentResult{}, ErrorData: &generated.ErrorResponse{ErrorData: test.attachmentsError, Message: new("Attachment search is unavailable.")}}
				}
				_ = json.NewEncoder(w).Encode(response)
			}))
			t.Cleanup(daemon.Close)
			command := newMediaCmd(func(context.Context) (*daemonclient.Client, func(), error) {
				client, err := daemonclient.New(daemonclient.Config{URL: daemon.URL, AllowInsecure: true})
				return client, func() {
					if client != nil {
						_ = client.Close()
					}
				}, err
			})
			var output bytes.Buffer
			command.SetOut(&output)
			command.SetArgs(test.args)
			err := command.ExecuteContext(t.Context())
			if test.status != 200 {
				require.ErrorContains(err, test.want[0])
				return
			}
			require.NoError(err)
			for _, want := range test.want {
				assert.Contains(output.String(), want)
			}
			for _, absent := range test.absent {
				assert.NotContains(output.String(), absent)
			}
			assert.NotContains(output.String(), "\x1b")
			assert.NotContains(output.String(), "\a")
		})
	}
}

func TestMediaSearchCLINumericGuards(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"search", "words", "--person", "-1"}, {"search", "words", "--limit", "0"}} {
		command := newMediaCmd(func(context.Context) (*daemonclient.Client, func(), error) {
			require.FailNow(t, "numeric validation must precede opening the daemon")
			return nil, nil, nil
		})
		command.SetArgs(args)
		require.Error(t, command.ExecuteContext(t.Context()))
	}
}
