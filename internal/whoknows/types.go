package whoknows

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/search"
)

var (
	ErrMissingFreeText  = errors.New("topic must contain free text")
	ErrInvalidParameter = errors.New("invalid parameter")
)

const (
	DefaultLimit    = 10
	MaxLimit        = 50
	DefaultEvidence = 3
	MaxEvidence     = 10
	DefaultMinScore = 0.5

	// ReasonNoEvidence marks an empty response: no message about the topic
	// was written by anyone but the owner or an automated sender.
	ReasonNoEvidence = "no_relevant_evidence"
	// ReasonSearchLimit marks an empty response where the search returned its
	// full pool and every matching message in it came from the owner or an
	// automated sender, so the answer may lie beyond the pool.
	ReasonSearchLimit = "search_limit"
)

// WhoKnowsRequest asks who in the archive has written about a topic.
// Omitted limit, evidence, and min_score take their defaults.
type WhoKnowsRequest struct {
	Topic    string   `json:"topic" minLength:"1" doc:"Plain words; search operators are refused"`
	Limit    *int     `json:"limit,omitempty" minimum:"1" maximum:"50" doc:"Saved people to return; default 10"`
	Evidence *int     `json:"evidence,omitempty" minimum:"1" maximum:"10" doc:"Citations per person; default 3"`
	MinScore *float64 `json:"min_score,omitempty" exclusiveMinimum:"0" maximum:"1" doc:"Lowest cosine similarity a message needs to count; default 0.5"`
	After    string   `json:"after,omitempty" doc:"YYYY-MM-DD or RFC3339; messages at or after this instant"`
	Before   string   `json:"before,omitempty" doc:"YYYY-MM-DD or RFC3339; messages before this instant"`
}

// WhoKnowsCitation is one message a person wrote about the topic.
// SenderAuthenticated is false for sources, such as email, where the From
// header is an assertion rather than a verified identity.
type WhoKnowsCitation struct {
	MessageID           int64      `json:"message_id"`
	SentAt              *time.Time `json:"sent_at,omitempty"`
	MessageType         string     `json:"message_type"`
	Subject             string     `json:"subject,omitempty"`
	Snippet             string     `json:"snippet,omitempty"`
	SenderAuthenticated bool       `json:"sender_authenticated"`
}

// WhoKnowsPerson is a saved person ranked for the topic. Score is
// MessageScore plus DeclaredBoost. A person with a matching ask_me_about value
// and no matching messages has zero AuthoredMessages and MessageScore.
type WhoKnowsPerson struct {
	Rank             int                `json:"rank"`
	PersonID         int64              `json:"person_id"`
	DisplayName      string             `json:"display_name,omitempty"`
	Score            float64            `json:"score"`
	MessageScore     float64            `json:"message_score"`
	DeclaredBoost    float64            `json:"declared_boost"`
	AuthoredMessages int                `json:"authored_messages"`
	AskMeAbout       []string           `json:"ask_me_about,omitempty"`
	Citations        []WhoKnowsCitation `json:"citations"`
}

// WhoKnowsContact is a sender bound to no saved person. Promote it with
// "msgvault person promote <participant_id>".
type WhoKnowsContact struct {
	Rank             int                `json:"rank"`
	ParticipantID    int64              `json:"participant_id"`
	DisplayName      string             `json:"display_name,omitempty"`
	EmailAddress     string             `json:"email_address,omitempty"`
	Score            float64            `json:"score"`
	MessageScore     float64            `json:"message_score"`
	AuthoredMessages int                `json:"authored_messages"`
	Citations        []WhoKnowsCitation `json:"citations"`
}

// WhoKnowsResponse lists people first, then unsaved contacts. Reason is set
// when both lists are empty. SearchedMessages is how many messages the search
// returned.
type WhoKnowsResponse struct {
	Topic            string            `json:"topic"`
	Reason           string            `json:"reason,omitempty"`
	SearchedMessages int               `json:"searched_messages"`
	People           []WhoKnowsPerson  `json:"people"`
	UnsavedContacts  []WhoKnowsContact `json:"unsaved_contacts"`
}

type (
	Request  = WhoKnowsRequest
	Response = WhoKnowsResponse
	Person   = WhoKnowsPerson
	Contact  = WhoKnowsContact
	Citation = WhoKnowsCitation
)

// Params is a validated Request with its defaults applied.
type Params struct {
	Topic    string
	FreeText string
	Limit    int
	Evidence int
	MinScore float64
	After    *time.Time
	Before   *time.Time
}

// Resolve applies defaults and validates the request. The topic must be plain
// words: a search operator such as "from:ana" or "a | b" would narrow the
// search by something other than meaning, so it is refused.
func (r Request) Resolve() (Params, error) {
	p := Params{
		Topic:    strings.TrimSpace(r.Topic),
		Limit:    DefaultLimit,
		Evidence: DefaultEvidence,
		MinScore: DefaultMinScore,
	}
	query := search.Parse(p.Topic)
	if query.HasOperators() || query.Err() != nil || len(query.UnsupportedOperators) > 0 {
		return p, fmt.Errorf("%w: topic must be plain words, without search operators", ErrInvalidParameter)
	}
	p.FreeText = strings.Join(query.TextTerms, " ")
	if strings.TrimSpace(p.FreeText) == "" {
		return p, ErrMissingFreeText
	}
	if r.Limit != nil {
		p.Limit = *r.Limit
	}
	if r.Evidence != nil {
		p.Evidence = *r.Evidence
	}
	if r.MinScore != nil {
		p.MinScore = *r.MinScore
	}
	if p.Limit < 1 || p.Limit > MaxLimit {
		return p, fmt.Errorf("%w: limit must be between 1 and %d", ErrInvalidParameter, MaxLimit)
	}
	if p.Evidence < 1 || p.Evidence > MaxEvidence {
		return p, fmt.Errorf("%w: evidence must be between 1 and %d", ErrInvalidParameter, MaxEvidence)
	}
	if math.IsNaN(p.MinScore) || p.MinScore <= 0 || p.MinScore > 1 {
		return p, fmt.Errorf("%w: min_score must be greater than 0 and at most 1", ErrInvalidParameter)
	}
	var err error
	if p.After, err = ParseDate(r.After); err != nil {
		return p, fmt.Errorf("%w: after: %w", ErrInvalidParameter, err)
	}
	if p.Before, err = ParseDate(r.Before); err != nil {
		return p, fmt.Errorf("%w: before: %w", ErrInvalidParameter, err)
	}
	if p.After != nil && p.Before != nil && !p.After.Before(*p.Before) {
		return p, fmt.Errorf("%w: after must precede before", ErrInvalidParameter)
	}
	return p, nil
}

// ParseDate reads YYYY-MM-DD or RFC3339. An empty string means no bound.
// Like the search operators, a date-only value is midnight UTC, so a window
// includes its after instant and excludes its before instant.
func ParseDate(raw string) (*time.Time, error) {
	if raw == "" {
		return nil, nil //nolint:nilnil // An omitted date leaves the filter unbounded.
	}
	layout := time.RFC3339
	if len(raw) == len(time.DateOnly) {
		layout = time.DateOnly
	}
	v, err := time.Parse(layout, raw)
	if err != nil {
		return nil, fmt.Errorf("date must be YYYY-MM-DD or RFC3339: %w", err)
	}
	return &v, nil
}
