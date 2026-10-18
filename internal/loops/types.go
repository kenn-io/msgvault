package loops

import (
	"time"

	"go.kenn.io/msgvault/internal/mime"
)

// Direction is supplied by Store.ClassifyActivityCandidate; detection must not
// infer ownership from display names or unscoped address matching.
type Direction string

const (
	Inbound        Direction = "inbound"
	Outbound       Direction = "outbound"
	Observed       Direction = "observed"
	ReplyOwed                = "reply_owed"
	AwaitingReply            = "awaiting_reply"
	StatusAll                = "all"
	RulesetVersion           = 1
)

// MaxConversationMessageLimit caps the metadata window loaded for one loop pass.
const MaxConversationMessageLimit = 10_000

type Thresholds struct {
	ReplyOwedAfter, AwaitingReplyAfter, Lookback time.Duration
	MaxGroupCounterparts, RunMessageLimit        int
}

func DefaultThresholds() Thresholds {
	return Thresholds{ReplyOwedAfter: 24 * time.Hour, AwaitingReplyAfter: 72 * time.Hour,
		Lookback: 365 * 24 * time.Hour, MaxGroupCounterparts: 3, RunMessageLimit: 12}
}

type LoopParticipant struct {
	ID, PersonID                           int64
	Email, Phone, DisplayName, CanonicalID string
	IsOwner                                bool
}

type LoopRecipient struct {
	LoopParticipant

	Type string
}

type LoopMessage struct {
	ID               int64
	OccurredAt       time.Time
	Direction        Direction
	OwnerAddress     string
	Sender           LoopParticipant
	Recipients       []LoopRecipient
	ListID           string
	Labels           []string
	Signals          *mime.AutomationSignals
	Draft            bool // Current source evidence marks the message as an unsent draft.
	Deleted          bool
	Subject, Snippet string
}

// ConversationView contains chronological metadata and lazy, per-primary-key
// body access. AnchorExists checks the live anchor in this conversation,
// distinguishing deletion or a conversation move from a bounded window gap.
// Extend loads only an extended Messages window; Detect retains the conversation
// metadata and callbacks from this view.
type ConversationView struct {
	ConversationID, SourceID     int64
	SourceType, ConversationType string
	Participants                 []LoopParticipant
	LastMessageAt                time.Time
	Messages                     []LoopMessage
	BodyText                     func(messageID int64) (string, error)
	AnchorExists                 func(messageID int64) (bool, error)
	Extend                       func() (ConversationView, error)
}

type LiveLoop struct {
	ID                          int64
	Kind                        string
	AnchorMessageID             int64
	CounterpartyParticipantID   int64
	CounterpartyPersonID        int64
	OwnerParticipationMessageID int64
}

type Suppressions struct {
	Participants, Persons, Conversations []int64
}

type Spec struct {
	Kind, Status, Summary string
	Anchor, LastActivity  LoopMessage
	Counterparty          LoopParticipant
	OwedSinceAt, OpensAt  time.Time
	TurnMessageIDs        []int64
	// PreviousTurnKnown, PreviousTurnAt, and PreviousTurnMessageID identify the
	// opposing boundary inside the window; the ID resolves equal-time ordering.
	PreviousTurnKnown           bool
	PreviousTurnAt              time.Time
	PreviousTurnMessageID       int64
	OwnerParticipationMessageID int64
	// TruncatedRun marks body-limited or unreadable evidence in the trailing run.
	TruncatedRun bool
}

type CloseDecision struct {
	ID        int64
	Reason    string
	At        time.Time
	MessageID int64
}

// Verdict separates ineligibility from a legitimate reply. Coverage gaps keep
// live rows and user state intact; infrastructure errors prevent queue ack.
type Verdict struct {
	Open         *Spec
	Close        []CloseDecision
	CoverageGaps []int64
	Err          error
}
