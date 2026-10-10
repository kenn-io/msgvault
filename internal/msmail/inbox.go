package msmail

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/msgraph"
)

// InboxProvider retains native Microsoft category editing behind signed inbox
// control. Read and folder metadata are preservation evidence; this adapter
// does not implement read-state, folder, or archive mutations.
type InboxProvider struct {
	client          *Client
	source          inboxcontrol.SourceIdentity
	writeCapability inboxcontrol.CapabilityStatus
	observedHash    string
}

var _ inboxcontrol.Provider = (*InboxProvider)(nil)

func NewInboxProvider(client *Client, source inboxcontrol.SourceIdentity) *InboxProvider {
	return &InboxProvider{client: client, source: source}
}

func (p *InboxProvider) WithWriteCapability(status inboxcontrol.CapabilityStatus) *InboxProvider {
	p.writeCapability = status
	return p
}

func (p *InboxProvider) target(request inboxcontrol.Request) (inboxcontrol.Target, error) {
	if p.client == nil || p.source.Validate() != nil || p.source.SourceType != SourceType || request.Target == nil {
		return inboxcontrol.Target{}, inboxcontrol.ErrUnavailable
	}
	target := *request.Target
	if target.Validate() != nil || target.SourceType != SourceType || target.SourceID != p.source.SourceID || target.SourceIdentifier != p.source.SourceIdentifier || target.AccountID != p.source.AccountID {
		return target, inboxcontrol.ErrDenied
	}
	return target, nil
}

func (p *InboxProvider) Observe(ctx context.Context, request inboxcontrol.Request) (inboxcontrol.State, error) {
	p.observedHash = ""
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
	if err := p.client.GetJSONOnce(ctx, "/me/mailFolders/inbox?$select=id", &folder, 1<<20); err != nil || folder.ID == "" {
		return inboxcontrol.State{}, inboxcontrol.ErrUnavailable
	}
	var message struct {
		ID             string    `json:"id"`
		Categories     *[]string `json:"categories"`
		Read           *bool     `json:"isRead"`
		ParentFolderID string    `json:"parentFolderId"`
		ETag           string    `json:"@odata.etag"`
	}
	path := "/me/messages/" + url.PathEscape(target.ProviderID) + "?$select=id,categories,isRead,parentFolderId"
	if err := p.client.GetJSONOnce(ctx, path, &message, 1<<20); err != nil {
		return inboxcontrol.State{}, inboxcontrol.ErrUnavailable
	}
	if message.ID != target.ProviderID {
		return inboxcontrol.State{}, inboxcontrol.ErrDenied
	}
	if message.Categories == nil || message.Read == nil || message.ParentFolderID == "" || message.ETag == "" || len(*message.Categories) > 100 {
		return inboxcontrol.State{}, inboxcontrol.ErrUnavailable
	}
	for _, tag := range *message.Categories {
		if _, err := emailtags.Normalize(emailtags.Change{Add: []string{tag}}, false); err != nil {
			return inboxcontrol.State{}, inboxcontrol.ErrUnavailable
		}
	}
	inbox := message.ParentFolderID == folder.ID
	state := inboxcontrol.State{Target: target, Inbox: &inbox, Read: message.Read, Tags: slices.Clone(*message.Categories), Location: message.ParentFolderID, Revision: message.ETag, ObservedAt: time.Now().UTC()}
	p.observedHash, err = inboxcontrol.SemanticFingerprint(state)
	if err != nil {
		return inboxcontrol.State{}, inboxcontrol.ErrUnavailable
	}
	return state, nil
}

func (p *InboxProvider) account(ctx context.Context) error {
	if p.client == nil || p.source.Validate() != nil || p.source.SourceType != SourceType {
		return inboxcontrol.ErrUnavailable
	}
	var profile struct {
		Mail              string `json:"mail"`
		UserPrincipalName string `json:"userPrincipalName"`
	}
	if err := p.client.GetJSONOnce(ctx, "/me?$select=mail,userPrincipalName", &profile, 1<<20); err != nil {
		return inboxcontrol.ErrUnavailable
	}
	if !strings.EqualFold(p.source.AccountID, profile.Mail) && !strings.EqualFold(p.source.AccountID, profile.UserPrincipalName) {
		return inboxcontrol.ErrDenied
	}
	return nil
}

// Capabilities describes the retained category lane. Although writes include
// If-Match, a fixture is not evidence of a guaranteed native conditional write.
func (p *InboxProvider) Capabilities(ctx context.Context, request inboxcontrol.Request) (*inboxcontrol.Capabilities, error) {
	if request.Source == nil || *request.Source != p.source || request.Target != nil {
		return nil, inboxcontrol.ErrDenied
	}
	if err := p.account(ctx); err != nil {
		return nil, err
	}
	result := &inboxcontrol.Capabilities{Source: p.source, LocationModel: "folders", ObservedAt: time.Now().UTC(), Operations: []inboxcontrol.Capability{}}
	for _, op := range []inboxcontrol.Operation{inboxcontrol.OpGetCapabilities, inboxcontrol.OpGetState, inboxcontrol.OpListFolders, inboxcontrol.OpTags, inboxcontrol.OpArchive, inboxcontrol.OpUnarchive, inboxcontrol.OpSetRead, inboxcontrol.OpSetUnread, inboxcontrol.OpMove, inboxcontrol.OpCreateFolder} {
		status, reason := inboxcontrol.CapabilityUnsupported, "This adapter exposes category changes only"
		switch op {
		case inboxcontrol.OpGetCapabilities, inboxcontrol.OpGetState:
			status, reason = inboxcontrol.CapabilitySupported, ""
		case inboxcontrol.OpTags:
			status, reason = p.writeCapability, ""
			switch status {
			case inboxcontrol.CapabilitySupported:
			case inboxcontrol.CapabilityPermissionRequired:
				reason = "Microsoft Mail.ReadWrite scope required"
			default:
				status, reason = inboxcontrol.CapabilityUnavailable, "Microsoft write scope could not be verified"
			}
		default:
			// Retain unsupported status outside the category lane.
		}
		result.Operations = append(result.Operations, inboxcontrol.Capability{Operation: op, Status: status, Reason: reason})
	}
	return result, nil
}

func (p *InboxProvider) change(request inboxcontrol.Request, before inboxcontrol.State) (emailtags.Change, error) {
	target, err := p.target(request)
	if err != nil {
		return emailtags.Change{}, err
	}
	if request.Operation != inboxcontrol.OpTags {
		return emailtags.Change{}, inboxcontrol.ErrUnavailable
	}
	if p.writeCapability != inboxcontrol.CapabilitySupported {
		if p.writeCapability == inboxcontrol.CapabilityPermissionRequired {
			return emailtags.Change{}, inboxcontrol.ErrDenied
		}
		return emailtags.Change{}, inboxcontrol.ErrUnavailable
	}
	hash, err := inboxcontrol.SemanticFingerprint(before)
	if err != nil || p.observedHash == "" || hash != p.observedHash || target != before.Target || before.Read == nil || before.Inbox == nil || before.Revision == "" || request.Tags == nil || request.Tags.Mailbox != "" || request.Tags.DryRun {
		return emailtags.Change{}, inboxcontrol.ErrPlanChanged
	}
	change, err := emailtags.Normalize(*request.Tags, false)
	if err != nil {
		return emailtags.Change{}, inboxcontrol.ErrInvalid
	}
	if len(emailtags.Project(before.Tags, change, false)) > 100 {
		return emailtags.Change{}, inboxcontrol.ErrInvalid
	}
	return change, nil
}

func (p *InboxProvider) Preview(_ context.Context, request inboxcontrol.Request, before inboxcontrol.State) (inboxcontrol.State, error) {
	change, err := p.change(request, before)
	if err != nil {
		return inboxcontrol.State{}, err
	}
	projected := before
	projected.Tags = emailtags.Project(before.Tags, change, false)
	return projected, nil
}

func (p *InboxProvider) Dispatch(ctx context.Context, request inboxcontrol.Request, before inboxcontrol.State) (inboxcontrol.DispatchResult, error) {
	change, err := p.change(request, before)
	if err != nil {
		return inboxcontrol.DispatchResult{}, errors.Join(inboxcontrol.ErrNoWrite, err)
	}
	add, remove := emailtags.Delta(before.Tags, change, false)
	if len(add)+len(remove) == 0 {
		return inboxcontrol.DispatchResult{}, nil
	}
	body := struct {
		Categories []string `json:"categories"`
	}{emailtags.Project(before.Tags, change, false)}
	err = p.client.PatchIfMatch(ctx, "/me/messages/"+url.PathEscape(before.Target.ProviderID), body, before.Revision)
	if err != nil {
		if errors.Is(err, msgraph.ErrPreconditionFailed) || errors.Is(err, msgraph.ErrBadRequest) || errors.Is(err, msgraph.ErrForbidden) || errors.Is(err, msgraph.ErrNotFound) {
			return inboxcontrol.DispatchResult{}, inboxcontrol.ErrNoWrite
		}
		return inboxcontrol.DispatchResult{}, inboxcontrol.ErrOutcomeUnknown
	}
	return inboxcontrol.DispatchResult{}, nil
}

func (p *InboxProvider) Verify(request inboxcontrol.Request, before, projected, after inboxcontrol.State) error {
	if request.Operation != inboxcontrol.OpTags || request.Tags == nil || before.Target != after.Target || projected.Target != after.Target || after.Read == nil || projected.Read == nil || after.Inbox == nil || projected.Inbox == nil || *after.Read != *projected.Read || *after.Inbox != *projected.Inbox || after.Location != projected.Location {
		return inboxcontrol.ErrOutcomeUnknown
	}
	if !emailtags.Verify(after.Tags, *request.Tags, false) {
		return inboxcontrol.ErrOutcomeUnknown
	}
	for _, tag := range projected.Tags {
		if !slices.Contains(after.Tags, tag) {
			return inboxcontrol.ErrOutcomeUnknown
		}
	}
	return nil
}

func (p *InboxProvider) Folders(context.Context, inboxcontrol.SourceIdentity) ([]inboxcontrol.Folder, error) {
	return nil, inboxcontrol.ErrUnavailable
}
