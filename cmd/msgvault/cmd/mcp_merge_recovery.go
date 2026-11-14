package cmd

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"slices"

	"go.kenn.io/msgvault/internal/apiprotocol"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func mergeRecoveryCapabilities(has mcpRouteCheck, caps *apiprotocol.MCPCapabilities) []string {
	var names []string
	for _, r := range []struct {
		name, id, method, path string
		query                  []string
	}{
		{"list_person_merges", "listPersonMerges", http.MethodGet, "/api/v1/people/{id}/merges", []string{"limit", "offset"}},
		{"get_person_merge", "getPersonMerge", http.MethodGet, "/api/v1/person-merges/{merge_id}", nil},
	} {
		if has(r.id, r.method, r.path, r.query...) {
			names = append(names, r.name)
		}
	}
	if slices.Contains(names, "get_person_merge") && has("listPersonAttributes", http.MethodGet, "/api/v1/people/{id}/attributes", "history") && has("decidePersonMergeCandidate", http.MethodPost, "/api/v1/person-merge-candidates/{candidate_id}/decision") && mcpRequestPropertiesPresent(caps, "decidePersonMergeCandidate", "person_id", "decision") {
		names = append(names, "decide_person_merge_candidate")
	}
	if slices.Contains(names, "get_person_merge") && has("splitPersonMerge", http.MethodPost, "/api/v1/people/{id}/split") && mcpRequestPropertiesPresent(caps, "splitPersonMerge", "merge_id", "participant_ids") {
		names = append(names, "split_person_merge")
	}
	return names
}

type mergeListArgs struct {
	PersonID int64  `json:"person_id"`
	Limit    *int64 `json:"limit,omitempty"`
	Offset   *int64 `json:"offset,omitempty"`
}
type mergeIDArgs struct {
	MergeID   int64 `json:"merge_id"`
	RowOffset int   `json:"row_offset,omitempty"`
}
type mergeDecisionArgs struct {
	PersonID    int64  `json:"person_id"`
	CandidateID int64  `json:"candidate_id"`
	MergeID     int64  `json:"merge_id"`
	ETag        string `json:"etag"`
	Decision    string `json:"decision"`
}
type mergeSplitArgs struct {
	PersonID       int64   `json:"person_id"`
	MergeID        int64   `json:"merge_id"`
	ParticipantIDs []int64 `json:"participant_ids"`
	ETag           string  `json:"etag"`
	Key            string  `json:"idempotency_key"`
	RowOffset      int     `json:"row_offset,omitempty"`
}

func rowPage[T any](rows []T, offset int) ([]T, bool) {
	if offset >= len(rows) {
		return rows[len(rows):], false
	}
	end := min(offset+500, len(rows))
	return rows[offset:end], end < len(rows)
}

func mergeContextRefusal(result *mcpserver.OperationResult) *mcpserver.OperationRefusalError {
	if result != nil {
		if failure, ok := result.Output.(operationFailureOutput); ok && failure.Error != "" && !failure.OperationMayHaveCompleted {
			return &mcpserver.OperationRefusalError{Code: failure.Error}
		}
	}
	return &mcpserver.OperationRefusalError{Code: "operation_context_unavailable"}
}

func (b *daemonMCPOperations) executeMergeRecovery(ctx context.Context, name string, args map[string]any) (*mcpserver.OperationResult, bool, error) {
	invalid := func() (*mcpserver.OperationResult, bool, error) {
		return operationFailure(invalidArguments, false), true, nil
	}
	var result *mcpserver.OperationResult
	var err error
	switch name {
	case "list_person_merges":
		a, e := decodeMCPOperationArguments[mergeListArgs](args)
		if e != nil {
			return invalid()
		}
		result, err = readMCPOperationJSON[generated.PersonMergesResponse](ctx, b.client, http.MethodGet, "/api/v1/people/{id}/merges", &generated.ListPersonMergesRequestOptions{PathParams: &generated.ListPersonMergesPath{ID: a.PersonID}, Query: &generated.ListPersonMergesQuery{Limit: a.Limit, Offset: a.Offset}}, http.StatusOK)
	case "get_person_merge":
		a, e := decodeMCPOperationArguments[mergeIDArgs](args)
		if e != nil || a.RowOffset < 0 {
			return invalid()
		}
		data, _, failure, readErr := readMCPOperationBytes(ctx, b.client, http.MethodGet, "/api/v1/person-merges/{merge_id}", &generated.GetPersonMergeRequestOptions{PathParams: &generated.GetPersonMergePath{MergeID: a.MergeID}}, mcpserver.MaxDownloadBytes, nil, http.StatusOK)
		if failure != nil {
			return failure, true, readErr
		}
		var detail generated.PersonMergeDetail
		if e := json.Unmarshal(data, &detail); e != nil {
			return operationFailure("invalid_operation_response", false), true, e
		}
		count := len(detail.Rows)
		var more bool
		detail.Rows, more = rowPage(detail.Rows, a.RowOffset)
		output := mcpserver.MergeDetail{PersonMergeDetail: detail, RowCount: count}
		if more {
			next := a.RowOffset + 500
			output.NextRowOffset = &next
		}
		result = &mcpserver.OperationResult{Output: output}
	case "decide_person_merge_candidate":
		a, e := decodeMCPOperationArguments[mergeDecisionArgs](args)
		if e != nil {
			return invalid()
		}
		result, err = readMCPOperationJSON[generated.PersonMergeReviewCandidate](ctx, b.client, http.MethodPost, "/api/v1/person-merge-candidates/{candidate_id}/decision", &generated.DecidePersonMergeCandidateRequestOptions{PathParams: &generated.DecidePersonMergeCandidatePath{CandidateID: a.CandidateID}, Header: &generated.DecidePersonMergeCandidateHeaders{IfMatch: a.ETag}, Body: &generated.DecidePersonMergeCandidateBody{PersonID: a.PersonID, Decision: generated.DecidePersonMergeCandidateRequestDecision(a.Decision)}}, http.StatusOK)
		if result != nil && !result.IsError {
			candidate, ok := result.Output.(generated.PersonMergeReviewCandidate)
			if !ok || result.ETag == "" {
				return operationFailure("invalid_operation_response", true), true, nil
			}
			result.Output = mcpserver.MergeCandidateReceipt{Candidate: candidate, ETag: result.ETag}
		}
	case "split_person_merge":
		a, e := decodeMCPOperationArguments[mergeSplitArgs](args)
		if e != nil || a.RowOffset < 0 {
			return invalid()
		}
		data, headers, failure, readErr := readMCPOperationBytes(ctx, b.client, http.MethodPost, "/api/v1/people/{id}/split", &generated.SplitPersonMergeRequestOptions{PathParams: &generated.SplitPersonMergePath{ID: a.PersonID}, Header: &generated.SplitPersonMergeHeaders{IfMatch: a.ETag, IdempotencyKey: a.Key}, Body: &generated.SplitPersonMergeBody{MergeID: a.MergeID, ParticipantIds: a.ParticipantIDs}}, mcpserver.MaxDownloadBytes, nil, http.StatusOK)
		if failure != nil {
			return failure, true, readErr
		}
		var split generated.PersonSplitResult
		if e := json.Unmarshal(data, &split); e != nil {
			return operationFailure("invalid_operation_response", true), true, e
		}
		if headers.Get("ETag") == "" || headers.Get("X-New-Person-ETag") == "" {
			return operationFailure("invalid_operation_response", true), true, nil
		}
		output := mcpserver.MergeSplitReceipt{PersonSplitResult: split, ETag: headers.Get("ETag"), NewPersonETag: headers.Get("X-New-Person-ETag"), AmbiguousRowCount: len(split.AmbiguousRows), UnrestoredRowCount: len(split.UnrestoredRows)}
		var moreA, moreU bool
		output.AmbiguousRows, moreA = rowPage(split.AmbiguousRows, a.RowOffset)
		output.UnrestoredRows, moreU = rowPage(split.UnrestoredRows, a.RowOffset)
		if moreA || moreU {
			next := a.RowOffset + 500
			output.NextRowOffset = &next
		}
		result = &mcpserver.OperationResult{Output: output}

	default:
		return nil, false, nil
	}
	return result, true, err
}

func (b *daemonMCPOperations) mergeRecoveryDisclosure(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	var mergeID int64
	var effect string
	var decision mergeDecisionArgs
	switch name {
	case "decide_person_merge_candidate":
		a, err := decodeMCPOperationArguments[mergeDecisionArgs](args)
		if err != nil {
			return "", true, &mcpserver.OperationRefusalError{Code: invalidArguments}
		}
		decision = a
		mergeID = a.MergeID
	case "split_person_merge":
		a, err := decodeMCPOperationArguments[mergeSplitArgs](args)
		if err != nil {
			return "", true, &mcpserver.OperationRefusalError{Code: invalidArguments}
		}
		mergeID = a.MergeID
		effect = "Split the selected absorbed participant lineage into a new person. Lineage labels are historical. The daemon checks the supplied revision, lineage and review restrictions for a new split. An identical completed request returns its saved result."
	default:
		return "", false, nil
	}
	var lineage any
	var attribute string
	var currentValue, absorbedValue *generated.AttributeValue
	detail, _, err := b.executeMergeRecovery(ctx, "get_person_merge", map[string]any{"merge_id": mergeID})
	if err != nil {
		return "", true, err
	}
	if detail == nil || detail.IsError {
		return "", true, mergeContextRefusal(detail)
	}
	value, ok := detail.Output.(mcpserver.MergeDetail)
	if !ok {
		return "", true, &mcpserver.OperationRefusalError{Code: "operation_context_unavailable"}
	}
	if name == "decide_person_merge_candidate" {
		i := slices.IndexFunc(value.ReviewCandidates, func(c generated.PersonMergeReviewCandidate) bool {
			return c.ID == decision.CandidateID
		})
		if i < 0 {
			return "", true, &mcpserver.OperationRefusalError{Code: "person_merge_candidate_not_found"}
		}
		candidate := value.ReviewCandidates[i]
		history := true
		attrs, err := readMCPOperationJSON[generated.PersonAttributesResponse](ctx, b.client, http.MethodGet, "/api/v1/people/{id}/attributes", &generated.ListPersonAttributesRequestOptions{PathParams: &generated.ListPersonAttributesPath{ID: decision.PersonID}, Query: &generated.ListPersonAttributesQuery{History: &history}}, http.StatusOK)
		if err != nil {
			return "", true, err
		}
		if attrs == nil || attrs.IsError {
			return "", true, mergeContextRefusal(attrs)
		}
		response, ok := attrs.Output.(generated.PersonAttributesResponse)
		if !ok {
			return "", true, &mcpserver.OperationRefusalError{Code: "operation_context_unavailable"}
		}
		for _, group := range response.Attributes {
			if group.Definition.ID != candidate.DefinitionID {
				continue
			}
			attribute = group.Definition.Label
			for _, v := range append(group.Current, group.History...) {
				if v.ID == candidate.SurvivorValueID {
					val := v.Value
					currentValue = &val
				}
				if v.ID == candidate.AbsorbedValueID {
					val := v.Value
					absorbedValue = &val
				}
			}
		}
		if attribute == "" || currentValue == nil || absorbedValue == nil {
			return "", true, &mcpserver.OperationRefusalError{Code: "person_merge_candidate_state_changed"}
		}
		lineage = candidate
		wanted := "accepted"
		if decision.Decision == "reject" {
			wanted = "rejected"
		}
		if decision.Decision == "accept" {
			effect = fmt.Sprintf("Replace the person's current value for attribute definition %d with absorbed value %d; exact split becomes unavailable.", candidate.DefinitionID, candidate.AbsorbedValueID)
		} else {
			effect = "Keep the current value."
		}
		if candidate.State == wanted {
			effect += " This decision has already been recorded; the daemon will replay it."
		}
	} else {
		value.Rows = nil
		lineage = value
	}
	data, err := json.Marshal(struct {
		Operation     string                    `json:"operation"`
		Attribute     string                    `json:"attribute,omitempty"`
		CurrentValue  *generated.AttributeValue `json:"current_value,omitempty"`
		AbsorbedValue *generated.AttributeValue `json:"absorbed_value,omitempty"`
		Lineage       any                       `json:"lineage,omitempty"`
		Changes       map[string]any            `json:"changes"`
		Effect        string                    `json:"effect"`
	}{name, attribute, currentValue, absorbedValue, lineage, args, effect}, json.Deterministic(true))
	return string(data), true, err
}
