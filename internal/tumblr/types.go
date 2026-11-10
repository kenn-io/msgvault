// Package tumblr imports per-blog HTML conversation exports without network access.
package tumblr

import (
	"database/sql"
	"log/slog"
)

const SourceType = "tumblr"
const MessageType = "tumblr"
const maxHTMLBytes = 64 << 20
const maxMediaBytes = 100 << 20
const maxEntries = 1000000
const maxMetadataBytes = 128 << 20

type ImportOptions struct {
	Blog           string
	AttachmentsDir string
	Logger         *slog.Logger
}

type ImportSummary struct {
	SourceID             int64
	OwnerKey             string
	Files                int
	FilesSkipped         int
	Conversations        int
	ForeignConversations int
	MessagesAdded        int
	MessagesPresent      int
	Unsupported          int
	Malformed            int
	Undated              int
	ConflictingDates     int
	UnknownBlocks        int
	UnsupportedTypes     map[string]int
	MediaStored          int
	MediaMissing         int
	MediaRemote          int
	MediaInvalid         int
	MediaOversized       int
}

type Participant struct{ Name, ID, Status string }
type Conversation struct {
	ID, Status, Filename     string
	Participants             []Participant
	Messages                 []Message
	Malformed, UnknownBlocks int
}
type Message struct {
	Sender, Type, Header, DateText, Epoch, HTML, Text string
	SentAt                                            sql.NullTime
	DateConflict                                      bool
	Targets, Media                                    []string
}
