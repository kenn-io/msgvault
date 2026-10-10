package gmail

import (
	"context"
	"time"

	"go.kenn.io/msgvault/internal/inboxcontrol"
)

// WithWriteCapability supplies the daemon's current credential-scope evidence.
// Current request authorization and provider enforcement remain independent.
func (p *InboxProvider) WithWriteCapability(status inboxcontrol.CapabilityStatus) *InboxProvider {
	p.writeCapability = status
	return p
}

func (p *InboxProvider) Capabilities(ctx context.Context, request inboxcontrol.Request) (*inboxcontrol.Capabilities, error) {
	if request.Source == nil || *request.Source != p.source || request.Target != nil {
		return nil, inboxcontrol.ErrDenied
	}
	if err := p.account(ctx); err != nil {
		return nil, err
	}
	_, catalogErr := p.folderCatalog(ctx)
	result := &inboxcontrol.Capabilities{Source: p.source, LocationModel: "labels", ObservedAt: time.Now().UTC(), Operations: []inboxcontrol.Capability{}}
	for _, op := range []inboxcontrol.Operation{inboxcontrol.OpGetCapabilities, inboxcontrol.OpGetState, inboxcontrol.OpListFolders, inboxcontrol.OpTags, inboxcontrol.OpArchive, inboxcontrol.OpUnarchive, inboxcontrol.OpSetRead, inboxcontrol.OpSetUnread, inboxcontrol.OpMove, inboxcontrol.OpCreateFolder} {
		status, reason := inboxcontrol.CapabilitySupported, ""
		if op != inboxcontrol.OpGetCapabilities && op != inboxcontrol.OpGetState && op != inboxcontrol.OpListFolders {
			status = p.writeCapability
			switch status {
			case inboxcontrol.CapabilitySupported:
			case inboxcontrol.CapabilityPermissionRequired:
				reason = "Gmail modification scope required"
			default:
				status, reason = inboxcontrol.CapabilityUnavailable, "Gmail modification scope could not be verified"
			}
		}
		if catalogErr != nil && (op == inboxcontrol.OpListFolders || op == inboxcontrol.OpTags || op == inboxcontrol.OpMove || op == inboxcontrol.OpCreateFolder) {
			status, reason = inboxcontrol.CapabilityUnavailable, "Live user-label catalog unavailable"
		}
		result.Operations = append(result.Operations, inboxcontrol.Capability{Operation: op, Status: status, Reason: reason})
	}
	return result, nil
}
