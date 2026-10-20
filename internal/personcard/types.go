// Package personcard assembles bounded, deterministic person projections.
// It performs no reads, writes, or model calls.
package personcard

import (
	"context"
	"time"
)

type Format string

const (
	FormatJSON      Format = "json"
	FormatMarkdown  Format = "markdown"
	DefaultMaxItems        = 3
	DefaultMaxBytes        = 16384
)

type Options struct {
	AsOf        time.Time
	MaxItems    int
	MaxBytes    int
	Format      Format
	Affiliation string
	WebBaseURL  string
}
type OpenLoopReader interface {
	ListPersonOpenLoops(ctx context.Context, personID int64, asOf time.Time, limit int) (OpenLoopPage, error)
}
type OpenLoopPage struct {
	Loops      []OpenLoop
	ComputedAt time.Time
	Truncated  bool
}
type OpenLoop struct {
	ID             string
	Kind           string
	Direction      string
	Status         string
	Summary        string
	DueAt          *time.Time
	LastActivityAt time.Time
	SourceID       int64
	Evidence       []EvidenceRef
	KataRef        string
}
type EvidenceRef struct {
	Kind string
	Ref  string
	At   time.Time
}

// Inputs contains current snapshots. The HTTP adapter owns joins and reader errors.
// Gaps records optional reader failures; successful empty reads are inferred here.
type Inputs struct {
	CollectedAt  time.Time
	Person       Person
	ContactState *ContactState
	Brief        *Brief
	Actions      []Action
	Facts        []FactChange
	Loops        OpenLoopPage
	Agenda       []AgendaItem
	Meetings     []Meeting
	Freshness    FreshnessInputs
	Coverage     Coverage
	Gaps         []string
}
type ContactState struct {
	LastContactAt    *time.Time
	Channel          string
	Ref              string
	LastInboundAt    *time.Time
	LastOutboundAt   *time.Time
	InteractionCount int64
	CadenceStatus    string
	CadenceDueAt     *time.Time
	ComputedAt       time.Time
	Stale            bool
}
type Action struct {
	MessageID     int64
	Ordinal       int
	Status        string
	Text          string
	Assignee      string
	AssigneeClass string
	DueDate       string
	OccurredAt    time.Time
	SourceID      int64
}
type FactChange struct {
	ID          int64
	User        bool
	Sensitive   bool
	Supported   bool
	Relation    string
	ChangedAt   time.Time
	Label       string
	Text        string
	EvidenceRef string
	SourceIDs   []int64
}
type AgendaItem struct {
	QualifiedRef string
	Title        string
	List         string
	WebURL       string
	Priority     *int
}
type Meeting struct {
	Ref          string
	Start        time.Time
	Basis        string
	Title        string
	Invited      bool // Invitation evidence is independent of note/transcript evidence.
	Participated bool
	Cancelled    bool
}

type PersonText struct {
	DisplayName          string   `json:"display_name"`
	OrganizationName     string   `json:"organization_name"`
	Title                string   `json:"title"`
	Categories           []string `json:"categories"`
	DisplayNameMore      bool     `json:"display_name_more,omitzero"`
	OrganizationNameMore bool     `json:"organization_name_more,omitzero"`
	TitleMore            bool     `json:"title_more,omitzero"`
}
type Person struct {
	PersonID        int64      `json:"person_id"`
	VCardUID        string     `json:"vcard_uid"`
	Tracked         bool       `json:"tracked"`
	Emails          []string   `json:"emails"`
	PrimaryChannel  string     `json:"primary_channel"`
	InferredChannel string     `json:"inferred_channel"`
	ContentTrust    string     `json:"content_trust"`
	UntrustedText   PersonText `json:"untrusted_text"`
	EmailsMore      bool       `json:"emails_more,omitzero"`
	CategoriesMore  bool       `json:"categories_more,omitzero"`
	VCardUIDMore    bool       `json:"vcard_uid_more,omitzero"`
}
type LastInteraction struct {
	At                   *time.Time `json:"at"`
	Channel              string     `json:"channel"`
	Direction            string     `json:"direction"`
	Ref                  string     `json:"ref"`
	WebURL               string     `json:"web_url"`
	LastInboundAt        *time.Time `json:"last_inbound_at"`
	LastOutboundAt       *time.Time `json:"last_outbound_at"`
	InteractionCount     int64      `json:"interaction_count"`
	CadenceStatus        string     `json:"cadence_status"`
	CadenceDueAt         *time.Time `json:"cadence_due_at"`
	CadenceOverdueAtAsOf bool       `json:"cadence_overdue_at_as_of"`
	ProjectionComputedAt time.Time  `json:"projection_computed_at"`
	ProjectionStale      bool       `json:"projection_stale"`
}
type Cite struct {
	Kind        string     `json:"kind"`
	Ref         string     `json:"ref"`
	EvidenceRef string     `json:"evidence_ref,omitempty"`
	WebURL      string     `json:"web_url"`
	At          *time.Time `json:"at"`
}
type ItemText struct {
	Text         string `json:"text"`
	Label        string `json:"label,omitempty"`
	Assignee     string `json:"assignee,omitempty"`
	StatusLabel  string `json:"status_label,omitempty"`
	List         string `json:"list,omitempty"`
	WebURL       string `json:"web_url,omitempty"`
	TextMore     bool   `json:"text_more,omitzero"`
	LabelMore    bool   `json:"label_more,omitzero"`
	AssigneeMore bool   `json:"assignee_more,omitzero"`
	ListMore     bool   `json:"list_more,omitzero"`
	WebURLMore   bool   `json:"web_url_more,omitzero"`
}
type Item struct {
	ID            string   `json:"id"`
	Kind          string   `json:"kind"`
	Subkind       string   `json:"subkind,omitempty"`
	Relation      string   `json:"relation,omitempty" enum:"support,contradict,supersede"`
	Rank          int      `json:"rank"`
	Date          string   `json:"date"`
	DateRole      string   `json:"date_role"`
	Overdue       bool     `json:"overdue"`
	Stale         bool     `json:"stale"`
	ContentTrust  string   `json:"content_trust"`
	UntrustedText ItemText `json:"untrusted_text"`
	AssigneeClass string   `json:"assignee_class,omitempty"`
	Cite          Cite     `json:"cite"`
	when          time.Time
	priority      *int
	ordinal       int
	meetingID     int64
}
type ActionCoverage struct {
	MeetingCount int  `json:"meeting_count"`
	Available    int  `json:"available"`
	Partial      int  `json:"partial"`
	Unsupported  int  `json:"unsupported"`
	Unavailable  int  `json:"unavailable"`
	Truncated    bool `json:"truncated"`
}
type Coverage struct {
	Actions              ActionCoverage `json:"actions"`
	AgendaTruncated      bool           `json:"agenda_truncated"`
	LoopsTruncated       bool           `json:"loops_truncated"`
	LoopsInvalidEvidence int            `json:"loops_invalid_evidence"`
	MeetingsTruncated    bool           `json:"meetings_truncated"`
}
type MeetingText struct {
	Title     string `json:"title"`
	TitleMore bool   `json:"title_more,omitzero"`
}
type RecentMeeting struct {
	Ref           string      `json:"ref"`
	Start         time.Time   `json:"start"`
	Basis         string      `json:"basis"`
	ContentTrust  string      `json:"content_trust"`
	UntrustedText MeetingText `json:"untrusted_text"`
}
type Meetings struct {
	WindowDays    int             `json:"window_days"`
	Count         int             `json:"count"`
	CountByBasis  map[string]int  `json:"count_by_basis"`
	LastMetAt     *time.Time      `json:"last_met_at"`
	LastInvitedAt *time.Time      `json:"last_invited_at"`
	Recent        []RecentMeeting `json:"recent"`
	RecentMore    bool            `json:"recent_more,omitzero"`
}
type BriefText struct {
	Text     string `json:"text"`
	TextMore bool   `json:"text_more,omitzero"`
}
type Brief struct {
	Version        int        `json:"version"`
	GeneratedAt    time.Time  `json:"generated_at"`
	ContentTrust   string     `json:"content_trust"`
	UntrustedText  *BriefText `json:"untrusted_text"`
	Citations      []string   `json:"citations"`
	Evidence       []string   `json:"evidence"`
	CitationsCount int        `json:"citations_count"`
	CitationsMore  bool       `json:"citations_more,omitzero"`
	EvidenceMore   bool       `json:"evidence_more,omitzero"`
}
type Card struct {
	Person             Person           `json:"person"`
	LastInteraction    *LastInteraction `json:"last_interaction"`
	Items              []Item           `json:"items"`
	ItemsDroppedByRank int              `json:"items_dropped_by_rank"`
	Coverage           Coverage         `json:"coverage"`
	Meetings           Meetings         `json:"meetings"`
	Brief              *Brief           `json:"brief"`
	Excluded           []string         `json:"excluded"`
	Freshness          FreshnessBlock   `json:"-"`
	Gaps               []string         `json:"-"`
	AsOf               time.Time        `json:"-"`
	affiliation        string
}
type Omitted struct {
	Brief    bool `json:"brief"`
	Items    int  `json:"items"`
	Meetings int  `json:"meetings"`
	Sources  int  `json:"sources"`
}

func (o Omitted) Any() bool { return o.Brief || o.Items > 0 || o.Meetings > 0 || o.Sources > 0 }

type CandidateText struct {
	DisplayName     string `json:"display_name"`
	DisplayNameMore bool   `json:"display_name_more,omitzero"`
}
type Candidate struct {
	PersonID      int64         `json:"person_id"`
	UntrustedText CandidateText `json:"untrusted_text"`
	ContentTrust  string        `json:"content_trust"`
}
type PersonCardResult struct {
	SchemaVersion  int            `json:"schema_version"`
	Status         string         `json:"status"`
	Ref            string         `json:"ref"`
	AsOf           time.Time      `json:"as_of"`
	CollectedAt    time.Time      `json:"collected_at"`
	Format         Format         `json:"format"`
	Card           *Card          `json:"card"`
	Content        *string        `json:"content"`
	ContentBytes   int            `json:"content_bytes"`
	Truncated      bool           `json:"truncated"`
	Omitted        Omitted        `json:"omitted"`
	Freshness      FreshnessBlock `json:"freshness"`
	Gaps           []string       `json:"gaps"`
	ParticipantID  *int64         `json:"participant_id"`
	Candidates     []Candidate    `json:"candidates"`
	CandidatesMore bool           `json:"candidates_more,omitzero"`
}
