package daemonclient

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"

	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// InboxCandidates reads bounded committed metadata only after current caller
// discovery admits the exact route. It never follows a redirect or retries.
func (c *Client) InboxCandidates(ctx context.Context, source inboxcontrol.SourceIdentity, scope inboxcontrol.Scope, limit int, cursor string) (*inboxcontrol.CandidatePage, error) {
	if source.Validate() != nil || limit < 1 || limit > 100 || len(cursor) > 16384 || (source.SourceType == "beeper" && scope != inboxcontrol.ScopeChat) || (source.SourceType != "beeper" && scope != inboxcontrol.ScopeMessage) {
		return nil, inboxcontrol.ErrInvalid
	}
	descriptor, err := c.MCPCapabilities(ctx)
	if err != nil || !descriptor.HasInboxCandidatesContract() {
		return nil, inboxcontrol.ErrUnavailable
	}
	maxItems := int64(limit)
	query := generated.ListInboxCandidatesQuery{SourceID: source.SourceID, SourceType: source.SourceType, SourceIdentifier: source.SourceIdentifier, AccountID: source.AccountID, Scope: generated.ListInboxCandidatesQueryScope(scope), Limit: &maxItems, Cursor: &cursor}
	transport := *c.httpClient
	transport.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := c.doGeneratedRequestWithHTTPClient(ctx, http.MethodGet, "/api/v1/inbox/candidates", &generated.ListInboxCandidatesRequestOptions{Query: &query}, &transport)
	if err != nil {
		return nil, inboxcontrol.ErrUnavailable
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, (8<<20)+1))
	if err != nil || len(data) > 8<<20 {
		return nil, inboxcontrol.ErrUnavailable
	}
	if response.StatusCode != http.StatusOK {
		cause := inboxcontrol.ErrUnavailable
		switch response.StatusCode {
		case http.StatusBadRequest:
			cause = inboxcontrol.ErrInvalid
		case http.StatusUnauthorized, http.StatusForbidden:
			cause = inboxcontrol.ErrDenied
		case http.StatusConflict:
			cause = inboxcontrol.ErrConflict
		}
		return nil, fmt.Errorf("%w (HTTP %d)", cause, response.StatusCode)
	}
	var page inboxcontrol.CandidatePage
	if json.Unmarshal(data, &page) != nil || page.Source != source || page.Scope != scope || page.ArchiveRevision == "" || page.Candidates == nil || len(page.Candidates) > limit || len(page.NextCursor) > 16384 {
		return nil, inboxcontrol.ErrUnavailable
	}
	for _, candidate := range page.Candidates {
		target := candidate.State.Target
		if target.Validate() != nil || target.SourceID != source.SourceID || target.SourceType != source.SourceType || target.SourceIdentifier != source.SourceIdentifier || target.AccountID != source.AccountID || target.Scope != scope || candidate.State.Inbox == nil || !*candidate.State.Inbox {
			return nil, inboxcontrol.ErrUnavailable
		}
	}
	return &page, nil
}
