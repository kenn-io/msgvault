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
		{"get_person_merge_snapshot", "getPersonMergeSnapshot", http.MethodGet, "/api/v1/person-merges/{merge_id}/snapshot", nil},
	} {
		if has(r.id, r.method, r.path, r.query...) {
			names = append(names, r.name)
		}
	}
	if slices.Contains(names, "get_person_merge") && has("decidePersonMergeCandidate", http.MethodPost, "/api/v1/person-merge-candidates/{candidate_id}/decision") && mcpRequestPropertiesPresent(caps, "decidePersonMergeCandidate", "person_id", "decision") {
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

const mergeRecoveryResponseLimit = 16 << 20

func mergeRefusal(code string) bool {
	switch code {
	case "person_merge_not_found", "person_merge_snapshot_corrupt", "invalid_merge_id", "invalid_candidate_id", "invalid_limit", "invalid_offset", "person_merge_revision_conflict", "person_split_idempotency_conflict", "person_merge_already_split", "person_split_reviewed_candidates", "person_split_merge_not_owned", "person_merge_candidate_not_found", "person_merge_candidate_state_changed", "person_split_invalid_participants", "person_merge_invalid", "invalid_if_match", "idempotency_key_required", "invalid_idempotency_key", "server_busy":
		return true
	}
	return false
}

func mergeContextRefusal(result *mcpserver.OperationResult) *mcpserver.OperationRefusalError {
	if result != nil {
		if failure, ok := result.Output.(struct {
			Error                     string `json:"error"`
			OperationMayHaveCompleted bool   `json:"operation_may_have_completed"`
		}); ok && failure.Error != "" && !failure.OperationMayHaveCompleted {
			return &mcpserver.OperationRefusalError{Code: failure.Error}
		}
	}
	return &mcpserver.OperationRefusalError{Code: "operation_context_unavailable"}
}

func (b *daemonMCPOperations) executeMergeRecovery(ctx context.Context, name string, args map[string]any) (*mcpserver.OperationResult, bool, error) {
	var result *mcpserver.OperationResult
	var err error
	switch name {
	case "list_person_merges":
		a, e := decodeMCPOperationArguments[mergeListArgs](args)
		if e != nil {
			return operationFailure("invalid_arguments", false), true, nil
		}
		result, err = readMCPOperationJSONWithClassifier[generated.PersonMergesResponse](ctx, b.client, http.MethodGet, "/api/v1/people/{id}/merges", &generated.ListPersonMergesRequestOptions{PathParams: &generated.ListPersonMergesPath{ID: a.PersonID}, Query: &generated.ListPersonMergesQuery{Limit: a.Limit, Offset: a.Offset}}, mergeRefusal, http.StatusOK)
	case "get_person_merge":
		a, e := decodeMCPOperationArguments[mergeIDArgs](args)
		if e != nil {
			return operationFailure("invalid_arguments", false), true, nil
		}
		data, _, failure, readErr := readMCPOperationBytes(ctx, b.client, http.MethodGet, "/api/v1/person-merges/{merge_id}", &generated.GetPersonMergeRequestOptions{PathParams: &generated.GetPersonMergePath{MergeID: a.MergeID}}, mergeRecoveryResponseLimit, mergeRefusal, http.StatusOK)
		if failure != nil {
			return failure, true, readErr
		}
		var detail struct {
			Labels           generated.PersonMergeLabels            `json:"labels"`
			Merge            generated.PersonMerge                  `json:"merge"`
			Participants     []generated.PersonMergeParticipant     `json:"participants"`
			ReviewCandidates []generated.PersonMergeReviewCandidate `json:"review_candidates"`
			Splits           []generated.PersonSplit                `json:"splits"`
			Rows             []struct{}                             `json:"rows"`
		}
		if e := json.Unmarshal(data, &detail); e != nil {
			return operationFailure("invalid_operation_response", false), true, e
		}
		result = &mcpserver.OperationResult{Output: mcpserver.MergeDetail{Labels: detail.Labels, Merge: detail.Merge, Participants: detail.Participants, ReviewCandidates: detail.ReviewCandidates, Splits: detail.Splits, RowCount: len(detail.Rows)}}
	case "get_person_merge_snapshot":
		a, e := decodeMCPOperationArguments[mergeIDArgs](args)
		if e != nil {
			return operationFailure("invalid_arguments", false), true, nil
		}
		data, _, failure, readErr := readMCPOperationBytes(ctx, b.client, http.MethodGet, "/api/v1/person-merges/{merge_id}/snapshot", &generated.GetPersonMergeSnapshotRequestOptions{PathParams: &generated.GetPersonMergeSnapshotPath{MergeID: a.MergeID}}, mergeRecoveryResponseLimit, mergeRefusal, http.StatusOK)
		if failure != nil {
			return failure, true, readErr
		}
		var raw generated.PersonMergeSnapshotResponse
		if e := json.Unmarshal(data, &raw); e != nil {
			return operationFailure("invalid_operation_response", false), true, e
		}
		var snapshot struct {
			Persons []mcpserver.MergeSnapshotPerson `json:"persons"`
			Rows    []struct{}                      `json:"rows"`
		}
		if raw.Version != 1 {
			return operationFailure("invalid_operation_response", false), true, nil
		}
		if e := json.Unmarshal(raw.Snapshot, &snapshot); e != nil {
			return operationFailure("invalid_operation_response", false), true, e
		}
		result = &mcpserver.OperationResult{Output: mcpserver.MergeSnapshot{Version: raw.Version, SHA256: raw.Sha256, Persons: snapshot.Persons, RowCount: len(snapshot.Rows)}}
	case "decide_person_merge_candidate":
		a, e := decodeMCPOperationArguments[mergeDecisionArgs](args)
		if e != nil {
			return operationFailure("invalid_arguments", false), true, nil
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
			return operationFailure("invalid_arguments", false), true, nil
		}
		data, _, failure, readErr := readMCPOperationBytes(ctx, b.client, http.MethodPost, "/api/v1/people/{id}/split", &generated.SplitPersonMergeRequestOptions{PathParams: &generated.SplitPersonMergePath{ID: a.PersonID}, Header: &generated.SplitPersonMergeHeaders{IfMatch: a.ETag, IdempotencyKey: a.Key}, Body: &generated.SplitPersonMergeBody{MergeID: a.MergeID, ParticipantIds: a.ParticipantIDs}}, mergeRecoveryResponseLimit, mergeRefusal, http.StatusOK)
		if failure != nil {
			return failure, true, readErr
		}
		var split generated.PersonSplitResult
		if e := json.Unmarshal(data, &split); e != nil {
			return operationFailure("invalid_operation_response", true), true, e
		}
		result = &mcpserver.OperationResult{Output: mcpserver.MergeSplitReceipt{Split: split.Split, SourcePerson: split.SourcePerson, NewPerson: split.NewPerson, ExactReversal: split.ExactReversal, UIDAliasDisposition: split.UIDAliasDisposition, IdentityRevision: split.IdentityRevision, CacheState: split.CacheState, AmbiguousRowCount: len(split.AmbiguousRows), UnrestoredRowCount: len(split.UnrestoredRows)}}
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
			return "", true, &mcpserver.OperationRefusalError{Code: "invalid_arguments"}
		}
		decision = a
		mergeID = a.MergeID
	case "split_person_merge":
		a, err := decodeMCPOperationArguments[mergeSplitArgs](args)
		if err != nil {
			return "", true, &mcpserver.OperationRefusalError{Code: "invalid_arguments"}
		}
		mergeID = a.MergeID
		effect = "Split the selected absorbed participant lineage into a new person. Lineage labels are historical. The daemon checks the supplied revision, lineage and review restrictions for a new split. An identical completed request returns its saved result."
	default:
		return "", false, nil
	}
	var lineage any
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
		if candidate.PersonID != decision.PersonID {
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
		lineage = value
	}
	data, err := json.Marshal(struct {
		Operation string         `json:"operation"`
		Lineage   any            `json:"lineage,omitempty"`
		Changes   map[string]any `json:"changes"`
		Effect    string         `json:"effect"`
	}{name, lineage, args, effect}, json.Deterministic(true))
	return string(data), true, err
}
