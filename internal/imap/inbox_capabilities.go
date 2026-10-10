package imap

import (
	"context"
	"slices"
	"strings"
	"time"

	imapapi "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

var _ inboxcontrol.CapabilityProvider = (*InboxProvider)(nil)

// Capabilities advertises native operations, not authority on every mailbox.
// Each mutation still preflights its exact mailbox, epoch, UID and permissions.
func (p *InboxProvider) Capabilities(ctx context.Context, request inboxcontrol.Request) (*inboxcontrol.Capabilities, error) {
	if request.Source == nil || request.Target != nil || *request.Source != p.source {
		return nil, inboxcontrol.ErrDenied
	}
	folders, err := p.Folders(ctx, *request.Source)
	if err != nil {
		return nil, err
	}
	move, seen, keywords := false, false, false
	err = p.client.withDraftConn(ctx, func(conn *imapclient.Client) error {
		move = conn.Caps().Has(imapapi.CapMove) && conn.Caps().Has(imapapi.CapUIDPlus)
		for _, folder := range folders {
			selected, err := conn.Select(folder.ID, &imapapi.SelectOptions{ReadOnly: true}).Wait()
			if err != nil || selected.UIDValidity != folder.UIDValidity {
				return inboxcontrol.ErrUnavailable
			}
			if selected.PermanentFlags == nil || slices.Contains(selected.PermanentFlags, imapapi.FlagSeen) {
				seen = true
			}
			for _, flag := range selected.PermanentFlags {
				if !strings.HasPrefix(string(flag), "\\") {
					keywords = true
				}
			}
			if selected.PermanentFlags == nil || slices.Contains(selected.PermanentFlags, imapapi.FlagWildcard) {
				for _, flag := range selected.Flags {
					if !strings.HasPrefix(string(flag), "\\") {
						keywords = true
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	archive := false
	if move {
		_, err := p.archiveFolder(ctx)
		archive = err == nil
	}
	result := &inboxcontrol.Capabilities{Source: p.source, LocationModel: "mailboxes", ObservedAt: time.Now().UTC()}
	for _, op := range []inboxcontrol.Operation{inboxcontrol.OpGetCapabilities, inboxcontrol.OpGetState, inboxcontrol.OpListFolders, inboxcontrol.OpTags, inboxcontrol.OpArchive, inboxcontrol.OpUnarchive, inboxcontrol.OpSetRead, inboxcontrol.OpSetUnread, inboxcontrol.OpMove, inboxcontrol.OpCreateFolder} {
		status, reason := inboxcontrol.CapabilitySupported, ""
		if op.IsMutation() {
			reason = "Exact mailbox preflight and current action permission required"
		}
		switch {
		case (op == inboxcontrol.OpMove || op == inboxcontrol.OpUnarchive || op == inboxcontrol.OpArchive) && !move:
			status, reason = inboxcontrol.CapabilityUnsupported, "Native MOVE and UIDPLUS required; no copy/delete fallback"
		case op == inboxcontrol.OpArchive && !archive:
			status, reason = inboxcontrol.CapabilityUnavailable, "One configured or SPECIAL-USE Archive mailbox required"
		case (op == inboxcontrol.OpSetRead || op == inboxcontrol.OpSetUnread) && !seen:
			status, reason = inboxcontrol.CapabilityUnsupported, "No persistent Seen support observed"
		case op == inboxcontrol.OpTags && !keywords:
			status, reason = inboxcontrol.CapabilityUnsupported, "No existing persistent keywords observed"
		}
		result.Operations = append(result.Operations, inboxcontrol.Capability{Operation: op, Status: status, Reason: reason})
	}
	return result, nil
}
