package imap

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	imapapi "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

// InboxProvider uses recorded mailbox epochs and UIDs, never Message-ID lookup.
// The shared daemon service owns authorization, leases and durable dispatch.
type InboxProvider struct {
	client *Client
	source inboxcontrol.SourceIdentity
}

func NewInboxProvider(client *Client, source inboxcontrol.SourceIdentity) *InboxProvider {
	return &InboxProvider{client: client, source: source}
}

var _ inboxcontrol.Provider = (*InboxProvider)(nil)

func (p *InboxProvider) Close() error { return p.client.Close() }

func (p *InboxProvider) target(request inboxcontrol.Request) (inboxcontrol.Target, error) {
	if p.client == nil || p.client.config == nil || request.Target == nil || request.Source != nil {
		return inboxcontrol.Target{}, inboxcontrol.ErrUnavailable
	}
	target := *request.Target
	if target.Validate() != nil || target.SourceType != "imap" || target.SourceID != p.source.SourceID || target.SourceIdentifier != p.source.SourceIdentifier || target.AccountID != p.source.AccountID || p.source.SourceIdentifier != p.client.config.Identifier() || p.source.AccountID != p.client.config.Username {
		return target, inboxcontrol.ErrDenied
	}
	return target, nil
}

func (p *InboxProvider) selectTarget(conn *imapclient.Client, target inboxcontrol.Target, readOnly bool) (*imapapi.SelectData, error) {
	selected, err := conn.Select(target.Mailbox, &imapapi.SelectOptions{ReadOnly: readOnly, CondStore: conn.Caps().Has(imapapi.CapCondStore)}).Wait()
	if err != nil {
		return nil, inboxcontrol.ErrUnavailable
	}
	p.client.selectedMailbox, p.client.selectedUIDValidity, p.client.selectedNumMessages = target.Mailbox, selected.UIDValidity, selected.NumMessages
	if !readOnly && (selected.ReadOnly == nil || *selected.ReadOnly) {
		return nil, inboxcontrol.ErrUnavailable
	}
	if selected.UIDValidity != target.UIDValidity {
		return nil, inboxcontrol.ErrPlanChanged
	}
	return selected, nil
}

func (p *InboxProvider) Observe(ctx context.Context, request inboxcontrol.Request) (inboxcontrol.State, error) {
	if request.Operation == inboxcontrol.OpCreateFolder {
		return p.observeFolders(ctx, request)
	}
	target, err := p.target(request)
	if err != nil {
		return inboxcontrol.State{}, err
	}
	state := inboxcontrol.State{Target: target, Location: target.Mailbox}
	err = p.client.withDraftConn(ctx, func(conn *imapclient.Client) error {
		selected, err := p.selectTarget(conn, target, true)
		if err != nil {
			return err
		}
		includeModSeq := selected.HighestModSeq > 0
		flags, modSeq, err := fetchKeywordObservation(conn, target.UID, includeModSeq)
		if err != nil {
			return inboxcontrol.ErrUnavailable
		}
		inbox, read := strings.EqualFold(target.Mailbox, "INBOX"), containsIMAPFlag(flags, string(imapapi.FlagSeen))
		state.Flags, state.Tags, state.Inbox, state.Read = flags, keywordTags(flags), &inbox, &read
		state.Revision = fmt.Sprintf("%d:%d:%d:%d", selected.UIDValidity, selected.UIDNext, selected.NumMessages, modSeq)
		state.ObservedAt = time.Now().UTC()
		return nil
	})
	return state, err
}

func containsIMAPFlag(flags []string, flag string) bool {
	return slices.ContainsFunc(flags, func(value string) bool { return strings.EqualFold(value, flag) })
}

func (p *InboxProvider) Preview(ctx context.Context, request inboxcontrol.Request, before inboxcontrol.State) (inboxcontrol.State, error) {
	if request.Operation == inboxcontrol.OpCreateFolder {
		return p.previewFolder(request, before)
	}
	target, err := p.target(request)
	if err != nil {
		return inboxcontrol.State{}, err
	}
	if before.Target != target || before.Read == nil || before.Inbox == nil {
		return inboxcontrol.State{}, inboxcontrol.ErrUnavailable
	}
	if request.Operation == inboxcontrol.OpTags {
		return p.previewKeywords(ctx, request, before)
	}
	if isIMAPMove(request.Operation) {
		return p.previewMove(ctx, request, before)
	}
	if request.Operation != inboxcontrol.OpSetRead && request.Operation != inboxcontrol.OpSetUnread {
		return inboxcontrol.State{}, inboxcontrol.ErrUnavailable
	}
	err = p.client.withDraftConn(ctx, func(conn *imapclient.Client) error {
		selected, err := p.selectTarget(conn, target, true)
		if err != nil {
			return err
		}
		if selected.PermanentFlags != nil && !slices.Contains(selected.PermanentFlags, imapapi.FlagSeen) {
			return inboxcontrol.ErrUnavailable
		}
		return nil
	})
	if err != nil {
		return inboxcontrol.State{}, err
	}
	projected := before
	read := request.Operation == inboxcontrol.OpSetRead
	projected.Read = &read
	projected.Flags = slices.DeleteFunc(slices.Clone(before.Flags), func(flag string) bool { return strings.EqualFold(flag, string(imapapi.FlagSeen)) })
	if read {
		projected.Flags = append(projected.Flags, string(imapapi.FlagSeen))
	}
	slices.Sort(projected.Flags)
	return projected, nil
}

func (p *InboxProvider) Dispatch(ctx context.Context, request inboxcontrol.Request, before inboxcontrol.State) (inboxcontrol.DispatchResult, error) {
	if request.Operation == inboxcontrol.OpCreateFolder {
		return p.createFolder(ctx, request, before)
	}
	projected, err := p.Preview(ctx, request, before)
	if err != nil {
		return inboxcontrol.DispatchResult{}, inboxcontrol.ErrNoWrite
	}
	if request.Operation == inboxcontrol.OpTags {
		return p.dispatchKeywords(ctx, request, before)
	}
	if isIMAPMove(request.Operation) {
		destination := projected.Folders[0]
		resolved := request
		resolved.Destination = &destination
		return p.dispatchMove(ctx, resolved, before)
	}
	if *projected.Read == *before.Read {
		return inboxcontrol.DispatchResult{}, nil
	}
	dispatched := false
	err = p.client.withDraftConn(ctx, func(conn *imapclient.Client) error {
		if _, err := p.selectTarget(conn, before.Target, false); err != nil {
			return err
		}
		op := imapapi.StoreFlagsDel
		if *projected.Read {
			op = imapapi.StoreFlagsAdd
		}
		dispatched = true
		_, err := conn.Store(imapapi.UIDSetNum(imapapi.UID(before.Target.UID)), &imapapi.StoreFlags{Op: op, Silent: true, Flags: []imapapi.Flag{imapapi.FlagSeen}}, nil).Collect()
		if err != nil {
			return fmt.Errorf("set IMAP read flag: %w", err)
		}
		return nil
	})
	if err != nil {
		if dispatched {
			return inboxcontrol.DispatchResult{}, inboxcontrol.ErrOutcomeUnknown
		}
		return inboxcontrol.DispatchResult{}, inboxcontrol.ErrNoWrite
	}
	return inboxcontrol.DispatchResult{}, nil
}

func (p *InboxProvider) Verify(request inboxcontrol.Request, before, projected, after inboxcontrol.State) error {
	if request.Operation == inboxcontrol.OpCreateFolder {
		return p.verifyFolder(request, before, projected, after)
	}
	sameTarget := before.Target == after.Target && projected.Target == after.Target
	if isIMAPMove(request.Operation) && request.Target != nil && len(projected.Folders) == 1 {
		destination := projected.Folders[0]
		origin := after.Target
		origin.Mailbox, origin.UIDValidity, origin.UID = before.Target.Mailbox, before.Target.UIDValidity, before.Target.UID
		sameTarget = origin == before.Target && after.Target == *request.Target && after.Target.Mailbox == destination.ID && after.Target.UIDValidity == destination.UIDValidity && after.Target.UID != 0 && after.Location == projected.Location
	}
	if !sameTarget || after.Read == nil || projected.Read == nil || *after.Read != *projected.Read || after.Inbox == nil || projected.Inbox == nil || *after.Inbox != *projected.Inbox {
		return inboxcontrol.ErrOutcomeUnknown
	}
	for _, flag := range projected.Flags {
		if !containsIMAPFlag(after.Flags, flag) {
			return inboxcontrol.ErrOutcomeUnknown
		}
	}
	if !*projected.Read && containsIMAPFlag(after.Flags, string(imapapi.FlagSeen)) {
		return inboxcontrol.ErrOutcomeUnknown
	}
	if request.Operation == inboxcontrol.OpTags {
		if request.Tags == nil || !emailtags.Verify(after.Tags, *request.Tags, true) {
			return inboxcontrol.ErrOutcomeUnknown
		}
	}
	return nil
}

func (p *InboxProvider) Folders(ctx context.Context, source inboxcontrol.SourceIdentity) ([]inboxcontrol.Folder, error) {
	if p.client == nil || p.client.config == nil || source != p.source || source.SourceType != "imap" || source.SourceIdentifier != p.client.config.Identifier() || source.AccountID != p.client.config.Username {
		return nil, inboxcontrol.ErrDenied
	}
	var folders []inboxcontrol.Folder
	err := p.client.withDraftConn(ctx, func(conn *imapclient.Client) error {
		mailboxes, err := conn.List("", "*", nil).Collect()
		if err != nil {
			return inboxcontrol.ErrUnavailable
		}
		seen := make(map[string]bool)
		for _, mailbox := range mailboxes {
			if slices.Contains(mailbox.Attrs, imapapi.MailboxAttrNoSelect) {
				continue
			}
			if mailbox.Mailbox == "" || seen[mailbox.Mailbox] {
				return inboxcontrol.ErrUnavailable
			}
			seen[mailbox.Mailbox] = true
			status, err := conn.Status(mailbox.Mailbox, &imapapi.StatusOptions{UIDValidity: true}).Wait()
			if err != nil || status.Mailbox != mailbox.Mailbox || status.UIDValidity == 0 {
				return inboxcontrol.ErrUnavailable
			}
			folders = append(folders, inboxcontrol.Folder{ID: mailbox.Mailbox, Name: mailbox.Mailbox, UIDValidity: status.UIDValidity})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(folders, func(a, b inboxcontrol.Folder) int { return strings.Compare(a.ID, b.ID) })
	return folders, nil
}
