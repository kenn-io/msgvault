package docbankmedia

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestRecordingSearchAcceptsNonlexicalReport(t *testing.T) {
	for _, mode := range []string{"semantic", "hybrid"} {
		t.Run(mode, func(t *testing.T) {
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request SearchRequest
				require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
				assert.Equal(t, mode, request.Mode)
				require.NoError(t, json.NewEncoder(w).Encode(SearchReport{
					RequestedMode: mode, ActualMode: mode, MediaSourceSelection: true,
					MediaSelections: []SearchMediaSelection{}, Results: []SearchHit{},
					Coverage: SearchCoverage{State: "incomplete", ScopedDocuments: 1},
				}))
			}))
			t.Cleanup(remote.Close)
			client, err := NewClient(remote.URL, nil)
			require.NoError(t, err)
			report, err := client.Search(t.Context(), SearchRequest{
				Query: "delivery time", Mode: mode, Profile: "recording-vectors", Limit: 20, ContentFirst: true,
				Fence:        &SearchFence{VaultUID: "fixture", ContentVersionIDs: []string{"123e4567-e89b-42d3-a456-426614174000"}},
				MediaSources: []SearchMediaSelector{{SearchMediaSource: SearchMediaSource{SourceID: "source", SourceVersionID: "version", ContentVersionID: "123e4567-e89b-42d3-a456-426614174000"}}},
			})
			require.NoError(t, err)
			assert.Equal(t, mode, report.ActualMode)
		})
	}
}

// Exact source identity is required independently of ranking mode. Every case
// first accepts the real tuple, then changes one attributed identity component.
func TestRecordingSearchExactSourceProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		require := require.New(ht)
		identity := fmt.Sprintf("%x", sha256.Sum256([]byte(hegel.Draw(ht, hegel.Text()))))
		mode := []string{"lexical", "semantic", "hybrid"}[hegel.Draw(ht, hegel.Integers(0, 2))]
		field := hegel.Draw(ht, hegel.Integers(0, 2))
		source := SearchMediaSource{SourceID: identity, SourceVersionID: "version-" + identity, ContentVersionID: "123e4567-e89b-42d3-a456-426614174000"}
		evidence := SearchEvidence{Kind: "rendition_segment", BuildID: "selected", SegmentID: "segment", MediaSources: []SearchMediaSource{source}}
		if mode != "lexical" {
			evidence.Kind, evidence.SegmentID = "embedding", ""
			evidence.InputKind, evidence.InputID, evidence.InputGenerationID = "rendition_chunk", "input", "generation"
			evidence.EmbeddingSetID, evidence.VectorSpaceID, evidence.SourceManifestChecksum = "set", "space", strings.Repeat("a", 64)
		}
		report := SearchReport{RequestedMode: mode, ActualMode: mode, MediaSourceSelection: true,
			MediaSelections: []SearchMediaSelection{{SearchMediaSource: source, Origin: "generated", Completeness: "complete", BuildID: "selected"}},
			Coverage:        SearchCoverage{State: "complete", ScopedDocuments: 1, CompleteDocuments: 1},
			Results:         []SearchHit{{VaultUID: "fixture", NodeID: 1, ContentVersionID: source.ContentVersionID, Rank: 1, Excerpt: "telescope", Evidence: []SearchEvidence{evidence}}}}
		var mu sync.Mutex
		remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			_ = json.NewEncoder(w).Encode(report)
		}))
		defer remote.Close()
		client, err := NewClient(remote.URL, nil)
		require.NoError(err)
		request := SearchRequest{Query: "telescope", Mode: mode, Profile: "vectors", ContentFirst: true, Limit: 20,
			Fence: &SearchFence{VaultUID: "fixture", ContentVersionIDs: []string{source.ContentVersionID}}, MediaSources: []SearchMediaSelector{{SearchMediaSource: source}}}
		got, err := client.Search(t.Context(), request)
		require.NoError(err)
		require.Len(got.Results, 1)
		require.Equal("telescope", got.Results[0].Excerpt)
		mu.Lock()
		changed := &report.Results[0].Evidence[0].MediaSources[0]
		switch field {
		case 0:
			changed.SourceID += "-foreign"
		case 1:
			changed.SourceVersionID += "-foreign"
		case 2:
			changed.ContentVersionID = "123e4567-e89b-42d3-a456-426614174001"
		}
		mu.Unlock()
		_, err = client.Search(t.Context(), request)
		require.ErrorIs(err, ErrInvalidReceipt)
	}, hegel.WithTestCases(64), hegel.WithDatabase(""))
}

func TestRecordingSearchEmbeddingEvidenceBoundary(t *testing.T) {
	for _, name := range []string{"semantic", "hybrid", "missing build", "wrong selected build", "missing input", "missing generation", "missing set", "missing space", "missing manifest", "wrong input kind", "unknown kind", "bad time", "wrong requested mode", "silent downgrade", "invalid coverage", "incomplete counts", "foreign source"} {
		t.Run(name, func(t *testing.T) {
			source := map[string]any{"source_id": "source", "source_version_id": "version", "content_version_id": "123e4567-e89b-42d3-a456-426614174000"}
			evidence := map[string]any{"kind": "embedding", "build_id": "selected", "input_id": "input", "input_kind": "rendition_chunk", "input_generation_id": "generation", "embedding_set_id": "set", "vector_space_id": "space", "source_manifest_checksum": strings.Repeat("a", 64), "media_sources": []any{source}}
			mode := "semantic"
			if name == "hybrid" {
				mode = "hybrid"
			}
			selection := map[string]any{"source_id": "source", "source_version_id": "version", "content_version_id": source["content_version_id"], "origin": "generated", "completeness": "complete", "build_id": "selected"}
			coverage := map[string]any{"state": "complete", "scoped_documents": 1, "complete_documents": 1}
			report := map[string]any{"requested_mode": mode, "actual_mode": mode, "media_source_selection": true, "media_selections": []any{selection}, "coverage": coverage, "results": []any{map[string]any{"vault_uid": "fixture", "node_id": 1, "content_version_id": source["content_version_id"], "rank": 1, "excerpt": "telescope arrives Friday", "evidence": []any{evidence}}}}
			switch name {
			case "hybrid":
				report["results"].([]any)[0].(map[string]any)["evidence"] = []any{map[string]any{"kind": "rendition_segment", "build_id": "selected", "segment_id": "segment", "media_sources": []any{source}}, evidence}
			case "missing build":
				delete(evidence, "build_id")
			case "wrong selected build":
				selection["build_id"] = "other"
			case "missing input":
				delete(evidence, "input_id")
			case "missing generation":
				delete(evidence, "input_generation_id")
			case "missing set":
				delete(evidence, "embedding_set_id")
			case "missing space":
				delete(evidence, "vector_space_id")
			case "missing manifest":
				delete(evidence, "source_manifest_checksum")
			case "wrong input kind":
				evidence["input_kind"] = "original_file"
			case "unknown kind":
				evidence["kind"] = "node_name"
			case "bad time":
				evidence["time_span"] = map[string]any{"start_ms": 3, "end_ms": 2}
			case "wrong requested mode":
				report["requested_mode"] = "lexical"
			case "silent downgrade":
				report["actual_mode"] = "lexical"
			case "invalid coverage":
				coverage["state"] = "ready"
			case "incomplete counts":
				coverage["complete_documents"] = 0
			case "foreign source":
				evidence["media_sources"] = []any{map[string]any{"source_id": "other", "source_version_id": "version", "content_version_id": source["content_version_id"]}}
			}
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				require.NoError(t, json.NewEncoder(w).Encode(report))
			}))
			t.Cleanup(remote.Close)
			client, err := NewClient(remote.URL, nil)
			require.NoError(t, err)
			got, err := client.Search(t.Context(), SearchRequest{Query: "arrival", Mode: mode, Limit: 20, Profile: "vectors", ContentFirst: true,
				Fence: &SearchFence{VaultUID: "fixture", ContentVersionIDs: []string{source["content_version_id"].(string)}}, MediaSources: []SearchMediaSelector{{SearchMediaSource: SearchMediaSource{SourceID: "source", SourceVersionID: "version", ContentVersionID: source["content_version_id"].(string)}}}})
			if name == "semantic" || name == "hybrid" {
				require.NoError(t, err)
				require.Len(t, got.Results, 1)
				assert.Equal(t, "telescope arrives Friday", got.Results[0].Excerpt)
			} else {
				require.Error(t, err)
			}
		})
	}
}
