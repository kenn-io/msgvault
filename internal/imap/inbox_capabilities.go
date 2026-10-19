package imap

import (
	"context"
	"strings"
	"time"

	imapapi "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

var _ inboxcontrol.CapabilityProvider = (*InboxProvider)(nil)

// Capabilities reports native support, never mailbox write authorization.
// Every mutation still requires exact-target preflight and a writable SELECT.
func (p *InboxProvider) Capabilities(ctx context.Context, request inboxcontrol.Request) (*inboxcontrol.Capabilities, error) {
	if request.Source == nil || request.Target != nil || !p.validSource(*request.Source) {
		return nil, inboxcontrol.ErrDenied
	}
	folders, err := p.Folders(ctx, p.source)
	if err != nil {
		return nil, err
	}
	var move, keywords, seen, conditional, inbox bool
	err = p.client.withDraftConn(ctx, func(conn *imapclient.Client) error {
		move = conn.Caps().Has(imapapi.CapMove) && conn.Caps().Has(imapapi.CapUIDPlus)
		for _, folder := range folders {
			target := inboxcontrol.Target{Mailbox: folder.ID, UIDValidity: folder.UIDValidity}
			selected, err := p.selectTarget(conn, target, true)
			if err != nil {
				return err
			}
			inbox = inbox || strings.EqualFold(folder.ID, "INBOX")
			conditional = conditional || (conn.Caps().Has(imapapi.CapCondStore) && selected.HighestModSeq > 0)
			// EXAMINE PERMANENTFLAGS may name existing keywords, but an empty
			// set does not veto the catalog or prove writable restrictions.
			for _, flags := range [][]imapapi.Flag{selected.Flags, selected.PermanentFlags} {
				for _, flag := range flags {
					value := string(flag)
					seen = seen || strings.EqualFold(value, string(imapapi.FlagSeen))
					keywords = keywords || (value != "" && !strings.HasPrefix(value, `\`))
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	result := &inboxcontrol.Capabilities{Source: p.source, LocationModel: "mailboxes", ConditionalWrite: conditional, ObservedAt: time.Now().UTC()}
	add := func(operation inboxcontrol.Operation, status inboxcontrol.CapabilityStatus, reason string) {
		result.Operations = append(result.Operations, inboxcontrol.Capability{Operation: operation, Status: status, Reason: reason})
	}
	for _, operation := range []inboxcontrol.Operation{inboxcontrol.OpGetState, inboxcontrol.OpListFolders, inboxcontrol.OpCreateFolder} {
		add(operation, inboxcontrol.CapabilitySupported, "Native protocol; exact live evidence and current grants required")
	}
	flagCapability := func(operation inboxcontrol.Operation, evidence bool) {
		if evidence {
			add(operation, inboxcontrol.CapabilitySupported, "Exact preflight and writable SELECT required")
		} else {
			add(operation, inboxcontrol.CapabilityUnavailable, "Mailbox flag catalog evidence missing")
		}
	}
	flagCapability(inboxcontrol.OpSetRead, seen)
	flagCapability(inboxcontrol.OpSetUnread, seen)
	flagCapability(inboxcontrol.OpTags, keywords)
	if !move {
		for _, operation := range []inboxcontrol.Operation{inboxcontrol.OpMove, inboxcontrol.OpArchive, inboxcontrol.OpUnarchive} {
			add(operation, inboxcontrol.CapabilityUnsupported, "Native MOVE and UIDPLUS required")
		}
		return result, nil
	}
	add(inboxcontrol.OpMove, inboxcontrol.CapabilitySupported, "Exact destination epoch, COPYUID mapping and source absence required")
	if inbox {
		add(inboxcontrol.OpUnarchive, inboxcontrol.CapabilitySupported, "Exact INBOX epoch and native MOVE proof required")
	} else {
		add(inboxcontrol.OpUnarchive, inboxcontrol.CapabilityUnavailable, "Exact selectable INBOX evidence missing")
	}
	if _, err := p.archiveFolder(ctx); err == nil {
		add(inboxcontrol.OpArchive, inboxcontrol.CapabilitySupported, "Configured or unique SPECIAL-USE archive; exact native MOVE proof required")
	} else {
		add(inboxcontrol.OpArchive, inboxcontrol.CapabilityUnavailable, "Configured or unique SPECIAL-USE archive evidence missing")
	}
	return result, nil
}
