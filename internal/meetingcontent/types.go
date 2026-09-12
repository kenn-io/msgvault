// Package meetingcontent normalizes archived meeting evidence and renders
// deterministic, size-bounded context packets.
package meetingcontent

import (
	"errors"
	"time"
)

type State string

const (
	StateAvailable        State = "available"
	StateEmpty            State = "empty"
	StateUnsupported      State = "unsupported"
	StateUnavailable      State = "unavailable"
	StateOmittedByRequest State = "omitted_by_request"
	StateTruncated        State = "truncated"
)

type Coverage string

const (
	CoverageAvailable   Coverage = "available"
	CoveragePartial     Coverage = "partial"
	CoverageUnsupported Coverage = "unsupported"
	CoverageUnavailable Coverage = "unavailable"
)

type Status string

const (
	StatusPending   Status = "pending"
	StatusCompleted Status = "completed"
	StatusCancelled Status = "cancelled"
	StatusUnknown   Status = "unknown"
)

type DurationBasis string

const (
	DurationProvider       DurationBasis = "provider"
	DurationScheduled      DurationBasis = "scheduled"
	DurationTranscriptSpan DurationBasis = "transcript_span"
)

type Format string

const (
	FormatJSON     Format = "json"
	FormatMarkdown Format = "markdown"
)

type Section struct {
	State  State  `json:"state"`
	Reason string `json:"reason,omitempty"`
	Text   string `json:"text,omitempty"`
}

type Segment struct {
	Speaker       string     `json:"speaker,omitempty"`
	Text          string     `json:"text"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
	EndedAt       *time.Time `json:"ended_at,omitempty"`
	OffsetSeconds *float64   `json:"offset_seconds,omitempty"`
}

type Transcript struct {
	State    State     `json:"state"`
	Reason   string    `json:"reason,omitempty"`
	Text     string    `json:"text,omitempty"`
	Segments []Segment `json:"segments,omitempty"`
}

type Action struct {
	Ordinal       int    `json:"ordinal"`
	SourceID      string `json:"source_id,omitempty"`
	Title         string `json:"title"`
	Description   string `json:"description,omitempty"`
	AssigneeName  string `json:"assignee_name,omitempty"`
	AssigneeEmail string `json:"assignee_email,omitempty"`
	Status        Status `json:"status"`
	SourceStatus  string `json:"source_status,omitempty"`
	DueDate       string `json:"due_date,omitempty"`
	Origin        string `json:"origin"`
	Locator       string `json:"locator"`
}

type Content struct {
	Summary            Section       `json:"summary"`
	Notes              Section       `json:"notes"`
	Transcript         Transcript    `json:"transcript"`
	Actions            []Action      `json:"actions"`
	SourceParticipants []Participant `json:"source_participants,omitempty"`
	ActionCoverage     Coverage      `json:"action_coverage"`
	ActionReason       string        `json:"action_reason,omitempty"`
	DurationSeconds    *float64      `json:"duration_seconds"`
	DurationBasis      DurationBasis `json:"duration_basis,omitempty"`
}

type Participant struct {
	ParticipantID *int64 `json:"participant_id,omitempty"`
	Name          string `json:"name,omitempty"`
	Email         string `json:"email,omitempty"`
	Role          string `json:"role"`
}

type MeetingRef struct {
	MessageID        int64      `json:"message_id"`
	ConversationID   int64      `json:"conversation_id"`
	SourceID         int64      `json:"source_id"`
	SourceType       string     `json:"source_type"`
	SourceIdentifier string     `json:"source_identifier"`
	SourceMessageID  string     `json:"source_message_id"`
	Title            string     `json:"title"`
	OccurredAt       *time.Time `json:"occurred_at"`
	ArchivePath      string     `json:"archive_path"`
}

type Entry struct {
	Meeting      MeetingRef    `json:"meeting"`
	Participants []Participant `json:"participants"`
	Content      Content       `json:"content"`
}

type Truncation struct {
	MessageID     int64  `json:"message_id"`
	Section       string `json:"section"`
	OriginalBytes int64  `json:"original_bytes"`
	IncludedBytes int64  `json:"included_bytes"`
	OmittedItems  int    `json:"omitted_items,omitempty"`
}

type Packet struct {
	SchemaVersion       int          `json:"schema_version"`
	ArchiveUID          string       `json:"archive_uid"`
	RequestedMessageIDs []int64      `json:"requested_message_ids"`
	Meetings            []Entry      `json:"meetings"`
	OmittedMessageIDs   []int64      `json:"omitted_message_ids"`
	Truncated           bool         `json:"truncated"`
	Truncations         []Truncation `json:"truncations"`
}

type PacketOptions struct {
	Format            Format `json:"format"`
	IncludeTranscript bool   `json:"include_transcript"`
	MaxBytes          int    `json:"max_bytes"`
}

type PacketResult struct {
	SchemaVersion     int     `json:"schema_version"`
	Format            Format  `json:"format"`
	Content           string  `json:"content"`
	ContentBytes      int     `json:"content_bytes"`
	Truncated         bool    `json:"truncated"`
	OmittedMessageIDs []int64 `json:"omitted_message_ids"`
}

type ActionRow struct {
	Meeting MeetingRef `json:"meeting"`
	Action  Action     `json:"action"`
}

type ActionCoverage struct {
	MeetingCount int64 `json:"meeting_count"`
	Available    int64 `json:"available"`
	Partial      int64 `json:"partial"`
	Unsupported  int64 `json:"unsupported"`
	Unavailable  int64 `json:"unavailable"`
}

type ScopeProvenance struct {
	Kind                 string `json:"kind"`
	CacheRevision        string `json:"cache_revision,omitempty"`
	LexicalIndexRevision string `json:"lexical_index_revision,omitempty"`
	VectorGeneration     *int64 `json:"vector_generation,omitempty"`
	CandidateSnapshotID  string `json:"candidate_snapshot_id,omitempty"`
}

type ActionsPage struct {
	SchemaVersion int             `json:"schema_version"`
	ArchiveUID    string          `json:"archive_uid"`
	Rows          []ActionRow     `json:"rows"`
	TotalCount    int64           `json:"total_count"`
	NextCursor    string          `json:"next_cursor,omitempty"`
	Coverage      ActionCoverage  `json:"coverage"`
	Scope         ScopeProvenance `json:"scope"`
}

type DurationTotals struct {
	MeetingCount         int64    `json:"meeting_count"`
	KnownDurationCount   int64    `json:"known_duration_count"`
	UnknownDurationCount int64    `json:"unknown_duration_count"`
	TotalKnownSeconds    float64  `json:"total_known_seconds"`
	AverageKnownSeconds  *float64 `json:"average_known_seconds"`
}

type BasisTotals struct {
	Basis        DurationBasis `json:"basis"`
	Count        int64         `json:"count"`
	TotalSeconds float64       `json:"total_seconds"`
}

type MonthTotals struct {
	Month  string         `json:"month"`
	Totals DurationTotals `json:"totals"`
}

type Metrics struct {
	SchemaVersion   int             `json:"schema_version"`
	ArchiveUID      string          `json:"archive_uid"`
	Totals          DurationTotals  `json:"totals"`
	FirstMeetingAt  *time.Time      `json:"first_meeting_at"`
	LastMeetingAt   *time.Time      `json:"last_meeting_at"`
	UndatedCount    int64           `json:"undated_count"`
	DurationByBasis []BasisTotals   `json:"duration_by_basis"`
	Months          []MonthTotals   `json:"months"`
	Scope           ScopeProvenance `json:"scope"`
}

var ErrBudgetTooSmall = errors.New("meeting context budget too small")
