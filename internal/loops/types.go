// Package loops finds conversations that still wait on a reply. It works them
// out from the archive when asked, so a reply that just synced to the same
// conversation clears its loop on the next list.
package loops

import (
	"time"

	"go.kenn.io/msgvault/internal/store"
)

const (
	ReplyOwed      = "reply_owed"
	AwaitingReply  = "awaiting_reply"
	DefaultLimit   = 50
	MaxLimit       = 200
	ReplyOwedAfter = 24 * time.Hour
	AwaitingAfter  = 72 * time.Hour
	// ActivityWindow is how recently a conversation must have live activity
	// for List and actions to read it. Snoozed conversations stay eligible.
	ActivityWindow = 30 * 24 * time.Hour
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

// Reader supplies the message facts Detect needs.
type Reader interface {
	Evidence(messageID int64) (store.LoopMessageEvidence, error)
}
