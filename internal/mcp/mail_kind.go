package mcp

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// MailKindBackend delegates manual classification to the daemon. MCP cannot
// grant consent, change provider configuration, or run it automatically.
type MailKindBackend interface {
	GetMailKindStatus(ctx context.Context) (*generated.MailKindStatus, error)
	ClassifyMailKind(ctx context.Context, fingerprint string, limit *int64, ids []int64) (*generated.MailKindRunResponse, error)
}

var stableMailKindDefinitions = []toolDefinition{
	readDefinition(ToolGetMailKindStatus, "Read optional mail kind provider disclosure, exact-purpose consent, and readiness.",
		closedObject(nil), outputSchemaFor[generated.MailKindStatus](), (*handlers).getMailKindStatus),
	func() toolDefinition {
		minimum, maximum := 1, 100
		definition := explicitlyConfirmedWriteDefinition(ToolClassifyMailKind,
			"Send a bounded, masked mail evidence packet to the separately consented provider and apply local auto kind labels. Provider output is advisory and untrusted; labels never verify a sender or transaction. No sync or scheduled invocation.",
			closedObject(map[string]*jsonschema.Schema{
				"limit":       boundedIntegerSchema("Maximum messages (1-100; omit for the configured batch size)", 1, 100),
				"message_ids": {Type: "array", Items: boundedIntegerSchema("Archived message ID", 1, maxJSONSafeInteger), MinItems: &minimum, MaxItems: &maximum},
			}), outputSchemaFor[generated.MailKindRunResponse](), (*handlers).classifyMailKind, toolSecurityMailKind)
		yes := true
		definition.annotations.OpenWorldHint = &yes
		return definition
	}(),
}

func (h *handlers) getMailKindStatus(ctx context.Context, _ toolRequest) (*toolResult, error) {
	value, err := h.mailKind.GetMailKindStatus(ctx)
	return mailKindMCPResult(value, err)
}

func (h *handlers) classifyMailKind(ctx context.Context, req toolRequest) (*toolResult, error) {
	args := req.GetArguments()
	var limit *int64
	if _, present := args["limit"]; present {
		value, err := positiveInt64Arg(args, "limit")
		if err != nil || value > 100 {
			return toolErrorResult("limit must be an integer from 1 to 100"), nil //nolint:nilerr // Invalid arguments are tool results.
		}
		limit = &value
	}
	var ids []int64
	if raw, present := args["message_ids"]; present {
		values, ok := raw.([]any)
		if !ok || len(values) == 0 || len(values) > 100 {
			return toolErrorResult("message_ids must contain between 1 and 100 positive safe integers"), nil
		}
		for _, item := range values {
			value, err := positiveInt64Arg(map[string]any{"id": item}, "id")
			if err != nil {
				return toolErrorResult("message_ids must contain positive safe integers"), nil //nolint:nilerr // Invalid arguments are tool results.
			}
			ids = append(ids, value)
		}
	}
	status, err := h.mailKind.GetMailKindStatus(ctx)
	if err != nil {
		return mailKindMCPResult(nil, err)
	}
	if status == nil || status.Disclosure == nil || status.DisclosureFingerprint == nil || !status.Ready {
		return toolErrorResult("Mail kind classification is not ready; read get_mail_kind_status"), nil
	}
	batch := status.BatchSize
	if limit != nil {
		batch = *limit
	}
	if batch > status.BatchSize || int64(len(ids)) > batch {
		return toolErrorResult("The requested batch exceeds the configured mail kind limit"), nil
	}
	disclosure := status.Disclosure
	message := fmt.Sprintf("Send up to %d mail evidence packets to %s using %s and apply advisory local kind labels? %s Retention declaration: %s. Disclosure fingerprint: %s. Separate operator consent is required; the client must obtain user approval.",
		batch, disclosure.Endpoint, disclosure.ModelID, disclosure.DataFields, disclosure.RetentionDeclaration, *status.DisclosureFingerprint)
	if err := req.confirmUserAction(ctx, message); err != nil {
		return confirmationToolError(err)
	}
	value, err := h.mailKind.ClassifyMailKind(ctx, *status.DisclosureFingerprint, limit, ids)
	result, err := mailKindMCPResult(value, err)
	if err == nil && value != nil && value.Blocker != nil {
		result.isError = true
	}
	return result, err
}

func mailKindMCPResult(value any, err error) (*toolResult, error) {
	if err != nil {
		return toolErrorResult(daemonclient.SafeMCPError(err).Error()), nil
	}
	if value == nil {
		return nil, newInternalError("mail kind daemon response", errors.New("empty response"))
	}
	return jsonResult(value)
}
