package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/beeper"
	"go.kenn.io/msgvault/internal/docbankmedia"
	"go.kenn.io/msgvault/internal/personscope"
	"go.kenn.io/msgvault/internal/personscope/resolver"
	"go.kenn.io/msgvault/internal/store"
)

type MediaSearchResult struct {
	MessageID      int64  `json:"message_id"`
	ConversationID int64  `json:"conversation_id"`
	AttachmentID   int64  `json:"attachment_id"`
	Origin         string `json:"origin" enum:"supplied,generated"`
	Excerpt        string `json:"excerpt" doc:"Plain transcript excerpt without search highlight markers"`
	StartMS        *int64 `json:"start_ms,omitempty"`
	EndMS          *int64 `json:"end_ms,omitempty"`
}

type MediaSearchResponse struct {
	Coverage               docbankmedia.SearchCoverage `json:"coverage"`
	PendingOccurrences     int                         `json:"pending_occurrences"`
	UnavailableOccurrences int                         `json:"unavailable_occurrences"`
	AttributionUnavailable int                         `json:"attribution_unavailable" doc:"Current recording occurrences whose selected transcript cannot be attributed, including searches with no hits"`
	Partial                bool                        `json:"partial" doc:"Coverage or occurrence attribution is incomplete, even with no matching excerpts"`
	Truncated              bool                        `json:"truncated"`
	Results                []MediaSearchResult         `json:"results"`
}

func (s *Server) handleMediaSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	mode := r.URL.Query().Get("mode")
	limit, found, err := queryInt(r, "limit")
	if !found {
		limit = 20
	}
	personID, _, personErr := queryInt64(r, "person_id")
	directions := make([]personscope.Direction, 0)
	for _, value := range r.URL.Query()["direction"] {
		for direction := range strings.SplitSeq(value, ",") {
			directions = append(directions, personscope.Direction(direction))
		}
	}
	if err != nil || personErr != nil || q == "" || len(q) > 8192 || limit < 1 || limit > 100 || personID < 0 ||
		(len(directions) > 0 && personID == 0) || (r.URL.Query().Has("person_id") && personID == 0) {
		s.rejectBadParam(w, errors.New("q is required and must be at most 8192 bytes, limit must be 1 to 100, and directions require a positive person_id"))
		return
	}
	if _, _, err := resolver.NormalizeDirections(directions); err != nil {
		s.rejectBadParam(w, errors.New("direction must be from_person, to_person, or group"))
		return
	}
	if mode == "semantic" || mode == "hybrid" {
		writeError(w, http.StatusServiceUnavailable, "media_search_mode_unavailable", "Media search supports lexical mode")
		return
	}
	if mode != "" && mode != "lexical" {
		s.rejectBadParam(w, errors.New("mode must be lexical, semantic, or hybrid"))
		return
	}
	reader := s.messageRecordings
	if reader == nil || reader.Client == nil {
		writeError(w, http.StatusServiceUnavailable, "media_search_unavailable", "Configure the Docbank integration to search transcripts")
		return
	}
	resolve := func() (*personscope.Scope, error) {
		if personID == 0 {
			return nil, nil //nolint:nilnil // No person scope searches the full visible archive.
		}
		resolution, err := resolver.Resolve(r.Context(), reader.Store, resolver.Reference{Kind: resolver.ReferencePerson, ID: personID}, directions)
		return &resolution.Scope, err
	}
	scope, err := resolve()
	if err != nil {
		s.writePersonScopeError(w, resolver.Reference{Kind: resolver.ReferencePerson, ID: personID}, err, "media")
		return
	}
	response, err := reader.search(r.Context(), q, limit, scope, resolve)
	if err != nil {
		if scopeErr, ok := errors.AsType[*mediaSearchScopeError](err); ok {
			s.writePersonScopeError(w, resolver.Reference{Kind: resolver.ReferencePerson, ID: personID}, scopeErr.err, "media")
			return
		}
		if s.writeIfContextError(w, err) {
			return
		}
		httpErr, httpError := errors.AsType[*docbankmedia.HTTPError](err)
		switch {
		case errors.Is(err, errMediaSearchScope):
			writeError(w, http.StatusBadRequest, "media_search_scope_limit", "Search supports at most 4096 media versions and source selectors, and 64 distinct current captions per recording source; set --person in the CLI or person_id in the API to narrow the scope")
		case httpError && httpErr.Status == http.StatusBadRequest:
			writeError(w, http.StatusBadRequest, "invalid_media_search", "Docbank rejected the search query")
		default:
			s.logger.Warn("search media transcripts", "code", docbankmedia.ErrorCode(err))
			writeError(w, http.StatusServiceUnavailable, "media_search_unavailable", "Docbank transcript search is unavailable")
		}
		return
	}
	writeJSON(w, http.StatusOK, response)
}

var errMediaSearchScope = errors.New("media search scope exceeds the supported fence")

type mediaSearchScopeError struct{ err error }

func (e *mediaSearchScopeError) Error() string { return e.err.Error() }
func (e *mediaSearchScopeError) Unwrap() error { return e.err }

func searchEligible(o store.MessageMediaOccurrence, origin string, revisions map[int64]string) bool {
	return o.RetentionState == store.BeeperMediaRetentionRetained && o.VaultUID != "" && o.DocbankSourceID != "" &&
		o.SourceVersionID != "" && o.ContentVersionID != "" && transcriptRevisionMatches(o, origin, revisions[o.AttachmentID])
}

func (reader *MessageRecordingReader) searchRevisions(ctx context.Context, occurrences []store.MessageMediaOccurrence) (map[int64]string, error) {
	ids := make([]int64, 0, len(occurrences))
	seen := make(map[int64]bool)
	for _, o := range occurrences {
		if !seen[o.AttachmentID] {
			seen[o.AttachmentID] = true
			ids = append(ids, o.AttachmentID)
		}
	}
	return beeper.MediaRevisions(ctx, reader.Store, ids)
}

func mediaSearchSource(o store.MessageMediaOccurrence) docbankmedia.SearchMediaSource {
	return docbankmedia.SearchMediaSource{SourceID: o.DocbankSourceID, SourceVersionID: o.SourceVersionID, ContentVersionID: o.ContentVersionID}
}

func (reader *MessageRecordingReader) search(ctx context.Context, query string, limit int, scope *personscope.Scope, resolve func() (*personscope.Scope, error)) (MediaSearchResponse, error) {
	response := MediaSearchResponse{Results: []MediaSearchResult{}, Coverage: docbankmedia.SearchCoverage{State: "unknown"}}
	occurrences, err := reader.Store.ListMediaSearchOccurrences(ctx, reader.Destination, scope)
	if err != nil {
		return response, err
	}
	versions := make(map[string]bool)
	selectors := []docbankmedia.SearchMediaSelector{}
	selectorIndex := make(map[docbankmedia.SearchMediaSource]int)
	fence := docbankmedia.SearchFence{}
	for _, o := range occurrences {
		if !searchEligible(o, "generated", nil) {
			continue
		}
		fence.VaultUID = o.VaultUID
		if !versions[o.ContentVersionID] {
			versions[o.ContentVersionID] = true
			fence.ContentVersionIDs = append(fence.ContentVersionIDs, o.ContentVersionID)
		}
		source := mediaSearchSource(o)
		if _, exists := selectorIndex[source]; !exists {
			selectorIndex[source] = len(selectors)
			selectors = append(selectors, docbankmedia.SearchMediaSelector{SearchMediaSource: source, SuppliedInputIDs: []string{}})
		}
	}
	if len(fence.ContentVersionIDs) > docbankmedia.MaxSearchVersions || len(selectors) > docbankmedia.MaxSearchVersions {
		return response, errMediaSearchScope
	}
	supplied := []store.MessageMediaOccurrence{}
	for _, o := range occurrences {
		if searchEligible(o, "generated", nil) && o.SuppliedInputID != "" {
			supplied = append(supplied, o)
		}
	}
	revisions, err := reader.searchRevisions(ctx, supplied)
	if err != nil {
		return response, err
	}
	for _, o := range supplied {
		if transcriptRevisionMatches(o, "supplied", revisions[o.AttachmentID]) {
			selector := &selectors[selectorIndex[mediaSearchSource(o)]]
			if !slices.Contains(selector.SuppliedInputIDs, o.SuppliedInputID) {
				if len(selector.SuppliedInputIDs) == docbankmedia.MaxSearchSuppliedInputs {
					return response, errMediaSearchScope
				}
				selector.SuppliedInputIDs = append(selector.SuppliedInputIDs, o.SuppliedInputID)
			}
		}
	}
	remoteCtx, cancel := context.WithTimeout(ctx, docbankBudget(ctx))
	defer cancel()
	request := docbankmedia.SearchRequest{Query: query, Mode: "lexical", Limit: docbankmedia.MaxSearchResults, Profile: suppliedTranscriptProfile, ContentFirst: true}
	var report docbankmedia.SearchReport
	if len(fence.ContentVersionIDs) == 0 {
		if err := reader.Client.ValidateSearch(remoteCtx, request); err != nil {
			return response, err
		}
		response.Coverage.State = "complete"
	} else {
		request.Fence, request.MediaSources = &fence, selectors
		report, err = reader.Client.Search(remoteCtx, request)
		if err != nil {
			return response, err
		}
		response.Coverage, response.Truncated = report.Coverage, report.Truncated
		response.Partial = response.Partial || report.Coverage.State != "complete"
	}
	// Resolve again so an identity change during remote work cannot broaden presentation.
	scope, err = resolve()
	if err != nil {
		return response, &mediaSearchScopeError{err}
	}
	current, err := reader.Store.ListMediaSearchOccurrences(ctx, reader.Destination, scope)
	if err != nil {
		return response, err
	}
	selections := make(map[docbankmedia.SearchMediaSource]docbankmedia.SearchMediaSelection, len(report.MediaSelections))
	for _, selection := range report.MediaSelections {
		selections[selection.SearchMediaSource] = selection
		response.Partial = response.Partial || selection.Completeness != "complete"
	}
	supplied = supplied[:0]
	fresh := make(map[string]store.MessageMediaOccurrence, len(current))
	for _, o := range current {
		fresh[o.OccurrenceRef] = o
		if selection, found := selections[mediaSearchSource(o)]; o.SuppliedInputID != "" && (!found || selection.Origin == "supplied") {
			supplied = append(supplied, o)
		}
	}
	revisions, err = reader.searchRevisions(ctx, supplied)
	if err != nil {
		return response, err
	}
	before := make(map[string]store.MessageMediaOccurrence, len(occurrences))
	for _, o := range occurrences {
		before[o.OccurrenceRef] = o
		if _, exists := fresh[o.OccurrenceRef]; !exists {
			response.Partial = true
		}
	}
	response.UnavailableOccurrences, err = reader.Store.MediaSearchLocalGaps(ctx, reader.Destination, scope)
	if err != nil {
		return response, err
	}
	currentSources := make(map[docbankmedia.SearchMediaSource][]store.MessageMediaOccurrence)
	for _, o := range current {
		previous, exists := before[o.OccurrenceRef]
		source := mediaSearchSource(o)
		stable := exists && o.MessageID == previous.MessageID && o.ConversationID == previous.ConversationID && o.AttachmentID == previous.AttachmentID &&
			o.VaultUID == previous.VaultUID && source == mediaSearchSource(previous)
		response.Partial = response.Partial || !stable
		selection, selected := selections[source]
		if !selected {
			currentRevision := o.SuppliedInputID == "" || transcriptRevisionMatches(o, "supplied", revisions[o.AttachmentID])
			if currentRevision && reader.UploadConsent && (o.RetentionState == store.BeeperMediaRetentionPending || o.RetentionState == store.BeeperMediaRetentionSourceUnavailable ||
				o.DeliveryPhase == "pending-artifact" || o.DeliveryPhase == "pending-process" || o.DeliveryPhase == "observing") {
				response.PendingOccurrences++
			} else {
				response.UnavailableOccurrences++
			}
			continue
		}
		if !stable || !searchEligible(o, selection.Origin, revisions) ||
			(selection.Origin == "supplied" && (o.Revision != previous.Revision || !suppliedTranscriptMatches(o, selection.SuppliedInputID))) {
			response.AttributionUnavailable++
			response.Partial = true
			continue
		}
		currentSources[source] = append(currentSources[source], o)
	}
	response.Partial = response.Partial || response.PendingOccurrences+response.UnavailableOccurrences > 0
	for _, hit := range report.Results {
		evidence := hit.Evidence[0]
		for _, source := range evidence.MediaSources {
			for _, o := range currentSources[source] {
				if len(response.Results) == limit {
					response.Truncated = true
					return response, nil
				}
				result := MediaSearchResult{MessageID: o.MessageID, ConversationID: o.ConversationID, AttachmentID: o.AttachmentID, Origin: selections[source].Origin, Excerpt: hit.Excerpt}
				if evidence.TimeSpan != nil {
					result.StartMS, result.EndMS = &evidence.TimeSpan.StartMS, &evidence.TimeSpan.EndMS
				}
				response.Results = append(response.Results, result)
			}
		}
	}
	return response, nil
}
