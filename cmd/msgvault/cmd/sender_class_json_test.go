package cmd

import (
	"encoding/json/v2"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/senderclass"
)

func TestSenderClassJSONReadPaths(t *testing.T) {
	for _, tc := range []struct {
		name    string
		readout senderclass.Readout
		want    map[string]any
	}{
		{name: "legacy", want: map[string]any{}},
		{name: "known", readout: senderclass.Readout{SenderClass: senderclass.Known, SenderClassBasis: senderclass.OwnerSentTo, SenderRelationship: senderclass.RelationshipKnown}, want: map[string]any{"sender_class": "known", "sender_class_basis": "owner_sent_to", "sender_relationship": "known"}},
		{name: "unavailable", readout: senderclass.UnavailableReadout(), want: map[string]any{"sender_class": "unverified", "sender_class_basis": "evidence_unavailable"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, path := range []string{"detail", "search", "hybrid"} {
				t.Run(path, func(t *testing.T) {
					assertions := assert.New(t)
					requirements := require.New(t)
					done := captureStdout(t)
					var err error
					switch path {
					case "detail":
						err = outputMessageJSON(os.Stdout, &query.MessageDetail{Readout: tc.readout})
					case "search":
						err = outputSearchResultsJSON([]query.MessageSummary{{Readout: tc.readout}})
					case "hybrid":
						err = outputHybridResultsJSON(&daemonclient.CLIHybridSearch{Results: []daemonclient.CLIHybridSearchResult{{Message: query.MessageSummary{Readout: tc.readout}}}}, false)
					}
					output := done()
					requirements.NoError(err)
					var got map[string]any
					switch path {
					case "detail":
						requirements.NoError(json.Unmarshal([]byte(output), &got))
					case "search":
						var rows []map[string]any
						requirements.NoError(json.Unmarshal([]byte(output), &rows))
						requirements.Len(rows, 1)
						got = rows[0]
					case "hybrid":
						var response struct {
							Results []map[string]any `json:"results"`
						}
						requirements.NoError(json.Unmarshal([]byte(output), &response))
						requirements.Len(response.Results, 1)
						got = response.Results[0]
					}
					for _, key := range []string{"sender_class", "sender_class_basis", "sender_relationship"} {
						if want, present := tc.want[key]; present {
							assertions.Equal(want, got[key])
						} else {
							assertions.NotContains(got, key)
						}
					}
				})
			}
		})
	}
}
