package beeper

import (
	"context"
	"time"

	"go.kenn.io/msgvault/internal/inboxcontrol"
)

// This names the reviewed operation/response vocabulary, not a claim that a
// matching server version attests to it. Source and fixture digests are retained
// in the native checkpoint receipt. Unknown later versions are not recognized.
// Primary SDK revision: 9d2d14d0eef05ef0c2cf6368f58d84489c56487a.
const inboxProfileName = "beeper-desktop-4.2.808/v1-observation-v1"

var (
	_ inboxcontrol.Provider           = (*InboxProvider)(nil)
	_ inboxcontrol.CapabilityProvider = (*InboxProvider)(nil)
)

func inboxProfileShape(info inboxObject) bool {
	app, err := decodeInboxObject(info["app"])
	if err != nil {
		return false
	}
	version, ok := inboxString(app["version"])
	if !ok || version != "4.2.808" {
		return false
	}
	for _, key := range []string{"name", "bundle_id"} {
		v, valid := inboxString(app[key])
		if !valid || v == "" {
			return false
		}
	}
	endpoints, err := decodeInboxObject(info["endpoints"])
	if err != nil {
		return false
	}
	spec, ok := inboxString(endpoints["spec"])
	return ok && spec != ""
}

// Capabilities is source-only discovery. The chat capability flags and current
// incoming window are not known here, so no chat mutation can be advertised.
func (p *InboxProvider) Capabilities(ctx context.Context, request inboxcontrol.Request) (*inboxcontrol.Capabilities, error) {
	if !p.validSource() || request.Source == nil || *request.Source != p.source || request.Target != nil {
		return nil, inboxcontrol.ErrDenied
	}
	ctx, cancel := context.WithTimeout(ctx, inboxObservationTimeout)
	defer cancel()
	info, err := p.getObject(ctx, "/v1/info")
	if err != nil {
		return nil, err
	}
	reason := "Unrecognized operation/response profile; chat evidence required"
	if inboxProfileShape(info) {
		reason = "Chat capabilities and primary-backed current incoming window required"
	}
	result := &inboxcontrol.Capabilities{Source: p.source, LocationModel: "chat-archive-flag", ObservedAt: time.Now().UTC()}
	for _, op := range []inboxcontrol.Operation{inboxcontrol.OpGetState, inboxcontrol.OpArchive, inboxcontrol.OpUnarchive, inboxcontrol.OpSetRead, inboxcontrol.OpSetUnread, inboxcontrol.OpTags, inboxcontrol.OpMove, inboxcontrol.OpListFolders, inboxcontrol.OpCreateFolder} {
		capability := inboxcontrol.Capability{Operation: op, Status: inboxcontrol.CapabilityUnavailable, Reason: reason}
		if op == inboxcontrol.OpGetState {
			capability.Status = inboxcontrol.CapabilitySupported
			capability.Reason = "Exact nonmerged chat observation"
		}
		if op == inboxcontrol.OpTags || op == inboxcontrol.OpMove || op == inboxcontrol.OpListFolders || op == inboxcontrol.OpCreateFolder {
			capability.Status = inboxcontrol.CapabilityUnsupported
			capability.Reason = "Beeper Inbox controls use chat archive and read markers"
		}
		result.Operations = append(result.Operations, capability)
	}
	return result, nil
}

// Preview and Dispatch deliberately have no transport path. Even an apparent
// already-target state lacks the complete fresh evidence required for a no-op.
func (*InboxProvider) Preview(context.Context, inboxcontrol.Request, inboxcontrol.State) (inboxcontrol.State, error) {
	return inboxcontrol.State{}, inboxcontrol.ErrUnavailable
}

func (*InboxProvider) Dispatch(context.Context, inboxcontrol.Request, inboxcontrol.State) (inboxcontrol.DispatchResult, error) {
	return inboxcontrol.DispatchResult{}, inboxcontrol.ErrUnavailable
}

func (*InboxProvider) Verify(inboxcontrol.Request, inboxcontrol.State, inboxcontrol.State, inboxcontrol.State) error {
	return inboxcontrol.ErrOutcomeUnknown
}

func (*InboxProvider) Folders(context.Context, inboxcontrol.SourceIdentity) ([]inboxcontrol.Folder, error) {
	return nil, inboxcontrol.ErrUnavailable
}
