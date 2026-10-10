package imap

import (
	"context"
	"fmt"
	"slices"
	"strings"

	imapapi "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

func (p *InboxProvider) previewMove(ctx context.Context, request inboxcontrol.Request, before inboxcontrol.State) (inboxcontrol.State, error) {
	destination := request.Destination
	if request.ResolvedFolder != nil {
		destination = request.ResolvedFolder
	} else if request.Operation == inboxcontrol.OpArchive {
		folder, err := p.archiveFolder(ctx)
		if err != nil {
			return inboxcontrol.State{}, err
		}
		destination = &folder
	}
	if destination == nil || destination.ID == "" || destination.UIDValidity == 0 || destination.ID == before.Target.Mailbox {
		return inboxcontrol.State{}, inboxcontrol.ErrUnavailable
	}
	err := p.client.withDraftConn(ctx, func(conn *imapclient.Client) error {
		if request.Operation == inboxcontrol.OpUnarchive {
			if request.Destination == nil || !strings.EqualFold(request.Destination.ID, "INBOX") {
				return inboxcontrol.ErrUnavailable
			}
			if strings.EqualFold(before.Target.Mailbox, "INBOX") {
				return inboxcontrol.ErrUnavailable
			}
			inbox, err := currentIMAPInbox(conn)
			if err != nil {
				return err
			}
			if request.Destination.UIDValidity == 0 || request.Destination.UIDValidity != inbox.UIDValidity {
				return inboxcontrol.ErrPlanChanged
			}
			if request.ResolvedFolder != nil &&
				(!strings.EqualFold(request.ResolvedFolder.ID, inbox.ID) || request.ResolvedFolder.UIDValidity != inbox.UIDValidity) {
				return inboxcontrol.ErrPlanChanged
			}
			destination = &inbox
		}
		// Move otherwise silently falls back to COPY, STORE Deleted and EXPUNGE.
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

func (p *InboxProvider) dispatchMove(ctx context.Context, request inboxcontrol.Request, before inboxcontrol.State) (inboxcontrol.DispatchResult, error) {
	dispatched := false
	var result inboxcontrol.DispatchResult
	err := p.client.withDraftConn(ctx, func(conn *imapclient.Client) error {
		if !conn.Caps().Has(imapapi.CapMove) || !conn.Caps().Has(imapapi.CapUIDPlus) {
			return inboxcontrol.ErrUnavailable
		}
		if _, err := p.selectTarget(conn, before.Target, false); err != nil {
			return err
		}
		if request.Operation == inboxcontrol.OpUnarchive {
			if request.Destination == nil || !strings.EqualFold(request.Destination.ID, "INBOX") {
				return inboxcontrol.ErrUnavailable
			}
			inbox, err := currentIMAPInbox(conn)
			if err != nil {
				return err
			}
			if request.Destination.UIDValidity == 0 || request.Destination.UIDValidity != inbox.UIDValidity {
				return inboxcontrol.ErrPlanChanged
			}
			request.Destination = &inbox
		}
		dispatched = true
		data, err := conn.Move(imapapi.UIDSetNum(imapapi.UID(before.Target.UID)), request.Destination.ID).Wait()
		if err != nil {
			return fmt.Errorf("move IMAP message: %w", err)
		}
		sourceSet, sourceOK := data.SourceUIDs.(imapapi.UIDSet)
		destSet, destOK := data.DestUIDs.(imapapi.UIDSet)
		if !sourceOK || !destOK || data.UIDValidity != request.Destination.UIDValidity {
			return inboxcontrol.ErrOutcomeUnknown
		}
		sources, sourceOK := sourceSet.Nums()
		destinations, destOK := destSet.Nums()
		if !sourceOK || !destOK || len(sources) != 1 || len(destinations) != 1 || uint32(sources[0]) != before.Target.UID || destinations[0] == 0 {
			return inboxcontrol.ErrOutcomeUnknown
		}
		target := before.Target
		target.Mailbox, target.UIDValidity, target.UID = request.Destination.ID, data.UIDValidity, uint32(destinations[0])
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

func currentIMAPInbox(conn *imapclient.Client) (inboxcontrol.Folder, error) {
	status, err := conn.Status("INBOX", &imapapi.StatusOptions{UIDValidity: true}).Wait()
	if err != nil || status == nil || !strings.EqualFold(status.Mailbox, "INBOX") || status.UIDValidity == 0 {
		return inboxcontrol.Folder{}, inboxcontrol.ErrUnavailable
	}
	return inboxcontrol.Folder{ID: "INBOX", Name: "INBOX", UIDValidity: status.UIDValidity}, nil
}

func isIMAPMove(operation inboxcontrol.Operation) bool {
	return operation == inboxcontrol.OpMove || operation == inboxcontrol.OpArchive || operation == inboxcontrol.OpUnarchive
}

// Names alone are never an Archive heuristic. Configuration overrides the
// native SPECIAL-USE selection, and the exact live mailbox epoch is signed.
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
				if slices.Contains(mailbox.Attrs, imapapi.MailboxAttrArchive) && !slices.Contains(mailbox.Attrs, imapapi.MailboxAttrNoSelect) {
					if name != "" {
						return inboxcontrol.ErrUnavailable
					}
					name = mailbox.Mailbox
				}
			}
		}
		if name == "" || strings.EqualFold(name, "INBOX") {
			return inboxcontrol.ErrUnavailable
		}
		status, err := conn.Status(name, &imapapi.StatusOptions{UIDValidity: true}).Wait()
		if err != nil || status.Mailbox != name || status.UIDValidity == 0 {
			return inboxcontrol.ErrUnavailable
		}
		folder = inboxcontrol.Folder{ID: name, Name: name, UIDValidity: status.UIDValidity}
		return nil
	})
	return folder, err
}

var _ inboxcontrol.MoveSourceVerifier = (*InboxProvider)(nil)

// VerifyMoveSource reads UID metadata in the original mailbox epoch. A failed
// FETCH, a changed epoch or any returned message is not proof of disappearance.
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
