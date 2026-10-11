package docbankmedia

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSearchFenceAndContract(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	t.Parallel()
	ids := make([]string, MaxSearchVersions)
	for i := range ids {
		ids[i] = uuid.NewString()
	}
	sources := make([]SearchMediaSelector, len(ids))
	for i, id := range ids {
		sources[i] = SearchMediaSelector{SourceID: strings.Repeat("\x01", 252) + fmt.Sprintf("%04x", i), SourceVersionID: strings.Repeat("\x02", 256), ContentVersionID: id, SuppliedInputIDs: []string{strings.Repeat("a", 64)}}
	}
	requests := 0
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var body json.RawMessage
		if !assert.NoError(json.NewDecoder(r.Body).Decode(&body)) {
			return
		}
		var request SearchRequest
		var wire struct {
			MediaSources []map[string]json.RawMessage `json:"media_sources"`
		}
		if !assert.NoError(json.Unmarshal(body, &request)) || !assert.NoError(json.Unmarshal(body, &wire)) {
			return
		}
		value, exists := wire.MediaSources[0]["supplied_input_ids"]
		assert.Equal(sources[0].SuppliedInputIDs != nil, exists)
		if sources[0].SuppliedInputIDs != nil {
			encoded, err := json.Marshal(sources[0].SuppliedInputIDs)
			if !assert.NoError(err) {
				return
			}
			assert.JSONEq(string(encoded), string(value))
		}
		assert.True(request.ContentFirst)
		assert.Equal("lexical", request.Mode)
		assert.Equal(ids, request.Fence.ContentVersionIDs)
		assert.Equal(sources, request.MediaSources)
		_ = json.NewEncoder(w).Encode(SearchReport{MediaSourceSelection: true, MediaSelections: []SearchMediaSelection{}, RequestedMode: "lexical", ActualMode: "lexical", Coverage: SearchCoverage{State: "unknown"}, Results: []SearchHit{}})
	}))
	t.Cleanup(remote.Close)
	client, err := NewClient(remote.URL, nil)
	require.NoError(err)
	request := SearchRequest{Query: "quarterly numbers", Mode: "lexical", Limit: 100, Profile: "supplied-transcript", ContentFirst: true, MediaSources: sources, Fence: &SearchFence{VaultUID: "vault", ContentVersionIDs: ids}}
	report, err := client.Search(t.Context(), request)
	require.NoError(err)
	assert.Equal("unknown", report.Coverage.State)
	for _, inputs := range [][]string{nil, {}} {
		for i := range sources {
			sources[i].SuppliedInputIDs = inputs
		}
		_, err = client.Search(t.Context(), request)
		require.NoError(err)
	}
	request.MediaSources = nil
	_, err = client.Search(t.Context(), request)
	require.ErrorIs(err, ErrInvalidRequest)
	assert.Equal(3, requests)
}

func TestSearchSelectedEvidenceBoundary(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"valid", "partial completeness", "degraded provenance", "unsupported completeness", "foreign vault", "foreign version", "wrong mode", "missing results", "null summary", "old producer empty", "malformed", "generated with supplied input", "summary only", "duplicate summary", "foreign summary", "empty summary origin", "empty summary completeness", "supplied allowed", "supplied excluded", "foreign source", "wrong content association", "missing build", "missing segment", "bad time", "duplicate build", "duplicate source across builds"} {
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			source := SearchMediaSource{SourceID: "source", SourceVersionID: "version", ContentVersionID: "123e4567-e89b-42d3-a456-426614174000"}
			evidence := SearchEvidence{Kind: "rendition_segment", BuildID: "build", SegmentID: "segment", MediaSources: []SearchMediaSource{source}}
			report := SearchReport{MediaSourceSelection: true, MediaSelections: []SearchMediaSelection{{SearchMediaSource: source, Origin: "generated", Completeness: "complete"}}, RequestedMode: "lexical", ActualMode: "lexical", Coverage: SearchCoverage{State: "complete", ScopedDocuments: 1, CompleteDocuments: 1}, Results: []SearchHit{{VaultUID: "vault", NodeID: 1, ContentVersionID: "123e4567-e89b-42d3-a456-426614174000", Rank: 1, Excerpt: "\x01words\x02", Evidence: []SearchEvidence{evidence}}}}
			e := &report.Results[0].Evidence[0]
			inputs := []string{}
			switch name {
			case "partial completeness":
				report.MediaSelections[0].Completeness = "partial"
			case "degraded provenance":
				report.MediaSelections[0].Completeness = "degraded_provenance"
			case "unsupported completeness":
				report.MediaSelections[0].Completeness = "uncertain"
			case "foreign vault":
				report.Results[0].VaultUID = "foreign"
			case "foreign version":
				report.Results[0].ContentVersionID = "foreign"
			case "wrong mode":
				report.ActualMode = "semantic"
			case "missing results":
				report.Results = nil
			case "old producer empty":
				report.MediaSourceSelection = false
				report.Results = []SearchHit{}
			case "null summary":
				report.MediaSelections = nil
			case "generated with supplied input":
				report.MediaSelections[0].SuppliedInputID = strings.Repeat("a", 64)
			case "summary only":
				report.Results = []SearchHit{}
			case "duplicate summary":
				report.MediaSelections = append(report.MediaSelections, report.MediaSelections[0])
			case "foreign summary":
				report.MediaSelections[0].SourceID = "foreign"
			case "empty summary origin":
				report.MediaSelections[0].Origin = ""
			case "empty summary completeness":
				report.MediaSelections[0].Completeness = ""
			case "supplied allowed", "supplied excluded":
				report.MediaSelections[0].Origin, report.MediaSelections[0].SuppliedInputID = "supplied", strings.Repeat("a", 64)
				if name != "supplied excluded" {
					inputs = []string{report.MediaSelections[0].SuppliedInputID}
				}
			case "foreign source":
				e.MediaSources[0].SourceID = "foreign"
			case "wrong content association":
				e.MediaSources[0].ContentVersionID = "other"
			case "missing build":
				e.BuildID = ""
			case "missing segment":
				e.SegmentID = ""
			case "bad time":
				e.TimeSpan = &MediaTimeSpan{StartMS: -1, EndMS: 2}
			case "duplicate build":
				report.Results = append(report.Results, report.Results[0])
			case "duplicate source across builds":
				other := report.Results[0]
				other.Evidence = []SearchEvidence{evidence}
				other.Evidence[0].BuildID = "another"
				report.Results = append(report.Results, other)
			}
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if name == "malformed" {
					_, _ = w.Write([]byte(`{"results":`))
					return
				}
				_ = json.NewEncoder(w).Encode(report)
			}))
			t.Cleanup(remote.Close)
			client, err := NewClient(remote.URL, nil)
			require.NoError(err)
			got, err := client.Search(t.Context(), SearchRequest{Query: "words", Mode: "lexical", Profile: "supplied-transcript", ContentFirst: true, Limit: 100, Fence: &SearchFence{VaultUID: "vault", ContentVersionIDs: []string{"123e4567-e89b-42d3-a456-426614174000"}}, MediaSources: []SearchMediaSelector{{SearchMediaSource: source, SuppliedInputIDs: inputs}}})
			if name == "valid" || name == "summary only" || name == "supplied allowed" || name == "partial completeness" || name == "degraded provenance" {
				require.NoError(err)
				if name != "summary only" {
					assert.Equal("words", got.Results[0].Excerpt)
				}
			} else {
				require.ErrorIs(err, ErrInvalidReceipt)
			}
		})
	}
}
