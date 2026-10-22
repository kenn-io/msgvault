package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"time"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func loopMCPCapabilities(hasRoute mcpRouteCheck, capabilities *apiprotocol.MCPCapabilities) []string {
	var names []string
	if hasRoute("listLoops", http.MethodGet, "/api/v1/loops", "person_id", "limit") {
		names = append(names, "list_open_loops")
	}
	for _, route := range []struct{ name, id, path string }{
		{"close_open_loop", "markLoopDone", "/api/v1/loops/{message_id}/done"},
		{"snooze_open_loop", "snoozeLoop", "/api/v1/loops/{message_id}/snooze"},
		{"reopen_open_loop", "reopenLoop", "/api/v1/loops/{message_id}/reopen"},
	} {
		if hasRoute(route.id, http.MethodPost, route.path) && (route.name != "snooze_open_loop" || mcpRequestPropertiesPresent(capabilities, route.id, "until")) {
			names = append(names, route.name)
		}
	}
	return names
}

type loopListArguments struct {
	PersonID *int64 `json:"person_id,omitempty"`
	Limit    *int64 `json:"limit,omitempty"`
}
type loopDecisionArguments struct {
	MessageID int64 `json:"message_id"`
}
type loopSnoozeArguments struct {
	MessageID int64  `json:"message_id"`
	Until     string `json:"until"`
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
		if input.Limit == nil {
			input.Limit = new(int64(50))
		}
		result, err := readMCPLoopJSON[generated.LoopListResponse](ctx, b.client, http.MethodGet, "/api/v1/loops", &generated.ListLoopsRequestOptions{Query: &generated.ListLoopsQuery{PersonID: input.PersonID, Limit: input.Limit}})
		return result, true, err
	}
	var id int64
	var until time.Time
	switch name {
	case "close_open_loop", "reopen_open_loop":
		input, err := decodeMCPOperationArguments[loopDecisionArguments](args)
		if err != nil || !validLoopID(input.MessageID) {
			return invalid()
		}
		id = input.MessageID
	case "snooze_open_loop":
		input, err := decodeMCPOperationArguments[loopSnoozeArguments](args)
		if err != nil || !validLoopID(input.MessageID) {
			return invalid()
		}
		until, err = b.loopDeadline(input.Until)
		if err != nil {
			return invalid()
		}
		id = input.MessageID
	default:
		return nil, false, nil
	}
	var path string
	var options runtime.RequestOptions
	switch name {
	case "close_open_loop":
		path = "/api/v1/loops/{message_id}/done"
		options = &generated.MarkLoopDoneRequestOptions{PathParams: &generated.MarkLoopDonePath{MessageID: id}}
	case "reopen_open_loop":
		path = "/api/v1/loops/{message_id}/reopen"
		options = &generated.ReopenLoopRequestOptions{PathParams: &generated.ReopenLoopPath{MessageID: id}}
	case "snooze_open_loop":
		path = "/api/v1/loops/{message_id}/snooze"
		options = &generated.SnoozeLoopRequestOptions{PathParams: &generated.SnoozeLoopPath{MessageID: id}, Body: &generated.SnoozeLoopBody{Until: until}}
	}
	result, err := readMCPLoopJSON[generated.LoopDecisionResponse](ctx, b.client, http.MethodPost, path, options)
	return result, true, err
}

func (b *daemonMCPOperations) loopOperationDisclosure(name string, args map[string]any) (string, bool, error) {
	var id int64
	var until string
	effect := "recheck the conversation and mark its currently eligible questions done"
	switch name {
	case "close_open_loop", "reopen_open_loop":
		input, err := decodeMCPOperationArguments[loopDecisionArguments](args)
		if err != nil || !validLoopID(input.MessageID) {
			return "", true, &mcpserver.OperationRefusalError{Code: "invalid_arguments"}
		}
		id = input.MessageID
		if name == "reopen_open_loop" {
			effect = "clear the saved decision on this exact question"
		}
	case "snooze_open_loop":
		input, err := decodeMCPOperationArguments[loopSnoozeArguments](args)
		if err != nil || !validLoopID(input.MessageID) {
			return "", true, &mcpserver.OperationRefusalError{Code: "invalid_arguments"}
		}
		deadline, err := b.loopDeadline(input.Until)
		if err != nil {
			return "", true, &mcpserver.OperationRefusalError{Code: "invalid_arguments"}
		}
		id, until = input.MessageID, deadline.Format(time.RFC3339Nano)
		effect = "recheck the conversation and snooze its currently eligible questions until the resolved instant"
	default:
		return "", false, nil
	}
	data, err := json.Marshal(struct {
		Operation string `json:"operation"`
		MessageID int64  `json:"message_id"`
		Until     string `json:"until,omitempty"`
		Effect    string `json:"effect"`
	}{name, id, until, effect}, json.Deterministic(true))
	return string(data), true, err
}

func loopRefusal(code string) bool {
	switch code {
	case "loop_not_found", "loop_decision_not_found", "loop_scan_incomplete", "loops_unavailable", "invalid_request", "server_busy":
		return true
	default:
		return false
	}
}

func readMCPLoopJSON[T any](ctx context.Context, client *daemonclient.Client, method, path string, options runtime.RequestOptions) (*mcpserver.OperationResult, error) {
	return readMCPOperationJSONWithClassifier[T](ctx, client, method, path, options, loopRefusal, http.StatusOK)
}
