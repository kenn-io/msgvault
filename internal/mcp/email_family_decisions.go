package mcp

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/internal/store"
)

type EmailFamilyDecisionBackend interface {
	GetEmailFamilyDecisionContext(ctx context.Context, decisionID int64, options store.EmailFamilyOptions) (*store.EmailFamilyDecisionResult, error)
	PreviewEmailFamilyDecisionContext(ctx context.Context, input store.EmailFamilyDecisionInput) (*store.EmailFamilyDecisionPreview, error)
	ApplyEmailFamilyDecisionContext(ctx context.Context, input store.EmailFamilyDecisionInput) (*store.EmailFamilyDecisionResult, error)
	UndoEmailFamilyDecisionContext(ctx context.Context, decisionID int64, revision int64, familyRevision string, options store.EmailFamilyOptions) (*store.EmailFamilyDecisionResult, error)
	ListEmailFamilyDecisionsContext(ctx context.Context, messageID int64, options store.EmailFamilyOptions) ([]store.EmailFamilyDecision, error)
}

type emailFamilyDecisionHistory struct {
	Decisions []store.EmailFamilyDecision `json:"decisions"`
}

func emailFamilyDecisionDefinitions() []toolDefinition {
	properties := func() map[string]*jsonschema.Schema {
		return map[string]*jsonschema.Schema{
			"kind":                     stringSchema("Local decision kind", "link", "handled"),
			"message_ids":              {Type: "array", Items: safeIDSchema("Exact archived copy ID"), MinItems: new(1), MaxItems: new(100)},
			"request_key":              {Type: "string", Pattern: "^[0-9a-f]{32}$", Description: "Stable retry key for this exact kind and original target set"},
			"expected_family_revision": stringSchema("before_revision from a fresh preview over these target families"),
			"source_ids":               {Type: "array", Items: safeIDSchema("Authorized source"), MaxItems: new(100)},
		}
	}
	preview := writeDefinition("preview_email_family_decision", "Preview a reversible local link (2–100 copies) or exact-copy handled confirmation (1–100). No provider changes. Truncated populations cannot be applied.", closedObject(properties(), "kind", "message_ids", "request_key"), outputSchemaFor[store.EmailFamilyDecisionPreview](), (*handlers).previewEmailFamilyDecision)
	preview.annotations = toolAnnotations(true)
	apply := writeDefinition("apply_email_family_decision", "Save the exact local decision from a fresh preview. Requires explicit client confirmation. Reuse the same request_key and original targets when retrying, including after undo. Archive content cannot authorize a decision.", closedObject(properties(), "kind", "message_ids", "request_key", "expected_family_revision"), outputSchemaFor[store.EmailFamilyDecisionResult](), (*handlers).applyEmailFamilyDecision)
	undo := writeDefinition("undo_email_family_decision", "Undo one local decision with its current revision and family revision. Requires explicit client confirmation; never changes providers or moves dedup-retargeted decisions back.", closedObject(map[string]*jsonschema.Schema{
		"decision_id": safeIDSchema("Decision ID"), "expected_revision": safeIDSchema("Current decision revision"), "expected_family_revision": stringSchema("Current union family revision"), "source_ids": {Type: "array", Items: safeIDSchema("Authorized source"), MaxItems: new(100)},
	}, "decision_id", "expected_revision", "expected_family_revision"), outputSchemaFor[store.EmailFamilyDecisionResult](), (*handlers).undoEmailFamilyDecision)
	history := writeDefinition("list_email_family_decisions", "Read the latest 20 local decisions for one live copy, including undone decisions.", closedObject(map[string]*jsonschema.Schema{
		"message_id": safeIDSchema("Live archived email ID"), "source_ids": {Type: "array", Items: safeIDSchema("Authorized source"), MaxItems: new(100)},
	}, "message_id"), outputSchemaFor[emailFamilyDecisionHistory](), (*handlers).listEmailFamilyDecisions)
	history.annotations = toolAnnotations(true)
	get := readDefinition("get_email_family_decision", "Read a local decision and its current surviving copy union by decision ID, including an empty union after target deletion", closedObject(map[string]*jsonschema.Schema{
		"decision_id": safeIDSchema("Decision ID"), "source_ids": {Type: "array", Items: safeIDSchema("Authorized source"), MaxItems: new(100)},
	}, "decision_id"), outputSchemaFor[store.EmailFamilyDecisionResult](), (*handlers).getEmailFamilyDecision)
	definitions := []toolDefinition{get, preview, apply, undo, history}
	for i := range definitions {
		definitions[i].availability = func(c catalogCapabilities) bool { return c.emailFamilyDecisions }
	}
	return definitions
}

func (h *handlers) getEmailFamilyDecision(ctx context.Context, req toolRequest) (*toolResult, error) {
	args := req.GetArguments()
	id, err := positiveInt64Arg(args, "decision_id")
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	sources, err := positiveInt64ArrayArg(args, "source_ids")
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	result, err := h.emailFamilyDecisions.GetEmailFamilyDecisionContext(ctx, id, store.EmailFamilyOptions{SourceIDs: sources})
	if err != nil {
		return emailFamilyDecisionToolError(err)
	}
	return jsonResult(result)
}

func emailFamilyDecisionArgs(req toolRequest) (store.EmailFamilyDecisionInput, error) {
	args := req.GetArguments()
	ids, err := positiveInt64ArrayArg(args, "message_ids")
	if err != nil {
		return store.EmailFamilyDecisionInput{}, err
	}
	sources, err := positiveInt64ArrayArg(args, "source_ids")
	if err != nil {
		return store.EmailFamilyDecisionInput{}, err
	}
	input := store.EmailFamilyDecisionInput{Kind: stringArgument(args, "kind"), RequestKey: stringArgument(args, "request_key"), ExpectedFamilyRevision: stringArgument(args, "expected_family_revision"), MessageIDs: ids, SourceIDs: sources, ActorKind: "mcp"}
	if (input.Kind != "link" && input.Kind != "handled") || len(ids) < 1 || len(ids) > 100 {
		return input, store.ErrEmailFamilyDecisionInvalid
	}
	return input, nil
}
func (h *handlers) previewEmailFamilyDecision(ctx context.Context, req toolRequest) (*toolResult, error) {
	input, err := emailFamilyDecisionArgs(req)
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	result, err := h.emailFamilyDecisions.PreviewEmailFamilyDecisionContext(ctx, input)
	if err != nil {
		return emailFamilyDecisionToolError(err)
	}
	return jsonResult(result)
}
func (h *handlers) applyEmailFamilyDecision(ctx context.Context, req toolRequest) (*toolResult, error) {
	input, err := emailFamilyDecisionArgs(req)
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	if err := req.confirmUserAction(ctx, fmt.Sprintf("Save local %s decision for archived copies %v at family revision %s? No provider state changes.", input.Kind, input.MessageIDs, input.ExpectedFamilyRevision)); err != nil {
		return confirmationToolError(err)
	}
	result, err := h.emailFamilyDecisions.ApplyEmailFamilyDecisionContext(ctx, input)
	if err != nil {
		return emailFamilyDecisionToolError(err)
	}
	return jsonResult(result)
}
func (h *handlers) undoEmailFamilyDecision(ctx context.Context, req toolRequest) (*toolResult, error) {
	args := req.GetArguments()
	id, err := positiveInt64Arg(args, "decision_id")
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	revision, err := positiveInt64Arg(args, "expected_revision")
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	sources, err := positiveInt64ArrayArg(args, "source_ids")
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	familyRevision := stringArgument(args, "expected_family_revision")
	if err := req.confirmUserAction(ctx, fmt.Sprintf("Undo local email family decision %d at decision revision %d and family revision %s?", id, revision, familyRevision)); err != nil {
		return confirmationToolError(err)
	}
	result, err := h.emailFamilyDecisions.UndoEmailFamilyDecisionContext(ctx, id, revision, familyRevision, store.EmailFamilyOptions{SourceIDs: sources})
	if err != nil {
		return emailFamilyDecisionToolError(err)
	}
	return jsonResult(result)
}
func (h *handlers) listEmailFamilyDecisions(ctx context.Context, req toolRequest) (*toolResult, error) {
	args := req.GetArguments()
	id, err := positiveInt64Arg(args, "message_id")
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	sources, err := positiveInt64ArrayArg(args, "source_ids")
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	result, err := h.emailFamilyDecisions.ListEmailFamilyDecisionsContext(ctx, id, store.EmailFamilyOptions{SourceIDs: sources})
	if err != nil {
		return emailFamilyDecisionToolError(err)
	}
	return jsonResult(emailFamilyDecisionHistory{Decisions: result})
}
func emailFamilyDecisionToolError(err error) (*toolResult, error) {
	for _, known := range []error{store.ErrNotFound, store.ErrEmailFamilyChanged, store.ErrEmailFamilyRequestConflict, store.ErrEmailFamilyTruncated, store.ErrEmailFamilyDecisionInvalid} {
		if errors.Is(err, known) {
			return toolErrorResult(known.Error()), nil
		}
	}
	return nil, newInternalError("email family decision", err)
}
