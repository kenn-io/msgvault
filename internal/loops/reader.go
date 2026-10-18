package loops

import (
	"context"
	"time"
)

// Reader is the shared in-process seam for person context consumers. A nil
// Reader means loops are not wired on that surface, not that a person has no
// open obligations. Service implements this interface.
type Reader interface {
	ListPersonOpenLoops(ctx context.Context, personID int64, limit int) (Page, error)
}

// Page.ComputedAt is applied_through: the certified archive frontier, not the
// time the last job ran. Truncated means additional matching open loops exist.
// The Reader and freshness contracts are also consumed by person context
// features; keep their field names and meanings stable.
type Page struct {
	Loops      []Loop
	ComputedAt time.Time
	Truncated  bool
}

type Counterparty struct {
	ParticipantID int64  `json:"participant_id"`
	PersonID      *int64 `json:"person_id"`
	DisplayName   string `json:"display_name"`
}

// Evidence contains archived references and optional extracted quote spans.
// List/detail surfaces never include a whole message body.
type Evidence struct {
	KataClosedRef     string    `json:"kata_closed_ref,omitempty"`
	KataReopenBlocked string    `json:"kata_reopen_blocked,omitempty"`
	MessageID         int64     `json:"message_id"`
	ConversationID    int64     `json:"conversation_id"`
	OccurredAt        time.Time `json:"occurred_at"`
	Subject           string    `json:"subject"`
	Quote             string    `json:"quote,omitempty"`
	QuoteStartRune    int       `json:"quote_start_rune,omitempty"`
	QuoteEndRune      int       `json:"quote_end_rune,omitempty"`
	TruncatedRun      bool      `json:"truncated_run,omitempty"`
	CoverageGap       bool      `json:"coverage_gap,omitempty"`
	OwnerNote         string    `json:"owner_note,omitempty"`
}

type Loop struct {
	ClosedBy              *string      `json:"closed_by"`
	Kata                  *LoopKata    `json:"kata"`
	ID                    int64        `json:"id"`
	TurnKey               string       `json:"turn_key"`
	Cycle                 int64        `json:"cycle"`
	Kind                  string       `json:"kind"`
	Status                string       `json:"status"`
	Channel               string       `json:"channel"`
	ConversationID        int64        `json:"conversation_id"`
	SourceID              int64        `json:"source_id"`
	Counterparty          Counterparty `json:"counterparty"`
	Summary               string       `json:"summary"`
	OwedSinceAt           time.Time    `json:"owed_since_at"`
	AnchorMessageID       *int64       `json:"anchor_message_id"`
	LastActivityMessageID *int64       `json:"last_activity_message_id"`
	LastActivityAt        time.Time    `json:"last_activity_at"`
	OpensAt               time.Time    `json:"opens_at"`
	DueAt                 *time.Time   `json:"due_at"`
	SnoozedUntil          *time.Time   `json:"snoozed_until"`
	AgeSeconds            int64        `json:"age_seconds"`
	Evidence              []Evidence   `json:"evidence"`
	ClosedAt              *time.Time   `json:"closed_at"`
	ClosedReason          *string      `json:"closed_reason"`
	ClosedMessageID       *int64       `json:"closed_message_id"`
	Detector              string       `json:"detector"`
	AgendaState           string       `json:"agenda_state"`
}
