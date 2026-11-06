package cmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query"
)

func TestSearchSnippetColumnOptIn(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	saved := searchShowSnippet
	t.Cleanup(func() { searchShowSnippet = saved })
	rows := []query.MessageSummary{{ID: 1, Subject: "Plan", Snippet: "stored preview", MatchSnippet: "needle context"}}
	var out bytes.Buffer
	searchShowSnippet = false
	require.NoError(writeSearchResultsTableWidth(&out, rows, 0))
	assert.NotContains(out.String(), "needle context")
	out.Reset()
	searchShowSnippet = true
	require.NoError(writeSearchResultsTableWidth(&out, rows, 0))
	assert.Contains(out.String(), "SNIPPET")
	assert.Contains(out.String(), "needle context")
	out.Reset()
	require.NoError(writeHybridResultsTableWidth(&out, []daemonclient.CLIHybridSearchResult{{ID: 1, Subject: "Plan", Message: query.MessageSummary{Snippet: "needle context"}}}, false, 0))
	assert.Contains(out.String(), "needle context")
	out.Reset()
	rows = []query.MessageSummary{{ID: 1, Subject: strings.Repeat("subject ", 20), FromEmail: "sender@example.com", Snippet: strings.Repeat("界", 160)}}
	require.NoError(writeSearchResultsTableWidth(&out, rows, 100))
	for line := range strings.SplitSeq(strings.TrimSpace(out.String()), "\n") {
		assert.LessOrEqual(ansi.StringWidth(line), 100)
	}
}

func TestSearchSnippetPreservesRunes(t *testing.T) {
	text := strings.Repeat("界", 180)
	got := searchSnippetText(text)
	assert.True(t, utf8.ValidString(got))
	assert.Equal(t, strings.Repeat("界", 160)+"…", got)
}

func TestSemanticJSONIncludesSearchContext(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	count := int64(1)

	done := captureStdout(t)
	require.NoError(outputHybridResultsJSON(&daemonclient.CLIHybridSearch{Results: []daemonclient.CLIHybridSearchResult{{ID: 1, AttachmentCount: &count, Message: query.MessageSummary{ConversationID: 7, Snippet: "needle context", AttachmentCount: 1, AttachmentNames: []string{"plan.pdf"}}}}}, false))
	var got struct {
		Results []map[string]any `json:"results"`
	}
	decoder := json.NewDecoder(strings.NewReader(done()))
	decoder.UseNumber()
	require.NoError(decoder.Decode(&got))
	require.Len(got.Results, 1)
	assert.Equal(json.Number("7"), got.Results[0]["conversation_id"])
	assert.Equal(json.Number("1"), got.Results[0]["attachment_count"])
	assert.Equal("needle context", got.Results[0]["snippet"])
	assert.Equal([]any{"plan.pdf"}, got.Results[0]["attachment_names"])
}

func TestSearchHintStaysOnStderr(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(strconv.FormatBool(active), func(t *testing.T) {
			assert := assert.New(t)

			resetSearchFlags()
			t.Cleanup(resetSearchFlags)
			cfg := &config.Config{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				assert.NoError(json.NewEncoder(w).Encode(map[string]any{"results": []any{}, "hybrid_available": active}))
			}))
			t.Cleanup(srv.Close)
			cfg.Remote.URL = srv.URL
			cfg.Remote.AllowInsecure = true
			root := newTestRootCmd()
			root.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
			root.AddCommand(searchCmd)
			var stderr bytes.Buffer
			root.SetErr(&stderr)
			root.SetArgs([]string{"search", "intent", "--json"})
			done := captureStdout(t)
			err := root.Execute()
			out := done()
			require.NoError(t, err)
			assert.JSONEq("[]", out)
			if active {
				assert.Contains(stderr.String(), "--mode hybrid")
			} else {
				assert.Empty(stderr.String())
			}
		})
	}
}

func TestSearchJSONAttachmentMetadataAvailability(t *testing.T) {
	for _, tc := range []struct {
		name  string
		names []string
		count int64
	}{
		{name: "unavailable"},
		{name: "known empty", names: []string{}},
		{name: "partial names", names: []string{"plan.pdf", "budget.csv"}, count: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			message := query.MessageSummary{HasAttachments: true, AttachmentNames: tc.names, AttachmentCount: int(tc.count)}
			var count *int64
			if tc.names != nil {
				count = &tc.count
			}
			for _, semantic := range []bool{false, true} {
				done := captureStdout(t)
				var err error
				if semantic {
					err = outputHybridResultsJSON(&daemonclient.CLIHybridSearch{Results: []daemonclient.CLIHybridSearchResult{{Message: message, AttachmentCount: count}}}, false)
				} else {
					err = outputSearchResultsJSON([]query.MessageSummary{message})
				}
				output := done()
				require.NoError(err)
				var rows []map[string]any
				if semantic {
					var page struct {
						Results []map[string]any `json:"results"`
					}
					require.NoError(json.Unmarshal([]byte(output), &page))
					rows = page.Results
				} else {
					require.NoError(json.Unmarshal([]byte(output), &rows))
				}
				require.Len(rows, 1)
				if tc.names == nil {
					assert.NotContains(rows[0], "attachment_names")
				} else {
					names := make([]any, len(tc.names))
					for i, name := range tc.names {
						names[i] = name
					}
					assert.Equal(names, rows[0]["attachment_names"])
				}
				if semantic && count == nil {
					assert.NotContains(rows[0], "attachment_count")
				} else {
					assert.InDelta(float64(tc.count), rows[0]["attachment_count"], 0)
				}
			}
		})
	}
}
