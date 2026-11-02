package cmd

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/senderauth"
)

func TestSenderAuthCLIJSONReaders(t *testing.T) {
	receipt := senderauth.UnknownVerdict("no_raw")
	for _, tc := range []struct {
		name   string
		render func(*testing.T, *senderauth.Verdict) []byte
	}{
		{"message and full thread", func(t *testing.T, v *senderauth.Verdict) []byte {
			t.Helper()
			must := require.New(t)
			var b bytes.Buffer
			must.NoError(outputMessageJSON(&b, &query.MessageDetail{ID: 1, SenderAuth: v}))
			return b.Bytes()
		}},
		{"keyword search", func(t *testing.T, v *senderauth.Verdict) []byte {
			t.Helper()
			must := require.New(t)
			done := captureStdout(t)
			err := outputSearchResultsJSON([]query.MessageSummary{{ID: 1, SenderAuth: v}})
			output := done()
			must.NoError(err)
			var rows []json.RawMessage
			must.NoError(json.Unmarshal([]byte(output), &rows))
			must.Len(rows, 1)
			return rows[0]
		}},
		{"semantic search", func(t *testing.T, v *senderauth.Verdict) []byte {
			t.Helper()
			must := require.New(t)
			done := captureStdout(t)
			err := outputHybridResultsJSON(&daemonclient.CLIHybridSearch{Results: []daemonclient.CLIHybridSearchResult{{ID: 1, Message: query.MessageSummary{ID: 1, SenderAuth: v}}}}, false)
			output := done()
			must.NoError(err)
			var response struct {
				Results []json.RawMessage `json:"results"`
			}
			must.NoError(json.Unmarshal([]byte(output), &response))
			must.Len(response.Results, 1)
			return response.Results[0]
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checks := assert.New(t)
			must := require.New(t)
			var row struct {
				SenderAuth *senderauth.Verdict `json:"sender_auth"`
			}
			must.NoError(json.Unmarshal(tc.render(t, receipt), &row))
			checks.Equal(receipt, row.SenderAuth)
			checks.NotContains(string(tc.render(t, nil)), `"sender_auth"`, "older-daemon metadata remains optional")
		})
	}
}
