package store

import (
	"encoding/json/jsontext"
	"errors"
	"time"
)

var (
	ErrLoopNotFound      = errors.New("open loop not found")
	ErrLoopAlreadyClosed = errors.New("open loop already closed")
	ErrInvalidLoop       = errors.New("invalid open loop")
)

type OpenLoop struct {
	Kata                                                             *LoopKataLink
	ID                                                               int64
	TurnKey                                                          string
	Cycle                                                            int64
	ConversationID, SourceID                                         int64
	Kind, Detector, Status, Channel, OwnerAddress                    string
	CounterpartyParticipantID, CounterpartyPersonID, AnchorMessageID *int64
	OwedSinceAt                                                      time.Time
	LastActivityMessageID                                            *int64
	LastActivityAt, OpensAt                                          time.Time
	DueAt, SnoozedUntil                                              *time.Time
	Summary                                                          string
	EvidenceJSON                                                     jsontext.Value
	RulesetVersion                                                   int
	FactClaimID                                                      *int64
	AgendaState                                                      string
	FulfilmentSuggestedAt, ClosedAt                                  *time.Time
	ClosedReason, ClosedBy                                           *string
	ClosedMessageID                                                  *int64
	CreatedAt, UpdatedAt                                             time.Time
	CounterpartyDisplayName                                          string
}

type LoopParticipant struct {
	ParticipantID                                       int64
	PersonID                                            *int64
	EmailAddress, PhoneNumber, DisplayName, CanonicalID string
	IsOwner                                             bool
}
type LoopRecipient struct {
	LoopParticipant

	RecipientType string
}
type LoopMessage struct {
	ID               int64
	OccurredAt       time.Time
	Direction        ActivityDirection
	OwnerAddress     string
	Sender           LoopParticipant
	Recipients       []LoopRecipient
	ListID           string
	Labels           []string
	Signals          *MessageAutomationSignals
	Draft            bool
	Deleted          bool
	Subject, Snippet string
}
type LoopConversation struct {
	ConversationID, SourceID     int64
	SourceType, ConversationType string
	Participants                 []LoopParticipant
	LastMessageAt                time.Time
	HistoryComplete              bool
	Messages                     []LoopMessage
}
type LoopListFilter struct {
	Kind, Status       string
	PersonID, SourceID int64
	IncludeSnoozed     bool
	Limit              int
	CursorAt           time.Time
	CursorID           int64
	HasCursor          bool
	Now                time.Time
}
type LoopCloseDecision struct {
	ID        int64
	Reason    string
	At        time.Time
	MessageID *int64
}
type LoopOpenDecision struct {
	TurnKey, Kind, Status, Channel, OwnerAddress, Summary            string
	ConversationID, SourceID, AnchorMessageID, LastActivityMessageID int64
	CounterpartyParticipantID, CounterpartyPersonID                  *int64
	OwedSinceAt, LastActivityAt, OpensAt                             time.Time
	EvidenceJSON                                                     jsontext.Value
	RulesetVersion                                                   int
	TurnMessageIDs                                                   []int64
	PreviousTurnKnown                                                bool
	PreviousTurnAt                                                   time.Time
	PreviousTurnMessageID                                            int64
}
type LoopVerdict struct {
	Close        []LoopCloseDecision
	Open         *LoopOpenDecision
	CoverageGaps []int64
}
type LoopApply struct {
	ConversationID, ObservedRevision int64
	Epoch                            string
	Now                              time.Time
	Verdict                          LoopVerdict
}
type LoopSuppression struct {
	ID                                      int64
	TargetKind                              string
	ParticipantID, PersonID, ConversationID *int64
	Note                                    string
	CreatedAt                               time.Time
}
type LoopSuppressionRequest struct {
	TargetKind                              string
	ParticipantID, PersonID, ConversationID *int64
	Note                                    string
	DryRun                                  bool
}
type LoopSuppressionResult struct {
	Suppression *LoopSuppression
	WouldClose  int64
}
