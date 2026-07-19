package slack

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// apiResponse is the envelope every Slack Web API method returns.
type apiResponse struct {
	OK       bool   `json:"ok"`
	Error    string `json:"error"`
	Needed   string `json:"needed"`   // set on missing_scope errors
	Provided string `json:"provided"` // set on missing_scope errors
	Metadata struct {
		NextCursor string `json:"next_cursor"`
	} `json:"response_metadata"`
}

// AuthTestResult identifies the token's workspace and user.
type AuthTestResult struct {
	URL    string `json:"url"`
	Team   string `json:"team"`
	User   string `json:"user"`
	TeamID string `json:"team_id"`
	UserID string `json:"user_id"`
}

// Conversation is one channel/group DM/DM from conversations.list or
// users.conversations.
type Conversation struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	IsChannel  bool   `json:"is_channel"`
	IsGroup    bool   `json:"is_group"`
	IsIM       bool   `json:"is_im"`
	IsMpim     bool   `json:"is_mpim"`
	IsPrivate  bool   `json:"is_private"`
	IsArchived bool   `json:"is_archived"`
	IsMember   bool   `json:"is_member"`
	User       string `json:"user"` // IM peer user ID (im only)
	NumMembers int    `json:"num_members"`
}

// User is one workspace member from users.list.
type User struct {
	ID       string `json:"id"`
	TeamID   string `json:"team_id"`
	Name     string `json:"name"` // legacy handle
	Deleted  bool   `json:"deleted"`
	IsBot    bool   `json:"is_bot"`
	Profile  UserProfile
	RealName string `json:"real_name"`
}

// UserProfile carries the displayable identity fields of a User.
type UserProfile struct {
	Email       string `json:"email"` // requires users:read.email; empty otherwise
	RealName    string `json:"real_name"`
	DisplayName string `json:"display_name"`
	BotID       string `json:"bot_id"`
}

// UnmarshalJSON flattens the nested profile object into User.
func (u *User) UnmarshalJSON(b []byte) error {
	type alias User // avoid recursion into this method
	var a struct {
		alias

		Profile UserProfile `json:"profile"`
	}
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*u = User(a.alias)
	u.Profile = a.Profile
	return nil
}

// DisplayName returns the best human-readable name for the user.
func (u *User) DisplayName() string {
	for _, s := range []string{u.Profile.DisplayName, u.Profile.RealName, u.RealName, u.Name} {
		if s != "" {
			return s
		}
	}
	return u.ID
}

// Reaction is one emoji reaction aggregate on a message.
type Reaction struct {
	Name  string   `json:"name"`  // emoji name, e.g. "thumbsup"
	Users []string `json:"users"` // reacting user IDs (may be truncated by the API)
	Count int      `json:"count"`
}

// File is a file shared into a conversation. URLPrivate is only fetched when
// its host is files.slack.com (see media.go); everything else is recorded as
// metadata.
type File struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	Mimetype           string `json:"mimetype"`
	Size               int64  `json:"size"`
	URLPrivate         string `json:"url_private"`
	URLPrivateDownload string `json:"url_private_download"`
	Permalink          string `json:"permalink"`
	Mode               string `json:"mode"` // "tombstone" when deleted, "external", …
	IsExternal         bool   `json:"is_external"`
}

// Edited marks a message as edited.
type Edited struct {
	User string `json:"user"`
	TS   string `json:"ts"`
}

// LegacyAttachment is Slack's pre-Block-Kit rich payload ("secondary
// attachment"). Bots and integrations often carry their entire content here
// with an empty message text; user messages get them as link unfurls.
type LegacyAttachment struct {
	Fallback string `json:"fallback"` // required plain-text summary
	Pretext  string `json:"pretext"`
	Title    string `json:"title"`
	Text     string `json:"text"`
	Fields   []struct {
		Title string `json:"title"`
		Value string `json:"value"`
	} `json:"fields"`
	Footer string `json:"footer"`
}

// Block is a Block Kit layout block, modelled only deeply enough to extract
// searchable text: section/header text, section fields, and context
// elements. rich_text blocks are deliberately not extracted — they duplicate
// the message's own text field.
type Block struct {
	Type     string         `json:"type"`
	Text     *BlockText     `json:"text"`
	Fields   []BlockText    `json:"fields"`
	Elements []BlockElement `json:"elements"`
}

// BlockText is a Block Kit text object (plain_text or mrkdwn).
type BlockText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// BlockElement is one entry of a block's elements array. Its text field is
// shape-shifting across block kinds (observed live): a plain string on
// context mrkdwn/plain_text elements, a nested text object on actions-block
// buttons, and absent on rich_text/image elements. All shapes must decode —
// a strict model here made conversations.history undecodable.
type BlockElement struct {
	Type string
	Text string
}

// UnmarshalJSON decodes a BlockElement tolerantly (see type comment).
func (e *BlockElement) UnmarshalJSON(b []byte) error {
	var probe struct {
		Type string          `json:"type"`
		Text json.RawMessage `json:"text"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return err
	}
	e.Type = probe.Type
	if len(probe.Text) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(probe.Text, &s) == nil {
		e.Text = s
		return nil
	}
	var bt BlockText
	if json.Unmarshal(probe.Text, &bt) == nil {
		e.Text = bt.Text
	}
	return nil
}

// Message is one message from conversations.history / conversations.replies.
type Message struct {
	Type        string     `json:"type"`    // "message"
	Subtype     string     `json:"subtype"` // "", "bot_message", "channel_join", "thread_broadcast", …
	TS          string     `json:"ts"`      // message identity within its channel
	ThreadTS    string     `json:"thread_ts"`
	User        string     `json:"user"`
	BotID       string     `json:"bot_id"`
	Username    string     `json:"username"` // bot display name
	Text        string     `json:"text"`
	Edited      *Edited    `json:"edited"`
	ReplyCount  int        `json:"reply_count"`
	LatestReply string     `json:"latest_reply"`
	Reactions   []Reaction `json:"reactions"`
	Files       []File     `json:"files"`
	// Attachments are legacy rich payloads; Blocks are Block Kit layout
	// blocks. Both feed FTS via payloadText (see mapping.go).
	Attachments []LegacyAttachment `json:"attachments"`
	Blocks      []Block            `json:"blocks"`
	// Raw holds the exact original JSON for this message, captured during
	// decode (see UnmarshalJSON) and archived verbatim so no API field is
	// lost to our partial struct modelling.
	Raw json.RawMessage `json:"-"`
}

// UnmarshalJSON decodes a Message while retaining the original bytes in Raw.
func (m *Message) UnmarshalJSON(b []byte) error {
	type alias Message // avoid recursion into this method
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*m = Message(a)
	m.Raw = json.RawMessage(strings.Clone(string(b)))
	return nil
}

// IsThreadRoot reports whether m is the root of a thread with replies.
func (m *Message) IsThreadRoot() bool {
	return m.ThreadTS != "" && m.ThreadTS == m.TS && m.ReplyCount > 0
}

// IsThreadReply reports whether m is a reply inside a thread (not the root).
// Broadcast replies ("thread_broadcast") also satisfy this.
func (m *Message) IsThreadReply() bool {
	return m.ThreadTS != "" && m.ThreadTS != m.TS
}

// Time converts a Slack ts string ("1734567890.123456") to UTC time.
// The zero time is returned for unparseable input.
func tsTime(ts string) time.Time {
	if ts == "" {
		return time.Time{}
	}
	sec, frac, _ := strings.Cut(ts, ".")
	s, err := strconv.ParseInt(sec, 10, 64)
	if err != nil {
		return time.Time{}
	}
	var ns int64
	if frac != "" {
		// Fractional part is microseconds, zero-padded to 6 digits.
		padded := (frac + "000000")[:6]
		if us, perr := strconv.ParseInt(padded, 10, 64); perr == nil {
			ns = us * int64(time.Microsecond)
		}
	}
	return time.Unix(s, ns).UTC()
}

// tsLess compares two Slack ts strings numerically (seconds, then fraction).
// String comparison is not safe: second counts can differ in digit length.
func tsLess(a, b string) bool {
	at, bt := tsTime(a), tsTime(b)
	if at.Equal(bt) {
		return a < b // fall back to lexical for stability
	}
	return at.Before(bt)
}

// ImportOptions configures one Import run.
type ImportOptions struct {
	// TeamID is the workspace to sync; with UserID it forms the msgvault
	// source identifier ("<team_id>:<user_id>").
	TeamID string
	// UserID is the archiving user (from auth.test at add-slack time).
	UserID string
	// Limit caps messages processed per conversation this run (0 = no
	// limit). A limited backfill leaves the conversation resumable.
	Limit int
	// Full ignores stored cursors: every conversation is re-walked and every
	// message re-upserted in place (repair path).
	Full bool
	// NoThreads skips per-root reply fetching (scoped first runs).
	NoThreads bool
	// AttachmentsDir is the content-addressed attachment store root. Empty
	// disables media download (as does NoMedia).
	AttachmentsDir string
	// NoMedia skips file downloads entirely.
	NoMedia bool
	// MaxMediaBytes caps individual file downloads (0 = 100 MB).
	MaxMediaBytes int64
	// ThreadLookback bounds how long a thread root stays tracked for new
	// replies (0 = 30 days). Replies to roots older than this are only
	// caught by --full runs (documented limitation).
	ThreadLookback time.Duration
	// IncludeChannels/ExcludeChannels filter by channel name (no "#").
	// Include empty = all memberships. DMs/group DMs are never filtered.
	IncludeChannels []string
	ExcludeChannels []string
	// Progress, if non-nil, is called after each conversation with a
	// human-readable status line.
	Progress func(msg string) `json:"-"`
}

// ImportSummary reports the outcome of one Import run.
type ImportSummary struct {
	SourceID               int64
	ConversationsProcessed int
	MessagesProcessed      int
	MessagesAdded          int
	RepliesFetched         int
	AttachmentsDownloaded  int
	AttachmentsPending     int
	FetchErrors            int
	Errors                 int
	Duration               time.Duration
}
