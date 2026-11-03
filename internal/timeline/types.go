// Package timeline defines bounded, read-only views over archived activity.
package timeline

import (
	"fmt"
	"strings"
	"time"
)

type Mode string // "day" | "relationship" | "anchor"
type Request struct {
	Mode                      Mode
	Date, From, To            string
	Days                      int
	With, Around              string
	Before, After             int
	Conversation              bool
	Tiers                     []int
	IncludeCold, IncludeNoise bool
	Brief                     bool
	Budget                    int
	Format                    string // "markdown" | "json"
	Cursor                    string
	Scope                     Scope
	// BeforeSet and AfterSet preserve explicit zero counts from transports.
	BeforeSet, AfterSet bool `json:"-"`
}
type Scope struct {
	Owner     bool
	PersonIDs []int64  // already authorized via AllowsPerson by the API; nil for owner
	OpenTools []string // MCP tools the caller may call; nil for owner = all
}
type Timeline struct {
	continuation      *Continuation
	Mode              Mode          `json:"mode"`
	Timezone          string        `json:"timezone"`
	TimezoneIsDefault bool          `json:"timezone_is_default"`
	Window            Window        `json:"window"`
	Now               time.Time     `json:"now"`
	Freshness         Freshness     `json:"freshness"`
	Person            *PersonHeader `json:"person,omitempty"`
	Anchor            *Anchor       `json:"anchor,omitempty"`
	Candidates        []Candidate   `json:"candidates,omitempty"`
	OpenLoops         *OpenLoops    `json:"open_loops,omitempty"`
	Upcoming          []Entry       `json:"upcoming,omitempty"`
	Days              []Day         `json:"days,omitempty"`
	History           []Entry       `json:"history,omitempty"`
	Gaps              []Gap         `json:"gaps,omitempty"`
	Omitted           Omitted       `json:"omitted"`
	Cursor            string        `json:"cursor,omitempty"`
}
type Gap struct {
	Section   string   `json:"section"`
	Code      string   `json:"code"`
	Retryable bool     `json:"retryable"`
	Items     []string `json:"items,omitempty"` // e.g. unexpandable series refs
}
type Open struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args"`
}

// Entry is a tagged union. Kind: "meeting" | "transcript" | "burst" | "message" | "call" | "loop" | "note".
type Entry struct {
	Kind       string        `json:"kind"`
	Key        string        `json:"key"`        // stable identity
	Ref        string        `json:"ref"`        // display ref
	AnchorRef  string        `json:"anchor_ref"` // valid --around value for this entry
	Time       time.Time     `json:"time"`
	Open       []Open        `json:"open"`
	Anchor     bool          `json:"anchor,omitempty"`
	Filtered   bool          `json:"filtered,omitempty"`
	HiddenText bool          `json:"hidden_text,omitempty"`
	Meeting    *MeetingEntry `json:"meeting,omitempty"`
	Burst      *BurstEntry   `json:"burst,omitempty"`
	Message    *MessageEntry `json:"message,omitempty"`
	Call       *CallEntry    `json:"call,omitempty"`
	Loop       *LoopEntry    `json:"loop,omitempty"`
	Note       *NoteEntry    `json:"note,omitempty"`
}

const (
	ModeDay          Mode = "day"
	ModeRelationship Mode = "relationship"
	ModeAnchor       Mode = "anchor"
)

type Window struct {
	From string `json:"from"`
	To   string `json:"to"`
}
type UntrustedText struct {
	Text      string `json:"text"`
	Truncated bool   `json:"truncated"`
}
type Freshness struct {
	AsOf           time.Time             `json:"as_of"`
	StaleThreshold int64                 `json:"stale_threshold"` // seconds
	Sources        []SourceFreshness     `json:"sources"`
	Projections    []ProjectionFreshness `json:"projections"`
	Warnings       []string              `json:"warnings"`
}
type SourceFreshness struct {
	SourceID    *int64   `json:"source_id"`
	Source      string   `json:"source"`
	Status      string   `json:"status"`
	LastRun     string   `json:"last_run"`
	Scheduling  string   `json:"scheduling"`
	Warnings    []string `json:"warnings"`
	SyncCommand string   `json:"sync_command"`
}
type ProjectionFreshness struct {
	Name     string     `json:"name"`
	Frontier *time.Time `json:"frontier"`
	State    string     `json:"state"`
}

// Person contains display metadata only; a zero PersonID denotes an unsaved contact.
type Person struct {
	TierSource    string `json:"tier_source"`
	Truncated     bool   `json:"truncated,omitzero"`
	PersonID      int64  `json:"person_id,omitempty"`
	ParticipantID int64  `json:"participant_id,omitempty"`
	Name          string `json:"name"`
	Tier          int    `json:"tier,omitempty"`
	Participation string `json:"participation,omitempty"`
	Credited      bool   `json:"credited,omitempty"`
}
type PersonHeader struct {
	Person             Person          `json:"person"`
	Ref                string          `json:"ref"`
	Channels           []string        `json:"channels"`
	FirstAt            *time.Time      `json:"first_at"`
	LastAt             *time.Time      `json:"last_at"`
	LastInboundAt      *time.Time      `json:"last_inbound_at"`
	LastOutboundAt     *time.Time      `json:"last_outbound_at"`
	InteractionCount   int             `json:"interaction_count"`
	CadenceStatus      string          `json:"cadence_status,omitempty"`
	DueAt              *time.Time      `json:"due_at,omitempty"`
	InitiatedByContact int             `json:"initiated_by_contact"`
	InitiatedByOwner   int             `json:"initiated_by_owner"`
	Monthly            [12]int         `json:"monthly"`
	Brief              *UntrustedText  `json:"brief,omitempty"`
	Agenda             []UntrustedText `json:"agenda,omitempty"`
}
type Anchor struct {
	Ref      string    `json:"ref"`
	Kind     string    `json:"kind"`
	Time     time.Time `json:"time"`
	Filtered bool      `json:"filtered"`
}
type Candidate struct {
	Truncated      bool    `json:"truncated,omitzero"`
	PersonID       int64   `json:"person_id"`
	PersonUID      string  `json:"person_uid"`
	ParticipantIDs []int64 `json:"participant_ids"`
	DisplayName    string  `json:"display_name"`
	Address        string  `json:"address"`
}
type OpenLoops struct {
	Items              []Entry `json:"items"`
	Truncated          bool    `json:"truncated"`
	CoverageIncomplete bool    `json:"coverage_incomplete"`
	SkippedUnparseable int     `json:"skipped_unparseable"`
}

// Day contains either a summary or detailed entries, never both.
type Day struct {
	Date     string      `json:"date"`
	Summary  *DaySummary `json:"summary,omitempty"`
	Meetings []Entry     `json:"meetings,omitempty"`
	Bursts   []Entry     `json:"bursts,omitempty"`
	Calls    []Entry     `json:"calls,omitempty"`
	Notes    []Entry     `json:"notes,omitempty"`
}
type DaySummary struct {
	PeopleCount     int      `json:"people_count"`
	PeopleTruncated bool     `json:"people_truncated,omitzero"`
	Meetings        int      `json:"meetings"`
	People          []Person `json:"people"`
	Truncated       bool     `json:"truncated"`
}
type Omitted struct {
	Budget map[string]any `json:"budget"`
	Noise  int            `json:"noise"`
	Cold   int            `json:"cold"`
}

// MeetingEntry is also the payload of an unlinked transcript entry.
type MeetingEntry struct {
	AttendeeCount       int           `json:"attendee_count"`
	AttendeesTruncated  bool          `json:"attendees_truncated,omitzero"`
	PeopleCount         int           `json:"people_count"`
	PeopleTruncated     bool          `json:"people_truncated,omitzero"`
	Title               UntrustedText `json:"untrusted_text"`
	Start               time.Time     `json:"start,omitzero"`
	End                 time.Time     `json:"end,omitzero"`
	Time                time.Time     `json:"time,omitzero"`
	AllDay              bool          `json:"all_day"`
	Status              string        `json:"status,omitempty"`
	DeletedFromSource   bool          `json:"deleted_from_source,omitempty,omitzero"`
	Attendees           []Person      `json:"attendees,omitempty"`
	People              []Person      `json:"people,omitempty"`
	Attended            bool          `json:"attended"`
	LinkedTranscriptIDs []int64       `json:"linked_transcript_ids,omitempty"`
	PossiblyRelated     []string      `json:"possibly_related,omitempty"`
	ActionItemCount     int           `json:"action_item_count"`
}
type BurstEntry struct {
	PeopleCount      int           `json:"people_count"`
	PeopleTruncated  bool          `json:"people_truncated,omitzero"`
	Channel          string        `json:"channel"`
	People           []Person      `json:"people"`
	Inbound          int           `json:"inbound"`
	Outbound         int           `json:"outbound"`
	FirstAt          time.Time     `json:"first_at"`
	LastAt           time.Time     `json:"last_at"`
	Snippet          UntrustedText `json:"untrusted_text"`
	LatestMessageIDs []int64       `json:"latest_message_ids"`
	Copies           map[int64]int `json:"copies,omitempty"`
	copyKeys         map[string]struct{}
}
type MessageEntry struct {
	PeopleCount     int           `json:"people_count"`
	PeopleTruncated bool          `json:"people_truncated,omitzero"`
	Direction       string        `json:"direction"`
	People          []Person      `json:"people"`
	Snippet         UntrustedText `json:"untrusted_text"`
}
type CallEntry struct {
	PeopleCount     int      `json:"people_count"`
	PeopleTruncated bool     `json:"people_truncated,omitzero"`
	Outcome         string   `json:"outcome"`
	DurationSeconds *float64 `json:"duration_seconds"`
	Precision       string   `json:"precision,omitempty"`
	Date            string   `json:"date,omitempty"`
	People          []Person `json:"people"`
}
type LoopEntry struct {
	Kind   string `json:"kind"`
	Age    string `json:"age"`
	Person Person `json:"person"`
}
type NoteEntry struct {
	PeopleCount     int           `json:"people_count"`
	PeopleTruncated bool          `json:"people_truncated,omitzero"`
	Text            UntrustedText `json:"untrusted_text"`
	People          []Person      `json:"people"`
}

// Validate checks the tagged union and its independently resolvable anchor.
func (e Entry) Validate() error {
	count := 0
	for _, present := range []bool{e.Meeting != nil, e.Burst != nil, e.Message != nil, e.Call != nil, e.Loop != nil, e.Note != nil} {
		if present {
			count++
		}
	}
	valid := false
	switch e.Kind {
	case entryKindMeeting, "transcript":
		valid = e.Meeting != nil
	case "burst":
		valid = e.Burst != nil
	case "message":
		valid = e.Message != nil
	case "call":
		valid = e.Call != nil
	case "loop":
		valid = e.Loop != nil
	case "note":
		valid = e.Note != nil
	}
	if count != 1 || !valid {
		return fmt.Errorf("%w: entry must have exactly its kind's payload", ErrInvalidRequest)
	}
	switch e.Kind {
	case entryKindMeeting:
		valid = strings.HasPrefix(e.AnchorRef, "evt:") && e.AnchorRef == e.Ref
	case "burst":
		valid = strings.HasPrefix(e.Key, "burst:") && e.AnchorRef == e.Key
	case "note":
		_, err := time.Parse(time.DateOnly, e.AnchorRef)
		valid = err == nil
	default:
		valid = strings.HasPrefix(e.Ref, "msg:") && e.AnchorRef == e.Ref
	}
	parsed, err := ParseAnchor(e.AnchorRef, time.UTC)
	if err != nil || !valid || (e.Kind == entryKindMeeting && parsed.Kind != entryKindMeeting) || (e.Kind == "burst" && parsed.Kind != "burst") || (e.Kind == "note" && parsed.Kind != "date") || ((e.Kind == "message" || e.Kind == "transcript" || e.Kind == "call" || e.Kind == "loop") && parsed.Kind != "message") {
		return fmt.Errorf("%w: invalid entry anchor", ErrInvalidRequest)
	}
	return nil
}
