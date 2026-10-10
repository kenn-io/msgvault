package beeper

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"net/http"

	"go.kenn.io/msgvault/internal/inboxcontrol"
)

// Send a native control POST once. A transport failure or an unverified response
// is unknown: the shared receipt service may read back, but must never retry it.
func (c *Client) inboxPost(ctx context.Context, path string, body []byte, target inboxcontrol.Target, archive bool) error {
	if err := c.limiter.Wait(ctx); err != nil {
		return inboxcontrol.ErrNoWrite
	}
	token, err := c.token(ctx)
	if err != nil {
		return inboxcontrol.ErrNoWrite
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return inboxcontrol.ErrNoWrite
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	transport := *c.http
	transport.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := transport.Do(request)
	if err != nil {
		return inboxcontrol.ErrOutcomeUnknown
	}
	defer func() { _ = response.Body.Close() }()
	switch response.StatusCode {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusUnprocessableEntity:
		return inboxcontrol.ErrNoWrite
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return inboxcontrol.ErrOutcomeUnknown
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxInboxResponseBytes+1))
	if err != nil || len(data) > maxInboxResponseBytes {
		return inboxcontrol.ErrOutcomeUnknown
	}
	// Archive is documented to return no chat body. Only independent GET can
	// verify its effect; read/unread responses also require exact native identity.
	if archive {
		return nil
	}
	var chat Chat
	if json.Unmarshal(data, &chat) != nil || chat.ID != target.ProviderID || chat.AccountID != target.AccountID || chat.Merge != nil || chat.MergedIntoChatID != "" {
		return inboxcontrol.ErrOutcomeUnknown
	}
	return nil
}
