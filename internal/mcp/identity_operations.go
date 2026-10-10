package mcp

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/identitycontrol"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// IdentityOperationBackend keeps mutation and authorization in the daemon.
type IdentityOperationBackend interface {
	PreviewIdentityOperation(ctx context.Context, request identitycontrol.PreviewRequest) (*generated.IdentityOperationPreviewResponse, error)
	ApplyIdentityOperation(ctx context.Context, request generated.IdentityOperationApplyRequest) (*generated.IdentityOperationApplyResponse, error)
	GetIdentityOperationReceipt(ctx context.Context, key string) (*generated.IdentityReceipt, error)
	GetIdentityOperationReceiptByID(ctx context.Context, id string) (*generated.IdentityReceipt, error)
	GetIdentityOperationReceiptForPrincipal(ctx context.Context, principal, key string) (*generated.IdentityReceipt, error)
}

var stableIdentityOperationDefinitions = sync.OnceValue(identityOperationDefinitions)

func identityOperationDefinitions() []toolDefinition {
	preview := readDefinition(ToolPreviewIdentityOperation,
		"Preview one verified participant pair or participant/person binding. Returns complete scoped native evidence and a five-minute signed token. Graph links preserve message authorship; linking does not merge a conversation or group.",
		closedObject(map[string]*jsonschema.Schema{
			"operation":            stringSchema("Exact identity action", "graph-link", "graph-unlink", "person-link", "person-unlink"),
			"participant_id":       safeIDSchema("Observed participant"),
			"other_participant_id": safeIDSchema("Second participant for graph actions"),
			toolArgPersonID:        safeIDSchema("Durable person for binding actions"),
		}, "operation", "participant_id"), outputSchemaFor[generated.IdentityOperationPreviewResponse](), (*handlers).previewIdentityOperation)
	receipt := readDefinition(ToolGetIdentityReceipt,
		"Read one committed native receipt without replaying a write. Use the original idempotency key after an unknown outcome. Owner recovery may instead select an exact receipt ID or the prior principal and key; it does not restore that principal's authority.",
		closedObject(map[string]*jsonschema.Schema{
			"idempotency_key": stringSchema("Original request key"), "receipt_id": stringSchema("Owner-only exact receipt ID"), "principal": stringSchema("Owner-only original principal, with key"),
		}), outputSchemaFor[generated.IdentityReceipt](), (*handlers).getIdentityOperationReceipt)
	definitions := []toolDefinition{preview, receipt}
	for _, entry := range []struct {
		name      string
		operation identitycontrol.Operation
		endpoint  string
	}{
		{ToolLinkParticipantIdentity, identitycontrol.OperationGraphLink, "other_participant_id"},
		{ToolUnlinkParticipantIdentity, identitycontrol.OperationGraphUnlink, "other_participant_id"},
		{ToolLinkParticipantToPerson, identitycontrol.OperationPersonLink, toolArgPersonID},
		{ToolUnlinkParticipantFromPerson, identitycontrol.OperationPersonUnlink, toolArgPersonID},
	} {
		operation := entry.operation
		properties := map[string]*jsonschema.Schema{
			"participant_id": safeIDSchema("Observed participant"), entry.endpoint: safeIDSchema("Exact second endpoint"),
			"expected_fingerprint": stringSchema("Exact fingerprint from native preview"), "preview_token": stringSchema("Exact signed native preview token"), "idempotency_key": stringSchema("Opaque key reused only for the same request"),
		}
		definition := explicitlyConfirmedWriteDefinition(entry.name,
			fmt.Sprintf("Apply one reviewed %s through the scoped native service. Requires the exact preview, fingerprint, idempotency key and client confirmation. Published contacts and active merge recovery remain guarded. Graph unlink retains durable person bindings. On an unknown outcome, read its receipt before deciding whether to retry.", operation),
			closedObject(properties, "participant_id", entry.endpoint, "expected_fingerprint", "preview_token", "idempotency_key"), outputSchemaFor[generated.IdentityOperationApplyResponse](),
			func(h *handlers, ctx context.Context, request toolRequest) (*toolResult, error) {
				return h.applyIdentityOperation(ctx, request, operation)
			}, toolSecurityIdentityDecision)
		definitions = append(definitions, definition)
	}
	return definitions
}

func identityIntent(args map[string]any, operation identitycontrol.Operation) (identitycontrol.PreviewRequest, error) {
	participant, err := requiredPeopleID(args, "participant_id")
	if err != nil {
		return identitycontrol.PreviewRequest{}, err
	}
	request := identitycontrol.PreviewRequest{Operation: operation, Target: identitycontrol.IdentityTarget{ParticipantID: participant}}
	if _, present := args["other_participant_id"]; present {
		request.Target.OtherParticipantID, err = requiredPeopleID(args, "other_participant_id")
		if err != nil {
			return request, err
		}
	}
	if _, present := args[toolArgPersonID]; present {
		request.Target.PersonID, err = requiredPeopleID(args, toolArgPersonID)
		if err != nil {
			return request, err
		}
	}
	return request, request.Validate()
}

func identityOperationResult(value any, err error) (*toolResult, error) {
	if err != nil {
		if _, unknown := errors.AsType[*daemonclient.IdentityOutcomeUnknownError](err); unknown {
			return toolErrorResult("identity_outcome_unknown: read get_identity_receipt with the original idempotency key before deciding whether to retry"), nil
		}
		return toolErrorResult(daemonclient.SafeMCPError(err).Error()), nil
	}
	return jsonResult(value)
}

func (h *handlers) previewIdentityOperation(ctx context.Context, request toolRequest) (*toolResult, error) {
	args := request.GetArguments()
	operation, err := requiredMCPText(args, "operation")
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	intent, err := identityIntent(args, identitycontrol.Operation(operation))
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	value, err := h.identityOperations.PreviewIdentityOperation(ctx, intent)
	return identityOperationResult(value, err)
}

func (h *handlers) applyIdentityOperation(ctx context.Context, request toolRequest, operation identitycontrol.Operation) (*toolResult, error) {
	args := request.GetArguments()
	intent, err := identityIntent(args, operation)
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	fingerprint, err := requiredMCPText(args, "expected_fingerprint")
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	token, err := requiredMCPText(args, "preview_token")
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	key, err := requiredMCPText(args, "idempotency_key")
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	target := generated.IdentityTarget{ParticipantID: intent.Target.ParticipantID}
	if intent.Target.OtherParticipantID != 0 {
		target.OtherParticipantID = &intent.Target.OtherParticipantID
	}
	if intent.Target.PersonID != 0 {
		target.PersonID = &intent.Target.PersonID
	}
	endpoints := fmt.Sprintf("participants %d and %d", intent.Target.ParticipantID, intent.Target.OtherParticipantID)
	if intent.Target.PersonID != 0 {
		endpoints = fmt.Sprintf("participant %d and person %d", intent.Target.ParticipantID, intent.Target.PersonID)
	}
	message := fmt.Sprintf("Confirm %s for %s using the exact reviewed fingerprint %q? This changes archive identity only and preserves separate message authorship. A completed request returns its saved receipt.", operation, endpoints, fingerprint)
	if err := request.confirmUserAction(ctx, message); err != nil {
		return confirmationToolError(err)
	}
	value, err := h.identityOperations.ApplyIdentityOperation(ctx, generated.IdentityOperationApplyRequest{Operation: generated.IdentityOperationApplyRequestOperation(operation), Target: target, ExpectedFingerprint: fingerprint, PreviewToken: token, IdempotencyKey: key})
	return identityOperationResult(value, err)
}

func (h *handlers) getIdentityOperationReceipt(ctx context.Context, request toolRequest) (*toolResult, error) {
	args := request.GetArguments()
	_, hasKey := args["idempotency_key"]
	_, hasID := args["receipt_id"]
	_, hasPrincipal := args["principal"]
	if hasKey == hasID || (hasPrincipal && !hasKey) {
		return toolErrorResult("select one exact idempotency key or owner receipt ID"), nil
	}
	var receipt *generated.IdentityReceipt
	var err error
	if hasID {
		value, parseErr := requiredMCPText(args, "receipt_id")
		if parseErr != nil {
			return toolErrorResult(parseErr.Error()), nil
		}
		receipt, err = h.identityOperations.GetIdentityOperationReceiptByID(ctx, value)
	} else {
		value, parseErr := requiredMCPText(args, "idempotency_key")
		if parseErr != nil {
			return toolErrorResult(parseErr.Error()), nil
		}
		if hasPrincipal {
			actor, parseErr := requiredMCPText(args, "principal")
			if parseErr != nil {
				return toolErrorResult(parseErr.Error()), nil
			}
			receipt, err = h.identityOperations.GetIdentityOperationReceiptForPrincipal(ctx, actor, value)
		} else {
			receipt, err = h.identityOperations.GetIdentityOperationReceipt(ctx, value)
		}
	}
	return identityOperationResult(receipt, err)
}
