package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/meetingcontent"
	"go.kenn.io/msgvault/internal/personscope/resolver"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

const maxPublicMeetingID = int64(9_007_199_254_740_991)

type meetingScopeResolution struct {
	Scope      store.MeetingQueryScope
	Provenance meetingcontent.ScopeProvenance
}

func (s *Server) resolveDirectMeetingScope(
	ctx context.Context,
	w http.ResponseWriter,
	request *MeetingScopeRequest,
	fields map[string]json.RawMessage,
) (meetingScopeResolution, bool) {
	resolved := meetingScopeResolution{Provenance: meetingcontent.ScopeProvenance{Kind: "direct"}}
	if request == nil {
		return resolved, true
	}
	if !validateOptionalMeetingIDs(w, "message_ids", request.MessageIDs, fields, true) ||
		!validateMeetingIDList(w, "source_ids", request.SourceIDs, fields, false) ||
		!validateMeetingIDList(w, "participant_ids", request.ParticipantIDs, fields, false) {
		return meetingScopeResolution{}, false
	}
	if request.MessageIDs != nil {
		ids := normalizedPublicMeetingIDs(*request.MessageIDs)
		resolved.Scope.MessageIDs = &ids
	}
	resolved.Scope.SourceIDs = normalizedPublicMeetingIDs(request.SourceIDs)
	resolved.Scope.ParticipantIDs = normalizedPublicMeetingIDs(request.ParticipantIDs)

	personProvided := meetingPositiveField(w, "person_id", request.PersonID, fields)
	if personProvided.invalid {
		return meetingScopeResolution{}, false
	}
	participantProvided := meetingPositiveField(w, "participant_id", request.ParticipantID, fields)
	if participantProvided.invalid {
		return meetingScopeResolution{}, false
	}
	if personProvided.present && participantProvided.present {
		writeError(w, http.StatusBadRequest, "invalid_meeting_scope",
			"person_id and participant_id are mutually exclusive")
		return meetingScopeResolution{}, false
	}
	if (personProvided.present || participantProvided.present) && fieldPresent(fields, "participant_ids") {
		writeError(w, http.StatusBadRequest, "invalid_meeting_scope",
			"a singular person reference cannot be combined with participant_ids")
		return meetingScopeResolution{}, false
	}

	if raw, ok := fields["domains"]; ok {
		if isJSONNull(raw) || len(request.Domains) == 0 || len(request.Domains) > 100 {
			writeError(w, http.StatusBadRequest, "invalid_meeting_scope",
				"domains must contain between 1 and 100 values")
			return meetingScopeResolution{}, false
		}
		resolved.Scope.Domains = make([]string, 0, len(request.Domains))
		for _, domain := range request.Domains {
			domain = strings.ToLower(strings.TrimSpace(domain))
			if domain == "" {
				writeError(w, http.StatusBadRequest, "invalid_meeting_scope", "domains must not contain empty values")
				return meetingScopeResolution{}, false
			}
			resolved.Scope.Domains = append(resolved.Scope.Domains, domain)
		}
		slices.Sort(resolved.Scope.Domains)
		resolved.Scope.Domains = slices.Compact(resolved.Scope.Domains)
	}
	if request.After != nil && request.Before != nil && !request.After.Before(*request.Before) {
		writeError(w, http.StatusBadRequest, "invalid_meeting_scope", "after must be before before")
		return meetingScopeResolution{}, false
	}
	resolved.Scope.After = request.After
	resolved.Scope.Before = request.Before
	if _, ok := fields["deletion"]; ok {
		switch request.Deletion {
		case "any", "active", "deleted":
		default:
			writeError(w, http.StatusBadRequest, "invalid_meeting_scope",
				"deletion must be any, active, or deleted")
			return meetingScopeResolution{}, false
		}
	}
	resolved.Scope.Deletion = request.Deletion
	if personProvided.present || participantProvided.present {
		reference := resolver.Reference{Kind: resolver.ReferencePerson, ID: request.PersonID}
		if participantProvided.present {
			reference = resolver.Reference{Kind: resolver.ReferenceParticipant, ID: request.ParticipantID}
		}
		person, err := resolver.Resolve(ctx, s.store, reference, nil)
		if err != nil {
			s.writePersonScopeError(w, reference, err, "meeting")
			return meetingScopeResolution{}, false
		}
		resolved.Scope.Person = &person.Scope
	}
	return resolved, true
}

type meetingOptionalPositive struct {
	present bool
	invalid bool
}

func meetingPositiveField(
	w http.ResponseWriter, name string, value int64, fields map[string]json.RawMessage,
) meetingOptionalPositive {
	raw, present := fields[name]
	if !present {
		return meetingOptionalPositive{}
	}
	if isJSONNull(raw) || value < 1 || value > maxPublicMeetingID {
		writeError(w, http.StatusBadRequest, "invalid_meeting_scope",
			name+" must be a positive JavaScript-safe integer")
		return meetingOptionalPositive{present: true, invalid: true}
	}
	return meetingOptionalPositive{present: true}
}

func validateOptionalMeetingIDs(
	w http.ResponseWriter,
	name string,
	values *[]int64,
	fields map[string]json.RawMessage,
	allowEmpty bool,
) bool {
	raw, present := fields[name]
	if !present {
		return true
	}
	if isJSONNull(raw) || values == nil {
		writeError(w, http.StatusBadRequest, "invalid_meeting_scope", name+" must be an array")
		return false
	}
	return validateMeetingIDs(w, name, *values, allowEmpty)
}

func validateMeetingIDList(
	w http.ResponseWriter,
	name string,
	values []int64,
	fields map[string]json.RawMessage,
	allowEmpty bool,
) bool {
	raw, present := fields[name]
	if !present {
		return true
	}
	if isJSONNull(raw) {
		writeError(w, http.StatusBadRequest, "invalid_meeting_scope", name+" must be an array")
		return false
	}
	return validateMeetingIDs(w, name, values, allowEmpty)
}

func validateMeetingIDs(w http.ResponseWriter, name string, values []int64, allowEmpty bool) bool {
	if (!allowEmpty && len(values) == 0) || len(values) > 100 {
		writeError(w, http.StatusBadRequest, "invalid_meeting_scope",
			fmt.Sprintf("%s must contain between %d and 100 values", name, map[bool]int{true: 0, false: 1}[allowEmpty]))
		return false
	}
	for _, id := range values {
		if id < 1 || id > maxPublicMeetingID {
			writeError(w, http.StatusBadRequest, "invalid_meeting_scope",
				name+" must contain only positive JavaScript-safe integers")
			return false
		}
	}
	return true
}

func normalizedPublicMeetingIDs(values []int64) []int64 {
	result := slices.Clone(values)
	slices.Sort(result)
	return slices.Compact(result)
}

func (s *Server) resolveMeetingExploreScope(
	ctx context.Context,
	w http.ResponseWriter,
	scope MeetingExploreScope,
	selection *ExploreSelection,
	limit int,
	contextExport bool,
) (meetingScopeResolution, bool) {
	predicate := scope.Predicate
	expectedRevision := scope.CacheRevision
	expectedProvenance := scope.SearchProvenance
	snapshotID := scope.CandidateSnapshotID
	selectionRequest := query.ExploreSelectionRequest{}
	if selection != nil {
		if selection.Mode != "explicit" && selection.Mode != "all_matching" {
			writeError(w, http.StatusBadRequest, "invalid_selection", "selection mode must be explicit or all_matching")
			return meetingScopeResolution{}, false
		}
		predicate = selection.Predicate
		expectedRevision = selection.CacheRevision
		expectedProvenance = selection.SearchProvenance
		snapshotID = selection.CandidateSnapshotID
		selectionRequest.ExcludedKeys = selection.Exclusions
		if selection.Mode == "explicit" {
			if len(selection.RowKeys) == 0 {
				writeError(w, http.StatusBadRequest, "invalid_selection", "explicit selection requires row_keys")
				return meetingScopeResolution{}, false
			}
			selectionRequest.IncludedKeys = selection.RowKeys
		}
	}
	if expectedRevision == "" {
		writeError(w, http.StatusBadRequest, "invalid_selection", "cache_revision is required")
		return meetingScopeResolution{}, false
	}
	prepared, err := prepareExplorePredicate(predicate)
	if err != nil {
		s.writeExploreFilterError(w, err, "invalid_selection_predicate")
		return meetingScopeResolution{}, false
	}
	if prepared.request.SearchMode == exploreSearchModeSemantic || prepared.request.SearchMode == exploreSearchModeHybrid {
		if snapshotID == "" {
			writeError(w, http.StatusBadRequest, "candidate_snapshot_required",
				"Semantic and hybrid meeting scope requires the server-issued candidate snapshot")
			return meetingScopeResolution{}, false
		}
		prepared.request.CandidateSnapshotID = snapshotID
	}
	if err := s.resolveExploreIdentityContext(ctx, prepared.request.Filters, &prepared.query.Context); err != nil {
		s.writeExploreFilterError(w, err, "invalid_selection_predicate")
		return meetingScopeResolution{}, false
	}
	searchSpec, resolvedSnapshotID, ok := s.resolveExploreSearch(ctx, w, prepared.request)
	if !ok || !requireCompleteCandidatePool(w, searchSpec) {
		return meetingScopeResolution{}, false
	}
	prepared.query.Search = searchSpec
	selectionRequest.Explore = prepared.query
	meetingResolver, ok := s.queryEngineForContext(ctx).(query.ExploreMeetingResolver)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "meetings_unavailable",
			"Meeting intelligence requires a committed analytical cache")
		return meetingScopeResolution{}, false
	}
	result, err := meetingResolver.ResolveExploreMeetings(ctx, selectionRequest, limit)
	if err != nil {
		s.writeExploreError(ctx, w, err)
		return meetingScopeResolution{}, false
	}
	if result == nil {
		s.writeMeetingError(w, errors.New("meeting resolver returned no result"))
		return meetingScopeResolution{}, false
	}
	if expectedRevision != result.CacheRevision {
		writeError(w, http.StatusConflict, "archive_revision_changed",
			"The committed analytical cache changed; resolve the meeting scope again")
		return meetingScopeResolution{}, false
	}
	if !sameSearchProvenance(expectedProvenance, result.SearchProvenance) {
		writeError(w, http.StatusConflict, "search_revision_changed",
			"The search index revision changed; resolve the meeting scope again")
		return meetingScopeResolution{}, false
	}
	if result.LimitExceeded {
		if contextExport {
			writeError(w, http.StatusBadRequest, "meeting_selection_too_large",
				"Meeting context accepts at most 100 meetings")
		} else {
			writeError(w, http.StatusConflict, "meeting_scope_too_large",
				"Explore meeting scope exceeds 10000 meetings; narrow the predicate")
		}
		return meetingScopeResolution{}, false
	}
	if selection != nil && selection.Mode == "explicit" {
		uniqueKeys := slices.Clone(selection.RowKeys)
		slices.Sort(uniqueKeys)
		uniqueKeys = slices.Compact(uniqueKeys)
		if result.SelectedCount != int64(len(uniqueKeys)) {
			writeError(w, http.StatusBadRequest, "selection_not_all_meetings",
				"Every explicit Explore row must still exist and be a meeting")
			return meetingScopeResolution{}, false
		}
	}
	if contextExport && result.SelectedCount != result.MeetingCount {
		writeError(w, http.StatusBadRequest, "selection_not_all_meetings",
			"Meeting context selections may contain only meetings")
		return meetingScopeResolution{}, false
	}
	if contextExport && (result.MeetingCount < 1 || result.MeetingCount > 100) {
		writeError(w, http.StatusBadRequest, "meeting_selection_too_large",
			"Meeting context requires between 1 and 100 meetings")
		return meetingScopeResolution{}, false
	}
	ids := slices.Clone(result.MessageIDs)
	authority := hashCanonicalValue(struct {
		Predicate           ExploreHTTPRequest     `json:"predicate"`
		CacheRevision       string                 `json:"cache_revision"`
		SearchProvenance    query.SearchProvenance `json:"search_provenance"`
		CandidateSnapshotID string                 `json:"candidate_snapshot_id,omitempty"`
	}{prepared.request, result.CacheRevision, result.SearchProvenance, resolvedSnapshotID}, false)
	return meetingScopeResolution{
		Scope: store.MeetingQueryScope{
			MessageIDs: &ids,
			Deletion:   string(prepared.query.Context.Deletion),
			Authority:  authority,
		},
		Provenance: meetingcontent.ScopeProvenance{
			Kind:                 "explore",
			CacheRevision:        result.CacheRevision,
			LexicalIndexRevision: result.SearchProvenance.LexicalIndexRevision,
			VectorGeneration:     result.SearchProvenance.VectorGeneration,
			CandidateSnapshotID:  resolvedSnapshotID,
		},
	}, true
}

func fieldPresent(fields map[string]json.RawMessage, name string) bool {
	_, ok := fields[name]
	return ok
}
