package imap

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	imapapi "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

// InboxProvider operates on exact Store-resolved mailbox copies. The shared
// Inbox Service owns authorization, execution fences and durable receipts.
type InboxProvider struct {
	client *Client
	source inboxcontrol.SourceIdentity
}

func NewInboxProvider(client *Client, source inboxcontrol.SourceIdentity) *InboxProvider {
	return &InboxProvider{client: client, source: source}
}

var _ inboxcontrol.Provider = (*InboxProvider)(nil)

func (p *InboxProvider) Close() error {
	if p.client == nil {
		return nil
	}
	return p.client.Close()
}

func (p *InboxProvider) validSource(source inboxcontrol.SourceIdentity) bool {
	return p.client != nil && p.client.config != nil && source == p.source && source.Validate() == nil && source.SourceType == "imap" && source.SourceIdentifier == p.client.config.Identifier() && source.AccountID == p.client.config.Username
}

func (p *InboxProvider) target(request inboxcontrol.Request) (inboxcontrol.Target, error) {
	if request.Target == nil || request.Source != nil || !p.validSource(p.source) {
		return inboxcontrol.Target{}, inboxcontrol.ErrDenied
	}
	target := *request.Target
	if target.Validate() != nil || target.SourceType != p.source.SourceType || target.SourceID != p.source.SourceID || target.SourceIdentifier != p.source.SourceIdentifier || target.AccountID != p.source.AccountID {
		return target, inboxcontrol.ErrDenied
	}
	return target, nil
}

func (p *InboxProvider) selectTarget(conn *imapclient.Client, target inboxcontrol.Target, readOnly bool) (*imapapi.SelectData, error) {
	// Never preserve a previous writable cache entry across failed selection.
	p.client.selectedMailbox = ""
	selected, err := conn.Select(target.Mailbox, &imapapi.SelectOptions{ReadOnly: readOnly, CondStore: conn.Caps().Has(imapapi.CapCondStore)}).Wait()
	if err != nil {
		return nil, inboxcontrol.ErrUnavailable
	}
	if selected.UIDValidity != target.UIDValidity {
		return nil, inboxcontrol.ErrPlanChanged
	}
	if !readOnly && (selected.ReadOnly == nil || *selected.ReadOnly) {
		return nil, inboxcontrol.ErrUnavailable
	}
	if !readOnly {
		p.client.selectedMailbox = target.Mailbox
	}
	p.client.selectedUIDValidity, p.client.selectedNumMessages = selected.UIDValidity, selected.NumMessages
	return selected, nil
}

func containsIMAPFlag(flags []string, flag string) bool { return emailtags.Contains(flags, flag, true) }

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
		includeModSeq := conn.Caps().Has(imapapi.CapCondStore) && selected.HighestModSeq > 0
		flags, modSeq, err := fetchKeywordObservation(conn, target.UID, includeModSeq)
		if err != nil {
			return inboxcontrol.ErrUnavailable
		}
		inbox, read := strings.EqualFold(target.Mailbox, "INBOX"), containsIMAPFlag(flags, string(imapapi.FlagSeen))
		state.Flags, state.Tags, state.Inbox, state.Read = flags, keywordTags(flags), &inbox, &read
		state.Revision = fmt.Sprintf("%d:%d", target.UIDValidity, modSeq)
		state.ObservedAt = time.Now().UTC()
		return nil
	})
	return state, err
}

func (p *InboxProvider) Preview(ctx context.Context, request inboxcontrol.Request, before inboxcontrol.State) (inboxcontrol.State, error) {
	if request.Operation == inboxcontrol.OpCreateFolder {
		return p.previewFolder(request, before)
	}
	target, err := p.target(request)
	if err != nil {
		return inboxcontrol.State{}, err
	}
	if before.Target != target || before.Read == nil || before.Inbox == nil || before.Flags == nil {
		return inboxcontrol.State{}, inboxcontrol.ErrUnavailable
	}
	if isIMAPMove(request.Operation) {
		return p.previewMove(ctx, request, before)
	}
	projected := before
	projected.Flags = slices.Clone(before.Flags)
	switch request.Operation {
	case inboxcontrol.OpSetRead, inboxcontrol.OpSetUnread:
		read := request.Operation == inboxcontrol.OpSetRead
		projected.Read = &read
		projected.Flags = slices.DeleteFunc(projected.Flags, func(flag string) bool { return strings.EqualFold(flag, string(imapapi.FlagSeen)) })
		if read {
			projected.Flags = append(projected.Flags, string(imapapi.FlagSeen))
		}
	case inboxcontrol.OpTags:
		if request.Tags == nil {
			return inboxcontrol.State{}, inboxcontrol.ErrInvalid
		}
		change, err := emailtags.Normalize(*request.Tags, "imap")
		if err != nil || change.Mailbox != "" {
			return inboxcontrol.State{}, inboxcontrol.ErrInvalid
		}
		// A mixed intent needs safe conditional replacement; two STOREs are not atomic.
		add, remove := emailtags.Delta(before.Tags, change, true)
		mixed := len(add) > 0 && len(remove) > 0
		if mixed && inboxMessageModSeq(before) == 0 {
			return inboxcontrol.State{}, inboxcontrol.ErrUnavailable
		}
		err = p.client.withDraftConn(ctx, func(conn *imapclient.Client) error {
			selected, err := p.selectTarget(conn, target, true)
			if err != nil {
				return err
			}
			if mixed && (!conn.Caps().Has(imapapi.CapCondStore) || selected.HighestModSeq == 0) {
				return inboxcontrol.ErrUnavailable
			}
			for _, keyword := range append(slices.Clone(change.Add), change.Remove...) {
				// EXAMINE PERMANENTFLAGS is not writable evidence. It may name an
				// existing keyword, but cannot veto a keyword in the FLAGS catalog.
				if !slices.ContainsFunc(selected.Flags, func(flag imapapi.Flag) bool { return strings.EqualFold(string(flag), keyword) }) && !slices.ContainsFunc(selected.PermanentFlags, func(flag imapapi.Flag) bool { return strings.EqualFold(string(flag), keyword) }) {
					return inboxcontrol.ErrUnavailable
				}
			}
			return nil
		})
		if err != nil {
			return inboxcontrol.State{}, err
		}
		projected.Tags = emailtags.Project(before.Tags, change, true)
		projected.Flags = slices.DeleteFunc(projected.Flags, func(flag string) bool { return emailtags.Contains(change.Remove, flag, true) })
		for _, keyword := range change.Add {
			if !containsIMAPFlag(projected.Flags, keyword) {
				projected.Flags = append(projected.Flags, keyword)
			}
		}
	default:
		return inboxcontrol.State{}, inboxcontrol.ErrUnavailable
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
	if isIMAPMove(request.Operation) {
		return p.dispatchMove(ctx, before, projected)
	}
	dispatched := false
	noWriteProved := false
	err = p.client.withDraftConn(ctx, func(conn *imapclient.Client) error {
		selected, err := p.selectTarget(conn, before.Target, false)
		if err != nil {
			return err
		}
		mixed := len(removedIMAPFlags(before.Flags, projected.Flags)) > 0 && len(removedIMAPFlags(projected.Flags, before.Flags)) > 0
		conditional := mixed && conn.Caps().Has(imapapi.CapCondStore) && selected.HighestModSeq > 0
		if mixed && !conditional {
			return inboxcontrol.ErrUnavailable
		}
		flags, modSeq, err := fetchKeywordObservation(conn, before.Target.UID, conditional)
		if err != nil {
			return err
		}
		if !sameIMAPFlags(flags, before.Flags) || (mixed && (modSeq == 0 || modSeq != inboxMessageModSeq(before))) {
			return inboxcontrol.ErrPlanChanged
		}
		add, remove := emailtags.Delta(before.Flags, emailtags.MessageTagChange{Add: projected.Flags, Remove: removedIMAPFlags(before.Flags, projected.Flags)}, true)
		// Validate actual writable permanent support, even for a satisfied intent.
		requested := []string{string(imapapi.FlagSeen)}
		if request.Operation == inboxcontrol.OpTags {
			requested = append(slices.Clone(request.Tags.Add), request.Tags.Remove...)
		}
		for _, flag := range requested {
			if selected.PermanentFlags != nil && !slices.ContainsFunc(selected.PermanentFlags, func(permanent imapapi.Flag) bool {
				return strings.EqualFold(string(permanent), flag) || (!strings.HasPrefix(flag, "\\") && permanent == imapapi.FlagWildcard)
			}) {
				return inboxcontrol.ErrUnavailable
			}
		}
		if len(add)+len(remove) == 0 {
			return nil
		}
		op, delta := imapapi.StoreFlagsAdd, add
		if len(remove) > 0 {
			op, delta = imapapi.StoreFlagsDel, remove
		}
		var options *imapapi.StoreOptions
		if mixed {
			op = imapapi.StoreFlagsSet
			delta = slices.DeleteFunc(slices.Clone(projected.Flags), func(flag string) bool { return strings.EqualFold(flag, `\Recent`) })
			for _, flag := range delta {
				if selected.PermanentFlags != nil && !slices.ContainsFunc(selected.PermanentFlags, func(permanent imapapi.Flag) bool {
					return strings.EqualFold(string(permanent), flag) || (!strings.HasPrefix(flag, "\\") && permanent == imapapi.FlagWildcard)
				}) {
					return inboxcontrol.ErrUnavailable
				}
			}
			options = &imapapi.StoreOptions{UnchangedSince: modSeq}
		}
		values := make([]imapapi.Flag, len(delta))
		for i, flag := range delta {
			values[i] = imapapi.Flag(flag)
		}
		dispatched = true
		cmd := conn.Store(imapapi.UIDSetNum(imapapi.UID(before.Target.UID)), &imapapi.StoreFlags{Op: op, Silent: true, Flags: values}, options)
		_, err = cmd.Collect()
		if err != nil {
			return fmt.Errorf("collect IMAP message flags: %w", err)
		}
		if modified := cmd.ModifiedUIDs(); len(modified) > 0 {
			if len(modified) == 1 && modified[0].Start == imapapi.UID(before.Target.UID) && modified[0].Stop == imapapi.UID(before.Target.UID) {
				noWriteProved = true
				return inboxcontrol.ErrNoWrite
			}
			return inboxcontrol.ErrOutcomeUnknown
		}
		return nil
	})
	if err != nil {
		if noWriteProved {
			return inboxcontrol.DispatchResult{}, inboxcontrol.ErrNoWrite
		}
		if dispatched {
			return inboxcontrol.DispatchResult{}, inboxcontrol.ErrOutcomeUnknown
		}
		return inboxcontrol.DispatchResult{}, inboxcontrol.ErrNoWrite
	}
	return inboxcontrol.DispatchResult{}, nil
}

func removedIMAPFlags(before, after []string) []string {
	var removed []string
	for _, flag := range before {
		if !containsIMAPFlag(after, flag) {
			removed = append(removed, flag)
		}
	}
	return removed
}

func sameIMAPFlags(a, b []string) bool {
	return len(removedIMAPFlags(a, b)) == 0 && len(removedIMAPFlags(b, a)) == 0
}

// imapEffectFlags excludes only the session-owned Recent marker from effect
// verification. Raw observations and signed preconditions keep every flag.
func imapEffectFlags(flags []string) []string {
	return slices.DeleteFunc(slices.Clone(flags), func(flag string) bool { return strings.EqualFold(flag, `\Recent`) })
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
		sameTarget = origin == before.Target && after.Target == *request.Target && after.Target.Mailbox == destination.ID && after.Target.UIDValidity == destination.UIDValidity && after.Target.UID > 0
	}
	if before.Flags == nil || projected.Flags == nil || after.Flags == nil || !sameIMAPFlags(keywordTags(after.Flags), after.Tags) || request.Target == nil || after.Target != *request.Target || !sameTarget || after.Read == nil || projected.Read == nil || *after.Read != *projected.Read || after.Inbox == nil || projected.Inbox == nil || *after.Inbox != *projected.Inbox || after.Location != projected.Location || !sameIMAPFlags(imapEffectFlags(projected.Flags), imapEffectFlags(after.Flags)) {
		return inboxcontrol.ErrOutcomeUnknown
	}
	if request.Operation == inboxcontrol.OpTags && (request.Tags == nil || !emailtags.Verify(after.Tags, *request.Tags, true)) {
		return inboxcontrol.ErrOutcomeUnknown
	}
	return nil
}

func (p *InboxProvider) Folders(ctx context.Context, source inboxcontrol.SourceIdentity) ([]inboxcontrol.Folder, error) {
	if !p.validSource(source) {
		return nil, inboxcontrol.ErrDenied
	}
	folders := []inboxcontrol.Folder{}
	err := p.client.withDraftConn(ctx, func(conn *imapclient.Client) error {
		mailboxes, err := conn.List("", "*", nil).Collect()
		if err != nil {
			return inboxcontrol.ErrUnavailable
		}
		seen := map[string]bool{}
		for _, mailbox := range mailboxes {
			if slices.Contains(mailbox.Attrs, imapapi.MailboxAttrNoSelect) {
				continue
			}
			if mailbox.Mailbox == "" || seen[mailbox.Mailbox] {
				return inboxcontrol.ErrUnavailable
			}
			seen[mailbox.Mailbox] = true
			status, err := conn.Status(mailbox.Mailbox, &imapapi.StatusOptions{UIDValidity: true}).Wait()
			if err != nil || status == nil || status.Mailbox != mailbox.Mailbox || status.UIDValidity == 0 {
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

func inboxMessageModSeq(state inboxcontrol.State) uint64 {
	epoch, revision, ok := strings.Cut(state.Revision, ":")
	if !ok || epoch != strconv.FormatUint(uint64(state.Target.UIDValidity), 10) {
		return 0
	}
	modSeq, err := strconv.ParseUint(revision, 10, 64)
	if err != nil {
		return 0
	}
	return modSeq
}
