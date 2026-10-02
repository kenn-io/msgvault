package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/apiprotocol"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/pkg/client/generated"
)

const mcpFactTargetsPath = "/api/v1/person-fact-targets"
const mcpFactTrackingPath = "/api/v1/people/{id}/tracking"
const mcpFactMergePath = "/api/v1/person-merges/{merge_id}"
const mcpFactOffsetKey = "offset"
const mcpFactFieldsKey = "fields"
const mcpFactInvalidArgumentCode = "invalid_operation_arguments"

var mcpFactRoutes = []mcpRecordRoute{
	{name: "list_fact_targets", id: "listPersonFactTargets", method: http.MethodGet, path: mcpFactTargetsPath, query: []string{mcpRelationshipSensitiveKey}},
	{name: "list_person_fact_evidence", id: "listPersonFactEvidence", method: http.MethodGet, path: "/api/v1/people/{id}/fact-evidence", query: []string{"target", mcpRelationshipLimitKey, mcpFactOffsetKey}},
	{name: "list_person_fact_status_events", id: "listPersonFactEvidenceStatusEvents", method: http.MethodGet, path: "/api/v1/people/{id}/fact-evidence-status-events", query: []string{"evidence_key", "supported", mcpRelationshipLimitKey, mcpFactOffsetKey}},
	{name: "list_person_fact_claims", id: "listPersonFactClaims", method: http.MethodGet, path: "/api/v1/people/{id}/fact-claims", query: []string{"target", mcpRelationshipLimitKey, mcpFactOffsetKey}, contextID: "listAttributeDefinitions", contextPath: mcpRecordDefinitionsPath, contextQuery: []string{"object_type", "include_hidden"}},
	{name: "list_person_fact_decisions", id: "listPersonFactDecisions", method: http.MethodGet, path: "/api/v1/people/{id}/fact-decisions", query: []string{"target", mcpRelationshipLimitKey, mcpFactOffsetKey}},
	{name: "list_person_fact_pins", id: "listPersonFactPins", method: http.MethodGet, path: "/api/v1/people/{id}/fact-pins"},
	{name: "set_person_fact_pin", id: "setPersonFactPin", method: http.MethodPut, path: "/api/v1/people/{id}/fact-pins/{kind}/{key}", properties: []string{"pinned"}, contextID: "listPersonFactPins", contextPath: "/api/v1/people/{id}/fact-pins"},
	{name: "get_person_tracking", id: "getPersonTracking", method: http.MethodGet, path: mcpFactTrackingPath},
	{name: "set_person_tracking", id: "setPersonTracking", method: http.MethodPut, path: mcpFactTrackingPath, properties: []string{"tracked"}, contextID: "getPersonTracking", contextPath: mcpFactTrackingPath},
	{name: "list_person_merges", id: "listPersonMerges", method: http.MethodGet, path: "/api/v1/people/{id}/merges", query: []string{mcpRelationshipLimitKey, mcpFactOffsetKey}},
	{name: "get_person_merge", id: "getPersonMerge", method: http.MethodGet, path: mcpFactMergePath},
	{name: "get_person_merge_snapshot", id: "getPersonMergeSnapshot", method: http.MethodGet, path: mcpFactMergePath + "/snapshot"},
	{name: "review_person_merge_candidate", id: "decidePersonMergeCandidate", method: http.MethodPost, path: "/api/v1/person-merge-candidates/{candidate_id}/decision", properties: []string{mcpRecordPersonIDKey, "decision"}, contextID: "getPersonMerge", contextPath: mcpFactMergePath},
	{name: "split_person", id: "splitPersonMerge", method: http.MethodPost, path: "/api/v1/people/{id}/split", properties: []string{"merge_id", "participant_ids"}, contextID: "getPersonMerge", contextPath: mcpFactMergePath},
}

func factMCPCapabilities(hasRoute mcpRouteCheck, capabilities *apiprotocol.MCPCapabilities) []string {
	names := []string{}
	for _, route := range mcpFactRoutes {
		if route.contextID != "" && !hasRoute(route.contextID, http.MethodGet, route.contextPath, route.contextQuery...) {
			continue
		}
		if route.name == "set_person_fact_pin" && (!hasRoute("listPersonFactTargets", http.MethodGet, mcpFactTargetsPath, mcpRelationshipSensitiveKey) || !hasRoute("getPersonTracking", http.MethodGet, mcpFactTrackingPath)) {
			continue
		}
		if slices.Contains([]string{"split_person", "review_person_merge_candidate"}, route.name) && !hasRoute("getPersonStructuredProfile", http.MethodGet, mcpRecordProfilePath) {
			continue
		}
		if hasRoute(route.id, route.method, route.path, route.query...) && (len(route.properties) == 0 || mcpRequestPropertiesPresent(capabilities, route.id, route.properties...)) {
			names = append(names, route.name)
		}
	}
	return names
}

type mcpFactInput struct {
	PersonID         int64                                                `json:"person_id"`
	MergeID          int64                                                `json:"merge_id"`
	CandidateID      int64                                                `json:"candidate_id"`
	Target           *string                                              `json:"target,omitzero"`
	EvidenceKey      *string                                              `json:"evidence_key,omitzero"`
	Supported        *bool                                                `json:"supported,omitzero"`
	Limit            *int64                                               `json:"limit,omitzero"`
	Offset           *int64                                               `json:"offset,omitzero"`
	Kind             *generated.SetPersonFactPinPathKind                  `json:"kind,omitzero"`
	Key              *string                                              `json:"key,omitzero"`
	Pinned           *bool                                                `json:"pinned,omitzero"`
	Tracked          *bool                                                `json:"tracked,omitzero"`
	Fields           *[]string                                            `json:"fields,omitzero"`
	IncludeSensitive bool                                                 `json:"include_sensitive"`
	ETag             string                                               `json:"etag"`
	IdempotencyKey   string                                               `json:"idempotency_key"`
	ParticipantIDs   []int64                                              `json:"participant_ids"`
	Decision         *generated.DecidePersonMergeCandidateRequestDecision `json:"decision,omitzero"`
}

func mcpFactArguments(name string, args map[string]any) (mcpFactInput, error) {
	var allowed []string
	switch name {
	case "list_fact_targets":
		allowed = []string{mcpRelationshipSensitiveKey}
	case "list_person_fact_evidence", "list_person_fact_claims":
		allowed = []string{mcpRecordPersonIDKey, "target", mcpRelationshipLimitKey, mcpFactOffsetKey, mcpFactFieldsKey, mcpRelationshipSensitiveKey}
	case "list_person_fact_decisions":
		allowed = []string{mcpRecordPersonIDKey, "target", mcpRelationshipLimitKey, mcpFactOffsetKey}
	case "list_person_fact_status_events":
		allowed = []string{mcpRecordPersonIDKey, "evidence_key", "supported", mcpRelationshipLimitKey, mcpFactOffsetKey}
	case "list_person_fact_pins", "get_person_tracking":
		allowed = []string{mcpRecordPersonIDKey}
	case "set_person_fact_pin":
		allowed = []string{mcpRecordPersonIDKey, "kind", "key", "pinned"}
	case "set_person_tracking":
		allowed = []string{mcpRecordPersonIDKey, "tracked"}
	case "list_person_merges":
		allowed = []string{mcpRecordPersonIDKey, mcpRelationshipLimitKey, mcpFactOffsetKey}
	case "get_person_merge":
		allowed = []string{"merge_id"}
	case "get_person_merge_snapshot":
		allowed = []string{"merge_id", mcpFactFieldsKey, mcpRelationshipSensitiveKey}
	case "review_person_merge_candidate":
		allowed = []string{"merge_id", mcpRecordPersonIDKey, "candidate_id", mcpRelationshipETagKey, "decision"}
	case "split_person":
		allowed = []string{mcpRecordPersonIDKey, "merge_id", "participant_ids", mcpRelationshipETagKey, "idempotency_key"}
	default:
		return mcpFactInput{}, errors.New("unknown fact operation")
	}
	for key, value := range args {
		if !slices.Contains(allowed, key) || value == nil {
			return mcpFactInput{}, errors.New("invalid fact argument")
		}
	}
	input, err := decodeMCPOperationArguments[mcpFactInput](args)
	if err != nil {
		return input, err
	}
	for key, id := range map[string]int64{mcpRecordPersonIDKey: input.PersonID, "merge_id": input.MergeID, "candidate_id": input.CandidateID} {
		if slices.Contains(allowed, key) && !mcpPositiveSafeID(id) {
			return input, errors.New("invalid fact ID")
		}
	}
	maxLimit := int64(200)
	if name == "list_person_merges" {
		maxLimit = 500
	}
	if input.Limit != nil && (*input.Limit < 1 || *input.Limit > maxLimit) || input.Offset != nil && (*input.Offset < 0 || *input.Offset > 9007199254740991) {
		return input, errors.New("invalid fact page")
	}
	if input.Target != nil {
		if _, err := personfacts.DecodeTargetRef(*input.Target); err != nil {
			return input, err
		}
	}
	if input.EvidenceKey != nil && !mcpBoundedOpaqueValue(*input.EvidenceKey, 512) {
		return input, errors.New("invalid evidence key")
	}
	if slices.Contains(allowed, mcpRelationshipETagKey) && !mcpBoundedOpaqueValue(input.ETag, 512) {
		return input, errors.New("invalid person ETag")
	}
	if name == "set_person_fact_pin" && (input.Kind == nil || input.Kind.Validate() != nil || input.Key == nil || !mcpBoundedOpaqueValue(*input.Key, 512) || input.Pinned == nil) {
		return input, errors.New("invalid fact pin")
	}
	if name == "set_person_tracking" && input.Tracked == nil {
		return input, errors.New("missing tracking state")
	}
	if name == "review_person_merge_candidate" && (input.Decision == nil || input.Decision.Validate() != nil) {
		return input, errors.New("invalid merge decision")
	}
	if name == "split_person" {
		if !mcpBoundedOpaqueValue(input.IdempotencyKey, 128) || input.ParticipantIDs == nil {
			return input, errors.New("invalid split arguments")
		}
		seen := map[int64]bool{}
		for _, id := range input.ParticipantIDs {
			if !mcpPositiveSafeID(id) || seen[id] {
				return input, errors.New("invalid split participants")
			}
			seen[id] = true
		}
	}
	if input.Fields != nil {
		permitted := []string{"persons", "rows", "columns"}
		if name == "list_person_fact_evidence" {
			permitted = []string{"excerpt", "source_details"}
		}
		if name == "list_person_fact_claims" {
			permitted = []string{"values"}
		}
		seen := map[string]bool{}
		for _, field := range *input.Fields {
			if !slices.Contains(permitted, field) || seen[field] {
				return input, errors.New("invalid fact fields")
			}
			seen[field] = true
		}
	}
	return input, nil
}

func (b *daemonMCPOperations) executeFactOperation(ctx context.Context, name string, args map[string]any) (*mcpserver.OperationResult, bool, error) {
	var route *mcpRecordRoute
	for i := range mcpFactRoutes {
		if mcpFactRoutes[i].name == name {
			route = &mcpFactRoutes[i]
			break
		}
	}
	if route == nil {
		return nil, false, nil
	}
	input, err := mcpFactArguments(name, args)
	if err != nil {
		return operationFailure(mcpFactInvalidArgumentCode, false), true, nil //nolint:nilerr // Parser text remains private.
	}
	var result *mcpserver.OperationResult
	switch name {
	case "list_fact_targets":
		result, err = readMCPOperationJSON[generated.Catalog](ctx, b.client, route.method, route.path, &generated.ListPersonFactTargetsRequestOptions{Query: &generated.ListPersonFactTargetsQuery{IncludeSensitive: new(input.IncludeSensitive)}}, http.StatusOK)
	case "list_person_fact_evidence":
		result, err = readMCPOperationJSON[generated.PersonFactEvidenceResponse](ctx, b.client, route.method, route.path, &generated.ListPersonFactEvidenceRequestOptions{PathParams: &generated.ListPersonFactEvidencePath{ID: input.PersonID}, Query: &generated.ListPersonFactEvidenceQuery{Target: input.Target, Limit: input.Limit, Offset: input.Offset}}, http.StatusOK)
	case "list_person_fact_status_events":
		result, err = readMCPOperationJSON[generated.PersonFactEvidenceStatusEventsResponse](ctx, b.client, route.method, route.path, &generated.ListPersonFactEvidenceStatusEventsRequestOptions{PathParams: &generated.ListPersonFactEvidenceStatusEventsPath{ID: input.PersonID}, Query: &generated.ListPersonFactEvidenceStatusEventsQuery{EvidenceKey: input.EvidenceKey, Supported: input.Supported, Limit: input.Limit, Offset: input.Offset}}, http.StatusOK)
	case "list_person_fact_claims":
		result, err = readMCPOperationJSON[generated.PersonFactClaimsResponse](ctx, b.client, route.method, route.path, &generated.ListPersonFactClaimsRequestOptions{PathParams: &generated.ListPersonFactClaimsPath{ID: input.PersonID}, Query: &generated.ListPersonFactClaimsQuery{Target: input.Target, Limit: input.Limit, Offset: input.Offset}}, http.StatusOK)
	case "list_person_fact_decisions":
		result, err = readMCPOperationJSON[generated.PersonFactDecisionsResponse](ctx, b.client, route.method, route.path, &generated.ListPersonFactDecisionsRequestOptions{PathParams: &generated.ListPersonFactDecisionsPath{ID: input.PersonID}, Query: &generated.ListPersonFactDecisionsQuery{Target: input.Target, Limit: input.Limit, Offset: input.Offset}}, http.StatusOK)
	case "list_person_fact_pins":
		result, err = readMCPOperationJSON[generated.PersonFactPinsResponse](ctx, b.client, route.method, route.path, &generated.ListPersonFactPinsRequestOptions{PathParams: &generated.ListPersonFactPinsPath{ID: input.PersonID}}, http.StatusOK)
	case "set_person_fact_pin":
		result, err = readMCPOperationJSON[generated.PersonFactPinWrite](ctx, b.client, route.method, route.path, &generated.SetPersonFactPinRequestOptions{PathParams: &generated.SetPersonFactPinPath{ID: input.PersonID, Kind: *input.Kind, Key: *input.Key}, Body: &generated.SetPersonFactPinRequest{Pinned: *input.Pinned}}, http.StatusOK)
	case "get_person_tracking":
		result, err = readMCPOperationJSON[generated.PersonTracking](ctx, b.client, route.method, route.path, &generated.GetPersonTrackingRequestOptions{PathParams: &generated.GetPersonTrackingPath{ID: input.PersonID}}, http.StatusOK)
	case "set_person_tracking":
		result, err = readMCPOperationJSON[generated.PersonTracking](ctx, b.client, route.method, route.path, &generated.SetPersonTrackingRequestOptions{PathParams: &generated.SetPersonTrackingPath{ID: input.PersonID}, Body: &generated.PutPersonTrackingRequest{Tracked: *input.Tracked}}, http.StatusOK)
	case "list_person_merges":
		result, err = readMCPOperationJSON[generated.PersonMergesResponse](ctx, b.client, route.method, route.path, &generated.ListPersonMergesRequestOptions{PathParams: &generated.ListPersonMergesPath{ID: input.PersonID}, Query: &generated.ListPersonMergesQuery{Limit: input.Limit, Offset: input.Offset}}, http.StatusOK)
	case "get_person_merge":
		result, err = readMCPOperationJSON[generated.PersonMergeDetail](ctx, b.client, route.method, route.path, &generated.GetPersonMergeRequestOptions{PathParams: &generated.GetPersonMergePath{MergeID: input.MergeID}}, http.StatusOK)
	case "get_person_merge_snapshot":
		result, err = readMCPOperationJSON[generated.PersonMergeSnapshotResponse](ctx, b.client, route.method, route.path, &generated.GetPersonMergeSnapshotRequestOptions{PathParams: &generated.GetPersonMergeSnapshotPath{MergeID: input.MergeID}}, http.StatusOK)
	case "review_person_merge_candidate":
		result, err = readMCPOperationJSON[generated.PersonMergeReviewCandidate](ctx, b.client, route.method, route.path, &generated.DecidePersonMergeCandidateRequestOptions{PathParams: &generated.DecidePersonMergeCandidatePath{CandidateID: input.CandidateID}, Header: &generated.DecidePersonMergeCandidateHeaders{IfMatch: input.ETag}, Body: &generated.DecidePersonMergeCandidateRequest{PersonID: input.PersonID, Decision: *input.Decision}}, http.StatusOK)
	case "split_person":
		result, err = readMCPOperationJSON[generated.PersonSplitResult](ctx, b.client, route.method, route.path, &generated.SplitPersonMergeRequestOptions{PathParams: &generated.SplitPersonMergePath{ID: input.PersonID}, Header: &generated.SplitPersonMergeHeaders{IfMatch: input.ETag, IdempotencyKey: input.IdempotencyKey}, Body: &generated.SplitPersonRequest{MergeID: input.MergeID, ParticipantIds: input.ParticipantIDs}}, http.StatusOK)
	}
	if err == nil && result != nil && !result.IsError {
		result, err = b.projectFactResult(ctx, result, input)
	}
	return result, true, err
}

// Raw columns retain the native portable SQL value representation and precision.
type mcpFactSnapshot struct {
	Version int64                           `json:"version"`
	Persons []mcpserver.MergeSnapshotPerson `json:"persons"`
	Rows    []mcpserver.MergeSnapshotRow    `json:"rows"`
}

func (b *daemonMCPOperations) projectFactResult(ctx context.Context, result *mcpserver.OperationResult, input mcpFactInput) (*mcpserver.OperationResult, error) {
	selected := func(field string) bool { return input.Fields != nil && slices.Contains(*input.Fields, field) }
	switch value := result.Output.(type) {
	case generated.PersonFactEvidenceResponse:
		for i := range value.Evidence {
			if !input.IncludeSensitive || !selected("excerpt") {
				value.Evidence[i].Excerpt = nil
			}
			if !input.IncludeSensitive || !selected("source_details") {
				value.Evidence[i].SourceRef = nil
				value.Evidence[i].SourceURL = nil
				value.Evidence[i].SubjectRef = nil
			}
		}
		result.Output = value
	case generated.PersonFactClaimsResponse:
		objectType := "person"
		definitions, err := readMCPOperationJSON[generated.AttributeDefinitionsResponse](ctx, b.client, http.MethodGet, mcpRecordDefinitionsPath, &generated.ListAttributeDefinitionsRequestOptions{Query: &generated.ListAttributeDefinitionsQuery{ObjectType: &objectType, IncludeHidden: new(true)}}, http.StatusOK)
		if err != nil || definitions == nil || definitions.IsError {
			return operationFailure("operation_context_unavailable", false), err
		}
		native, ok := definitions.Output.(generated.AttributeDefinitionsResponse)
		if !ok {
			return operationFailure("invalid_operation_response", false), nil
		}
		metadata := map[string]generated.AttributeDefinition{}
		for _, definition := range native.Definitions {
			metadata[definition.UniversalID] = definition
		}
		claims := make([]mcpserver.FactClaimSelection, 0, len(value.Claims))
		for _, claim := range value.Claims {
			definition, known := metadata[claim.Target.Key]
			known = known && claim.Target.Kind == "attribute" || claim.Target.Kind == "employment" && claim.Target.Key == "system:employment"
			sensitive := definition.IsSensitive || strings.EqualFold(definition.Slug, "notes")
			allowValues := known && (input.Fields == nil || selected("values")) && (!sensitive || input.IncludeSensitive && selected("values"))
			projected := mcpserver.FactClaimSelection{PersonFactClaim: claim}
			if allowValues {
				projected.SubmittedValue = &claim.SubmittedValue
			} else {
				projected.NormalizedValue = nil
			}
			claims = append(claims, projected)
		}
		result.Output = mcpserver.FactClaimSelections{Claims: claims}
	case generated.PersonMergeSnapshotResponse:
		if value.Version != 1 || len(value.Sha256) != 64 {
			return operationFailure("invalid_operation_response", false), nil
		}
		var snapshot mcpFactSnapshot
		if err := json.Unmarshal(value.Snapshot, &snapshot, json.RejectUnknownMembers(true)); err != nil {
			return operationFailure("invalid_operation_response", false), err
		}
		if snapshot.Version != value.Version {
			return operationFailure("invalid_operation_response", false), nil
		}
		fields := []string{"persons", "rows"}
		if input.Fields != nil {
			fields = *input.Fields
		}
		projected := mcpserver.MergeSnapshotSelection{Version: value.Version, SourceSHA256: value.Sha256, SourceVerified: true}
		if slices.Contains(fields, "persons") {
			projected.Persons = &snapshot.Persons
		}
		if slices.Contains(fields, "rows") || slices.Contains(fields, "columns") {
			if !input.IncludeSensitive || !selected("columns") {
				for i := range snapshot.Rows {
					snapshot.Rows[i].Columns = nil
				}
			}
			projected.Rows = &snapshot.Rows
		}
		result.Output = projected
	case generated.PersonMergeReviewCandidate:
		if result.ETag == "" {
			return operationFailure("invalid_operation_response", true), nil
		}
		result.Output = mcpserver.MergeCandidateDecision{ETag: result.ETag, Candidate: value}
	case generated.PersonSplitResult:
		if result.ETag == "" || result.NewPersonETag == "" {
			return operationFailure("invalid_operation_response", true), nil
		}
		result.Output = mcpserver.PersonSplitRecord{ETag: result.ETag, NewPersonETag: result.NewPersonETag, Result: value}
	}
	return result, nil
}

func (b *daemonMCPOperations) factOperationDisclosure(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	if !slices.Contains([]string{"set_person_fact_pin", "set_person_tracking", "review_person_merge_candidate", "split_person"}, name) {
		return "", false, nil
	}
	input, err := mcpFactArguments(name, args)
	if err != nil {
		return "", true, &mcpserver.OperationRefusalError{Code: mcpFactInvalidArgumentCode}
	}
	current := map[string]any{}
	read := func(label, tool string, arguments map[string]any) error {
		result, handled, err := b.executeFactOperation(ctx, tool, arguments)
		if err != nil {
			return err
		}
		if !handled || result == nil || result.IsError {
			return &mcpserver.OperationRefusalError{Code: "operation_context_unavailable"}
		}
		current[label] = result.Output
		return nil
	}
	switch name {
	case "set_person_tracking":
		err = read("tracking", "get_person_tracking", map[string]any{mcpRecordPersonIDKey: input.PersonID})
	case "set_person_fact_pin":
		err = read("tracking", "get_person_tracking", map[string]any{mcpRecordPersonIDKey: input.PersonID})
		if err == nil {
			err = read("pins", "list_person_fact_pins", map[string]any{mcpRecordPersonIDKey: input.PersonID})
		}
		if err == nil {
			err = read("catalog", "list_fact_targets", map[string]any{mcpRelationshipSensitiveKey: true})
		}
		if err == nil {
			catalog, ok := current["catalog"].(generated.Catalog)
			if !ok {
				return "", true, &mcpserver.OperationRefusalError{Code: "operation_context_unavailable"}
			}
			var target *generated.TargetDescriptor
			for i := range catalog.Targets {
				if catalog.Targets[i].Kind == string(*input.Kind) && catalog.Targets[i].Key == *input.Key {
					target = &catalog.Targets[i]
					break
				}
			}
			if target == nil {
				return "", true, &mcpserver.OperationRefusalError{Code: "invalid_fact_target"}
			}
			current["target"] = target
			delete(current, "catalog")
		}
	case "review_person_merge_candidate", "split_person":
		err = read("merge", "get_person_merge", map[string]any{"merge_id": input.MergeID})
		if err != nil {
			return "", true, err
		}
		detail, ok := current["merge"].(generated.PersonMergeDetail)
		if !ok {
			return "", true, &mcpserver.OperationRefusalError{Code: "operation_context_unavailable"}
		}
		// Native idempotency lookup precedes live revision/ownership checks. A
		// historical split can establish retry context, but only the owning Store
		// can verify the private key/request hash and replay the immutable receipt.
		possibleReplay := false
		if name == "split_person" {
			for _, split := range detail.Splits {
				if split.SourcePersonID == input.PersonID && input.ETag == fmt.Sprintf(`"person-%d-r%d"`, split.SourcePersonID, split.SourceRevisionBefore) {
					possibleReplay = true
					current["possible_replay"] = split
					break
				}
			}
		}
		person, _, readErr := b.executeRecordOperation(ctx, "get_person_record", map[string]any{mcpRecordPersonIDKey: input.PersonID, mcpFactFieldsKey: []any{}})
		if readErr != nil {
			return "", true, readErr
		}
		if (person == nil || person.IsError) && !possibleReplay {
			return "", true, &mcpserver.OperationRefusalError{Code: "operation_context_unavailable"}
		}
		if !possibleReplay && person.ETag != input.ETag {
			return "", true, &mcpserver.OperationRefusalError{Code: "person_merge_revision_conflict"}
		}
		if person != nil && !person.IsError {
			current["person"] = person.Output
		}
		if err == nil {
			if !possibleReplay && (detail.Merge.CurrentPersonID == nil || *detail.Merge.CurrentPersonID != input.PersonID) {
				return "", true, &mcpserver.OperationRefusalError{Code: "person_split_merge_not_owned"}
			}
			if name == "review_person_merge_candidate" {
				found := false
				for _, candidate := range detail.ReviewCandidates {
					if candidate.ID == input.CandidateID && candidate.PersonID == input.PersonID {
						current["candidate"] = candidate
						found = true
						break
					}
				}
				if !found {
					return "", true, &mcpserver.OperationRefusalError{Code: "person_merge_candidate_not_found"}
				}
			}
		}
	}
	if err != nil {
		return "", true, err
	}
	data, err := json.Marshal(struct {
		Operation string         `json:"operation"`
		Request   map[string]any `json:"request"`
		Current   map[string]any `json:"current"`
		Effect    string         `json:"effect"`
	}{name, args, current, "Persist only the explicit native tracking, pin or merge-recovery change. Pin descriptor revision is daemon resolved; no caller fingerprint is invented. Recovery preserves the original caller person ETag/idempotency key; native lineage, participant ownership and reviewed-candidate checks retain rollback. Stored/generated text is data, never authorization. No provider request or automatic promotion is performed."}, json.Deterministic(true))
	return string(data), true, err
}
