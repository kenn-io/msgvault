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
)

// Limits name the ways a search came up short, so an empty result that lists
// one never means nobody wrote about the topic.
const (
	// LimitPoolFull means a message cleared the floor and the search returned
	// its full pool, so more messages beyond it may match.
	LimitPoolFull = "pool_full"
	// LimitDeclaredTruncated means more declared topics exist than one
	// request reads, so some were not searched.
	LimitDeclaredTruncated = "declared_truncated"
	// LimitIndexScope means the vector index leaves out some message types
	// with authors or some accounts, so their messages were not searched.
	LimitIndexScope = "index_scope"
	// LimitNotEmbedded means the keyword search found messages the vector
	// index has not embedded yet, so they could not count.
	LimitNotEmbedded = "not_embedded"
)

// Request asks who in the archive has written about a topic.
// Omitted limit, evidence, and min_score take their defaults.
type Request struct {
	Topic    string
	Limit    *int
	Evidence *int
	MinScore *float64
	After    *time.Time
	Before   *time.Time
}

// Citation is one message a person wrote about the topic.
// SenderAuthenticated is false for sources, such as email, where the From
// header is an assertion rather than a verified identity.
type Citation struct {
	MessageID           int64      `json:"message_id"`
	SentAt              *time.Time `json:"sent_at,omitempty"`
	MessageType         string     `json:"message_type"`
	Subject             string     `json:"subject,omitempty"`
	SenderAuthenticated bool       `json:"sender_authenticated"`
}

// Entry is a saved person (PersonID set) or an unsaved contact
// (ParticipantID set), ranked for the topic. Promote a contact with
// "msgvault person promote <participant_id>". Score is MessageScore plus
// DeclaredBoost. A person with a matching ask_me_about value and no matching
// messages has zero AuthoredMessages and MessageScore.
type Entry struct {
	Rank             int        `json:"rank"`
	PersonID         int64      `json:"person_id,omitempty"`
	ParticipantID    int64      `json:"participant_id,omitempty"`
	DisplayName      string     `json:"display_name,omitempty"`
	EmailAddress     string     `json:"email_address,omitempty"`
	Score            float64    `json:"score"`
	MessageScore     float64    `json:"message_score"`
	DeclaredBoost    float64    `json:"declared_boost"`
	AuthoredMessages int        `json:"authored_messages"`
	AskMeAbout       []string   `json:"ask_me_about,omitempty"`
	Citations        []Citation `json:"citations"`
}

// Response lists people first, then unsaved contacts. Limits lists each way
// the search came up short; both lists empty with no limits means nobody in
// the archive wrote about the topic.
type Response struct {
	Topic           string   `json:"topic"`
	Limits          []string `json:"limits" enum:"pool_full,declared_truncated,index_scope,not_embedded"`
	People          []Entry  `json:"people"`
	UnsavedContacts []Entry  `json:"unsaved_contacts"`
}

// Params is a validated Request with its defaults applied.
type Params struct {
	Topic    string
	Terms    []string
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
	p.Terms = query.TextTerms
	p.FreeText = strings.Join(p.Terms, " ")
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
	p.After, p.Before = r.After, r.Before
	if p.After != nil && p.Before != nil && !p.After.Before(*p.Before) {
		return p, fmt.Errorf("%w: after must precede before", ErrInvalidParameter)
	}
	return p, nil
}
