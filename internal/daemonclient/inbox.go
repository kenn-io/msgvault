package daemonclient

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"slices"

	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/pkg/client/generated"
)

const InboxMinAPISchemaVersion = "3.3.0"

// ControlInbox sends one exact request to the daemon. It preserves uncertain
// receipts and never retries a control POST after a response or transport error.
func (c *Client) ControlInbox(ctx context.Context, request inboxcontrol.Request) (*inboxcontrol.Result, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	descriptor, err := c.MCPCapabilities(ctx)
	if err != nil || !descriptor.HasInboxContract() {
		return nil, fmt.Errorf("%w: daemon must advertise the current signed inbox control contract", inboxcontrol.ErrUnavailable)
	}
	if !slices.Contains(descriptor.InboxOperations, string(request.Operation)) {
		return nil, inboxcontrol.ErrDenied
	}
	data, err := json.Marshal(request)
	if err != nil {
		return nil, inboxcontrol.ErrInvalid
	}
	var body generated.ControlInboxBody
	if json.Unmarshal(data, &body) != nil {
		return nil, inboxcontrol.ErrInvalid
	}
	transport := *c.httpClient
	transport.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := c.doGeneratedRequestWithHTTPClient(ctx, http.MethodPost, "/api/v1/inbox/control", &generated.ControlInboxRequestOptions{Body: &body}, &transport)
	if err != nil {
		return nil, inboxResponseFailure(request)
	}
	defer func() { _ = response.Body.Close() }()
	data, err = io.ReadAll(io.LimitReader(response.Body, (8<<20)+1))
	if err != nil || len(data) > 8<<20 {
		return nil, inboxResponseFailure(request)
	}
	if response.StatusCode != http.StatusOK {
		var failure struct {
			Error  string               `json:"error"`
			Result *inboxcontrol.Result `json:"result"`
		}
		if json.Unmarshal(data, &failure) != nil {
			return nil, inboxResponseFailure(request)
		}
		cause := inboxResponseFailure(request)
		switch failure.Error {
		case "inbox_outcome_unknown":
			cause = inboxcontrol.ErrOutcomeUnknown
		case "inbox_denied", "unauthorized":
			cause = inboxcontrol.ErrDenied
		case "invalid_inbox_request", "bad_request", "request_too_large", "unsupported_media_type":
			cause = inboxcontrol.ErrInvalid
		case "inbox_conflict":
			cause = inboxcontrol.ErrConflict
		case "inbox_unavailable":
			cause = inboxcontrol.ErrUnavailable
		case "inbox_internal":
			cause = inboxcontrol.ErrInternal
		case "inbox_reconcile_only":
			cause = inboxcontrol.ErrReconcileOnly
		case "inbox_provider_rejected":
			cause = inboxcontrol.ErrNoWrite
		}
		// Provider/daemon response text and URLs never enter the error surface.
		return failure.Result, fmt.Errorf("%w (HTTP %d)", cause, response.StatusCode)
	}
	var result inboxcontrol.Result
	if json.Unmarshal(data, &result) != nil {
		return nil, inboxResponseFailure(request)
	}
	if request.Operation.IsMutation() {
		if request.DryRun {
			if result.Before == nil || result.Projected == nil || result.PreviewToken == "" {
				return nil, inboxcontrol.ErrUnavailable
			}
		} else if result.Receipt == nil || result.Receipt.ID == "" || result.Receipt.Status == "" {
			return nil, inboxcontrol.ErrOutcomeUnknown
		}
	}
	return &result, nil
}

func inboxResponseFailure(request inboxcontrol.Request) error {
	if request.Operation.IsMutation() && !request.DryRun {
		return inboxcontrol.ErrOutcomeUnknown
	}
	return inboxcontrol.ErrUnavailable
}
