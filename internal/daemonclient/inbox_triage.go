package daemonclient

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"slices"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// PreviewInboxTriage reads one bounded proposal without authorizing any write.
func (c *Client) PreviewInboxTriage(ctx context.Context, input inboxcontrol.TriageInput) (*inboxcontrol.TriageProposal, error) {
	if err := input.Validate(); err != nil {
		return nil, err
	}
	data, err := json.Marshal(input)
	if err != nil || len(data) > 1<<20 {
		return nil, inboxcontrol.ErrInvalid
	}
	var body generated.PreviewInboxTriageBody
	if json.Unmarshal(data, &body) != nil {
		return nil, inboxcontrol.ErrInvalid
	}
	data, err = c.triageRequest(ctx, "previewInboxTriage", "/api/v1/inbox/triage/preview", []string{"source", "items"}, &generated.PreviewInboxTriageRequestOptions{Body: &body}, false)
	if err != nil {
		return nil, err
	}
	var proposal inboxcontrol.TriageProposal
	if json.Unmarshal(data, &proposal) != nil || validateClientTriageProposal(proposal) != nil || proposal.Source != input.Source || len(proposal.Items) != len(input.Items) {
		return nil, inboxcontrol.ErrUnavailable
	}
	for i, item := range proposal.Items {
		original := input.Items[i]
		if *item.Request.Target != original.Target || !slices.Equal(item.Categories, original.Categories) || !slices.Equal(item.EvidenceMessageIDs, original.EvidenceMessageIDs) || (original.IdempotencyKey != "" && item.Request.IdempotencyKey != original.IdempotencyKey) {
			return nil, inboxcontrol.ErrUnavailable
		}
	}
	return &proposal, nil
}

// ApplyInboxTriage forwards the complete envelope once. Even expired proposals
// reach the daemon so authorized durable receipts can be recovered without writes.
func (c *Client) ApplyInboxTriage(ctx context.Context, proposal inboxcontrol.TriageProposal) ([]inboxcontrol.Result, error) {
	if err := validateClientTriageProposal(proposal); err != nil {
		return nil, err
	}
	data, err := json.Marshal(proposal)
	if err != nil || len(data) > 1<<20 {
		return nil, inboxcontrol.ErrInvalid
	}
	var body generated.ApplyInboxTriageBody
	if json.Unmarshal(data, &body) != nil {
		return nil, inboxcontrol.ErrInvalid
	}
	data, callErr := c.triageRequest(ctx, "applyInboxTriage", "/api/v1/inbox/triage/apply", []string{"source", "items", "mapping_revision", "archive_revision", "incoming_watermark", "issued_at", "expires_at", "preview_token"}, &generated.ApplyInboxTriageRequestOptions{Body: &body}, true)
	var results []inboxcontrol.Result
	if callErr != nil {
		var failure struct {
			Results []inboxcontrol.Result `json:"results"`
		}
		if len(data) != 0 && json.Unmarshal(data, &failure) == nil {
			results = failure.Results
		}
		if !triageReceiptBindingsValid(proposal, results) {
			return results, inboxcontrol.ErrOutcomeUnknown
		}
		return results, callErr
	}
	if json.Unmarshal(data, &results) != nil || len(results) != len(proposal.Items) || !triageReceiptBindingsValid(proposal, results) {
		return results, inboxcontrol.ErrOutcomeUnknown
	}
	for _, result := range results {
		if result.Receipt == nil || result.Receipt.ID == "" || result.Receipt.Status != inboxcontrol.StatusVerified {
			return results, inboxcontrol.ErrOutcomeUnknown
		}
	}
	return results, nil
}

func triageReceiptBindingsValid(proposal inboxcontrol.TriageProposal, results []inboxcontrol.Result) bool {
	if len(results) != 0 && len(results) != len(proposal.Items) {
		return false
	}
	for i, result := range results {
		receipt := result.Receipt
		if receipt == nil {
			continue
		}
		request := proposal.Items[i].Request
		if receipt.ID == "" || receipt.SourceID != proposal.Source.SourceID || receipt.IdempotencyKey != request.IdempotencyKey || receipt.Intent.Operation != inboxcontrol.OpTags || receipt.Intent.Target == nil || *receipt.Intent.Target != *request.Target {
			return false
		}
	}
	return true
}

// These are structural checks only. The daemon verifies signatures, evidence,
// current authority and state; clients never reinterpret an envelope's intent.
func validateClientTriageProposal(proposal inboxcontrol.TriageProposal) error {
	if proposal.PreviewToken == "" || len(proposal.PreviewToken) > 16384 || proposal.MappingRevision <= 0 || proposal.ArchiveRevision == "" || proposal.IncomingWatermark == "" || proposal.IssuedAt.IsZero() || proposal.ExpiresAt.IsZero() || !proposal.ExpiresAt.After(proposal.IssuedAt) || len(proposal.Items) < 1 || len(proposal.Items) > 100 {
		return inboxcontrol.ErrInvalid
	}
	input := inboxcontrol.TriageInput{Source: proposal.Source}
	for _, item := range proposal.Items {
		r := item.Request
		if !item.RetainInbox || r.Operation != inboxcontrol.OpTags || r.DryRun || r.PreviewToken != "" || r.Target == nil || r.Expected == nil || r.Expected.Target != *r.Target || r.IdempotencyKey == "" || r.Tags == nil || len(r.Tags.Remove) != 0 || item.Projected.Target != *r.Target {
			return inboxcontrol.ErrInvalid
		}
		intent := r
		intent.DryRun = true
		intent.Expected = nil
		intent.IdempotencyKey = ""
		if intent.Validate() != nil {
			return inboxcontrol.ErrInvalid
		}
		input.Items = append(input.Items, inboxcontrol.TriageItemInput{Target: *r.Target, Categories: item.Categories, EvidenceMessageIDs: item.EvidenceMessageIDs, IdempotencyKey: r.IdempotencyKey})
	}
	return input.Validate()
}

func triageRouteAllowed(descriptor *apiprotocol.MCPCapabilities, operation, path string, properties []string) bool {
	if descriptor == nil || descriptor.Version != 1 {
		return false
	}
	for _, route := range descriptor.Routes {
		if route.OperationID != operation || route.Method != http.MethodPost || route.Path != path {
			continue
		}
		for _, field := range properties {
			if !slices.Contains(route.RequestProperties, field) {
				return false
			}
		}
		return true
	}
	return false
}

func (c *Client) triageRequest(ctx context.Context, operation, path string, properties []string, options runtime.RequestOptions, applying bool) ([]byte, error) {
	descriptor, err := c.MCPCapabilities(ctx)
	if err != nil || !triageRouteAllowed(descriptor, operation, path, properties) {
		return nil, inboxcontrol.ErrUnavailable
	}
	cause := inboxcontrol.ErrUnavailable
	if applying {
		cause = inboxcontrol.ErrOutcomeUnknown
	}
	transport := *c.httpClient
	transport.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := c.doGeneratedRequestWithHTTPClient(ctx, http.MethodPost, path, options, &transport)
	if err != nil {
		return nil, cause
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, (8<<20)+1))
	if err != nil || len(data) > 8<<20 {
		return nil, cause
	}
	if response.StatusCode == http.StatusOK {
		return data, nil
	}
	var failure struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(data, &failure) == nil {
		switch failure.Error {
		case "inbox_denied", "unauthorized":
			cause = inboxcontrol.ErrDenied
		case "invalid_inbox_request", "bad_request", "request_too_large", "unsupported_media_type":
			cause = inboxcontrol.ErrInvalid
		case "inbox_conflict":
			cause = inboxcontrol.ErrConflict
		case "inbox_unavailable", "operation_in_progress":
			cause = inboxcontrol.ErrUnavailable
		case "inbox_internal":
			cause = inboxcontrol.ErrInternal
		case "inbox_outcome_unknown":
			cause = inboxcontrol.ErrOutcomeUnknown
		case "inbox_reconcile_only":
			cause = inboxcontrol.ErrReconcileOnly
		case "inbox_provider_rejected":
			cause = inboxcontrol.ErrNoWrite
		}
	}
	return data, fmt.Errorf("%w (HTTP %d)", cause, response.StatusCode)
}
