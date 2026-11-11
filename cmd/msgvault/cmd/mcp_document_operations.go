package cmd

import (
	"context"
	"net/http"

	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func documentMCPCapabilities(hasRoute mcpRouteCheck) []string {
	var names []string
	if hasRoute("getDocbankAttachmentStatus", http.MethodGet, "/api/v1/integrations/docbank/attachments") {
		names = append(names, "get_docbank_attachment_status")
	}
	if hasRoute("backfillDocbankAttachments", http.MethodPost, "/api/v1/integrations/docbank/attachments/backfill") {
		names = append(names, "retry_docbank_attachment_mirror")
	}
	return names
}

func (b *daemonMCPOperations) executeDocumentOperation(ctx context.Context, name string, _ map[string]any) (*mcpserver.OperationResult, bool, error) {
	if name != "get_docbank_attachment_status" && name != "retry_docbank_attachment_mirror" {
		return nil, false, nil
	}
	if name == "retry_docbank_attachment_mirror" {
		result, err := readMCPOperationJSONWithClassifier[generated.DocbankAttachmentBackfillResponse](ctx, b.client, http.MethodPost, "/api/v1/integrations/docbank/attachments/backfill", nil, documentRefusal, http.StatusAccepted)
		if err == nil && !result.IsError && result.Output.(generated.DocbankAttachmentBackfillResponse).Status != "scheduled" {
			return operationFailure("invalid_operation_response", true), true, nil
		}
		return result, true, err
	}
	result, err := readMCPOperationJSON[generated.DocbankAttachmentStatus](ctx, b.client, http.MethodGet, "/api/v1/integrations/docbank/attachments", nil, http.StatusOK)
	if err != nil || result.IsError {
		return result, true, err
	}
	status := result.Output.(generated.DocbankAttachmentStatus)
	if err := status.State.Validate(); err != nil {
		return operationFailure("invalid_operation_response", false), true, nil
	}
	safe := mcpserver.DocumentMirrorStatus{State: string(status.State), UploadConsent: status.UploadConsent}
	if c := status.Counts; c != nil {
		safe.Counts = &mcpserver.DocumentMirrorCounts{Sending: c.Sending, Delivered: c.Delivered, Remaining: c.Remaining, Failed: c.Failed}
	}
	if p := status.Processing; p != nil {
		safe.Processing = &mcpserver.DocumentProcessingCounts{Waiting: p.Waiting, Requested: p.Requested, Extracted: p.Extracted}
		for _, count := range p.Outcomes {
			safe.Processing.Other += count
		}
	}
	result.Output = safe
	return result, true, nil
}

func documentRefusal(code string) bool { return code == "conflict" || code == "server_busy" }
