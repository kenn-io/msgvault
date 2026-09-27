package teams

import (
	"context"

	"go.kenn.io/msgvault/internal/msgraph"
)

// ErrMediaTooLarge classifies hosted media that exceeds its configured cap.
var ErrMediaTooLarge = msgraph.ErrTooLarge

var errGraphNotFound = msgraph.ErrNotFound

// TokenFunc returns a bearer token for a Graph API request.
type TokenFunc = msgraph.TokenFunc

// Client adds the Teams endpoints to the shared Graph transport.
type Client struct {
	*msgraph.Client
}

// NewClient creates a Client. baseURL is injected so tests can point at
// httptest servers. qps controls the token-bucket rate limit (default 5).
func NewClient(baseURL string, token TokenFunc, qps float64) *Client {
	return &Client{msgraph.NewClient(baseURL, token, qps)}
}

func (c *Client) getJSON(ctx context.Context, url string, out any) error {
	return c.GetJSON(ctx, url, out)
}

func pageThrough[T any](ctx context.Context, c *Client, startURL string, fn func([]T)) (string, error) {
	return msgraph.PageThrough(ctx, c.Client, startURL, fn)
}

func pageThroughLimit[T any](ctx context.Context, c *Client, startURL string, limit int, fn func([]T)) (string, bool, error) {
	return msgraph.PageThroughLimit(ctx, c.Client, startURL, limit, fn)
}

// SelfChatID is the Teams chat a user holds with themselves. Graph never
// returns it from /me/chats, and a metadata read of /chats/48:notes fails with
// "Call made for a thread which is not a ChatThread". Its messages endpoint
// answers normally, including the incremental lastModifiedDateTime filter, so
// once the chat is in the list it syncs through the same path as every other
// chat.
const SelfChatID = "48:notes"

func (c *Client) ListChats(ctx context.Context) ([]Chat, error) {
	var out []Chat
	_, err := pageThrough[Chat](ctx, c, "/me/chats?$top=50", func(p []Chat) { out = append(out, p...) })
	return out, err
}

func (c *Client) ListJoinedTeams(ctx context.Context) ([]JoinedTeam, error) {
	var out []JoinedTeam
	_, err := pageThrough[JoinedTeam](ctx, c, "/me/joinedTeams", func(p []JoinedTeam) { out = append(out, p...) })
	return out, err
}

func (c *Client) ListChannels(ctx context.Context, teamID string) ([]Channel, error) {
	var out []Channel
	_, err := pageThrough[Channel](ctx, c, "/teams/"+teamID+"/channels", func(p []Channel) { out = append(out, p...) })
	return out, err
}

// ListChatMessages: sinceISO empty = full backfill; non-empty = incremental.
func (c *Client) ListChatMessages(ctx context.Context, chatID, sinceISO string, limit int) ([]ChatMessage, bool, error) {
	url := "/me/chats/" + chatID + "/messages?$top=50"
	if sinceISO != "" {
		// Graph rejects "ge" on lastModifiedDateTime for /chats/{id}/messages
		// with BadRequest ("operationKind 'GreaterThanOrEqual' is not allowed
		// in $filter"); only gt/lt are accepted. The cursor is the newest
		// lastModifiedDateTime already ingested, so gt is also the correct
		// boundary.
		url += "&$filter=lastModifiedDateTime%20gt%20" + sinceISO + "&$orderby=lastModifiedDateTime%20desc"
	}
	var out []ChatMessage
	_, truncated, err := pageThroughLimit[ChatMessage](ctx, c, url, limit, func(p []ChatMessage) { out = append(out, p...) })
	return out, truncated, err
}

// ChannelMessagesDelta drives the delta endpoint (or a stored deltaLink) to
// completion, returning all messages and the new deltaLink.
func (c *Client) ChannelMessagesDelta(ctx context.Context, teamID, channelID, deltaLink string, limit int) ([]ChatMessage, string, bool, error) {
	start := deltaLink
	if start == "" {
		start = "/teams/" + teamID + "/channels/" + channelID + "/messages/delta"
	}
	var out []ChatMessage
	newDelta, truncated, err := pageThroughLimit[ChatMessage](ctx, c, start, limit, func(p []ChatMessage) { out = append(out, p...) })
	return out, newDelta, truncated, err
}

func (c *Client) ListChannelMessages(ctx context.Context, teamID, channelID string, limit int) ([]ChatMessage, bool, error) {
	var out []ChatMessage
	_, truncated, err := pageThroughLimit[ChatMessage](ctx, c, "/teams/"+teamID+"/channels/"+channelID+"/messages?$top=50", limit, func(p []ChatMessage) { out = append(out, p...) })
	return out, truncated, err
}

func (c *Client) ListReplies(ctx context.Context, teamID, channelID, messageID string, limit int) ([]ChatMessage, bool, error) {
	var out []ChatMessage
	_, truncated, err := pageThroughLimit[ChatMessage](ctx, c, "/teams/"+teamID+"/channels/"+channelID+"/messages/"+messageID+"/replies", limit, func(p []ChatMessage) { out = append(out, p...) })
	return out, truncated, err
}

// GetUser fetches a user by object ID from the Graph /users endpoint,
// selecting the fields needed for participant email resolution.
func (c *Client) GetUser(ctx context.Context, id string) (*GraphUser, error) {
	var u GraphUser
	if err := c.getJSON(ctx, "/users/"+id+"?$select=id,mail,userPrincipalName,displayName", &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// ListChatMembers returns all members of the given chat.
func (c *Client) ListChatMembers(ctx context.Context, chatID string) ([]ChatMember, error) {
	var out []ChatMember
	_, err := pageThrough[ChatMember](ctx, c, "/chats/"+chatID+"/members", func(p []ChatMember) { out = append(out, p...) })
	return out, err
}

// ListTeamMembers returns all members of the given team. Standard channels
// inherit the team roster, which is the membership media policy evaluates them
// against.
func (c *Client) ListTeamMembers(ctx context.Context, teamID string) ([]ChatMember, error) {
	var out []ChatMember
	_, err := pageThrough[ChatMember](ctx, c, "/teams/"+teamID+"/members", func(p []ChatMember) { out = append(out, p...) })
	return out, err
}

// ListChannelMembers returns all members of the given channel. Private and
// shared channels are governed by this roster rather than the team's.
func (c *Client) ListChannelMembers(ctx context.Context, teamID, channelID string) ([]ChatMember, error) {
	var out []ChatMember
	_, err := pageThrough[ChatMember](ctx, c, "/teams/"+teamID+"/channels/"+channelID+"/members",
		func(p []ChatMember) { out = append(out, p...) })
	return out, err
}
