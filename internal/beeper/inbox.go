package beeper

import (
	"context"
	"crypto/sha256"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"time"

	"go.kenn.io/msgvault/internal/inboxcontrol"
)

const maxInboxResponseBytes = 1 << 20

// InboxProvider binds one configured Desktop installation to an archived account.
// The shared daemon service owns authorization, execution leases and receipts.
type InboxProvider struct {
	client *Client
	source inboxcontrol.SourceIdentity
	chat   *Chat
	state  inboxcontrol.State
	latest *inboxMessageMetadata
}

func NewInboxProvider(client *Client, source inboxcontrol.SourceIdentity) *InboxProvider {
	return &InboxProvider{client: client, source: source}
}

var _ inboxcontrol.Provider = (*InboxProvider)(nil)

func (p *InboxProvider) Close() error { return nil }

func (p *InboxProvider) target(request inboxcontrol.Request) (inboxcontrol.Target, error) {
	if p.client == nil || request.Target == nil || request.Source != nil {
		return inboxcontrol.Target{}, inboxcontrol.ErrUnavailable
	}
	target := *request.Target
	if target.Validate() != nil || target.SourceType != "beeper" || target.SourceType != p.source.SourceType || target.SourceID != p.source.SourceID || target.SourceIdentifier != p.source.SourceIdentifier || target.AccountID != p.source.AccountID || p.source.SourceIdentifier != p.source.AccountID {
		return target, inboxcontrol.ErrDenied
	}
	return target, nil
}

// Control observations are bounded, sent once and never follow redirects. A
// provider error never exposes response bodies, URLs, bearer tokens or drafts.
func (c *Client) inboxBytes(ctx context.Context, path string) ([]byte, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, inboxcontrol.ErrUnavailable
	}
	token, err := c.token(ctx)
	if err != nil {
		return nil, inboxcontrol.ErrUnavailable
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, inboxcontrol.ErrUnavailable
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	transport := *c.http
	transport.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := transport.Do(request)
	if err != nil {
		return nil, inboxcontrol.ErrUnavailable
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, inboxcontrol.ErrUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxInboxResponseBytes+1))
	if err != nil || len(body) > maxInboxResponseBytes {
		return nil, inboxcontrol.ErrUnavailable
	}
	return body, nil
}
func (c *Client) inboxChat(ctx context.Context, chatID string) (*Chat, error) {
	body, err := c.inboxBytes(ctx, "/v1/chats/"+url.PathEscape(chatID)+"?maxParticipantCount=1")
	if err != nil {
		return nil, err
	}
	var chat Chat
	if json.Unmarshal(body, &chat) != nil {
		return nil, inboxcontrol.ErrUnavailable
	}
	return &chat, nil
}

func (p *InboxProvider) Observe(ctx context.Context, request inboxcontrol.Request) (inboxcontrol.State, error) {
	p.chat, p.latest, p.state = nil, nil, inboxcontrol.State{}
	target, err := p.target(request)
	if err != nil {
		return inboxcontrol.State{}, err
	}
	chat, err := p.client.inboxChat(ctx, target.ProviderID)
	if err != nil {
		return inboxcontrol.State{}, err
	}
	state, err := inboxChatState(p.client.baseURL, target, chat, nil)
	if err != nil {
		return inboxcontrol.State{}, err
	}
	switch request.Operation {
	case inboxcontrol.OpArchive, inboxcontrol.OpUnarchive, inboxcontrol.OpSetRead, inboxcontrol.OpSetUnread:
		latest, err := p.client.inboxLatestMessage(ctx, target)
		if err != nil {
			return inboxcontrol.State{}, err
		}
		if latest == nil && request.Operation == inboxcontrol.OpSetRead {
			return inboxcontrol.State{}, inboxcontrol.ErrUnavailable
		}
		p.latest = latest
		state, err = inboxChatState(p.client.baseURL, target, chat, latest)
		if err != nil {
			return inboxcontrol.State{}, err
		}
	default:
		// Other operations do not require a latest-message write boundary.
	}
	p.chat, p.state = chat, state
	return state, nil
}

// Sync and control interpret the same native markers. Missing evidence remains
// unknown; control mutations also bind the exact latest-message identity.
func inboxChatState(installation string, target inboxcontrol.Target, chat *Chat, latest *inboxMessageMetadata) (inboxcontrol.State, error) {
	if target.Validate() != nil || chat == nil || chat.ID != target.ProviderID || chat.AccountID != target.AccountID || chat.Merge != nil || chat.MergedIntoChatID != "" {
		return inboxcontrol.State{}, inboxcontrol.ErrDenied
	}
	if chat.UnreadCount != nil && *chat.UnreadCount < 0 {
		return inboxcontrol.State{}, inboxcontrol.ErrUnavailable
	}
	state := inboxcontrol.State{Target: target, MarkedUnread: chat.IsMarkedUnread, ObservedAt: time.Now().UTC()}
	if chat.IsArchived != nil {
		state.Inbox = new(!*chat.IsArchived)
	}
	if chat.IsMarkedUnread != nil && *chat.IsMarkedUnread || chat.UnreadCount != nil && *chat.UnreadCount > 0 {
		state.Read = new(false)
	} else if chat.IsMarkedUnread != nil && chat.UnreadCount != nil {
		state.Read = new(true)
	}
	if chat.LastReadMessageSortKey != "" {
		state.Flags = []string{"read-through:" + chat.LastReadMessageSortKey}
	}
	if latest != nil {
		state.LastMessageID = latest.ID
	}
	// Sign installation, current native support and non-marker metadata. Drafts
	// contribute only a digest; no composer content is returned in inbox state.
	draft := append(jsontext.Value(nil), chat.Draft...)
	if len(draft) > 0 {
		if err := draft.Format(jsontext.ReorderRawObjects(true), jsontext.CanonicalizeRawInts(false), jsontext.CanonicalizeRawFloats(false)); err != nil {
			return inboxcontrol.State{}, inboxcontrol.ErrUnavailable
		}
	}
	revision, err := json.Marshal(struct {
		Installation string
		Capabilities *ChatCapabilities
		LastActivity time.Time
		Draft        any
		Latest       *inboxMessageMetadata
	}{installation, chat.Capabilities, chat.LastActivity, draft, latest}, json.Deterministic(true))
	if err != nil {
		return inboxcontrol.State{}, inboxcontrol.ErrUnavailable
	}
	state.Revision = fmt.Sprintf("%x", sha256.Sum256(revision))
	return state, nil
}

func (p *InboxProvider) Preview(_ context.Context, request inboxcontrol.Request, before inboxcontrol.State) (inboxcontrol.State, error) {
	target, err := p.target(request)
	if err != nil {
		return inboxcontrol.State{}, err
	}
	if p.chat == nil || before.Target != target || p.state.Target != target || before.Revision != p.state.Revision || before.LastMessageID != p.state.LastMessageID {
		return inboxcontrol.State{}, inboxcontrol.ErrPlanChanged
	}
	// A mutation cannot prove that independent markers were preserved when
	// the installation did not report them.
	if before.Inbox == nil || before.Read == nil || before.MarkedUnread == nil {
		return inboxcontrol.State{}, inboxcontrol.ErrUnavailable
	}
	projected := before
	switch request.Operation {
	case inboxcontrol.OpArchive, inboxcontrol.OpUnarchive:
		if p.chat.Capabilities == nil || p.chat.Capabilities.Archive == nil || !*p.chat.Capabilities.Archive || before.Inbox == nil {
			return inboxcontrol.State{}, inboxcontrol.ErrUnavailable
		}
		projected.Inbox = new(request.Operation == inboxcontrol.OpUnarchive)
	case inboxcontrol.OpSetRead:
		if p.chat.Capabilities == nil || p.chat.Capabilities.MarkAsUnread == nil || !*p.chat.Capabilities.MarkAsUnread || before.MarkedUnread == nil || before.Read == nil || p.latest == nil || p.latest.ID == "" || p.latest.ID != before.LastMessageID {
			return inboxcontrol.State{}, inboxcontrol.ErrUnavailable
		}
		projected.Read, projected.MarkedUnread = new(true), new(false)
		projected.Flags = []string{"read-through:" + p.latest.SortKey}
	case inboxcontrol.OpSetUnread:
		// Beeper brings archived chats back to Inbox when marked unread. A
		// read-state intent must not implicitly perform an archive action.
		// An already-unread chat remains a safe no-op.
		if !*before.Inbox && !*before.MarkedUnread {
			return inboxcontrol.State{}, inboxcontrol.ErrUnavailable
		}
		if p.chat.Capabilities == nil || p.chat.Capabilities.MarkAsUnread == nil || !*p.chat.Capabilities.MarkAsUnread || before.MarkedUnread == nil {
			return inboxcontrol.State{}, inboxcontrol.ErrUnavailable
		}
		projected.MarkedUnread, projected.Read = new(true), new(false)
	default:
		return inboxcontrol.State{}, inboxcontrol.ErrUnavailable
	}
	return projected, nil
}
func (p *InboxProvider) Dispatch(ctx context.Context, request inboxcontrol.Request, before inboxcontrol.State) (inboxcontrol.DispatchResult, error) {
	projected, err := p.Preview(ctx, request, before)
	if err != nil {
		return inboxcontrol.DispatchResult{}, inboxcontrol.ErrNoWrite
	}
	if equalInboxBool(before.Inbox, projected.Inbox) && equalInboxBool(before.Read, projected.Read) && equalInboxBool(before.MarkedUnread, projected.MarkedUnread) && slices.Equal(before.Flags, projected.Flags) {
		return inboxcontrol.DispatchResult{}, nil
	}
	path := "/v1/chats/" + url.PathEscape(before.Target.ProviderID)
	body := []byte(`{}`)
	archive := request.Operation == inboxcontrol.OpArchive || request.Operation == inboxcontrol.OpUnarchive
	if archive {
		path += "/archive"
		body, err = json.Marshal(map[string]bool{"archived": request.Operation == inboxcontrol.OpArchive})
		if err != nil {
			return inboxcontrol.DispatchResult{}, inboxcontrol.ErrNoWrite
		}
	} else if request.Operation == inboxcontrol.OpSetRead {
		path += "/read"
		body, err = json.Marshal(map[string]string{"messageID": before.LastMessageID})
		if err != nil {
			return inboxcontrol.DispatchResult{}, inboxcontrol.ErrNoWrite
		}
	} else if request.Operation == inboxcontrol.OpSetUnread {
		path += "/unread"
	} else {
		return inboxcontrol.DispatchResult{}, inboxcontrol.ErrNoWrite
	}
	return inboxcontrol.DispatchResult{}, p.client.inboxPost(ctx, path, body, before.Target, archive)
}
func equalInboxBool(a, b *bool) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }
func (p *InboxProvider) Verify(request inboxcontrol.Request, before, projected, after inboxcontrol.State) error {
	if after.Target != before.Target || projected.Target != before.Target || after.Revision != before.Revision || projected.Revision != before.Revision || after.ObservedAt.IsZero() || after.LastMessageID != before.LastMessageID || !slices.Equal(after.Flags, projected.Flags) || !equalInboxBool(after.Inbox, projected.Inbox) || !equalInboxBool(after.Read, projected.Read) || !equalInboxBool(after.MarkedUnread, projected.MarkedUnread) {
		return inboxcontrol.ErrOutcomeUnknown
	}
	switch request.Operation {
	case inboxcontrol.OpArchive, inboxcontrol.OpUnarchive, inboxcontrol.OpSetRead, inboxcontrol.OpSetUnread:
		return nil
	default:
		return inboxcontrol.ErrOutcomeUnknown
	}
}
func (p *InboxProvider) Folders(context.Context, inboxcontrol.SourceIdentity) ([]inboxcontrol.Folder, error) {
	return nil, fmt.Errorf("%w: chat folders unsupported", inboxcontrol.ErrUnavailable)
}
