package docbankmedia

import (
	"context"
	"net/http"
	"slices"
	"strings"
)

const MaxSearchVersions = 4096
const MaxSearchResults = 100
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
}

func validSearchTranscript(origin, input, completeness string) bool {
	return completeness != "" && (origin == "generated" && input == "" || origin == "supplied" && input != "")
}

type SearchEvidence struct {
	Kind         string              `json:"kind"`
	BuildID      string              `json:"build_id"`
	SegmentID    string              `json:"segment_id"`
	TimeSpan     *MediaTimeSpan      `json:"time_span,omitempty"`
	MediaSources []SearchMediaSource `json:"media_sources"`
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
}

func validSearchRequest(request SearchRequest) bool {
	return strings.TrimSpace(request.Query) != "" &&
		request.Mode == "lexical" && request.Profile != "" && request.ContentFirst &&
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
	var report SearchReport
	if err := c.jsonRequestLimits(ctx, http.MethodPost, "/api/v1/search", request, &report, maxSearchBytes, maxSearchBytes); err != nil {
		return SearchReport{}, err
	}
	if report.MediaSelections == nil || len(report.MediaSelections) > MaxSearchVersions || len(report.MediaSelections) > len(sources) || !report.MediaSourceSelection || report.ActualMode != "lexical" || report.RequestedMode != "lexical" || report.Results == nil || len(report.Results) > request.Limit ||
		report.Coverage.State == "" || report.Coverage.ScopedDocuments < 0 || report.Coverage.CompleteDocuments < 0 ||
		report.Coverage.CompleteDocuments > report.Coverage.ScopedDocuments {
		return SearchReport{}, ErrInvalidReceipt
	}
	selections := make(map[SearchMediaSource]SearchMediaSelection, len(report.MediaSelections))
	for _, selection := range report.MediaSelections {
		source, known := sources[selection.SearchMediaSource]
		_, duplicate := selections[selection.SearchMediaSource]
		if !known || duplicate || !validSearchTranscript(selection.Origin, selection.SuppliedInputID, selection.Completeness) ||
			(selection.Origin == "supplied" && source.SuppliedInputIDs != nil && !slices.Contains(source.SuppliedInputIDs, selection.SuppliedInputID)) {
			return SearchReport{}, ErrInvalidReceipt
		}
		selections[selection.SearchMediaSource] = selection
	}
	selected := make(map[SearchMediaSource]bool)
	builds := make(map[[2]string]bool)
	for i := range report.Results {
		hit := &report.Results[i]
		if hit.VaultUID != request.Fence.VaultUID || !ids[hit.ContentVersionID] || hit.NodeID <= 0 || hit.Rank <= 0 || len(hit.Excerpt) > 2048 || len(hit.Evidence) != 1 {
			return SearchReport{}, ErrInvalidReceipt
		}
		for _, evidence := range hit.Evidence {
			if evidence.Kind != "rendition_segment" || evidence.BuildID == "" || evidence.SegmentID == "" ||
				len(evidence.MediaSources) == 0 {
				return SearchReport{}, ErrInvalidReceipt
			}
			key := [2]string{hit.ContentVersionID, evidence.BuildID}
			if builds[key] {
				return SearchReport{}, ErrInvalidReceipt
			}
			builds[key] = true
			for _, source := range evidence.MediaSources {
				_, known := selections[source]
				if !known || selected[source] || source.ContentVersionID != hit.ContentVersionID {
					return SearchReport{}, ErrInvalidReceipt
				}
				selected[source] = true
			}
			if evidence.TimeSpan != nil && (evidence.TimeSpan.StartMS < 0 || evidence.TimeSpan.EndMS <= evidence.TimeSpan.StartMS) {
				return SearchReport{}, ErrInvalidReceipt
			}
		}
		hit.Excerpt = strings.ReplaceAll(strings.ReplaceAll(hit.Excerpt, "\x01", ""), "\x02", "")
	}
	return report, nil
}
