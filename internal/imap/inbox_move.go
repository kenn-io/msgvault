package imap

import (
	"context"
	"slices"
	"strings"

	imapapi "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

var _ inboxcontrol.MoveSourceVerifier = (*InboxProvider)(nil)

func isIMAPMove(operation inboxcontrol.Operation) bool {
	return operation == inboxcontrol.OpMove || operation == inboxcontrol.OpArchive || operation == inboxcontrol.OpUnarchive
}

func (p *InboxProvider) previewMove(ctx context.Context, request inboxcontrol.Request, before inboxcontrol.State) (inboxcontrol.State, error) {
	destination := request.Destination
	if request.ResolvedFolder != nil {
		destination = request.ResolvedFolder
	}
	if request.Operation == inboxcontrol.OpArchive {
		folder, err := p.archiveFolder(ctx)
		if err != nil {
			return inboxcontrol.State{}, err
		}
		if destination != nil && *destination != folder {
			return inboxcontrol.State{}, inboxcontrol.ErrPlanChanged
		}
		destination = &folder
	}
	if destination == nil || destination.ID == "" || destination.UIDValidity == 0 || destination.ParentID != "" || destination.ID == before.Target.Mailbox {
		return inboxcontrol.State{}, inboxcontrol.ErrUnavailable
	}
	if request.Operation == inboxcontrol.OpUnarchive && (!strings.EqualFold(destination.ID, "INBOX") || strings.EqualFold(before.Target.Mailbox, "INBOX")) {
		return inboxcontrol.State{}, inboxcontrol.ErrUnavailable
	}
	err := p.client.withDraftConn(ctx, func(conn *imapclient.Client) error {
		// The native client otherwise falls back to COPY/Deleted/EXPUNGE.
		if !conn.Caps().Has(imapapi.CapMove) || !conn.Caps().Has(imapapi.CapUIDPlus) {
			return inboxcontrol.ErrUnavailable
		}
		target := before.Target
		target.Mailbox, target.UIDValidity = destination.ID, destination.UIDValidity
		_, err := p.selectTarget(conn, target, true)
		return err
	})
	if err != nil {
		return inboxcontrol.State{}, err
	}
	projected := before
	projected.Location = destination.ID
	projected.Folders = []inboxcontrol.Folder{*destination}
	inbox := strings.EqualFold(destination.ID, "INBOX")
	projected.Inbox = &inbox
	return projected, nil
}

func singletonIMAPUID(set imapapi.NumSet) (imapapi.UID, bool) {
	uids, ok := set.(imapapi.UIDSet)
	if !ok || len(uids) != 1 || uids[0].Start == 0 || uids[0].Start != uids[0].Stop {
		return 0, false
	}
	return uids[0].Start, true
}

func (p *InboxProvider) dispatchMove(ctx context.Context, request inboxcontrol.Request, before, projected inboxcontrol.State) (inboxcontrol.DispatchResult, error) {
	if len(projected.Folders) != 1 {
		return inboxcontrol.DispatchResult{}, inboxcontrol.ErrNoWrite
	}
	destination := projected.Folders[0]
	dispatched := false
	result := inboxcontrol.DispatchResult{}
	err := p.client.withDraftConn(ctx, func(conn *imapclient.Client) error {
		if !conn.Caps().Has(imapapi.CapMove) || !conn.Caps().Has(imapapi.CapUIDPlus) {
			return inboxcontrol.ErrUnavailable
		}
		if _, err := p.selectTarget(conn, before.Target, false); err != nil {
			return err
		}
		flags, _, err := fetchKeywordObservation(conn, before.Target.UID, false)
		if err != nil || !sameIMAPFlags(flags, before.Flags) {
			return inboxcontrol.ErrPlanChanged
		}
		status, err := conn.Status(destination.ID, &imapapi.StatusOptions{UIDValidity: true}).Wait()
		if err != nil || status == nil || status.Mailbox != destination.ID || status.UIDValidity != destination.UIDValidity {
			return inboxcontrol.ErrPlanChanged
		}
		dispatched = true
		data, err := conn.Move(imapapi.UIDSetNum(imapapi.UID(before.Target.UID)), destination.ID).Wait()
		if err != nil || data == nil {
			return inboxcontrol.ErrOutcomeUnknown
		}
		source, sourceOK := singletonIMAPUID(data.SourceUIDs)
		dest, destOK := singletonIMAPUID(data.DestUIDs)
		if !sourceOK || !destOK || source != imapapi.UID(before.Target.UID) || data.UIDValidity != destination.UIDValidity {
			return inboxcontrol.ErrOutcomeUnknown
		}
		target := before.Target
		target.Mailbox, target.UIDValidity, target.UID = destination.ID, data.UIDValidity, uint32(dest)
		result.Target = &target
		return nil
	})
	if err != nil {
		if dispatched {
			return result, inboxcontrol.ErrOutcomeUnknown
		}
		return result, inboxcontrol.ErrNoWrite
	}
	return result, nil
}

// VerifyMoveSource independently proves absence in the original mailbox epoch.
// A changed epoch, failed FETCH or any returned item leaves the outcome unknown.
func (p *InboxProvider) VerifyMoveSource(ctx context.Context, origin inboxcontrol.Target) error {
	if _, err := p.target(inboxcontrol.Request{Target: &origin}); err != nil {
		return inboxcontrol.ErrOutcomeUnknown
	}
	err := p.client.withDraftConn(ctx, func(conn *imapclient.Client) error {
		if _, err := p.selectTarget(conn, origin, true); err != nil {
			return err
		}
		messages, err := conn.Fetch(imapapi.UIDSetNum(imapapi.UID(origin.UID)), &imapapi.FetchOptions{UID: true}).Collect()
		if err != nil || len(messages) != 0 {
			return inboxcontrol.ErrOutcomeUnknown
		}
		return nil
	})
	if err != nil {
		return inboxcontrol.ErrOutcomeUnknown
	}
	return nil
}

// Configuration overrides SPECIAL-USE. A mailbox name alone is not evidence
// that it is the account's archive. Sign the exact live destination epoch.
func (p *InboxProvider) archiveFolder(ctx context.Context) (inboxcontrol.Folder, error) {
	name := p.client.config.ArchiveMailbox
	var folder inboxcontrol.Folder
	err := p.client.withDraftConn(ctx, func(conn *imapclient.Client) error {
		if name == "" {
			if !conn.Caps().Has(imapapi.CapSpecialUse) {
				return inboxcontrol.ErrUnavailable
			}
			mailboxes, err := conn.List("", "*", &imapapi.ListOptions{SelectSpecialUse: true, ReturnSpecialUse: true}).Collect()
			if err != nil {
				return inboxcontrol.ErrUnavailable
			}
			for _, mailbox := range mailboxes {
				if !slices.Contains(mailbox.Attrs, imapapi.MailboxAttrArchive) || slices.Contains(mailbox.Attrs, imapapi.MailboxAttrNoSelect) {
					continue
				}
				if name != "" {
					return inboxcontrol.ErrUnavailable
				}
				name = mailbox.Mailbox
			}
		}
		if name == "" || strings.EqualFold(name, "INBOX") {
			return inboxcontrol.ErrUnavailable
		}
		status, err := conn.Status(name, &imapapi.StatusOptions{UIDValidity: true}).Wait()
		if err != nil || status == nil || status.Mailbox != name || status.UIDValidity == 0 {
			return inboxcontrol.ErrUnavailable
		}
		folder = inboxcontrol.Folder{ID: name, Name: name, UIDValidity: status.UIDValidity}
		return nil
	})
	return folder, err
}
