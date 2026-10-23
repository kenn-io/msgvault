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
		name   string
		args   []string
		status int
		want   string
	}{
		{"table", []string{"search", "quarterly", "numbers", "--person", "7", "--direction", "from_person"}, 200, "generated"},
		{"recordings unavailable", []string{"search", "quartz"}, 200, "Docbank transcript search is unavailable"},
		{"attachments unavailable", []string{"search", "quarterly numbers"}, 200, "Attachment search is unavailable."},
		{"scope limit", []string{"search", "quartz"}, 200, "Narrow the attachment search scope."},
		{"terminal", []string{"search", "quarterly numbers"}, 200, "Coverage complete"},
		{"json", []string{"search", "quarterly numbers", "--json"}, 200, `"message_id":11`},
		{"mode delegated", []string{"search", "words", "--mode", "hybrid"}, 503, "media_search_mode_unavailable"},
		{"upper limit delegated", []string{"search", "words", "--limit", "101"}, 400, "invalid_limit"},
		{"person relation delegated", []string{"search", "words", "--direction", "from_person"}, 400, "invalid_person"},
		{"direction delegated", []string{"search", "words", "--person", "7", "--direction", "invalid"}, 400, "invalid_direction"},
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
					_ = json.NewEncoder(w).Encode(map[string]string{"error": test.want, "message": test.want})
					return
				}
				state, excerpt := "complete", "quarterly numbers"
				if test.name == "terminal" {
					state, excerpt = "\x1b[31mcomplete\x1b[0m\a", "quarterly\x1b[31m numbers\x1b[0m\a"
				}
				response := generated.MediaSearchResponse{
					Coverage:    generated.SearchCoverage{State: state},
					Results:     []generated.MediaSearchResult{{MessageID: 11, ConversationID: 22, AttachmentID: 33, Origin: generated.MediaSearchResultOriginGenerated, Excerpt: excerpt}},
					Attachments: &generated.MediaAttachmentSearch{Results: []generated.MediaAttachmentResult{{MessageID: 44, ConversationID: 22, AttachmentID: 55, Filename: "report.pdf", Excerpt: "quartz"}}, Truncated: true},
				}
				switch test.name {
				case "recordings unavailable":
					response.Results = []generated.MediaSearchResult{}
					response.Partial = true
					response.RecordingsError = &generated.ErrorResponse{ErrorData: "media_search_unavailable", Message: new(test.want)}
				case "attachments unavailable", "scope limit":
					code := "transport"
					if test.name == "scope limit" {
						code = "media_search_scope_limit"
					}
					response.Attachments = &generated.MediaAttachmentSearch{Results: []generated.MediaAttachmentResult{}, ErrorData: &generated.ErrorResponse{ErrorData: code, Message: new(test.want)}}
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
				require.ErrorContains(err, test.want)
				return
			}
			require.NoError(err)
			assert.Contains(output.String(), test.want)
			if test.name == "recordings unavailable" {
				assert.NotContains(output.String(), "quarterly numbers")
			} else {
				assert.Contains(output.String(), "quarterly numbers")
			}
			assert.NotContains(output.String(), "\x1b")
			assert.NotContains(output.String(), "\a")
			if test.name == "scope limit" || test.name == "attachments unavailable" {
				assert.NotContains(output.String(), "report.pdf")
				assert.NotContains(output.String(), "More may match")
			} else {
				assert.Contains(output.String(), "report.pdf")
				if test.name != "json" {
					assert.Contains(output.String(), "More may match")
				}
			}
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
