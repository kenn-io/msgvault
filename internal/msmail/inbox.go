package msmail

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/msgraph"
)

// InboxProvider changes only categories on an exact immutable message. The
// shared Inbox service owns authorization, signed plans and durable receipts.
type InboxProvider struct {
	client          *Client
	source          inboxcontrol.SourceIdentity
	writeCapability inboxcontrol.CapabilityStatus
}

func NewInboxProvider(client *Client, source inboxcontrol.SourceIdentity) *InboxProvider {
	return &InboxProvider{client: client, source: source}
}

var _ inboxcontrol.Provider = (*InboxProvider)(nil)
var _ inboxcontrol.CapabilityProvider = (*InboxProvider)(nil)

// WithWriteCapability supplies current Mail.ReadWrite evidence from the daemon.
// Missing evidence fails closed; this does not replace request authorization.
func (p *InboxProvider) WithWriteCapability(status inboxcontrol.CapabilityStatus) *InboxProvider {
	p.writeCapability = status
	return p
}

func (p *InboxProvider) target(request inboxcontrol.Request) (inboxcontrol.Target, error) {
	if p.client == nil || p.source.SourceType != SourceType || p.source.Validate() != nil {
		return inboxcontrol.Target{}, inboxcontrol.ErrUnavailable
	}
	if request.Target == nil {
		return inboxcontrol.Target{}, inboxcontrol.ErrDenied
	}
	t := *request.Target
	if t.Validate() != nil || t.SourceType != SourceType || t.SourceID != p.source.SourceID || t.SourceIdentifier != p.source.SourceIdentifier || t.AccountID != p.source.AccountID {
		return t, inboxcontrol.ErrDenied
	}
	return t, nil
}

func (p *InboxProvider) account(ctx context.Context) error {
	if p.client == nil || p.source.SourceType != SourceType || p.source.Validate() != nil {
		return inboxcontrol.ErrUnavailable
	}
	var profile msgraph.MailboxProfile
	if err := p.client.GetJSONOnce(ctx, "/me?$select=mail,userPrincipalName,proxyAddresses", &profile); err != nil {
		return inboxcontrol.ErrUnavailable
	}
	if !profile.Matches(p.source.AccountID) {
		return inboxcontrol.ErrDenied
	}
	return nil
}

type inboxSnapshot struct {
	ID         string    `json:"id"`
	Categories *[]string `json:"categories"`
	Read       *bool     `json:"isRead"`
	Parent     string    `json:"parentFolderId"`
	ETag       string    `json:"@odata.etag"`
}

func (m inboxSnapshot) complete(id string) bool {
	return m.ID == id && m.Categories != nil && m.Read != nil && strings.TrimSpace(m.Parent) != "" && strings.TrimSpace(m.ETag) != ""
}

func (p *InboxProvider) Observe(ctx context.Context, request inboxcontrol.Request) (inboxcontrol.State, error) {
	target, err := p.target(request)
	if err != nil {
		return inboxcontrol.State{}, err
	}
	if err := p.account(ctx); err != nil {
		return inboxcontrol.State{}, err
	}
	var folder struct {
		ID string `json:"id"`
	}
	if err := p.client.GetJSONOnce(ctx, "/me/mailFolders/inbox?$select=id", &folder); err != nil || strings.TrimSpace(folder.ID) == "" {
		return inboxcontrol.State{}, inboxcontrol.ErrUnavailable
	}
	var m inboxSnapshot
	if err := p.client.GetJSONOnce(ctx, "/me/messages/"+url.PathEscape(target.ProviderID)+"?$select=id,categories,isRead,parentFolderId", &m); err != nil {
		return inboxcontrol.State{}, inboxcontrol.ErrUnavailable
	}
	if !m.complete(target.ProviderID) {
		return inboxcontrol.State{}, inboxcontrol.ErrUnavailable
	}
	inbox := m.Parent == folder.ID
	return inboxcontrol.State{Target: target, Tags: append([]string{}, (*m.Categories)...), Read: m.Read, Inbox: &inbox, Location: m.Parent, Revision: m.ETag, ObservedAt: time.Now().UTC()}, nil
}

func (p *InboxProvider) change(request inboxcontrol.Request, before inboxcontrol.State) (emailtags.MessageTagChange, error) {
	if request.Operation != inboxcontrol.OpTags {
		return emailtags.MessageTagChange{}, inboxcontrol.ErrUnavailable
	}
	target, err := p.target(request)
	if err != nil {
		return emailtags.MessageTagChange{}, err
	}
	if target != before.Target || before.Read == nil || before.Inbox == nil || strings.TrimSpace(before.Location) == "" || strings.TrimSpace(before.Revision) == "" {
		return emailtags.MessageTagChange{}, inboxcontrol.ErrDenied
	}
	if p.writeCapability != inboxcontrol.CapabilitySupported {
		return emailtags.MessageTagChange{}, inboxcontrol.ErrUnavailable
	}
	if request.Tags == nil || request.Tags.DryRun || request.Tags.Mailbox != "" {
		return emailtags.MessageTagChange{}, inboxcontrol.ErrInvalid
	}
	change, err := emailtags.Normalize(*request.Tags, SourceType)
	if err != nil {
		return emailtags.MessageTagChange{}, inboxcontrol.ErrInvalid
	}
	return change, nil
}

func (p *InboxProvider) Preview(_ context.Context, request inboxcontrol.Request, before inboxcontrol.State) (inboxcontrol.State, error) {
	change, err := p.change(request, before)
	if err != nil {
		return inboxcontrol.State{}, err
	}
	projected := before
	projected.Tags = emailtags.Project(before.Tags, change, true)
	return projected, nil
}

func (p *InboxProvider) Dispatch(ctx context.Context, request inboxcontrol.Request, before inboxcontrol.State) (inboxcontrol.DispatchResult, error) {
	change, err := p.change(request, before)
	if err != nil {
		return inboxcontrol.DispatchResult{}, errors.Join(inboxcontrol.ErrNoWrite, err)
	}
	add, remove := emailtags.Delta(before.Tags, change, true)
	if len(add)+len(remove) == 0 {
		return inboxcontrol.DispatchResult{}, nil
	}
	body := struct {
		Categories []string `json:"categories"`
	}{emailtags.Project(before.Tags, change, true)}
	_, err = p.client.SendNoRetry(ctx, http.MethodPatch, "/me/messages/"+url.PathEscape(before.Target.ProviderID), body, before.Revision)
	if err != nil {
		status, _ := errors.AsType[*msgraph.StatusError](err)
		_, throttled := errors.AsType[*msgraph.ThrottledError](err)
		if errors.Is(err, msgraph.ErrPreconditionFailed) || errors.Is(err, msgraph.ErrNotFound) || errors.Is(err, msgraph.ErrForbidden) || errors.Is(err, msgraph.ErrUnauthorized) || errors.Is(err, msgraph.ErrNotSent) || throttled || (status != nil && status.StatusCode == http.StatusBadRequest) {
			return inboxcontrol.DispatchResult{}, inboxcontrol.ErrNoWrite
		}
		return inboxcontrol.DispatchResult{}, inboxcontrol.ErrOutcomeUnknown
	}
	return inboxcontrol.DispatchResult{}, nil
}

func (p *InboxProvider) Verify(request inboxcontrol.Request, before, projected, after inboxcontrol.State) error {
	change, err := p.change(request, before)
	if err != nil {
		return inboxcontrol.ErrOutcomeUnknown
	}
	want := emailtags.Project(before.Tags, change, true)
	if projected.Target != before.Target || after.Target != before.Target || before.Read == nil || before.Inbox == nil || after.Read == nil || after.Inbox == nil || projected.Read == nil || projected.Inbox == nil || *after.Read != *before.Read || *after.Inbox != *before.Inbox || *projected.Read != *before.Read || *projected.Inbox != *before.Inbox || after.Location != before.Location || projected.Location != before.Location || strings.TrimSpace(after.Revision) == "" || after.Tags == nil || !sameCategories(want, projected.Tags) || !sameCategories(want, after.Tags) || !emailtags.Verify(after.Tags, change, true) {
		return inboxcontrol.ErrOutcomeUnknown
	}
	return nil
}

func sameCategories(want, got []string) bool {
	want, got = slices.Clone(want), slices.Clone(got)
	slices.Sort(want)
	slices.Sort(got)
	return slices.Equal(want, got)
}

func (p *InboxProvider) Folders(context.Context, inboxcontrol.SourceIdentity) ([]inboxcontrol.Folder, error) {
	return nil, inboxcontrol.ErrUnavailable
}

func (p *InboxProvider) Capabilities(ctx context.Context, request inboxcontrol.Request) (*inboxcontrol.Capabilities, error) {
	if request.Source == nil || *request.Source != p.source || request.Target != nil {
		return nil, inboxcontrol.ErrDenied
	}
	if err := p.account(ctx); err != nil {
		return nil, err
	}
	result := &inboxcontrol.Capabilities{Source: p.source, LocationModel: "folders", ObservedAt: time.Now().UTC()}
	for _, op := range []inboxcontrol.Operation{inboxcontrol.OpGetCapabilities, inboxcontrol.OpGetState, inboxcontrol.OpTags, inboxcontrol.OpListFolders, inboxcontrol.OpArchive, inboxcontrol.OpUnarchive, inboxcontrol.OpSetRead, inboxcontrol.OpSetUnread, inboxcontrol.OpMove, inboxcontrol.OpCreateFolder} {
		status, reason := inboxcontrol.CapabilityUnsupported, "Native operation unavailable"
		if op == inboxcontrol.OpGetCapabilities || op == inboxcontrol.OpGetState {
			status, reason = inboxcontrol.CapabilitySupported, ""
		}
		if op == inboxcontrol.OpTags {
			status = p.writeCapability
			switch status {
			case inboxcontrol.CapabilitySupported:
				reason = ""
			case inboxcontrol.CapabilityPermissionRequired:
				reason = "Mail.ReadWrite scope required"
			default:
				status, reason = inboxcontrol.CapabilityUnavailable, "Current Mail.ReadWrite scope could not be verified"
			}
		}
		result.Operations = append(result.Operations, inboxcontrol.Capability{Operation: op, Status: status, Reason: reason})
	}
	return result, nil
}
