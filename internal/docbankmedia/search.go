package docbankmedia

import (
	"context"
	"net/http"
	"slices"
	"strings"
)

const MaxSearchVersions = 4096
const MaxSearchResults = 100
const MaxSearchSuppliedInputs = 64
const maxSearchBytes = 16 << 20

type SearchFence struct {
	VaultUID          string   `json:"vault_uid"`
	ContentVersionIDs []string `json:"content_version_ids"`
}

type SearchRequest struct {
	Query        string                `json:"query"`
	Mode         string                `json:"mode"`
	Limit        int                   `json:"limit"`
	Profile      string                `json:"profile"`
	ContentFirst bool                  `json:"content_first"`
	Fence        *SearchFence          `json:"fence,omitempty"`
	MediaSources []SearchMediaSelector `json:"media_sources,omitempty"`
}

type SearchMediaSource struct {
	SourceID         string `json:"source_id"`
	SourceVersionID  string `json:"source_version_id"`
	ContentVersionID string `json:"content_version_id"`
}

type SearchMediaSelector struct {
	SearchMediaSource

	SuppliedInputIDs []string `json:"supplied_input_ids,omitzero"`
}

type SearchMediaSelection struct {
	SearchMediaSource

	Origin          string `json:"origin"`
	SuppliedInputID string `json:"supplied_input_id,omitempty"`
	Completeness    string `json:"completeness"`
	BuildID         string `json:"build_id,omitempty"`
}

func validSearchTranscript(origin, input, completeness string) bool {
	return (completeness == "complete" || completeness == "partial" || completeness == "degraded_provenance") &&
		(origin == "generated" && input == "" || origin == "supplied" && input != "")
}

type SearchEvidence struct {
	Kind                   string              `json:"kind"`
	BuildID                string              `json:"build_id"`
	SegmentID              string              `json:"segment_id"`
	VectorSpaceID          string              `json:"vector_space_id,omitempty"`
	EmbeddingSetID         string              `json:"embedding_set_id,omitempty"`
	InputGenerationID      string              `json:"input_generation_id,omitempty"`
	InputID                string              `json:"input_id,omitempty"`
	InputKind              string              `json:"input_kind,omitempty"`
	SourceManifestChecksum string              `json:"source_manifest_checksum,omitempty"`
	TimeSpan               *MediaTimeSpan      `json:"time_span,omitempty"`
	MediaSources           []SearchMediaSource `json:"media_sources"`
}

type SearchHit struct {
	VaultUID         string           `json:"vault_uid"`
	NodeID           int64            `json:"node_id"`
	ContentVersionID string           `json:"content_version_id"`
	Excerpt          string           `json:"excerpt"`
	Rank             int              `json:"rank"`
	Evidence         []SearchEvidence `json:"evidence"`
}

type SearchCoverage struct {
	BindingRequired   bool   `json:"binding_required"`
	ScopedDocuments   int    `json:"scoped_documents"`
	CompleteDocuments int    `json:"complete_documents"`
	State             string `json:"state"`
}

type SearchReport struct {
	MediaSourceSelection bool                   `json:"media_source_selection"`
	MediaSelections      []SearchMediaSelection `json:"media_selections"`
	RequestedMode        string                 `json:"requested_mode"`
	ActualMode           string                 `json:"actual_mode"`
	Coverage             SearchCoverage         `json:"coverage"`
	Results              []SearchHit            `json:"results"`
	Truncated            bool                   `json:"truncated"`
	Degradations         []string               `json:"degradations,omitempty"`
}

func validSearchRequest(request SearchRequest) bool {
	return strings.TrimSpace(request.Query) != "" &&
		(request.Mode == "lexical" || request.Mode == "semantic" || request.Mode == "hybrid") && request.Profile != "" && request.ContentFirst &&
		request.Limit > 0 && request.Limit <= MaxSearchResults
}

// ValidateSearch checks an empty local scope without submitting an empty fence.
func (c *Client) ValidateSearch(ctx context.Context, request SearchRequest) error {
	if !validSearchRequest(request) || request.Fence != nil || len(request.MediaSources) != 0 {
		return ErrInvalidRequest
	}
	var response struct {
		Valid bool `json:"valid"`
	}
	if err := c.jsonRequest(ctx, http.MethodPost, "/api/v1/search/validate", request, &response); err != nil {
		return err
	}
	if !response.Valid {
		return ErrInvalidReceipt
	}
	return nil
}

func (c *Client) Search(ctx context.Context, request SearchRequest) (SearchReport, error) {
	if !validSearchRequest(request) || request.Fence == nil || request.Fence.VaultUID == "" || len(request.MediaSources) == 0 {
		return SearchReport{}, ErrInvalidRequest
	}
	ids := make(map[string]bool, len(request.Fence.ContentVersionIDs))
	for _, id := range request.Fence.ContentVersionIDs {
		ids[id] = true
	}
	sources := make(map[SearchMediaSource]SearchMediaSelector, len(request.MediaSources))
	for _, source := range request.MediaSources {
		sources[source.SearchMediaSource] = source
	}
	report, err := c.searchReport(ctx, request)
	if err != nil {
		return SearchReport{}, err
	}
	if report.MediaSelections == nil || len(report.MediaSelections) > MaxSearchVersions || len(report.MediaSelections) > len(sources) || !report.MediaSourceSelection {
		return SearchReport{}, ErrInvalidReceipt
	}
	selections := make(map[SearchMediaSource]SearchMediaSelection, len(report.MediaSelections))
	for _, selection := range report.MediaSelections {
		source, known := sources[selection.SearchMediaSource]
		_, duplicate := selections[selection.SearchMediaSource]
		if !known || duplicate || !validSearchTranscript(selection.Origin, selection.SuppliedInputID, selection.Completeness) ||
			(request.Mode != "lexical" && selection.BuildID == "") ||
			(selection.Origin == "supplied" && source.SuppliedInputIDs != nil && !slices.Contains(source.SuppliedInputIDs, selection.SuppliedInputID)) {
			return SearchReport{}, ErrInvalidReceipt
		}
		selections[selection.SearchMediaSource] = selection
	}
	selected := make(map[SearchMediaSource]bool)
	builds := make(map[[2]string]bool)
	for i := range report.Results {
		hit := &report.Results[i]
		if !validSearchHit(*hit, request.Fence.VaultUID, ids) || len(hit.Evidence) < 1 || len(hit.Evidence) > 2 {
			return SearchReport{}, ErrInvalidReceipt
		}
		// Hybrid carries a lexical and embedding reference for the same selected
		// build. The producer puts the excerpt's evidence first; fan out once.
		hitSources := make(map[SearchMediaSource]bool)
		first := hit.Evidence[0]
		key := [2]string{hit.ContentVersionID, first.BuildID}
		if builds[key] {
			return SearchReport{}, ErrInvalidReceipt
		}
		builds[key] = true
		kinds := make(map[string]bool)
		for _, evidence := range hit.Evidence {
			if !validRecordingEvidence(evidence, report.ActualMode) || evidence.BuildID != first.BuildID ||
				len(evidence.MediaSources) != len(first.MediaSources) || kinds[evidence.Kind] {
				return SearchReport{}, ErrInvalidReceipt
			}
			kinds[evidence.Kind] = true
			withinEvidence := make(map[SearchMediaSource]bool)
			for _, source := range evidence.MediaSources {
				selection, known := selections[source]
				if !known || selected[source] || withinEvidence[source] || source.ContentVersionID != hit.ContentVersionID ||
					(selection.BuildID != "" && selection.BuildID != evidence.BuildID) || !slices.Contains(first.MediaSources, source) {
					return SearchReport{}, ErrInvalidReceipt
				}
				withinEvidence[source], hitSources[source] = true, true
			}
			if evidence.TimeSpan != nil && (evidence.TimeSpan.StartMS < 0 || evidence.TimeSpan.EndMS <= evidence.TimeSpan.StartMS) {
				return SearchReport{}, ErrInvalidReceipt
			}
		}
		for source := range hitSources {
			selected[source] = true
		}
		hit.Excerpt = plainExcerpt(hit.Excerpt)
	}
	return report, nil
}

// searchReport applies the report checks shared by recording and document search.
func (c *Client) searchReport(ctx context.Context, request SearchRequest) (SearchReport, error) {
	var report SearchReport
	if err := c.jsonRequestLimits(ctx, http.MethodPost, "/api/v1/search", request, &report, maxSearchBytes, maxSearchBytes); err != nil {
		return SearchReport{}, err
	}
	modeOK := report.ActualMode == request.Mode || request.Mode == "hybrid" && report.ActualMode == "lexical" &&
		(slices.Contains(report.Degradations, "semantic_unavailable") || slices.Contains(report.Degradations, "semantic_unconfigured"))
	modeOK = modeOK || request.Mode != "lexical" && report.MediaSourceSelection && report.ActualMode == "unavailable" && len(report.Results) == 0 &&
		report.Coverage.State == "incomplete" && report.Coverage.CompleteDocuments == 0
	coverageOK := report.Coverage.State == "complete" || report.Coverage.State == "incomplete" || report.Coverage.State == "unknown"
	if !modeOK || report.RequestedMode != request.Mode || report.Results == nil || len(report.Results) > request.Limit ||
		!coverageOK || report.Coverage.ScopedDocuments < 0 || report.Coverage.CompleteDocuments < 0 ||
		report.Coverage.CompleteDocuments > report.Coverage.ScopedDocuments ||
		(report.Coverage.State == "complete" && report.Coverage.CompleteDocuments != report.Coverage.ScopedDocuments) {
		return SearchReport{}, ErrInvalidReceipt
	}
	return report, nil
}

func validRecordingEvidence(evidence SearchEvidence, mode string) bool {
	if evidence.BuildID == "" || len(evidence.MediaSources) == 0 || len(evidence.MediaSources) > MaxSearchVersions {
		return false
	}
	switch evidence.Kind {
	case "rendition_segment":
		return mode != "semantic" && evidence.SegmentID != ""
	case "embedding":
		return mode != "lexical" && evidence.InputKind == "rendition_chunk" && evidence.InputID != "" &&
			evidence.InputGenerationID != "" && evidence.EmbeddingSetID != "" && evidence.VectorSpaceID != "" &&
			len(evidence.SourceManifestChecksum) == 64 && strings.Trim(evidence.SourceManifestChecksum, "0123456789abcdef") == ""
	default:
		return false
	}
}

// plainExcerpt removes the match markers Docbank puts around each hit.
func plainExcerpt(excerpt string) string {
	return strings.ReplaceAll(strings.ReplaceAll(excerpt, "\x01", ""), "\x02", "")
}

func validSearchHit(hit SearchHit, vault string, ids map[string]bool) bool {
	return hit.VaultUID == vault && ids[hit.ContentVersionID] && hit.NodeID > 0 && hit.Rank > 0 && len(hit.Excerpt) <= 2048
}

// VaultFence fences versions to Docbank's vault via an empty filtered
// fence, so stale versions don't fail it; search skips them on its own.
func (c *Client) VaultFence(ctx context.Context, versions []string) (SearchFence, error) {
	request := map[string]map[string]string{"filters": {"modified_before": "0001-01-01T00:00:00Z"}}
	var response struct {
		Fence SearchFence `json:"fence"`
	}
	if err := c.jsonRequest(ctx, http.MethodPost, "/api/v1/processing/source-fences/resolve", request, &response); err != nil {
		return SearchFence{}, err
	}
	if response.Fence.VaultUID == "" {
		return SearchFence{}, ErrInvalidReceipt
	}
	return SearchFence{VaultUID: response.Fence.VaultUID, ContentVersionIDs: versions}, nil
}

// SearchDocuments searches exact mirrored versions without transcript selectors.
func (c *Client) SearchDocuments(ctx context.Context, request SearchRequest) (SearchReport, error) {
	ids := make(map[string]bool, len(request.Fence.ContentVersionIDs))
	for _, id := range request.Fence.ContentVersionIDs {
		ids[id] = true
	}
	report, err := c.searchReport(ctx, request)
	if err != nil {
		return SearchReport{}, err
	}
	results := make([]SearchHit, 0, len(report.Results))
	for _, hit := range report.Results {
		if !validSearchHit(hit, request.Fence.VaultUID, ids) {
			return SearchReport{}, ErrInvalidReceipt
		}
		content := false
		for _, evidence := range hit.Evidence {
			switch evidence.Kind {
			case "content_blob", "rendition_segment":
				content = true
			case "node_name":
			default:
				return SearchReport{}, ErrInvalidReceipt
			}
		}
		if !content {
			continue
		}
		hit.Excerpt = plainExcerpt(hit.Excerpt)
		results = append(results, hit)
	}
	report.Results = results
	return report, nil
}
