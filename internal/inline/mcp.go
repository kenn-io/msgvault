package inline

import (
	"context"
	stdjson "encoding/json"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	DefaultMCPEndpoint  = "https://mcp.inline.chat/mcp/v2"
	mcpMaxResponseBytes = 32 << 20
)

// ErrContract means a provider result cannot be archived without risking
// incorrect identity, silently missing messages, or misidentified media.
var ErrContract = errors.New("inline MCP contract error")

// MCPClient calls a fixed set of read tools. MCP is a deterministic transport;
// no model, agent, or provider write tool participates in archive ingestion.
type MCPClient struct {
	mu               sync.Mutex
	endpoint         string
	http             *http.Client
	session          *mcp.ClientSession
	ownerID          int64
	closed           bool
	invalidPrincipal bool
}

var _ Client = (*MCPClient)(nil)

func NewMCPClient(ctx context.Context, endpoint string, httpClient *http.Client) (*MCPClient, error) {
	if endpoint == "" {
		endpoint = DefaultMCPEndpoint
	}
	if _, err := validateMCPEndpoint(endpoint); err != nil {
		return nil, err
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	// The SDK reads JSON responses in full. Bound both JSON and SSE response
	// streams while retaining the caller's authentication and redirect policy.
	boundedClient := *httpClient
	base := boundedClient.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	boundedClient.Transport = &mcpResponseTransport{base: base, limit: mcpMaxResponseBytes}
	c := &MCPClient{endpoint: endpoint, http: &boundedClient}
	if err := c.connect(ctx); err != nil {
		return nil, err
	}
	if _, err := c.Me(ctx); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

type mcpResponseTransport struct {
	base  http.RoundTripper
	limit int64
}

func (t *mcpResponseTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.ContentLength > t.limit {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%w: MCP response exceeds %d bytes", ErrContract, t.limit)
	}
	resp.Body = &mcpResponseBody{ReadCloser: resp.Body, remaining: t.limit}
	return resp, nil
}

type mcpResponseBody struct {
	io.ReadCloser

	remaining int64
}

func (b *mcpResponseBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if b.remaining == 0 {
		// Probe one byte beyond the limit: an exact-size body remains valid,
		// while overflow produces an error instead of a misleading EOF.
		var extra [1]byte
		n, err := b.ReadCloser.Read(extra[:])
		if n != 0 {
			return 0, fmt.Errorf("%w: MCP response exceeds byte limit", ErrContract)
		}
		return 0, err
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.ReadCloser.Read(p)
	b.remaining -= int64(n)
	return n, err
}

func validateMCPEndpoint(endpoint string) (*url.URL, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "/mcp/v2" && u.Path != "/mcp") {
		return nil, errors.New("inline MCP endpoint must be an exact /mcp/v2 or /mcp URL")
	}
	if u.Scheme == "https" && u.Host == "mcp.inline.chat" {
		return u, nil
	}
	if u.Scheme == "http" && isLoopbackHost(u.Hostname()) {
		return u, nil
	}
	return nil, errors.New("inline MCP endpoint must use https://mcp.inline.chat (HTTP loopback is permitted for tests)")
}

func isLoopbackHost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func (c *MCPClient) connect(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "msgvault", Version: "dev"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: c.endpoint, HTTPClient: c.http, DisableStandaloneSSE: true, MaxRetries: -1}, nil)
	if err != nil {
		return fmt.Errorf("connect Inline MCP: %w", err)
	}
	c.session = session
	return nil
}

func (c *MCPClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	if c.session == nil {
		return nil
	}
	if err := c.session.Close(); err != nil {
		return fmt.Errorf("close Inline MCP session: %w", err)
	}
	return nil
}

func (c *MCPClient) call(ctx context.Context, name string, args map[string]any) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errors.New("inline MCP client is closed")
	}
	if c.invalidPrincipal {
		return nil, fmt.Errorf("%w: account changed; create a new client", ErrContract)
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	result, err := c.session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if errors.Is(err, mcp.ErrSessionMissing) {
		// A provider restart or idle expiry invalidates the session, not the
		// archive cursor. Reinitialize once, verify its principal, then retry.
		_ = c.session.Close()
		if err = c.connect(ctx); err != nil {
			return nil, err
		}
		me, checkErr := c.session.CallTool(ctx, &mcp.CallToolParams{Name: "account.me", Arguments: map[string]any{}})
		if checkErr != nil {
			return nil, fmt.Errorf("verify reconnected Inline account: %w", checkErr)
		}
		raw, checkErr := inlineToolJSON(me)
		if checkErr != nil {
			return nil, checkErr
		}
		account, checkErr := decodeAccount(raw)
		if checkErr != nil {
			return nil, checkErr
		}
		if c.ownerID != 0 && account.UserID != c.ownerID {
			c.invalidPrincipal = true
			return nil, fmt.Errorf("%w: reconnected account differs from archive owner", ErrContract)
		}
		c.ownerID = account.UserID
		result, err = c.session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	}
	if err != nil {
		return nil, fmt.Errorf("Inline tool %s: %w", name, err)
	}
	raw, err := inlineToolJSON(result)
	if err != nil {
		return nil, fmt.Errorf("Inline tool %s: %w", name, err)
	}
	return raw, nil
}

func inlineToolJSON(result *mcp.CallToolResult) ([]byte, error) {
	if result == nil {
		return nil, fmt.Errorf("%w: missing tool result", ErrContract)
	}
	if result.IsError {
		return nil, errors.New("Inline tool rejected the read; check account access and messages:read authorization")
	}
	if result.StructuredContent != nil {
		data, err := json.Marshal(result.StructuredContent, json.Deterministic(true))
		if err != nil {
			return nil, fmt.Errorf("%w: invalid structured result", ErrContract)
		}
		return data, nil
	}
	var text strings.Builder
	for _, content := range result.Content {
		if v, ok := content.(*mcp.TextContent); ok {
			text.WriteString(v.Text)
		}
	}
	data := []byte(text.String())
	if !stdjson.Valid(data) {
		return nil, fmt.Errorf("%w: missing or malformed JSON tool result", ErrContract)
	}
	return data, nil
}

func decodeAccount(raw []byte) (Account, error) {
	var v struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
		Session struct {
			Scopes []string `json:"scopes"`
		} `json:"session"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return Account{}, fmt.Errorf("%w: malformed account", ErrContract)
	}
	id, err := positiveID(v.User.ID)
	if err != nil || !slices.Contains(v.Session.Scopes, "messages:read") {
		return Account{}, fmt.Errorf("%w: account identity or messages:read scope missing", ErrContract)
	}
	return Account{UserID: id, Origin: ProductionOrigin}, nil
}

func (c *MCPClient) Me(ctx context.Context) (Account, error) {
	raw, err := c.call(ctx, "account.me", map[string]any{})
	if err != nil {
		return Account{}, err
	}
	a, err := decodeAccount(raw)
	if err != nil {
		return Account{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ownerID != 0 && c.ownerID != a.UserID {
		c.invalidPrincipal = true
		return Account{}, fmt.Errorf("%w: account changed", ErrContract)
	}
	c.ownerID = a.UserID
	return a, nil
}

type mcpChat struct {
	ChatID string `json:"chatId"`
	Title  string `json:"title"`
	Kind   string `json:"kind"`
	Peer   *struct {
		UserID stdjson.RawMessage `json:"userId"`
	} `json:"peer"`
}

// Discover enumerates the complete catalog authorized by the OAuth grant.
// An explicit source marker prevents an older MCP/server deployment from
// silently returning the historical capped, visible-dialog-only list.
func (c *MCPClient) Discover(ctx context.Context) ([]Conversation, error) {
	var out []Conversation
	var after int64
	for {
		args := map[string]any{"includeSubthreads": true, "sort": "id", "limit": 50}
		if after != 0 {
			args["afterChatId"] = strconv.FormatInt(after, 10)
		}
		raw, err := c.call(ctx, "conversations.list", args)
		if err != nil {
			return nil, err
		}
		var v struct {
			Items    []stdjson.RawMessage `json:"items"`
			Sort     string               `json:"sort"`
			Next     stdjson.RawMessage   `json:"nextAfterChatId"`
			Complete *bool                `json:"subthreadsIncluded"`
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, fmt.Errorf("%w: malformed conversation catalog", ErrContract)
		}
		if v.Complete == nil || !*v.Complete {
			return nil, fmt.Errorf("%w: complete conversation discovery requires an Inline server and MCP upgrade", ErrContract)
		}
		if v.Items == nil || len(v.Items) > 50 || v.Next == nil || v.Sort != "id" {
			return nil, fmt.Errorf("%w: malformed catalog page", ErrContract)
		}
		last := after
		for _, item := range v.Items {
			var chat mcpChat
			if err := json.Unmarshal(item, &chat); err != nil {
				return nil, fmt.Errorf("%w: malformed catalog conversation", ErrContract)
			}
			id, err := positiveID(chat.ChatID)
			if err != nil || id <= last {
				return nil, fmt.Errorf("%w: catalog IDs do not increase", ErrContract)
			}
			if chat.Kind != "dm" && chat.Kind != "home_thread" && chat.Kind != "space_chat" {
				return nil, fmt.Errorf("%w: unknown catalog conversation kind", ErrContract)
			}
			out = append(out, Conversation{ID: id, Title: chat.Title, Type: conversationType(chat.Kind), MemberCountKnown: false, Raw: item})
			last = id
		}
		if string(v.Next) == "null" {
			return out, nil
		}
		var next string
		if err := json.Unmarshal(v.Next, &next); err != nil {
			return nil, fmt.Errorf("%w: malformed catalog cursor", ErrContract)
		}
		nextID, err := positiveID(next)
		if err != nil || len(v.Items) == 0 || nextID != last || nextID <= after {
			return nil, fmt.Errorf("%w: catalog cursor does not advance", ErrContract)
		}
		after = nextID
	}
}

func (c *MCPClient) Conversation(ctx context.Context, chatID int64) (Conversation, error) {
	if !validID(chatID) {
		return Conversation{}, errors.New("inline chat ID must be a positive safe integer")
	}
	raw, err := c.call(ctx, "conversations.get", map[string]any{"chatId": strconv.FormatInt(chatID, 10)})
	if err != nil {
		return Conversation{}, err
	}
	var v struct {
		Chat    mcpChat `json:"chat"`
		Details *struct {
			ParentChatID    *string `json:"parentChatId"`
			ParentMessageID *string `json:"parentMessageId"`
		} `json:"details"`
	}
	if err := json.Unmarshal(raw, &v); err != nil || v.Details == nil {
		return Conversation{}, fmt.Errorf("%w: malformed conversation", ErrContract)
	}
	id, err := positiveID(v.Chat.ChatID)
	if err != nil || id != chatID {
		return Conversation{}, fmt.Errorf("%w: conversation ID mismatch", ErrContract)
	}
	if v.Chat.Kind != "dm" && v.Chat.Kind != "home_thread" && v.Chat.Kind != "space_chat" {
		return Conversation{}, fmt.Errorf("%w: unknown conversation kind", ErrContract)
	}
	parent, err := optionalID(v.Details.ParentChatID)
	if err != nil {
		return Conversation{}, err
	}
	anchor, err := optionalID(v.Details.ParentMessageID)
	if err != nil {
		return Conversation{}, err
	}
	conversation := Conversation{ID: id, Type: conversationType(v.Chat.Kind), Title: v.Chat.Title, ParentChatID: parent, RootMessageID: anchor, Raw: raw}
	if v.Chat.Kind == "dm" && v.Chat.Peer != nil {
		if v.Chat.Peer.UserID == nil {
			return Conversation{}, fmt.Errorf("%w: DM peer identity missing", ErrContract)
		}
		if string(v.Chat.Peer.UserID) != "null" {
			var rawPeerID string
			if err := json.Unmarshal(v.Chat.Peer.UserID, &rawPeerID); err != nil {
				return Conversation{}, fmt.Errorf("%w: malformed DM peer identity", ErrContract)
			}
			peerID, err := positiveID(rawPeerID)
			if err != nil {
				return Conversation{}, err
			}
			c.mu.Lock()
			ownerID := c.ownerID
			c.mu.Unlock()
			conversation.MemberCountKnown = true
			conversation.MemberCount = 2
			if peerID == ownerID {
				conversation.MemberCount = 1
			}
		}
	}
	// Group participants and grants do not establish complete effective
	// membership. Their media policy always receives an unknown count.
	return conversation, nil
}

type mcpMedia struct {
	Kind      string   `json:"kind"`
	ID        *string  `json:"id"`
	URL       *string  `json:"url"`
	FileName  *string  `json:"fileName"`
	MIMEType  *string  `json:"mimeType"`
	SizeBytes *int64   `json:"sizeBytes"`
	Width     *int     `json:"width"`
	Height    *int     `json:"height"`
	Duration  *float64 `json:"durationSeconds"`
}

type mcpMessage struct {
	ID         string    `json:"id"`
	ChatID     string    `json:"chatId"`
	FromID     *string   `json:"fromId"`
	Text       *string   `json:"text"`
	Out        *bool     `json:"out"`
	SenderName string    `json:"senderDisplayName"`
	Date       *string   `json:"date"`
	EditDate   *string   `json:"editDate"`
	ReplyToID  *string   `json:"replyToMsgId"`
	Media      *mcpMedia `json:"media"`
}

func decodeMCPMessage(raw stdjson.RawMessage, chatID int64) (Message, error) {
	var v mcpMessage
	if err := json.Unmarshal(raw, &v); err != nil {
		return Message{}, fmt.Errorf("%w: malformed message", ErrContract)
	}
	id, err := positiveID(v.ID)
	if err != nil {
		return Message{}, err
	}
	chat, err := positiveID(v.ChatID)
	if err != nil || chat != chatID {
		return Message{}, fmt.Errorf("%w: message chat ID mismatch", ErrContract)
	}
	if v.Text == nil || v.Out == nil || v.Date == nil {
		return Message{}, fmt.Errorf("%w: message text, direction, or date missing", ErrContract)
	}
	from, err := optionalID(v.FromID)
	if err != nil {
		return Message{}, err
	}
	reply, err := optionalID(v.ReplyToID)
	if err != nil {
		return Message{}, err
	}
	date, err := messageTime(v.Date)
	if err != nil {
		return Message{}, err
	}
	edit, err := messageTime(v.EditDate)
	if err != nil {
		return Message{}, err
	}
	msg := Message{ID: id, ChatID: chat, SenderID: from, Text: *v.Text, SenderName: v.SenderName, SentAt: date, EditedAt: edit, IsFromMe: *v.Out, ReplyToMessageID: reply, Raw: raw, RawFormat: RawMCPFormat}
	if v.Media != nil {
		media, err := decodeMCPMedia(*v.Media, chatID, id)
		if err != nil {
			return Message{}, err
		}
		if media != nil {
			msg.Media = []Media{*media}
		}
	}
	return msg, nil
}

func (c *MCPClient) Messages(ctx context.Context, chatID, beforeID int64) (Page, error) {
	if !validID(chatID) || beforeID < 0 || beforeID > MaxID {
		return Page{}, errors.New("invalid Inline history IDs")
	}
	args := map[string]any{"chatId": strconv.FormatInt(chatID, 10), "limit": 50}
	if beforeID != 0 {
		args["offsetId"] = strconv.FormatInt(beforeID, 10)
	}
	raw, err := c.call(ctx, "messages.list", args)
	if err != nil {
		return Page{}, err
	}
	var v struct {
		Chat       mcpChat              `json:"chat"`
		NextOffset stdjson.RawMessage   `json:"nextOffsetId"`
		Messages   []stdjson.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(raw, &v); err != nil || v.NextOffset == nil || v.Messages == nil {
		return Page{}, fmt.Errorf("%w: malformed history page", ErrContract)
	}
	id, err := positiveID(v.Chat.ChatID)
	if err != nil || id != chatID {
		return Page{}, fmt.Errorf("%w: history chat ID mismatch", ErrContract)
	}
	page := Page{Messages: make([]Message, 0, len(v.Messages))}
	if string(v.NextOffset) != "null" {
		var next string
		if err := json.Unmarshal(v.NextOffset, &next); err != nil {
			return Page{}, fmt.Errorf("%w: malformed history cursor", ErrContract)
		}
		page.NextBeforeID, err = positiveID(next)
		if err != nil {
			return Page{}, err
		}
		page.HasMore = true
	}
	if len(v.Messages) > 50 {
		return Page{}, fmt.Errorf("%w: history exceeds requested page size", ErrContract)
	}
	for _, item := range v.Messages {
		msg, err := decodeMCPMessage(item, chatID)
		if err != nil {
			return Page{}, err
		}
		if beforeID != 0 && msg.ID >= beforeID {
			return Page{}, fmt.Errorf("%w: history violates exclusive offset", ErrContract)
		}
		page.Messages = append(page.Messages, msg)
	}
	slices.SortFunc(page.Messages, func(a, b Message) int {
		if a.ID > b.ID {
			return -1
		}
		if a.ID < b.ID {
			return 1
		}
		return 0
	})
	for i := 1; i < len(page.Messages); i++ {
		if page.Messages[i-1].ID == page.Messages[i].ID {
			return Page{}, fmt.Errorf("%w: duplicate message ID", ErrContract)
		}
	}
	if page.HasMore && (len(page.Messages) == 0 || page.NextBeforeID != page.Messages[len(page.Messages)-1].ID || (beforeID != 0 && page.NextBeforeID >= beforeID)) {
		return Page{}, fmt.Errorf("%w: history cursor does not advance", ErrContract)
	}
	return page, nil
}

func (c *MCPClient) Files(ctx context.Context, chatID int64, messageIDs []int64) ([]Media, error) {
	if !validID(chatID) || len(messageIDs) == 0 || len(messageIDs) > 20 {
		return nil, errors.New("Inline files require a chat and 1-20 message IDs")
	}
	ids := make([]string, 0, len(messageIDs))
	wanted := make(map[int64]bool, len(messageIDs))
	for _, id := range messageIDs {
		if !validID(id) || wanted[id] {
			return nil, errors.New("Inline files require distinct positive message IDs")
		}
		wanted[id] = true
		ids = append(ids, strconv.FormatInt(id, 10))
	}
	raw, err := c.call(ctx, "files.get", map[string]any{"chatId": strconv.FormatInt(chatID, 10), "messageIds": ids, "includeUrlPreviews": false})
	if err != nil {
		return nil, err
	}
	var v struct {
		Chat  mcpChat `json:"chat"`
		Items []struct {
			Message stdjson.RawMessage `json:"message"`
			Files   []struct {
				mcpMedia

				Source    string `json:"source"`
				MessageID string `json:"messageId"`
			} `json:"files"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &v); err != nil || v.Items == nil {
		return nil, fmt.Errorf("%w: malformed file result", ErrContract)
	}
	id, err := positiveID(v.Chat.ChatID)
	if err != nil || id != chatID {
		return nil, fmt.Errorf("%w: file chat ID mismatch", ErrContract)
	}
	var out []Media
	seen := make(map[int64]bool)
	for _, item := range v.Items {
		msg, err := decodeMCPMessage(item.Message, chatID)
		if err != nil {
			return nil, err
		}
		if !wanted[msg.ID] || seen[msg.ID] {
			return nil, fmt.Errorf("%w: unexpected file message ID", ErrContract)
		}
		seen[msg.ID] = true
		if len(item.Files) > 1 {
			return nil, fmt.Errorf("%w: unexpected direct file count", ErrContract)
		}
		for _, file := range item.Files {
			fid, err := positiveID(file.MessageID)
			if err != nil || fid != msg.ID || file.Source != "message_media" {
				return nil, fmt.Errorf("%w: unexpected file occurrence", ErrContract)
			}
			media, err := decodeMCPMedia(file.mcpMedia, chatID, msg.ID)
			if err != nil {
				return nil, err
			}
			if media != nil {
				if len(msg.Media) != 1 || media.ID != msg.Media[0].ID {
					return nil, fmt.Errorf("%w: file differs from its message media", ErrContract)
				}
				out = append(out, *media)
			}
		}
	}
	return out, nil
}

func decodeMCPMedia(v mcpMedia, chatID, messageID int64) (*Media, error) {
	if v.Kind == "nudge" {
		return nil, nil //nolint:nilnil // A valid nudge has no downloadable media to archive.
	}
	if v.Kind != "photo" && v.Kind != "video" && v.Kind != "document" && v.Kind != "voice" {
		return nil, fmt.Errorf("%w: unsupported direct media kind", ErrContract)
	}
	m := &Media{ID: v.Kind, ChatID: chatID, MessageID: messageID, Role: v.Kind}
	if v.ID != nil {
		if _, err := positiveID(*v.ID); err != nil {
			return nil, err
		}
		m.ID += ":" + *v.ID
	}
	if v.URL != nil {
		m.URL = *v.URL
	}
	if v.FileName != nil {
		m.Filename = *v.FileName
	}
	if v.MIMEType != nil {
		m.MIMEType = *v.MIMEType
	}
	if v.SizeBytes != nil {
		m.Size = *v.SizeBytes
	}
	if v.Width != nil {
		m.Width = *v.Width
	}
	if v.Height != nil {
		m.Height = *v.Height
	}
	if m.Size < 0 || m.Width < 0 || m.Height < 0 {
		return nil, fmt.Errorf("%w: negative media dimensions", ErrContract)
	}
	if v.Duration != nil {
		if math.IsNaN(*v.Duration) || math.IsInf(*v.Duration, 0) || *v.Duration < 0 || *v.Duration > float64(math.MaxInt/1000) {
			return nil, fmt.Errorf("%w: invalid media duration", ErrContract)
		}
		m.DurationMS = int(*v.Duration * 1000)
	}
	return m, nil
}

func positiveID(raw string) (int64, error) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || !validID(id) || strconv.FormatInt(id, 10) != raw {
		return 0, fmt.Errorf("%w: invalid decimal ID", ErrContract)
	}
	return id, nil
}

func optionalID(raw *string) (int64, error) {
	if raw == nil || *raw == "0" {
		return 0, nil
	}
	return positiveID(*raw)
}
func messageTime(raw *string) (time.Time, error) {
	if raw == nil {
		return time.Time{}, nil
	}
	seconds, err := strconv.ParseInt(*raw, 10, 64)
	if err != nil || seconds < 0 || seconds > 253402300799 {
		return time.Time{}, fmt.Errorf("%w: invalid message timestamp", ErrContract)
	}
	return time.Unix(seconds, 0).UTC(), nil
}
