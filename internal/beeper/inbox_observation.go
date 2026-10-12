package beeper

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"go.kenn.io/msgvault/internal/inboxcontrol"
)

const (
	inboxResponseBytes      = 1 << 20
	inboxObservationTimeout = 5 * time.Second
)

// InboxProvider observes an exact Store-resolved Beeper account/chat binding.
// The daemon must check source lifetime, conversation identity and current
// grants before using this provider. It cannot currently admit any mutation:
// local message enumeration does not prove a sufficient current incoming window.
type InboxProvider struct {
	client *Client
	source inboxcontrol.SourceIdentity
}

// NewInboxProvider reuses the native client's credentials, limiter and bounded
// GET transport. Control reads do not follow redirects or change importer policy.
func NewInboxProvider(client *Client, source inboxcontrol.SourceIdentity) *InboxProvider {
	if client != nil && client.http != nil {
		copyClient := *client
		copyHTTP := *client.http
		copyHTTP.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		copyClient.http = &copyHTTP
		client = &copyClient
	}
	return &InboxProvider{client: client, source: source}
}

type inboxObject map[string]jsontext.Value

func decodeInboxObject(raw []byte) (inboxObject, error) {
	var object inboxObject
	if !jsontext.Value(raw).IsValid(jsontext.AllowDuplicateNames(false)) {
		return nil, inboxcontrol.ErrUnavailable
	}
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, inboxcontrol.ErrUnavailable
	}
	return object, nil
}

func inboxString(raw jsontext.Value) (string, bool) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", false
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", false
	}
	return v, true
}

func inboxBoolean(raw jsontext.Value) *bool {
	switch string(bytes.TrimSpace(raw)) {
	case "true":
		v := true
		return &v
	case "false":
		v := false
		return &v
	default:
		return nil
	}
}

func inboxAbsentOrNull(raw jsontext.Value) bool {
	return len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func (p *InboxProvider) validSource() bool {
	return p != nil && p.client != nil && p.client.http != nil && p.client.token != nil && p.client.limiter != nil && p.source.Validate() == nil && p.source.SourceType == "beeper" && p.source.AccountID == p.source.SourceIdentifier
}

func (p *InboxProvider) validTarget(target *inboxcontrol.Target) bool {
	return p.validSource() && target != nil && target.Validate() == nil && target.SourceType == p.source.SourceType && target.SourceID == p.source.SourceID && target.SourceIdentifier == p.source.SourceIdentifier && target.AccountID == p.source.AccountID
}

func (p *InboxProvider) getObject(ctx context.Context, path string) (inboxObject, error) {
	raw, err := p.client.fetch(ctx, path, inboxResponseBytes, false)
	if err != nil {
		return nil, inboxcontrol.ErrUnavailable
	}
	return decodeInboxObject(raw)
}

func validInboxChat(chat inboxObject, target inboxcontrol.Target) bool {
	id, idOK := inboxString(chat["id"])
	account, accountOK := inboxString(chat["accountID"])
	kind, kindOK := inboxString(chat["type"])
	return idOK && accountOK && kindOK && id == target.ProviderID && account == target.AccountID && (kind == "single" || kind == "group") && inboxAbsentOrNull(chat["merge"]) && inboxAbsentOrNull(chat["mergedIntoChatID"])
}

// Observe returns presence-aware provider markers. Read=true and an authorized
// LastMessageID require current-window admission that is not established yet.
func (p *InboxProvider) Observe(ctx context.Context, request inboxcontrol.Request) (inboxcontrol.State, error) {
	if !p.validTarget(request.Target) {
		return inboxcontrol.State{}, inboxcontrol.ErrDenied
	}
	ctx, cancel := context.WithTimeout(ctx, inboxObservationTimeout)
	defer cancel()
	info, err := p.getObject(ctx, "/v1/info")
	if err != nil {
		return inboxcontrol.State{}, err
	}
	chat, err := p.getObject(ctx, "/v1/chats/"+url.PathEscape(request.Target.ProviderID)+"?maxParticipantCount=-1")
	if err != nil {
		return inboxcontrol.State{}, err
	}
	if !validInboxChat(chat, *request.Target) {
		return inboxcontrol.State{}, inboxcontrol.ErrDenied
	}
	window := p.localInboxWindow(ctx, *request.Target)
	state := inboxcontrol.State{Target: *request.Target, ObservedAt: time.Now().UTC(), MarkedUnread: inboxBoolean(chat["isMarkedUnread"])}
	if archived := inboxBoolean(chat["isArchived"]); archived != nil {
		v := !*archived
		state.Inbox = &v
	}
	var unread int64
	count := chat["unreadCount"]
	if (state.MarkedUnread != nil && *state.MarkedUnread) || (!inboxAbsentOrNull(count) && json.Unmarshal(count, &unread) == nil && unread > 0) {
		v := false
		state.Read = &v
	}
	if marker, ok := inboxString(chat["lastReadMessageSortKey"]); ok {
		state.Flags = []string{"beeper:last-read-message-sort-key:" + marker}
	}
	control := inboxObject{}
	for _, key := range []string{"id", "accountID", "type", "isArchived", "unreadCount", "isMarkedUnread", "lastReadMessageSortKey", "capabilities", "merge", "mergedIntoChatID"} {
		if raw, ok := chat[key]; ok {
			control[key] = raw
		}
	}
	encoded, err := json.Marshal(struct {
		Target      inboxcontrol.Target
		Source      inboxcontrol.SourceIdentity
		Chat        inboxObject
		Frontier    string
		Profile     string
		Info        inboxObject
		LocalWindow inboxWindow
	}{state.Target, p.source, control, "unavailable", inboxProfileName, info, window}, json.Deterministic(true))
	if err != nil {
		return inboxcontrol.State{}, inboxcontrol.ErrUnavailable
	}
	digest := sha256.Sum256(encoded)
	state.Revision = fmt.Sprintf("beeper-observation-v1:%s", hex.EncodeToString(digest[:]))
	return state, nil
}
