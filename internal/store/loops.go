package store

import (
	"iter"
	"time"
)

// LoopParticipant is one side of a message with the identity fields a loop
// shows.
type LoopParticipant struct {
	ParticipantID int64
	PersonID      int64
	DisplayName   string
	Address       string
	IsOwner       bool
}

type LoopRecipient struct {
	LoopParticipant

	Type string
}

// LoopMessage is one live, non-draft email or chat message with its
// owner-relative direction.
type LoopMessage struct {
	ID             int64
	ConversationID int64
	OccurredAt     time.Time
	Direction      ActivityDirection
	// FromAddress preserves the archived sender envelope across participant merges.
	FromAddress string
	Sender      LoopParticipant
	Recipients  []LoopRecipient
	ListID      string
	Subject     string
	Snippet     string
	// HasFileAttachment is true only when the archive confirms a non-inline file.
	HasFileAttachment bool
}

// LoopConversation is an email thread or direct chat with its newest messages.
type LoopConversation struct {
	ConversationID   int64
	SourceID         int64
	ConversationType string
	GroupChat        bool
	// Messages are the newest live messages, newest first.
	Messages []LoopMessage

	earlier iter.Seq2[LoopMessage, error]
}

// Walk yields the conversation's live messages newest first: Messages, then
// older pages read only as far as the caller asks.
func (c LoopConversation) Walk() iter.Seq2[LoopMessage, error] {
	return func(yield func(LoopMessage, error) bool) {
		for _, m := range c.Messages {
			if !yield(m, nil) {
				return
			}
		}
		if c.earlier == nil {
			return
		}
		for m, err := range c.earlier {
			if !yield(m, err) {
				return
			}
		}
	}
}

// LoopMessageEvidence retains classification facts, never message text.
type LoopMessageEvidence struct {
	Empty           bool `json:"empty"`
	Acknowledgement bool `json:"acknowledgement"`
	Asks            bool `json:"asks"`
	Automated       bool `json:"automated"`
	Attachment      bool `json:"attachment"`
	Unreadable      bool `json:"unreadable"`
}
