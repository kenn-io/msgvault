package mcp

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/internal/freshness"
	"go.kenn.io/msgvault/internal/loops"
)

// LoopBackend uses the daemon-owned loop service without opening the archive.
type LoopBackend interface {
	ListOpenLoops(ctx context.Context, opts loops.ListOptions) (loops.ListResponse, error)
	GetOpenLoop(ctx context.Context, id int64) (loops.DetailResponse, error)
	GetWaitingLoops(ctx context.Context, opts loops.WaitingOptions) (loops.WaitingResponse, error)
	CloseOpenLoop(ctx context.Context, id int64, note string) (loops.DetailResponse, error)
	SnoozeOpenLoop(ctx context.Context, id int64, until time.Time) (loops.DetailResponse, error)
	ListLoopSuppressions(ctx context.Context) ([]loops.Suppression, error)
	CreateLoopSuppression(ctx context.Context, request loops.SuppressionRequest) (loops.SuppressionResult, error)
	DeleteLoopSuppression(ctx context.Context, id int64) error
}

type listOpenLoopsResponse struct {
	Status     string                `json:"status"`
	Items      *[]loops.Loop         `json:"items,omitempty"`
	Groups     *[]loops.WaitingGroup `json:"groups,omitempty"`
	Count      *int                  `json:"count,omitempty"`
	NextCursor string                `json:"next_cursor,omitempty"`
	Freshness  freshness.Report      `json:"freshness"`
	NextAction string                `json:"next_action,omitempty"`
}

type personProfileOpenLoops struct {
	Count         int              `json:"count"`
	WaitingOnMe   int              `json:"waiting_on_me"`
	WaitingOnThem int              `json:"waiting_on_them"`
	Items         []loops.Loop     `json:"items"`
	Freshness     freshness.Report `json:"freshness"`
}

type unmuteOpenLoopsResponse struct {
	Removed       bool  `json:"removed"`
	SuppressionID int64 `json:"suppression_id"`
}

func loopDefinitions() []toolDefinition {
	available := func(c catalogCapabilities) bool { return c.loops }
	list := readDefinition(ToolListOpenLoops, "Read daemon-derived unanswered turns with archive freshness. Waiting ranks people owed a reply; stale waiting returns status, freshness and next_action without rows unless allow_stale is true. Mention staleness to the user whenever freshness.stale is true.", closedObject(map[string]*jsonschema.Schema{
		"view":        stringSchema("View to read (default open); waiting cannot combine with kind or person_id", "open", "waiting", "held", "closed"),
		"kind":        stringSchema("Filter non-waiting views by kind; use view=open to combine with this filter", "reply_owed", "awaiting_reply", "commitment_owed_by_me", "commitment_owed_to_me"),
		"person_id":   safeIDSchema("Filter non-waiting views by durable person ID; use view=open to combine with this filter"),
		"limit":       boundedIntegerWithDefault("Maximum loops (1-200, default 50); waiting limits people and allows 1-50", 1, 200, 50),
		"cursor":      stringSchema("Opaque next_cursor for non-waiting views"),
		"allow_stale": booleanSchema("Explicitly allow stale results for waiting"),
	}), outputSchemaFor[listOpenLoopsResponse](), (*handlers).listOpenLoops)
	get := readDefinition(ToolGetOpenLoop, "Read one open-loop history row and archived evidence references with freshness. Mention staleness when freshness.stale is true.", closedObject(map[string]*jsonschema.Schema{"id": safeIDSchema("Loop ID")}, "id"), outputSchemaFor[loops.DetailResponse](), (*handlers).getOpenLoop)
	closeLoop := explicitlyConfirmedWriteDefinition(ToolCloseOpenLoop, "Mark a loop done after explicit client confirmation of its ID and optional owner note.", closedObject(map[string]*jsonschema.Schema{"id": safeIDSchema("Loop ID"), "note": stringSchema("Optional owner note")}, "id"), outputSchemaFor[loops.DetailResponse](), (*handlers).closeOpenLoop, toolSecurityLoopWrite)
	snooze := explicitlyConfirmedWriteDefinition(ToolSnoozeOpenLoop, "Snooze a loop until the exact future instant disclosed to and confirmed by the client. Dates use this MCP process's local timezone; durations start when approval is requested.", closedObject(map[string]*jsonschema.Schema{"id": safeIDSchema("Loop ID"), "until": stringSchema("Future RFC3339 timestamp, YYYY-MM-DD in the MCP process's local timezone, or positive duration such as 24h")}, "id", "until"), outputSchemaFor[loops.DetailResponse](), (*handlers).snoozeOpenLoop, toolSecurityLoopWrite)
	target := func() map[string]*jsonschema.Schema {
		return map[string]*jsonschema.Schema{"target_kind": stringSchema("Exact target type", "person", "participant", "conversation"), "id": safeIDSchema("Target ID, not a suppression record ID")}
	}
	muteProps := target()
	muteProps["note"] = stringSchema("Optional owner note")
	muteProps["dry_run"] = booleanSchema("Preview how many loops would close without persisting a mute")
	mute := explicitlyConfirmedWriteDefinition(ToolMuteOpenLoops, "Mute a person, participant or conversation, or preview affected loops, after client confirmation of the exact target, note and dry_run value.", closedObject(muteProps, "target_kind", "id"), outputSchemaFor[loops.SuppressionResult](), (*handlers).muteOpenLoops, toolSecurityLoopWrite)
	unmute := explicitlyConfirmedWriteDefinition(ToolUnmuteOpenLoops, "Remove the durable mute record matching the exact target after client confirmation. The daemon queues the target for re-evaluation.", closedObject(target(), "target_kind", "id"), outputSchemaFor[unmuteOpenLoopsResponse](), (*handlers).unmuteOpenLoops, toolSecurityLoopWrite)
	definitions := []toolDefinition{list, get, closeLoop, snooze, mute, unmute}
	for i := range definitions {
		definitions[i].availability = available
	}
	return definitions
}

func (h *handlers) listOpenLoops(ctx context.Context, req toolRequest) (*toolResult, error) {
	if h.loops == nil {
		return toolErrorResult("open loops are unavailable"), nil
	}
	args := req.GetArguments()
	view := "open"
	if raw, ok := args["view"]; ok {
		value, valid := raw.(string)
		if !valid || !slices.Contains([]string{"open", "waiting", "held", "closed"}, value) {
			return toolErrorResult("view must be open, waiting, held or closed"), nil
		}
		view = value
	}
	kind, err := loopOptionalString(args, "kind")
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	if kind != "" && !slices.Contains([]string{"reply_owed", "awaiting_reply", "commitment_owed_by_me", "commitment_owed_to_me"}, kind) {
		return toolErrorResult("invalid loop kind"), nil
	}
	personID, err := positiveInt64Arg(args, "person_id")
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	cursor, err := loopOptionalString(args, "cursor")
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	limit := 50
	if _, ok := args["limit"]; ok {
		value, e := requiredPeopleID(args, "limit")
		if e != nil || value > 200 {
			return toolErrorResult("limit must be an integer between 1 and 200"), nil //nolint:nilerr // MCP validation failures are tool results.
		}
		limit = int(value)
	}
	allowStale, err := loopOptionalBool(args, "allow_stale")
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	if view == "waiting" {
		if _, kindSet := args["kind"]; kindSet {
			return toolErrorResult("waiting does not support kind; use view=open for filters"), nil
		}
		if _, personSet := args["person_id"]; personSet {
			return toolErrorResult("waiting does not support person_id; use view=open for filters"), nil
		}
		if cursor != "" {
			return toolErrorResult("waiting does not support cursor; use view=open for pagination"), nil
		}
		if limit > 50 {
			return toolErrorResult("waiting limit must be between 1 and 50"), nil
		}
		result, err := h.loops.GetWaitingLoops(ctx, loops.WaitingOptions{Top: limit, AllowStale: allowStale})
		if err != nil {
			if stale, ok := errors.AsType[*loops.StaleError](err); ok {
				return jsonResult(listOpenLoopsResponse{Status: "stale", Freshness: stale.Response.Freshness, NextAction: "Refresh the sources listed in freshness.sources using their sync_command, then run msgvault loops run; or explicitly set allow_stale=true."})
			}
			return nil, newInternalError("list waiting loops", err)
		}
		if result.Groups == nil {
			result.Groups = []loops.WaitingGroup{}
		}
		return jsonResult(listOpenLoopsResponse{Status: "ok", Groups: &result.Groups, Count: &result.Count, Freshness: result.Freshness})
	}
	result, err := h.loops.ListOpenLoops(ctx, loops.ListOptions{Status: view, Kind: kind, PersonID: personID, Limit: limit, Cursor: cursor})
	if err != nil {
		return nil, newInternalError("list open loops", err)
	}
	if result.Items == nil {
		result.Items = []loops.Loop{}
	}
	return jsonResult(listOpenLoopsResponse{Status: "ok", Items: &result.Items, NextCursor: result.NextCursor, Freshness: result.Freshness})
}
func (h *handlers) getOpenLoop(ctx context.Context, req toolRequest) (*toolResult, error) {
	if h.loops == nil {
		return toolErrorResult("open loops are unavailable"), nil
	}
	id, err := requiredPeopleID(req.GetArguments(), "id")
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	result, err := h.loops.GetOpenLoop(ctx, id)
	if err != nil {
		return nil, newInternalError("get open loop", err)
	}
	return jsonResult(result)
}
func (h *handlers) personOpenLoops(ctx context.Context, personID int64) (*personProfileOpenLoops, error) {
	if h.loops == nil {
		return nil, nil //nolint:nilnil // Unwired differs from a certified empty projection.
	}
	result := &personProfileOpenLoops{Items: []loops.Loop{}}
	cursor := ""
	seen := map[string]bool{}
	for {
		page, err := h.loops.ListOpenLoops(ctx, loops.ListOptions{PersonID: personID, Status: "open", Limit: 200, Cursor: cursor})
		if err != nil {
			return nil, err
		}
		result.Freshness = page.Freshness
		for _, item := range page.Items {
			result.Count++
			switch item.Kind {
			case "reply_owed", "commitment_owed_by_me":
				result.WaitingOnMe++
			case "awaiting_reply", "commitment_owed_to_me":
				result.WaitingOnThem++
			}
			if len(result.Items) < 5 {
				result.Items = append(result.Items, item)
			}
		}
		if page.NextCursor == "" {
			return result, nil
		}
		if seen[page.NextCursor] {
			return nil, errors.New("loop pagination did not advance")
		}
		seen[page.NextCursor] = true
		cursor = page.NextCursor
	}
}

type loopMutationRequest struct {
	ID                      int64
	TargetKind, Note, Until string
	DryRun                  bool
}

func parseLoopMutation(args map[string]any, action string) (loopMutationRequest, error) {
	var result loopMutationRequest
	var err error
	result.ID, err = requiredPeopleID(args, "id")
	if err != nil {
		return result, err
	}
	if float64(result.ID) > maxJSONSafeInteger {
		return result, errors.New("id must be a safe positive integer")
	}
	if action == ToolMuteOpenLoops || action == ToolUnmuteOpenLoops {
		result.TargetKind, err = loopOptionalString(args, "target_kind")
		if err != nil {
			return result, err
		}
		if !slices.Contains([]string{"person", "participant", "conversation"}, result.TargetKind) {
			return result, errors.New("target_kind must be person, participant or conversation")
		}
	}
	if action == ToolCloseOpenLoop || action == ToolMuteOpenLoops {
		result.Note, err = loopOptionalString(args, "note")
		if err != nil {
			return result, err
		}
	}
	if action == ToolMuteOpenLoops {
		result.DryRun, err = loopOptionalBool(args, "dry_run")
		if err != nil {
			return result, err
		}
	}
	if action == ToolSnoozeOpenLoop {
		result.Until, err = loopOptionalString(args, "until")
		if err != nil {
			return result, err
		}
		if result.Until == "" {
			return result, errors.New("until is required")
		}
	}
	return result, nil
}
func loopOptionalString(args map[string]any, key string) (string, error) {
	raw, ok := args[key]
	if !ok {
		return "", nil
	}
	value, valid := raw.(string)
	if !valid {
		return "", fmt.Errorf("%s must be text", key)
	}
	return value, nil
}
func loopOptionalBool(args map[string]any, key string) (bool, error) {
	raw, ok := args[key]
	if !ok {
		return false, nil
	}
	value, valid := raw.(bool)
	if !valid {
		return false, fmt.Errorf("%s must be a boolean", key)
	}
	return value, nil
}
func (h *handlers) closeOpenLoop(ctx context.Context, req toolRequest) (*toolResult, error) {
	return h.mutateLoop(ctx, req, ToolCloseOpenLoop)
}
func (h *handlers) snoozeOpenLoop(ctx context.Context, req toolRequest) (*toolResult, error) {
	return h.mutateLoop(ctx, req, ToolSnoozeOpenLoop)
}
func (h *handlers) muteOpenLoops(ctx context.Context, req toolRequest) (*toolResult, error) {
	return h.mutateLoop(ctx, req, ToolMuteOpenLoops)
}
func (h *handlers) unmuteOpenLoops(ctx context.Context, req toolRequest) (*toolResult, error) {
	return h.mutateLoop(ctx, req, ToolUnmuteOpenLoops)
}
func (h *handlers) mutateLoop(ctx context.Context, req toolRequest, action string) (*toolResult, error) {
	if h.loops == nil {
		return toolErrorResult("open loops are unavailable"), nil
	}
	request, err := parseLoopMutation(req.GetArguments(), action)
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	snapshot, err := json.Marshal(req.GetArguments(), json.Deterministic(true))
	if err != nil {
		return toolErrorResult("invalid loop request"), nil //nolint:nilerr // MCP validation failures are tool results.
	}
	referenceTime := req.confirmationTime
	if referenceTime.IsZero() {
		referenceTime = time.Now()
	}
	var until time.Time
	var suppressionID int64
	disclosure := ""
	switch action {
	case ToolCloseOpenLoop:
		disclosure = fmt.Sprintf("Close open loop %d as done. Owner note: %q.", request.ID, request.Note)
	case ToolSnoozeOpenLoop:
		until, err = loops.ParseSnoozeUntil(request.Until, referenceTime)
		if err != nil {
			return toolErrorResult(err.Error()), nil
		}
		disclosure = fmt.Sprintf("Snooze open loop %d until %s (requested %q).", request.ID, until.Format(time.RFC3339Nano), request.Until)
	case ToolMuteOpenLoops:
		disclosure = fmt.Sprintf("Mute open loops for %s %d. Owner note: %q. dry_run=%t; true previews affected loops without persisting a mute.", request.TargetKind, request.ID, request.Note, request.DryRun)
	case ToolUnmuteOpenLoops:
		rows, err := h.loops.ListLoopSuppressions(ctx)
		if err != nil {
			return nil, newInternalError("find loop suppression", err)
		}
		for _, row := range rows {
			if row.TargetKind != request.TargetKind {
				continue
			}
			var target *int64
			switch request.TargetKind {
			case "person":
				target = row.PersonID
			case "participant":
				target = row.ParticipantID
			case "conversation":
				target = row.ConversationID
			}
			if target != nil && *target == request.ID {
				suppressionID = row.ID
				break
			}
		}
		if suppressionID <= 0 {
			return toolErrorResult("no mute exists for that exact target"), nil
		}
		disclosure = fmt.Sprintf("Unmute open loops for %s %d by removing suppression %d; queue its conversations for re-evaluation.", request.TargetKind, request.ID, suppressionID)
	}
	if err := req.confirmUserAction(ctx, disclosure); err != nil {
		return confirmationToolError(err)
	}
	// The SDK owns this argument map. Approval cannot authorize changes to any
	// raw or parsed field while a client is obtaining confirmation.
	reread, parseErr := parseLoopMutation(req.GetArguments(), action)
	encoded, encodeErr := json.Marshal(req.GetArguments(), json.Deterministic(true))
	if parseErr != nil || encodeErr != nil || reread != request || !bytes.Equal(snapshot, encoded) {
		return toolErrorResult("action cancelled; loop arguments changed after confirmation"), nil //nolint:nilerr // MCP validation failures are tool results.
	}
	if action == ToolSnoozeOpenLoop {
		reparsed, err := loops.ParseSnoozeUntil(reread.Until, referenceTime)
		if err != nil || !reparsed.Equal(until) {
			return toolErrorResult("action cancelled; snooze instant changed after confirmation"), nil //nolint:nilerr // MCP validation failures are tool results.
		}
		if !reparsed.After(time.Now()) {
			return toolErrorResult("snooze instant has passed; re-run with a later until"), nil
		}
	}
	switch action {
	case ToolCloseOpenLoop:
		result, err := h.loops.CloseOpenLoop(ctx, request.ID, request.Note)
		if err != nil {
			return nil, newInternalError("close open loop", err)
		}
		return jsonResult(result)
	case ToolSnoozeOpenLoop:
		result, err := h.loops.SnoozeOpenLoop(ctx, request.ID, until)
		if err != nil {
			return nil, newInternalError("snooze open loop", err)
		}
		return jsonResult(result)
	case ToolMuteOpenLoops:
		input := loops.SuppressionRequest{TargetKind: request.TargetKind, Note: request.Note, DryRun: request.DryRun}
		switch request.TargetKind {
		case "person":
			input.PersonID = &request.ID
		case "participant":
			input.ParticipantID = &request.ID
		case "conversation":
			input.ConversationID = &request.ID
		}
		result, err := h.loops.CreateLoopSuppression(ctx, input)
		if err != nil {
			return nil, newInternalError("mute open loops", err)
		}
		return jsonResult(result)
	case ToolUnmuteOpenLoops:
		if err := h.loops.DeleteLoopSuppression(ctx, suppressionID); err != nil {
			return nil, newInternalError("unmute open loops", err)
		}
		return jsonResult(unmuteOpenLoopsResponse{Removed: true, SuppressionID: suppressionID})
	}
	return toolErrorResult("unsupported loop action"), nil
}
