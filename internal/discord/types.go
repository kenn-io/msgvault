package discord

import (
	"context"
	"encoding/json"
	"time"
)

// API is the read-only Discord surface consumed by the catalog and importer.
type API interface {
	Me(ctx context.Context) (User, error)
	Guilds(ctx context.Context) ([]Guild, error)
	Guild(ctx context.Context, guildID string) (Guild, error)
	GuildChannels(ctx context.Context, guildID string) ([]Channel, error)
	ActiveThreads(ctx context.Context, guildID string) ([]Channel, error)
	ArchivedThreads(ctx context.Context, channelID string, private bool, before time.Time) (ThreadPage, error)
	Messages(ctx context.Context, channelID string, query MessageQuery) ([]Message, error)
	Message(ctx context.Context, channelID, messageID string) (Message, error)
}

// User contains the identity fields used for bot validation and participant
// enrichment.
type User struct {
	ID            string  `json:"id"`
	Username      string  `json:"username"`
	Discriminator string  `json:"discriminator"`
	GlobalName    string  `json:"global_name"`
	Avatar        string  `json:"avatar"`
	Bot           bool    `json:"bot"`
	System        bool    `json:"system"`
	Banner        *string `json:"banner"`
	AccentColor   *int    `json:"accent_color"`
}

// Guild contains the metadata needed to bind and display a Discord source.
type Guild struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Icon        string   `json:"icon"`
	OwnerID     string   `json:"owner_id"`
	Description string   `json:"description"`
	Features    []string `json:"features"`
	Unavailable bool     `json:"unavailable"`
}

// Channel contains catalog and conversation metadata for guild channels and
// threads. Discord categories are represented here for catalog context but are
// not made message containers by the importer.
type Channel struct {
	ID                   string           `json:"id"`
	Type                 int              `json:"type"`
	GuildID              string           `json:"guild_id"`
	Position             int              `json:"position"`
	Name                 string           `json:"name"`
	Topic                string           `json:"topic"`
	NSFW                 bool             `json:"nsfw"`
	LastMessageID        string           `json:"last_message_id"`
	ParentID             string           `json:"parent_id"`
	OwnerID              string           `json:"owner_id"`
	RateLimitPerUser     int              `json:"rate_limit_per_user"`
	MessageCount         int              `json:"message_count"`
	MemberCount          int              `json:"member_count"`
	Flags                int              `json:"flags"`
	AppliedTags          []string         `json:"applied_tags"`
	AvailableTags        []ForumTag       `json:"available_tags"`
	DefaultReactionEmoji *DefaultReaction `json:"default_reaction_emoji"`
	ThreadMetadata       *ThreadMetadata  `json:"thread_metadata"`
}

type ForumTag struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Moderated bool   `json:"moderated"`
	EmojiID   string `json:"emoji_id"`
	EmojiName string `json:"emoji_name"`
}

type DefaultReaction struct {
	EmojiID   string `json:"emoji_id"`
	EmojiName string `json:"emoji_name"`
}

type ThreadMetadata struct {
	Archived            bool       `json:"archived"`
	AutoArchiveDuration int        `json:"auto_archive_duration"`
	ArchiveTimestamp    time.Time  `json:"archive_timestamp"`
	Locked              bool       `json:"locked"`
	Invitable           bool       `json:"invitable"`
	CreateTimestamp     *time.Time `json:"create_timestamp"`
}

// ThreadPage is one public or private archived-thread response. NextBefore is
// the archive timestamp to pass to the next call when HasMore is true.
type ThreadPage struct {
	Threads    []Channel
	HasMore    bool
	NextBefore time.Time
}

// GuildMember contains member data attached to an observed message payload.
// Version 1 does not enumerate the guild roster.
type GuildMember struct {
	User                       User       `json:"user"`
	Nick                       string     `json:"nick"`
	Avatar                     string     `json:"avatar"`
	Roles                      []string   `json:"roles"`
	JoinedAt                   *time.Time `json:"joined_at"`
	Pending                    bool       `json:"pending"`
	CommunicationDisabledUntil *time.Time `json:"communication_disabled_until"`
}

// MessageQuery controls Discord's mutually exclusive message cursors. Limit
// may be zero to use Discord's default, otherwise it must be from 1 through
// 100.
type MessageQuery struct {
	Around string
	Before string
	After  string
	Limit  int
}

type Message struct {
	ID                string            `json:"id"`
	ChannelID         string            `json:"channel_id"`
	GuildID           string            `json:"guild_id"`
	Author            User              `json:"author"`
	Member            *GuildMember      `json:"member"`
	Content           string            `json:"content"`
	Timestamp         time.Time         `json:"timestamp"`
	EditedTimestamp   *time.Time        `json:"edited_timestamp"`
	TTS               bool              `json:"tts"`
	MentionEveryone   bool              `json:"mention_everyone"`
	Mentions          []User            `json:"mentions"`
	MentionRoles      []string          `json:"mention_roles"`
	MentionChannels   []ChannelMention  `json:"mention_channels"`
	Attachments       []Attachment      `json:"attachments"`
	Embeds            []Embed           `json:"embeds"`
	Reactions         []Reaction        `json:"reactions"`
	Pinned            bool              `json:"pinned"`
	WebhookID         string            `json:"webhook_id"`
	Type              int               `json:"type"`
	Activity          json.RawMessage   `json:"activity"`
	Application       json.RawMessage   `json:"application"`
	ApplicationID     string            `json:"application_id"`
	MessageReference  *MessageReference `json:"message_reference"`
	Flags             int               `json:"flags"`
	ReferencedMessage *Message          `json:"referenced_message"`
	Interaction       json.RawMessage   `json:"interaction"`
	Thread            *Channel          `json:"thread"`
	Components        json.RawMessage   `json:"components"`
	StickerItems      []StickerItem     `json:"sticker_items"`
	Poll              *Poll             `json:"poll"`
	Raw               json.RawMessage   `json:"-"`
}

// UnmarshalJSON retains the complete API object while decoding the fields the
// importer reads. This keeps future Discord fields archival without turning
// the REST client into a general SDK.
func (m *Message) UnmarshalJSON(data []byte) error {
	type messageAlias Message
	var decoded messageAlias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*m = Message(decoded)
	m.Raw = append(json.RawMessage(nil), data...)
	return nil
}

type ChannelMention struct {
	ID      string `json:"id"`
	GuildID string `json:"guild_id"`
	Type    int    `json:"type"`
	Name    string `json:"name"`
}

type Attachment struct {
	ID          string  `json:"id"`
	Filename    string  `json:"filename"`
	Title       string  `json:"title"`
	Description string  `json:"description"`
	ContentType string  `json:"content_type"`
	Size        int64   `json:"size"`
	URL         string  `json:"url"`
	ProxyURL    string  `json:"proxy_url"`
	Height      *int    `json:"height"`
	Width       *int    `json:"width"`
	Ephemeral   bool    `json:"ephemeral"`
	Duration    float64 `json:"duration_secs"`
	Waveform    string  `json:"waveform"`
	Flags       int     `json:"flags"`
}

type Embed struct {
	Title       string         `json:"title"`
	Type        string         `json:"type"`
	Description string         `json:"description"`
	URL         string         `json:"url"`
	Timestamp   *time.Time     `json:"timestamp"`
	Color       int            `json:"color"`
	Footer      *EmbedFooter   `json:"footer"`
	Image       *EmbedMedia    `json:"image"`
	Thumbnail   *EmbedMedia    `json:"thumbnail"`
	Video       *EmbedMedia    `json:"video"`
	Provider    *EmbedProvider `json:"provider"`
	Author      *EmbedAuthor   `json:"author"`
	Fields      []EmbedField   `json:"fields"`
}

type EmbedFooter struct {
	Text    string `json:"text"`
	IconURL string `json:"icon_url"`
}

type EmbedMedia struct {
	URL      string `json:"url"`
	ProxyURL string `json:"proxy_url"`
	Height   int    `json:"height"`
	Width    int    `json:"width"`
}

type EmbedProvider struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

type EmbedAuthor struct {
	Name    string `json:"name"`
	URL     string `json:"url"`
	IconURL string `json:"icon_url"`
}

type EmbedField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline"`
}

type Reaction struct {
	Count        int `json:"count"`
	CountDetails *struct {
		Burst  int `json:"burst"`
		Normal int `json:"normal"`
	} `json:"count_details"`
	Me         bool     `json:"me"`
	MeBurst    bool     `json:"me_burst"`
	Emoji      Emoji    `json:"emoji"`
	BurstColor []string `json:"burst_colors"`
}

type Emoji struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Animated bool   `json:"animated"`
}

type MessageReference struct {
	MessageID       string `json:"message_id"`
	ChannelID       string `json:"channel_id"`
	GuildID         string `json:"guild_id"`
	FailIfNotExists bool   `json:"fail_if_not_exists"`
}

type StickerItem struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Format int    `json:"format_type"`
}

type Poll struct {
	Question         PollMedia    `json:"question"`
	Answers          []PollAnswer `json:"answers"`
	Expiry           *time.Time   `json:"expiry"`
	AllowMultiselect bool         `json:"allow_multiselect"`
	LayoutType       int          `json:"layout_type"`
	Results          *PollResults `json:"results"`
}

type PollMedia struct {
	Text  string `json:"text"`
	Emoji *Emoji `json:"emoji"`
}

type PollAnswer struct {
	AnswerID  int       `json:"answer_id"`
	PollMedia PollMedia `json:"poll_media"`
}

type PollResults struct {
	Finalized    bool              `json:"is_finalized"`
	AnswerCounts []PollAnswerCount `json:"answer_counts"`
}

type PollAnswerCount struct {
	ID      int  `json:"id"`
	Count   int  `json:"count"`
	MeVoted bool `json:"me_voted"`
}
