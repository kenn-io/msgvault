// Package loops finds conversations that still wait on a reply. It works them
// out from the archive when asked, so a reply that just synced to the same
// conversation clears its loop on the next list.
package loops

import (
	"time"

	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
)

const (
	ReplyOwed      = "reply_owed"
	AwaitingReply  = "awaiting_reply"
	DefaultLimit   = 50
	MaxLimit       = 200
	ReplyOwedAfter = 24 * time.Hour
	AwaitingAfter  = 72 * time.Hour
	// Lookback is how recent a conversation's last message must be for it to
	// list. A loop that lists keeps the age of the message that started it.
	Lookback = 30 * 24 * time.Hour
)

// Loop is one conversation turn that waits on a reply.
type Loop struct {
	Kind           string
	Anchor         store.LoopMessage
	ConversationID int64
	SourceID       int64
	Channel        store.ActivityChannel
	Counterparty   store.LoopParticipant
	Summary        string
	OwedSince      time.Time
	LastActivity   time.Time
	Questions      []store.LoopMessage
}

// Reader supplies what Detect cannot read from the loaded messages. Each read
// is one primary-key lookup, made only for messages the walk examines.
type Reader interface {
	Body(messageID int64) (string, error)
	// Original reports automation signals, file-attachment evidence, and
	// unreadable=true when the stored original fails MIME parsing. A message
	// with no stored original has no signals or attachment evidence.
	Original(messageID int64) (signals mime.AutomationSignals, hasFileAttachment bool, unreadable bool, err error)
}
