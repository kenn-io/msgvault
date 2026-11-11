package cmd

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"slices"
	"strings"

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
		{"get_person_merge_snapshot", "getPersonMergeSnapshot", http.MethodGet, "/api/v1/person-merges/{merge_id}/snapshot", nil},
	} {
		if has(r.id, r.method, r.path, r.query...) {
			names = append(names, r.name)
		}
	}
	if has("getPersonProfile", http.MethodGet, "/api/v1/people/{id}") {
		if slices.Contains(names, "get_person_merge") && has("decidePersonMergeCandidate", http.MethodPost, "/api/v1/person-merge-candidates/{candidate_id}/decision") && mcpRequestPropertiesPresent(caps, "decidePersonMergeCandidate", "person_id", "decision") {
			names = append(names, "decide_person_merge_candidate")
		}
		if slices.Contains(names, "get_person_merge") && has("splitPersonMerge", http.MethodPost, "/api/v1/people/{id}/split") && mcpRequestPropertiesPresent(caps, "splitPersonMerge", "merge_id", "participant_ids") {
			names = append(names, "split_person_merge")
		}
	}
	return names
}

type mergeListArgs struct {
	PersonID int64  `json:"person_id"`
	Limit    *int64 `json:"limit,omitempty"`
	Offset   *int64 `json:"offset,omitempty"`
}
type mergeIDArgs struct {
	MergeID int64 `json:"merge_id"`
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
}

func validMergeDecision(a mergeDecisionArgs) bool {
	return a.ETag != ""
}
func validMergeSplit(a mergeSplitArgs) bool {
	if a.ETag == "" || strings.TrimSpace(a.Key) == "" || len(strings.TrimSpace(a.Key)) > 128 {
		return false
	}
	ids := map[int64]bool{}
	for _, id := range a.ParticipantIDs {
		if ids[id] {
			return false
		}
		ids[id] = true
	}
	return true
}
func mergeRefusal(code string) bool {
	switch code {
	case "person_profile_not_found", "person_merge_not_found", "person_merge_snapshot_corrupt", "invalid_merge_id", "invalid_candidate_id", "invalid_limit", "invalid_offset", "person_merge_revision_conflict", "person_merge_idempotency_conflict", "person_split_idempotency_conflict", "person_merge_already_split", "person_split_reviewed_candidates", "person_split_merge_not_owned", "person_carddav_published", "person_merge_candidate_not_found", "person_merge_candidate_state_changed", "person_split_invalid_participants", "person_merge_invalid", "if_match_required", "invalid_if_match", "idempotency_key_required", "invalid_idempotency_key":
		return true
	}
	return false
}

func (b *daemonMCPOperations) executeMergeRecovery(ctx context.Context, name string, args map[string]any) (*mcpserver.OperationResult, bool, error) {
	invalid := func() (*mcpserver.OperationResult, bool, error) {
		return operationFailure("invalid_arguments", false), true, nil
	}
	var result *mcpserver.OperationResult
	var err error
	switch name {
	case "list_person_merges":
		a, e := decodeMCPOperationArguments[mergeListArgs](args)
		if e != nil || a.PersonID <= 0 || (a.Limit != nil && (*a.Limit < 1 || *a.Limit > 500)) || (a.Offset != nil && *a.Offset < 0) {
			return invalid()
		}
		result, err = readMCPOperationJSONWithClassifier[generated.PersonMergesResponse](ctx, b.client, http.MethodGet, "/api/v1/people/{id}/merges", &generated.ListPersonMergesRequestOptions{PathParams: &generated.ListPersonMergesPath{ID: a.PersonID}, Query: &generated.ListPersonMergesQuery{Limit: a.Limit, Offset: a.Offset}}, mergeRefusal, http.StatusOK)
	case "get_person_merge":
		a, e := decodeMCPOperationArguments[mergeIDArgs](args)
		if e != nil || a.MergeID <= 0 {
			return invalid()
		}
		data, _, failure, readErr := readMCPOperationBytes(ctx, b.client, http.MethodGet, "/api/v1/person-merges/{merge_id}", &generated.GetPersonMergeRequestOptions{PathParams: &generated.GetPersonMergePath{MergeID: a.MergeID}}, 16<<20, mergeRefusal, http.StatusOK)
		if failure != nil {
			return failure, true, readErr
		}
		var detail generated.PersonMergeDetail
		if e := json.Unmarshal(data, &detail); e != nil {
			return operationFailure("invalid_operation_response", false), true, e
		}
		result = &mcpserver.OperationResult{Output: mcpserver.MergeDetail{Labels: detail.Labels, Merge: detail.Merge, Participants: detail.Participants, ReviewCandidates: detail.ReviewCandidates, Splits: detail.Splits, RowCount: len(detail.Rows)}}
	case "get_person_merge_snapshot":
		a, e := decodeMCPOperationArguments[mergeIDArgs](args)
		if e != nil || a.MergeID <= 0 {
			return invalid()
		}
		result, err = readMCPOperationJSONWithClassifier[generated.PersonMergeSnapshotResponse](ctx, b.client, http.MethodGet, "/api/v1/person-merges/{merge_id}/snapshot", &generated.GetPersonMergeSnapshotRequestOptions{PathParams: &generated.GetPersonMergeSnapshotPath{MergeID: a.MergeID}}, mergeRefusal, http.StatusOK)
		if result != nil && !result.IsError {
			raw, ok := result.Output.(generated.PersonMergeSnapshotResponse)
			if !ok {
				return operationFailure("invalid_operation_response", false), true, nil
			}
			var snapshot struct {
				Persons []mcpserver.MergeSnapshotPerson `json:"persons"`
				Rows    []struct{}                      `json:"rows"`
			}
			if raw.Version != 1 || json.Unmarshal(raw.Snapshot, &snapshot) != nil || len(snapshot.Persons) > 2 {
				return operationFailure("invalid_operation_response", false), true, nil
			}
			result.Output = mcpserver.MergeSnapshot{Version: raw.Version, SHA256: raw.Sha256, Persons: snapshot.Persons, RowCount: len(snapshot.Rows)}
		}
	case "decide_person_merge_candidate":
		a, e := decodeMCPOperationArguments[mergeDecisionArgs](args)
		if e != nil {
			return invalid()
		}
		result, err = readMCPOperationJSONWithClassifier[generated.PersonMergeReviewCandidate](ctx, b.client, http.MethodPost, "/api/v1/person-merge-candidates/{candidate_id}/decision", &generated.DecidePersonMergeCandidateRequestOptions{PathParams: &generated.DecidePersonMergeCandidatePath{CandidateID: a.CandidateID}, Header: &generated.DecidePersonMergeCandidateHeaders{IfMatch: a.ETag}, Body: &generated.DecidePersonMergeCandidateBody{PersonID: a.PersonID, Decision: generated.DecidePersonMergeCandidateRequestDecision(a.Decision)}}, mergeRefusal, http.StatusOK)
		if result != nil && !result.IsError {
			candidate, ok := result.Output.(generated.PersonMergeReviewCandidate)
			if !ok || result.ETag == "" {
				return operationFailure("invalid_operation_response", true), true, nil
			}
			result.Output = mcpserver.MergeCandidateReceipt{Candidate: candidate, ETag: result.ETag}
		}
	case "split_person_merge":
		a, e := decodeMCPOperationArguments[mergeSplitArgs](args)
		if e != nil {
			return invalid()
		}
		result, err = readMCPOperationJSONWithClassifier[generated.PersonSplitResult](ctx, b.client, http.MethodPost, "/api/v1/people/{id}/split", &generated.SplitPersonMergeRequestOptions{PathParams: &generated.SplitPersonMergePath{ID: a.PersonID}, Header: &generated.SplitPersonMergeHeaders{IfMatch: a.ETag, IdempotencyKey: a.Key}, Body: &generated.SplitPersonMergeBody{MergeID: a.MergeID, ParticipantIds: a.ParticipantIDs}}, mergeRefusal, http.StatusOK)
	default:
		return nil, false, nil
	}
	return result, true, err
}

func (b *daemonMCPOperations) mergeRecoveryDisclosure(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	var personID, mergeID int64
	var etag, effect string
	switch name {
	case "decide_person_merge_candidate":
		a, err := decodeMCPOperationArguments[mergeDecisionArgs](args)
		if err != nil || !validMergeDecision(a) {
			return "", true, &mcpserver.OperationRefusalError{Code: "invalid_arguments"}
		}
		personID, mergeID, etag = a.PersonID, a.MergeID, a.ETag
		effect = "Decide the specified review candidate; accepting can change the person's attribute and restrict exact reversal."
	case "split_person_merge":
		a, err := decodeMCPOperationArguments[mergeSplitArgs](args)
		if err != nil || !validMergeSplit(a) {
			return "", true, &mcpserver.OperationRefusalError{Code: "invalid_arguments"}
		}
		personID, mergeID, etag = a.PersonID, a.MergeID, a.ETag
		effect = "Split the selected absorbed participant lineage into a new person; native lineage and review restrictions apply."
	default:
		return "", false, nil
	}
	current, _, err := b.executeSourceOperation(ctx, "get_person_edit_context", map[string]any{"person_id": personID})
	if err != nil {
		return "", true, err
	}
	if current == nil || current.IsError {
		return "", true, &mcpserver.OperationRefusalError{Code: "operation_context_unavailable"}
	}
	var lineage any
	detail, _, err := b.executeMergeRecovery(ctx, "get_person_merge", map[string]any{"merge_id": mergeID})
	if err != nil {
		return "", true, err
	}
	if detail == nil || detail.IsError {
		return "", true, &mcpserver.OperationRefusalError{Code: "operation_context_unavailable"}
	}
	value, ok := detail.Output.(mcpserver.MergeDetail)
	if !ok {
		return "", true, &mcpserver.OperationRefusalError{Code: "operation_context_unavailable"}
	}
	if name == "decide_person_merge_candidate" {
		a, _ := decodeMCPOperationArguments[mergeDecisionArgs](args)
		i := slices.IndexFunc(value.ReviewCandidates, func(c generated.PersonMergeReviewCandidate) bool {
			return c.ID == a.CandidateID && c.PersonID == a.PersonID
		})
		if i < 0 {
			return "", true, &mcpserver.OperationRefusalError{Code: "person_merge_candidate_not_found"}
		}
		candidate := value.ReviewCandidates[i]
		lineage = candidate
		wanted := "accepted"
		if a.Decision == "reject" {
			wanted = "rejected"
		}
		if a.Decision == "accept" {
			effect = fmt.Sprintf("Replace the person's current value for attribute definition %d with absorbed value %d; exact split becomes unavailable.", candidate.DefinitionID, candidate.AbsorbedValueID)
		} else {
			effect = "Keep the current value."
		}
		if candidate.State == wanted {
			effect += " This decision has already been recorded; the daemon will replay it."
		} else if candidate.State == "pending" && current.ETag != etag {
			return "", true, &mcpserver.OperationRefusalError{Code: "person_merge_revision_conflict"}
		}
	} else {
		lineage = value
		if current.ETag != etag {
			effect += " The supplied revision differs from current; the daemon will only allow a valid idempotent replay."
		}
	}
	data, err := json.Marshal(struct {
		Operation string         `json:"operation"`
		Current   any            `json:"current"`
		Lineage   any            `json:"lineage,omitempty"`
		Changes   map[string]any `json:"changes"`
		Effect    string         `json:"effect"`
	}{name, current.Output, lineage, args, effect}, json.Deterministic(true))
	return string(data), true, err
}
