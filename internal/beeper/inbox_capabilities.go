package beeper

import (
	"context"
	"encoding/json/v2"
	"time"

	"go.kenn.io/msgvault/internal/inboxcontrol"
)

var _ inboxcontrol.CapabilityProvider = (*InboxProvider)(nil)

func (p *InboxProvider) Capabilities(ctx context.Context, request inboxcontrol.Request) (*inboxcontrol.Capabilities, error) {
	if p.source.Validate() != nil || p.source.SourceType != sourceTypeBeeper || p.source.SourceIdentifier != p.source.AccountID || request.Source == nil || *request.Source != p.source || request.Target != nil {
		return nil, inboxcontrol.ErrDenied
	}
	if p.client == nil {
		return nil, inboxcontrol.ErrUnavailable
	}
	body, err := p.client.inboxBytes(ctx, "/v1/accounts")
	if err != nil {
		return nil, err
	}
	var accounts []struct {
		AccountID string `json:"accountID"`
	}
	if json.Unmarshal(body, &accounts) != nil || len(accounts) == 0 || len(accounts) > 1000 {
		return nil, inboxcontrol.ErrUnavailable
	}
	seen := map[string]bool{}
	for _, account := range accounts {
		binding := p.source
		binding.AccountID, binding.SourceIdentifier = account.AccountID, account.AccountID
		if binding.Validate() != nil || seen[account.AccountID] {
			return nil, inboxcontrol.ErrUnavailable
		}
		seen[account.AccountID] = true
	}
	if !seen[p.source.AccountID] {
		return nil, inboxcontrol.ErrUnavailable
	}
	result := &inboxcontrol.Capabilities{Source: p.source, LocationModel: "chat-archive", ObservedAt: time.Now().UTC()}
	for _, op := range []inboxcontrol.Operation{inboxcontrol.OpGetCapabilities, inboxcontrol.OpGetState, inboxcontrol.OpArchive, inboxcontrol.OpUnarchive, inboxcontrol.OpSetRead, inboxcontrol.OpSetUnread, inboxcontrol.OpTags, inboxcontrol.OpMove, inboxcontrol.OpListFolders, inboxcontrol.OpCreateFolder} {
		status, reason := inboxcontrol.CapabilityUnsupported, "Beeper chats have no native folder or tag operation"
		switch op {
		case inboxcontrol.OpGetCapabilities, inboxcontrol.OpGetState:
			status, reason = inboxcontrol.CapabilitySupported, ""
		case inboxcontrol.OpArchive, inboxcontrol.OpUnarchive, inboxcontrol.OpSetRead, inboxcontrol.OpSetUnread:
			// Native support and marker evidence vary by chat. Source discovery
			// must not infer them by scanning unrelated chats or choosing one.
			status, reason = inboxcontrol.CapabilityUnavailable, "Requires current native capability and marker evidence for an exact chat"
		default:
			// Retain unsupported status for operations without a native lane.
		}
		result.Operations = append(result.Operations, inboxcontrol.Capability{Operation: op, Status: status, Reason: reason})
	}
	return result, nil
}
