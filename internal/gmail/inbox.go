package gmail

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/oauth"
)

// InboxProvider applies native label deltas to one Store-resolved Gmail account.
// Gmail has no conditional message-label write: a signed preview is a freshness
// check, and independent readback proves the result after the single write.
type InboxProvider struct {
	client          *Client
	source          inboxcontrol.SourceIdentity
	writeCapability inboxcontrol.CapabilityStatus
}

func NewInboxProvider(client *Client, source inboxcontrol.SourceIdentity) *InboxProvider {
	return &InboxProvider{client: client, source: source}
}

var _ inboxcontrol.Provider = (*InboxProvider)(nil)

func (p *InboxProvider) Close() error { return p.client.Close() }

func (p *InboxProvider) target(request inboxcontrol.Request) (inboxcontrol.Target, error) {
	if p.client == nil || request.Target == nil {
		return inboxcontrol.Target{}, inboxcontrol.ErrUnavailable
	}
	t := *request.Target
	if err := t.Validate(); err != nil {
		return t, inboxcontrol.ErrDenied
	}
	if t.SourceType != "gmail" || t.Scope != inboxcontrol.ScopeMessage || t.SourceID != p.source.SourceID || t.SourceIdentifier != p.source.SourceIdentifier || t.AccountID != p.source.AccountID {
		return t, inboxcontrol.ErrDenied
	}
	return t, nil
}

func (p *InboxProvider) account(ctx context.Context) error {
	if p.client == nil || p.source.SourceType != "gmail" || p.source.Validate() != nil {
		return inboxcontrol.ErrUnavailable
	}
	profile, err := p.client.GetProfile(ctx)
	if err != nil {
		return inboxcontrol.ErrUnavailable
	}
	if profile == nil || !oauth.SameGoogleAccount(p.source.AccountID, profile.EmailAddress) {
		return inboxcontrol.ErrDenied
	}
	return nil
}

func (p *InboxProvider) Observe(ctx context.Context, request inboxcontrol.Request) (inboxcontrol.State, error) {
	if request.Source != nil {
		return p.observeFolders(ctx, request)
	}
	target, err := p.target(request)
	if err != nil {
		return inboxcontrol.State{}, err
	}
	if err = p.account(ctx); err != nil {
		return inboxcontrol.State{}, err
	}
	labels, history, err := p.client.inboxMetadata(ctx, target.ProviderID)
	if err != nil {
		return inboxcontrol.State{}, err
	}

	inbox := slices.Contains(labels, "INBOX")
	read := !slices.Contains(labels, "UNREAD")
	return inboxcontrol.State{Target: target, Tags: labels, Inbox: &inbox, Read: &read, Revision: strconv.FormatUint(history, 10), ObservedAt: time.Now().UTC()}, nil
}

func (p *InboxProvider) change(ctx context.Context, request inboxcontrol.Request, before inboxcontrol.State) (emailtags.Change, error) {
	target, err := p.target(request)
	if err != nil {
		return emailtags.Change{}, err
	}
	if target != before.Target || before.Inbox == nil || before.Read == nil {
		return emailtags.Change{}, inboxcontrol.ErrDenied
	}
	var change emailtags.Change
	switch request.Operation {
	case inboxcontrol.OpArchive:
		change.Remove = []string{"INBOX"}
	case inboxcontrol.OpUnarchive:
		change.Add = []string{"INBOX"}
	case inboxcontrol.OpSetRead:
		change.Remove = []string{"UNREAD"}
	case inboxcontrol.OpSetUnread:
		change.Add = []string{"UNREAD"}
	case inboxcontrol.OpTags:
		if request.Tags == nil {
			return change, inboxcontrol.ErrInvalid
		}
		change, err = emailtags.Normalize(*request.Tags, false)
		if err != nil || change.Mailbox != "" {
			return change, inboxcontrol.ErrInvalid
		}
	case inboxcontrol.OpMove:
		if request.OriginFolder == nil || request.Destination == nil || request.OriginFolder.ID == request.Destination.ID || !slices.Contains(before.Tags, request.OriginFolder.ID) {
			return change, inboxcontrol.ErrInvalid
		}
		change.Add = []string{request.Destination.ID}
		change.Remove = []string{request.OriginFolder.ID}
	default:
		return change, inboxcontrol.ErrUnavailable
	}
	if request.Operation == inboxcontrol.OpTags || request.Operation == inboxcontrol.OpMove {
		labels, err := p.client.ListLabels(ctx)
		if err != nil {
			return change, inboxcontrol.ErrUnavailable
		}
		for _, id := range append(slices.Clone(change.Add), change.Remove...) {
			if !slices.ContainsFunc(labels, func(l *Label) bool { return l != nil && l.Type == "user" && l.ID == id }) {
				return change, inboxcontrol.ErrUnavailable
			}
		}
	}
	return change, nil
}

func (p *InboxProvider) Preview(ctx context.Context, request inboxcontrol.Request, before inboxcontrol.State) (inboxcontrol.State, error) {
	if request.Operation == inboxcontrol.OpCreateFolder {
		return p.previewFolder(request, before)
	}
	change, err := p.change(ctx, request, before)
	if err != nil {
		return inboxcontrol.State{}, err
	}
	projected := before
	projected.Tags = emailtags.Project(before.Tags, change, false)
	inbox := slices.Contains(projected.Tags, "INBOX")
	read := !slices.Contains(projected.Tags, "UNREAD")
	projected.Inbox = &inbox
	projected.Read = &read
	return projected, nil
}

func (p *InboxProvider) Dispatch(ctx context.Context, request inboxcontrol.Request, before inboxcontrol.State) (inboxcontrol.DispatchResult, error) {
	if request.Operation == inboxcontrol.OpCreateFolder {
		return p.createFolder(ctx, request, before)
	}
	change, err := p.change(ctx, request, before)
	if err != nil {
		return inboxcontrol.DispatchResult{}, errors.Join(inboxcontrol.ErrNoWrite, err)
	}
	add, remove := emailtags.Delta(before.Tags, change, false)
	if len(add)+len(remove) == 0 {
		return inboxcontrol.DispatchResult{}, nil
	}
	body, err := json.Marshal(struct {
		Add    []string `json:"addLabelIds,omitempty"`
		Remove []string `json:"removeLabelIds,omitempty"`
	}{add, remove})
	if err != nil {
		return inboxcontrol.DispatchResult{}, inboxcontrol.ErrNoWrite
	}
	_, err = p.client.request(ctx, OpMessagesModify, http.MethodPost, fmt.Sprintf("/users/%s/messages/%s/modify", url.PathEscape(p.client.userID), url.PathEscape(before.Target.ProviderID)), body)
	if err != nil {
		if errors.Is(err, errWriteOutcomeUnknown) {
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
	if before.Target != after.Target || projected.Target != after.Target || after.Inbox == nil || after.Read == nil || projected.Inbox == nil || projected.Read == nil || *after.Inbox != *projected.Inbox || *after.Read != *projected.Read {
		return inboxcontrol.ErrOutcomeUnknown
	}
	// Verify requested additions/removals and preserve every unrelated prior label.
	// Concurrent additions are allowed; never overwrite them with a full snapshot.
	for _, id := range projected.Tags {
		if !slices.Contains(after.Tags, id) {
			return inboxcontrol.ErrOutcomeUnknown
		}
	}
	for _, id := range before.Tags {
		if !slices.Contains(projected.Tags, id) && slices.Contains(after.Tags, id) {
			return inboxcontrol.ErrOutcomeUnknown
		}
	}
	if request.Operation == inboxcontrol.OpTags &&
		(request.Tags == nil || !emailtags.Verify(after.Tags, *request.Tags, false)) {
		return inboxcontrol.ErrOutcomeUnknown
	}
	return nil
}

func (p *InboxProvider) Folders(ctx context.Context, source inboxcontrol.SourceIdentity) ([]inboxcontrol.Folder, error) {
	if source != p.source {
		return nil, inboxcontrol.ErrDenied
	}
	if err := p.account(ctx); err != nil {
		return nil, err
	}
	return p.folderCatalog(ctx)
}

// inboxMetadata reads only authoritative identity, labels and the history
// watermark. Missing markers are not an empty label set.
func (c *Client) inboxMetadata(ctx context.Context, id string) ([]string, uint64, error) {
	path := fmt.Sprintf("/users/%s/messages/%s?format=metadata&fields=id,labelIds,historyId", url.PathEscape(c.userID), url.PathEscape(id))
	data, err := c.request(ctx, OpMessagesGet, http.MethodGet, path, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("read inbox metadata: %w", err)
	}
	var message struct {
		ID        string    `json:"id"`
		Labels    *[]string `json:"labelIds"`
		HistoryID string    `json:"historyId"`
	}
	if json.Unmarshal(data, &message) != nil || message.ID != id || message.Labels == nil {
		return nil, 0, inboxcontrol.ErrUnavailable
	}
	history, err := strconv.ParseUint(message.HistoryID, 10, 64)
	if err != nil || history == 0 {
		return nil, 0, inboxcontrol.ErrUnavailable
	}
	return slices.Clone(*message.Labels), history, nil
}

// GetMessageLabelsBatch supplies the optional metadata-only sync path. Each
// result preserves its own read failure; no message bodies are downloaded.
func (c *Client) GetMessageLabelsBatch(ctx context.Context, ids []string) ([]MessageLabelsBatchResult, error) {
	results := make([]MessageLabelsBatchResult, len(ids))
	for i, id := range ids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		labels, history, err := c.inboxMetadata(ctx, id)
		if _, gone := errors.AsType[*NotFoundError](err); gone {
			err = errors.Join(ErrMessageGone, err)
		}
		results[i] = MessageLabelsBatchResult{ID: id, LabelIDs: labels, HistoryID: history, Err: err}
	}
	return results, nil
}
