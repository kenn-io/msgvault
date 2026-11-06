// Package signalandroid imports messages from a decrypted Signal Android
// backup database into the msgvault unified schema.
//
// The input is database.sqlite with DatabaseVersion.sbf beside it, and optionally
// the Attachment_*.bin files that signalbackup-tools writes when it unpacks a Signal
// backup into a directory. The importer reads Signal Android database
// versions 185 through 330.
package signalandroid

import (
	"database/sql"
	"time"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textimport"
)

// Identifier formats written by the importer. They depend only on Signal's
// own identities, never on local row IDs, so a re-import of a newer backup,
// a restored backup or a backup from a new phone updates the same rows.
const (
	// SourceType is the sources.source_type of a Signal import.
	SourceType = "signal"
	// MessageType is the messages.message_type of an imported message.
	MessageType = "signal"
	// ACIIdentifierType is the participant identifier type that records a
	// Signal account's ACI (account identity UUID).
	ACIIdentifierType = "signal_aci"
	// rawFormat names the JSON stored in message_raw.
	rawFormat = "signal_android_json"

	directACIPrefix   = "signal:aci:"
	directPNIPrefix   = "signal:pni:"
	directPhonePrefix = "signal:tel:"
	groupPrefix       = "signal:group:"
)

// ImportOptions configures a Signal import.
type ImportOptions struct {
	// Phone is the account owner's phone number in E.164 format. It
	// identifies the source and the owner's participant.
	Phone string

	// DisplayName is an optional display name for the owner.
	DisplayName string

	// AttachmentFilesDir holds the Attachment_<id>_<unique>.bin files
	// written next to database.sqlite. Empty means no attachment files.
	AttachmentFilesDir string

	// AttachmentsDir is msgvault's content-addressed attachment store.
	AttachmentsDir string

	// MaxAttachmentSize skips attachment files larger than this many bytes.
	MaxAttachmentSize int64

	// Limit stops after this many messages are written (0 = no limit; must be non-negative).
	Limit int

	// BatchSize is the number of messages read per query.
	BatchSize int
}

// DefaultOptions returns ImportOptions with the default limits.
func DefaultOptions() ImportOptions {
	return ImportOptions{
		MaxAttachmentSize: 100 * 1024 * 1024,
		BatchSize:         500,
	}
}

// ImportSummary reports what an import did.
type ImportSummary struct {
	Duration           time.Duration
	SourceID           int64
	ChatsProcessed     int64
	ChatsSkipped       int64
	MessagesProcessed  int64
	MessagesAdded      int64
	MessagesUpdated    int64 // Messages already archived, including unchanged messages.
	MessagesSkipped    int64
	MessagesEdited     int64
	Participants       int64
	ReactionsAdded     int64
	RepliesLinked      int64
	RepliesUnresolved  int64
	AttachmentsFound   int64
	AttachmentsStored  int64
	AttachmentsMissing int64
	Errors             int64
}

// ImportProgress receives progress callbacks during an import.
type ImportProgress = textimport.Progress[ImportSummary]

// NullProgress ignores progress callbacks.
type NullProgress = textimport.NullProgress[ImportSummary]

// sigRecipient is one row of the Signal recipient table.
type sigRecipient struct {
	ID      int64
	ACI     string
	PNI     string
	E164    string
	GroupID string
	Name    string
}

// sigThread is one chat: a thread and the recipient it belongs to.
type sigThread struct {
	ID          int64
	RecipientID int64
}

// sigMessage is a visible message row: the latest revision of a message,
// with the identity fields of its original revision.
type sigMessage struct {
	ThreadID         int64          `json:"thread_id"`
	RowID            int64          `json:"row_id"`
	OriginalRowID    sql.NullInt64  `json:"original_row_id,omitzero"`
	AuthorID         int64          `json:"author_recipient_id"`
	DateSent         int64          `json:"date_sent"`
	RevisionDateSent int64          `json:"revision_date_sent"`
	DateReceived     int64          `json:"date_received"`
	Type             int64          `json:"type"`
	Body             sql.NullString `json:"body"`
	QuoteID          int64          `json:"quote_id,omitzero"`
	QuoteAuthor      int64          `json:"quote_author,omitzero"`
	StoryType        int64          `json:"story_type,omitzero"`
	ParentStoryID    int64          `json:"parent_story_id,omitzero"`
	RemoteDeleted    int64          `json:"remote_deleted,omitzero"`
	ScheduledDate    int64          `json:"scheduled_date,omitzero"`
}

// sigAttachment is one attachment row.
type sigAttachment struct {
	Role          store.AttachmentRole
	ID            int64
	MessageID     int64
	ContentType   string
	FileName      string
	Caption       string
	Width         int64
	Height        int64
	VoiceNote     bool
	VideoGIF      bool
	StickerPackID string
}

// sigReaction is one reaction on a message.
type sigReaction struct {
	RootID   int64
	AuthorID int64
	Emoji    string
	DateSent int64
}

// sigMention is one @-mention inside a message body.
type sigMention struct {
	RecipientID int64
	RangeStart  int64
}
