package daemonclient

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// MessageTags never retries a provider mutation after a transport error.
func (c *Client) MessageTags(ctx context.Context, id int64, change *emailtags.Change, mailbox string) (*emailtags.Result, error) {
	if change != nil && !change.DryRun {
		return c.controlMessageTags(ctx, id, *change)
	}
	result, _, err := c.readMessageTags(ctx, id, change, mailbox)
	return result, err
}

func (c *Client) readMessageTags(ctx context.Context, id int64, change *emailtags.Change, mailbox string) (*emailtags.Result, *inboxcontrol.Target, error) {
	method := http.MethodGet
	path := fmt.Sprintf("/api/v1/messages/%d/tags", id)
	var options runtime.RequestOptions = &generated.GetMessageTagsRequestOptions{Query: &generated.GetMessageTagsQuery{Mailbox: optionalString(mailbox)}}
	if change != nil {
		method = http.MethodPost
		options = &generated.UpdateMessageTagsRequestOptions{Body: &generated.UpdateMessageTagsBody{Add: change.Add, Remove: change.Remove, Mailbox: optionalString(change.Mailbox), DryRun: &change.DryRun}}
	}
	transport := *c.httpClient
	transport.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.doGeneratedRequestWithHTTPClient(ctx, method, path, options, &transport)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, (8<<20)+1))
	if err != nil || len(body) > 8<<20 {
		return nil, nil, errors.New("cannot read bounded message tag response")
	}
	if resp.StatusCode != http.StatusOK {
		var failure emailtags.Error
		if err := json.Unmarshal(body, &failure); err != nil || failure.Code == "" {
			return nil, nil, fmt.Errorf("message tags request failed (HTTP %d)", resp.StatusCode)
		}
		return failure.Result, nil, &failure
	}
	var result struct {
		emailtags.Result

		Target *inboxcontrol.Target `json:"target,omitempty"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, nil, errors.New("cannot decode message tags")
	}
	return &result.Result, result.Target, nil
}

func (c *Client) controlMessageTags(ctx context.Context, id int64, change emailtags.Change) (*emailtags.Result, error) {
	if id <= 0 {
		return nil, emailtags.Failure("invalid_request", "message ID must be positive", nil, nil)
	}
	if _, err := emailtags.Normalize(change, false); err != nil {
		return nil, err
	}
	result, target, err := c.readMessageTags(ctx, id, nil, change.Mailbox)
	if result != nil {
		result.Verified = false
	}
	if err != nil {
		return result, err
	}
	if target == nil || target.Validate() != nil || target.ItemID != id || target.SourceID != result.SourceID || target.SourceType != result.Provider || (change.Mailbox != "" && change.Mailbox != target.Mailbox) {
		return result, emailtags.Failure("provider_unavailable", "daemon did not resolve an exact signed tag target", result, nil)
	}
	change.Mailbox, change.DryRun = "", false
	request := inboxcontrol.Request{Operation: inboxcontrol.OpTags, Target: target, Tags: &change, DryRun: true}
	preview, err := c.ControlInbox(ctx, request)
	if err != nil {
		return result, tagControlFailure(err, result)
	}
	if preview == nil || preview.Before == nil || preview.Projected == nil || preview.Before.Target != *target || preview.Projected.Target != *target || preview.PreviewToken == "" {
		return result, emailtags.Failure("provider_unavailable", "daemon did not return a complete signed tag preview", result, nil)
	}
	key := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		return result, emailtags.Failure("local_failed", "cannot prepare tag operation key", result, nil)
	}
	request.DryRun, request.Expected, request.PreviewToken, request.IdempotencyKey = false, preview.Before, preview.PreviewToken, "tags-"+hex.EncodeToString(key)
	result.Verified, result.DryRun = false, false
	result.Before = preview.Before.Tags
	result.Tags, result.Flags = preview.Before.Tags, preview.Before.Flags
	result.IdempotencyKey = request.IdempotencyKey
	controlled, err := c.ControlInbox(ctx, request)
	if controlled != nil {
		if controlled.After != nil {
			result.Tags, result.Flags = controlled.After.Tags, controlled.After.Flags
		}
		if controlled.Receipt != nil {
			result.ReceiptID, result.ReceiptStatus = controlled.Receipt.ID, string(controlled.Receipt.Status)
			result.Verified = controlled.Receipt.Status == inboxcontrol.StatusVerified
		}
	}
	if err != nil {
		return result, tagControlFailure(err, result)
	}
	if !result.Verified {
		return result, emailtags.Failure("remote_unknown", "tag outcome is not verified; inspect the receipt before retrying", result, nil)
	}
	return result, nil
}

func tagControlFailure(cause error, result *emailtags.Result) error {
	code, message := "provider_unavailable", "signed tag control is unavailable"
	switch {
	case errors.Is(cause, inboxcontrol.ErrOutcomeUnknown):
		code, message = "remote_unknown", "tag outcome is unknown; inspect the receipt before retrying"
	case errors.Is(cause, inboxcontrol.ErrInternal):
		code, message = "local_failed", "tag control failed; inspect the returned receipt or operation key"
	case errors.Is(cause, inboxcontrol.ErrReconcileOnly):
		code, message = "remote_accepted_local_failed", "provider tags verified; reconcile the receipt before retrying"
	case errors.Is(cause, inboxcontrol.ErrDenied):
		code, message = "insufficient_scope", "tag operation is not authorized"
	case errors.Is(cause, inboxcontrol.ErrInvalid):
		code, message = "invalid_request", "invalid signed tag intent"
	case errors.Is(cause, inboxcontrol.ErrConflict), errors.Is(cause, inboxcontrol.ErrPlanChanged):
		code, message = "stale_identity", "tag preview changed; preview again before writing"
	case errors.Is(cause, inboxcontrol.ErrNoWrite):
		code, message = "provider_rejected", "provider rejected tags without a write"
	}
	return emailtags.Failure(code, message, result, nil)
}
