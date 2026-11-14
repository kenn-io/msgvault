package cmd

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"net/http"
	"slices"

	"go.kenn.io/msgvault/internal/apiprotocol"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func mergeRecoveryCapabilities(has mcpRouteCheck, caps *apiprotocol.MCPCapabilities) []string {
	var names []string
	if has("listPersonMerges", http.MethodGet, "/api/v1/people/{id}/merges", "limit", "offset") {
		names = append(names, "list_person_merges")
	}
	if !has("getPersonMerge", http.MethodGet, "/api/v1/person-merges/{merge_id}") {
		return names
	}
	names = append(names, "get_person_merge")
	if has("getAttributeDefinition", http.MethodGet, "/api/v1/attribute-definitions/{id}") && has("listPersonAttributes", http.MethodGet, "/api/v1/people/{id}/attributes", "history", "slug") && has("decidePersonMergeCandidate", http.MethodPost, "/api/v1/person-merge-candidates/{candidate_id}/decision") && mcpRequestPropertiesPresent(caps, "decidePersonMergeCandidate", "person_id", "decision") {
		names = append(names, "decide_person_merge_candidate")
	}
	if has("splitPersonMerge", http.MethodPost, "/api/v1/people/{id}/split") && mcpRequestPropertiesPresent(caps, "splitPersonMerge", "merge_id", "participant_ids") {
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

func rowPage[T any](rows []T, offset int) ([]T, *int) {
	start := min(offset, len(rows))
	end := min(start+500, len(rows))
	if end == len(rows) {
		return rows[start:], nil
	}
	return rows[start:end], &end
}

// Context reads are GETs, so a failed read carries a settled native code.
func mergeContextRefusal(result *mcpserver.OperationResult, err error) error {
	if err != nil {
		return err
	}
	if failure, ok := result.Output.(operationFailureOutput); ok {
		return &mcpserver.OperationRefusalError{Code: failure.Error}
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
		output := mcpserver.MergeDetail{RowCount: len(detail.Rows)}
		detail.Rows, output.NextRowOffset = rowPage(detail.Rows, a.RowOffset)
		output.PersonMergeDetail = detail
		result = &mcpserver.OperationResult{Output: output}
	case "decide_person_merge_candidate":
		a, e := decodeMCPOperationArguments[mergeDecisionArgs](args)
		if e != nil {
			return invalid()
		}
		result, err = readMCPOperationJSON[generated.PersonMergeReviewCandidate](ctx, b.client, http.MethodPost, "/api/v1/person-merge-candidates/{candidate_id}/decision", &generated.DecidePersonMergeCandidateRequestOptions{PathParams: &generated.DecidePersonMergeCandidatePath{CandidateID: a.CandidateID}, Header: &generated.DecidePersonMergeCandidateHeaders{IfMatch: a.ETag}, Body: &generated.DecidePersonMergeCandidateBody{PersonID: a.PersonID, Decision: generated.DecidePersonMergeCandidateRequestDecision(a.Decision)}}, http.StatusOK)
		if candidate, ok := result.Output.(generated.PersonMergeReviewCandidate); ok {
			if result.ETag == "" {
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
		if e := json.Unmarshal(data, &split); e != nil || headers.Get("ETag") == "" || headers.Get("X-New-Person-Etag") == "" {
			return operationFailure("invalid_operation_response", true), true, e
		}
		output := mcpserver.MergeSplitReceipt{PersonSplitResult: split, ETag: headers.Get("ETag"), NewPersonETag: headers.Get("X-New-Person-Etag"), AmbiguousRowCount: len(split.AmbiguousRows), UnrestoredRowCount: len(split.UnrestoredRows)}
		var nextAmbiguous, nextUnrestored *int
		output.AmbiguousRows, nextAmbiguous = rowPage(split.AmbiguousRows, a.RowOffset)
		output.UnrestoredRows, nextUnrestored = rowPage(split.UnrestoredRows, a.RowOffset)
		output.NextRowOffset = cmp.Or(nextAmbiguous, nextUnrestored)
		result = &mcpserver.OperationResult{Output: output}
	default:
		return nil, false, nil
	}
	return result, true, err
}

func (b *daemonMCPOperations) mergeRecoveryDisclosure(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	var decision mergeDecisionArgs
	var split mergeSplitArgs
	var err error
	switch name {
	case "decide_person_merge_candidate":
		decision, err = decodeMCPOperationArguments[mergeDecisionArgs](args)
		split.MergeID = decision.MergeID
	case "split_person_merge":
		split, err = decodeMCPOperationArguments[mergeSplitArgs](args)
	default:
		return "", false, nil
	}
	if err != nil {
		return "", true, &mcpserver.OperationRefusalError{Code: invalidArguments}
	}
	detail, _, err := b.executeMergeRecovery(ctx, "get_person_merge", map[string]any{"merge_id": split.MergeID})
	merge, ok := detail.Output.(mcpserver.MergeDetail)
	if !ok {
		return "", true, mergeContextRefusal(detail, err)
	}
	shown := struct {
		Operation     string                    `json:"operation"`
		Attribute     string                    `json:"attribute,omitempty"`
		CurrentValue  *generated.AttributeValue `json:"current_value"`
		SurvivorValue *generated.AttributeValue `json:"survivor_value,omitempty"`
		AbsorbedValue *generated.AttributeValue `json:"absorbed_value,omitempty"`
		Lineage       any                       `json:"lineage,omitempty"`
		Changes       map[string]any            `json:"changes"`
		Effect        string                    `json:"effect"`
	}{Operation: name, Changes: args}
	if name == "split_person_merge" {
		merge.Rows = nil
		shown.Lineage = merge
		shown.Effect = "Split the selected absorbed participant lineage into a new person. Lineage labels are historical. The daemon checks the supplied revision, lineage and review restrictions for a new split. An identical completed request returns its saved result."
	} else {
		i := slices.IndexFunc(merge.ReviewCandidates, func(c generated.PersonMergeReviewCandidate) bool {
			return c.ID == decision.CandidateID
		})
		if i < 0 {
			return "", true, &mcpserver.OperationRefusalError{Code: "person_merge_candidate_not_found"}
		}
		candidate := merge.ReviewCandidates[i]
		shown.Lineage = candidate
		wanted := "rejected"
		shown.Effect = "Keep the current value."
		if decision.Decision == "accept" {
			wanted = "accepted"
			shown.Effect = "Replace the current value with the absorbed value; exact split becomes unavailable."
		}
		if candidate.State == wanted {
			shown.Effect = "This decision has already been recorded; the daemon will replay it."
		} else {
			definition, err := readMCPOperationJSON[generated.AttributeDefinition](ctx, b.client, http.MethodGet, "/api/v1/attribute-definitions/{id}", &generated.GetAttributeDefinitionRequestOptions{PathParams: &generated.GetAttributeDefinitionPath{ID: candidate.DefinitionID}}, http.StatusOK)
			attribute, ok := definition.Output.(generated.AttributeDefinition)
			if !ok {
				return "", true, mergeContextRefusal(definition, err)
			}
			options := &generated.ListPersonAttributesRequestOptions{PathParams: &generated.ListPersonAttributesPath{ID: decision.PersonID}, Query: &generated.ListPersonAttributesQuery{History: new(true), Slug: &attribute.Slug}}
			data, _, failure, err := readMCPOperationBytes(ctx, b.client, http.MethodGet, "/api/v1/people/{id}/attributes", options, mcpserver.MaxDownloadBytes, nil, http.StatusOK)
			if failure != nil {
				return "", true, mergeContextRefusal(failure, err)
			}
			var response generated.PersonAttributesResponse
			if err := json.Unmarshal(data, &response); err != nil {
				return "", true, err
			}

			for _, group := range response.Attributes {
				if group.Definition.ID != candidate.DefinitionID {
					continue
				}
				shown.Attribute = group.Definition.Label
				if len(group.Current) > 0 {
					shown.CurrentValue = &group.Current[0].Value
				}
				for _, v := range append(group.Current, group.History...) {
					if v.ID == candidate.SurvivorValueID {
						shown.SurvivorValue = &v.Value
					}
					if v.ID == candidate.AbsorbedValueID {
						shown.AbsorbedValue = &v.Value
					}
				}
			}
			if shown.Attribute == "" || shown.SurvivorValue == nil || shown.AbsorbedValue == nil {
				return "", true, &mcpserver.OperationRefusalError{Code: "person_merge_candidate_state_changed"}
			}
		}
	}
	data, err := json.Marshal(shown, json.Deterministic(true))
	return string(data), true, err
}
