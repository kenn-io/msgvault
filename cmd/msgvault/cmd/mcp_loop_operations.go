package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/loops"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/pkg/client/generated"
)

const commitmentDecisionPath = "/api/v1/loops/commitments/{commitment_id}/decision"

func loopMCPCapabilities(hasRoute mcpRouteCheck, capabilities *apiprotocol.MCPCapabilities) []string {
	var names []string
	if hasRoute("listLoops", http.MethodGet, "/api/v1/loops", "person_id", "limit") {
		names = append(names, "list_open_loops")
	}
	for _, route := range []struct {
		name, id, path string
		query          []string
	}{
		{"close_open_loop", "markLoopDone", "/api/v1/loops/{message_id}/done", []string{"question_message_ids"}},
		{"snooze_open_loop", "snoozeLoop", "/api/v1/loops/{message_id}/snooze", []string{"question_message_ids"}},
		{"reopen_open_loop", "reopenLoop", "/api/v1/loops/{message_id}/reopen", nil},
	} {
		if hasRoute(route.id, http.MethodPost, route.path, route.query...) && (route.name != "snooze_open_loop" || mcpRequestPropertiesPresent(capabilities, route.id, "until")) {
			names = append(names, route.name)
		}
	}
	if hasRoute("setCommitmentDecision", http.MethodPost, commitmentDecisionPath) &&
		mcpRequestPropertiesPresent(capabilities, "setCommitmentDecision", "state", "until") {
		names = append(names, "set_commitment_decision")
	}
	return names
}

type loopListArguments struct {
	PersonID      *int64 `json:"person_id,omitempty"`
	Limit         *int64 `json:"limit,omitempty"`
	IncludeClosed *bool  `json:"include_closed,omitempty"`
}
type loopDecisionArguments struct {
	MessageID int64 `json:"message_id"`
}
type loopDoneArguments struct {
	MessageID          int64   `json:"message_id"`
	QuestionMessageIDs []int64 `json:"question_message_ids"`
}
type loopSnoozeArguments struct {
	MessageID          int64   `json:"message_id"`
	QuestionMessageIDs []int64 `json:"question_message_ids"`
	Until              string  `json:"until"`
}

type commitmentDecisionArguments struct {
	ID    string `json:"commitment_id"`
	State string `json:"state"`
	Until string `json:"until,omitempty"`
}

func (b *daemonMCPOperations) commitmentDecisionInput(args map[string]any) (commitmentDecisionArguments, *time.Time, error) {
	input, err := decodeMCPOperationArguments[commitmentDecisionArguments](args)
	if err != nil {
		return input, nil, err
	}
	if _, err := loops.ParseCommitmentID(input.ID); err != nil {
		return input, nil, err
	}
	switch input.State {
	case "snoozed":
		until, err := b.loopDeadline(input.Until)
		return input, &until, err
	case "open", "done":
		if input.Until == "" {
			return input, nil, nil
		}
	}
	return input, nil, errors.New("invalid commitment decision")
}

// validLoopQuestions requires at least one listed question, each a safe ID.
func validLoopQuestions(ids []int64) bool {
	return len(ids) > 0 && !slices.ContainsFunc(ids, func(id int64) bool { return !validLoopID(id) })
}

func validLoopID(id int64) bool { return id > 0 && id <= 1<<53-1 }

func (b *daemonMCPOperations) loopDeadline(value string) (time.Time, error) {
	until, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		until, err = time.ParseInLocation(time.DateOnly, value, time.Local)
	}
	if err != nil || !until.After(b.now()) {
		return time.Time{}, errors.New("invalid snooze deadline")
	}
	return until, nil
}

func (b *daemonMCPOperations) executeLoopOperation(ctx context.Context, name string, args map[string]any) (*mcpserver.OperationResult, bool, error) {
	invalid := func() (*mcpserver.OperationResult, bool, error) {
		return operationFailure("invalid_arguments", false), true, nil
	}
	if name == "list_open_loops" {
		input, err := decodeMCPOperationArguments[loopListArguments](args)
		if err != nil || input.PersonID != nil && !validLoopID(*input.PersonID) || input.Limit != nil && (*input.Limit < 1 || *input.Limit > 200) {
			return invalid()
		}
		if input.IncludeClosed != nil && !b.includeClosedLoops {
			return operationFailure("operation_unavailable", false), true, nil
		}
		if input.Limit == nil {
			input.Limit = new(int64(50))
		}
		result, err := readMCPLoopJSON[generated.LoopListResponse](ctx, b.client, http.MethodGet, "/api/v1/loops", &generated.ListLoopsRequestOptions{Query: &generated.ListLoopsQuery{PersonID: input.PersonID, Limit: input.Limit, IncludeClosed: input.IncludeClosed}})
		return result, true, err
	}
	if name == "set_commitment_decision" {
		input, until, err := b.commitmentDecisionInput(args)
		if err != nil {
			return invalid()
		}
		options := &generated.SetCommitmentDecisionRequestOptions{
			PathParams: &generated.SetCommitmentDecisionPath{CommitmentID: input.ID},
			Body:       &generated.SetCommitmentDecisionBody{State: generated.CommitmentDecisionRequestState(input.State), Until: until},
		}
		result, err := readMCPLoopJSON[generated.CommitmentDecisionResponse](ctx, b.client, http.MethodPost, commitmentDecisionPath, options)
		return result, true, err
	}
	var id int64
	var questions []int64
	var until time.Time
	switch name {
	case "reopen_open_loop":
		input, err := decodeMCPOperationArguments[loopDecisionArguments](args)
		if err != nil || !validLoopID(input.MessageID) {
			return invalid()
		}
		id = input.MessageID
	case "close_open_loop":
		input, err := decodeMCPOperationArguments[loopDoneArguments](args)
		if err != nil || !validLoopID(input.MessageID) || !validLoopQuestions(input.QuestionMessageIDs) {
			return invalid()
		}
		id, questions = input.MessageID, input.QuestionMessageIDs
	case "snooze_open_loop":
		input, err := decodeMCPOperationArguments[loopSnoozeArguments](args)
		if err != nil || !validLoopID(input.MessageID) || !validLoopQuestions(input.QuestionMessageIDs) {
			return invalid()
		}
		until, err = b.loopDeadline(input.Until)
		if err != nil {
			return invalid()
		}
		id, questions = input.MessageID, input.QuestionMessageIDs
	default:
		return nil, false, nil
	}
	var path string
	var options runtime.RequestOptions
	switch name {
	case "close_open_loop":
		path = "/api/v1/loops/{message_id}/done"
		options = &generated.MarkLoopDoneRequestOptions{PathParams: &generated.MarkLoopDonePath{MessageID: id}, Query: &generated.MarkLoopDoneQuery{QuestionMessageIds: questions}}
	case "reopen_open_loop":
		path = "/api/v1/loops/{message_id}/reopen"
		options = &generated.ReopenLoopRequestOptions{PathParams: &generated.ReopenLoopPath{MessageID: id}}
	case "snooze_open_loop":
		path = "/api/v1/loops/{message_id}/snooze"
		options = &generated.SnoozeLoopRequestOptions{PathParams: &generated.SnoozeLoopPath{MessageID: id}, Query: &generated.SnoozeLoopQuery{QuestionMessageIds: questions}, Body: &generated.SnoozeLoopBody{Until: until}}
	}
	result, err := readMCPLoopJSON[generated.LoopDecisionResponse](ctx, b.client, http.MethodPost, path, options)
	return result, true, err
}

func (b *daemonMCPOperations) loopOperationDisclosure(name string, args map[string]any) (string, bool, error) {
	if name == "set_commitment_decision" {
		input, until, err := b.commitmentDecisionInput(args)
		if err != nil {
			return "", true, &mcpserver.OperationRefusalError{Code: "invalid_arguments"}
		}
		if until != nil {
			input.Until = until.Format(time.RFC3339Nano)
		}
		data, err := json.Marshal(input, json.Deterministic(true))
		return string(data), true, err
	}
	var id int64
	var questions []int64
	var until string
	switch name {
	case "reopen_open_loop":
		input, err := decodeMCPOperationArguments[loopDecisionArguments](args)
		if err != nil || !validLoopID(input.MessageID) {
			return "", true, &mcpserver.OperationRefusalError{Code: "invalid_arguments"}
		}
		id = input.MessageID
	case "close_open_loop":
		input, err := decodeMCPOperationArguments[loopDoneArguments](args)
		if err != nil || !validLoopID(input.MessageID) || !validLoopQuestions(input.QuestionMessageIDs) {
			return "", true, &mcpserver.OperationRefusalError{Code: "invalid_arguments"}
		}
		id, questions = input.MessageID, input.QuestionMessageIDs
	case "snooze_open_loop":
		input, err := decodeMCPOperationArguments[loopSnoozeArguments](args)
		if err != nil || !validLoopID(input.MessageID) || !validLoopQuestions(input.QuestionMessageIDs) {
			return "", true, &mcpserver.OperationRefusalError{Code: "invalid_arguments"}
		}
		deadline, err := b.loopDeadline(input.Until)
		if err != nil {
			return "", true, &mcpserver.OperationRefusalError{Code: "invalid_arguments"}
		}
		id, questions, until = input.MessageID, input.QuestionMessageIDs, deadline.Format(time.RFC3339Nano)
	default:
		return "", false, nil
	}
	effect := map[string]string{
		"close_open_loop":  "mark exactly the listed questions done",
		"snooze_open_loop": "snooze exactly the listed questions until the deadline in until",
		"reopen_open_loop": "clear the saved decision on this exact question",
	}[name]
	data, err := json.Marshal(struct {
		Operation          string  `json:"operation"`
		MessageID          int64   `json:"message_id"`
		QuestionMessageIDs []int64 `json:"question_message_ids,omitempty"`
		Until              string  `json:"until,omitempty"`
		Effect             string  `json:"effect"`
	}{name, id, questions, until, effect}, json.Deterministic(true))
	return string(data), true, err
}

func loopRefusal(code string) bool {
	switch code {
	case "commitment_not_found", "loop_not_found", "loop_decision_not_found", "loop_scan_incomplete", "loops_unavailable", "invalid_request", "server_busy":
		return true
	default:
		return false
	}
}

func readMCPLoopJSON[T any](ctx context.Context, client *daemonclient.Client, method, path string, options runtime.RequestOptions) (*mcpserver.OperationResult, error) {
	return readMCPOperationJSONWithClassifier[T](ctx, client, method, path, options, loopRefusal, http.StatusOK)
}
