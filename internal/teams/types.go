package teams

import (
	"context"
	"encoding/json"
	"time"
)

// ---- Graph response envelopes ----

type listResponse[T any] struct {
	Value     []T    `json:"value"`
	NextLink  string `json:"@odata.nextLink"`
	DeltaLink string `json:"@odata.deltaLink"`
}

// ---- Chats & channels ----

type Chat struct {
	ID         string `json:"id"`
	ChatType   string `json:"chatType"` // oneOnOne | group | meeting
	Topic      string `json:"topic"`
	OnlineInfo *struct {
		JoinWebURL string `json:"joinWebUrl"`
	} `json:"onlineMeetingInfo"`
}

type JoinedTeam struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
}

type Channel struct {
	ID             string `json:"id"`
	DisplayName    string `json:"displayName"`
	MembershipType string `json:"membershipType"` // standard | private | shared
}

// ---- Messages ----

type ChatMessage struct {
	ID                   string       `json:"id"`
	ReplyToID            string       `json:"replyToId"`
	MessageType          string       `json:"messageType"`
	CreatedDateTime      time.Time    `json:"createdDateTime"`
	LastModifiedDateTime time.Time    `json:"lastModifiedDateTime"`
	DeletedDateTime      *time.Time   `json:"deletedDateTime"`
	Subject              string       `json:"subject"`
	Importance           string       `json:"importance"`
	From                 *IdentitySet `json:"from"`
	Body                 MessageBody  `json:"body"`
	Attachments          []Attachment `json:"attachments"`
	Mentions             []Mention    `json:"mentions"`
	Reactions            []Reaction   `json:"reactions"`
	// EventDetail is the polymorphic eventDetail payload (default-returned by
	// Graph on systemEventMessage items). Kept as RawMessage so the typed lens
	// below can parse the call-recording fields without modelling every subtype.
	EventDetail json.RawMessage `json:"eventDetail,omitempty"`
	// Raw holds the exact original JSON for this message, captured during decode
	// (see UnmarshalJSON). It is archived verbatim so no Graph field is lost to
	// our partial struct modelling.
	Raw json.RawMessage `json:"-"`
}

// UnmarshalJSON decodes a ChatMessage while retaining the exact original bytes
// in Raw, so the archived raw blob is lossless even for fields we do not model.
func (m *ChatMessage) UnmarshalJSON(b []byte) error {
	type alias ChatMessage // avoid recursion into this method
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*m = ChatMessage(a)
	m.Raw = append(json.RawMessage(nil), b...)
	return nil
}

// eventMessageDetail is the polymorphic eventDetail payload. Only the
// call-recording fields we surface are typed; the full JSON is preserved
// via ChatMessage.EventDetail.
type eventMessageDetail struct {
	ODataType                string `json:"@odata.type"`
	CallRecordingURL         string `json:"callRecordingUrl"`
	CallRecordingDisplayName string `json:"callRecordingDisplayName"`
}

// callRecording returns the recording URL and display name when this message's
// eventDetail is a callRecordingEventMessageDetail. ok is false otherwise.
func (m *ChatMessage) callRecording() (url, name string, ok bool) {
	if len(m.EventDetail) == 0 {
		return "", "", false
	}
	var d eventMessageDetail
	if err := json.Unmarshal(m.EventDetail, &d); err != nil {
		return "", "", false
	}
	if d.CallRecordingURL == "" {
		return "", "", false
	}
	return d.CallRecordingURL, d.CallRecordingDisplayName, true
}

type MessageBody struct {
	ContentType string `json:"contentType"` // html | text
	Content     string `json:"content"`
}

type IdentitySet struct {
	User        *Identity `json:"user"`
	Application *Identity `json:"application"`
}

type Identity struct {
	ID               string `json:"id"`
	DisplayName      string `json:"displayName"`
	UserIdentityType string `json:"userIdentityType"` // aadUser | emailUser | anonymousGuest | skypeUser | ...
}

type Attachment struct {
	ID          string `json:"id"`
	ContentType string `json:"contentType"` // "reference" => shared file link
	ContentURL  string `json:"contentUrl"`
	Name        string `json:"name"`
}

type Mention struct {
	ID          int          `json:"id"`
	MentionText string       `json:"mentionText"`
	Mentioned   *IdentitySet `json:"mentioned"`
}

type Reaction struct {
	ReactionType    string       `json:"reactionType"` // like | heart | laugh | ...
	CreatedDateTime time.Time    `json:"createdDateTime"`
	User            *IdentitySet `json:"user"`
}

// GraphUser is the subset of /users/{id} we resolve for participant email.
type GraphUser struct {
	ID                string `json:"id"`
	Mail              string `json:"mail"`
	UserPrincipalName string `json:"userPrincipalName"`
	DisplayName       string `json:"displayName"`
}

// ---- Importer options/summary ----

type ImportOptions struct {
	Email           string
	AttachmentsDir  string
	IncludeChannels bool      // default true; allows chats-only runs
	Limit           int       // 0 = no limit (per-conversation message cap, for scoped runs)
	After           time.Time // zero = no lower bound
	// Full forces a complete backfill: the prior sync cursor and any interrupted
	// checkpoint are ignored, so every chat/channel message is re-fetched and
	// re-persisted (upsert). Use to repair messages after an importer change.
	Full bool
	// OnlyIncomplete (BackfillInlineMedia only) restricts the run to messages
	// whose inline media was not fully downloaded, so transient fetch failures
	// can be retried without re-fetching everything.
	OnlyIncomplete bool
	// Progress, if non-nil, is called after each conversation with a human-readable
	// status line. Safe to leave nil (silent mode).
	Progress func(msg string) `json:"-"`
	// EmbedEnqueuer, if non-nil, queues persisted message IDs for vector embedding.
	// Enqueue failures are counted in the summary but do not abort the import.
	EmbedEnqueuer EmbedEnqueuer `json:"-"`
}

type EmbedEnqueuer interface {
	EnqueueMessages(ctx context.Context, messageIDs []int64) error
}

type ImportSummary struct {
	Duration           time.Duration
	SourceID           int64
	ChatsProcessed     int64
	ChannelsProcessed  int64
	MessagesProcessed  int64
	MessagesAdded      int64
	MessagesUpdated    int64
	ReactionsAdded     int64
	AttachmentsFound   int64
	InlineImagesCopied int64
	Participants       int64
	Errors             int64
}

// ChatMember is a member of a chat (direct or group), returned by /chats/{id}/members.
type ChatMember struct {
	ID          string `json:"id"`
	UserID      string `json:"userId"`
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
}
