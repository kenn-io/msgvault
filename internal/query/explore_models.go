package query

import "time"

// ExploreRequest is the canonical modality-neutral analytical request. The
// API layer validates the finite grouping/sort catalog before constructing it.
type ExploreRequest struct {
	Context      Context      `json:"context"`
	Search       SearchSpec   `json:"search"`
	Grouping     []GroupSpec  `json:"grouping,omitempty"`
	Presentation Presentation `json:"presentation,omitempty"`
	Sort         []SortSpec   `json:"sort,omitempty"`
	Page         PageSpec     `json:"page"`
}

// Context narrows the archive before logical chat rows are aggregated.
type Context struct {
	SourceIDs      []int64        `json:"source_ids,omitempty"`
	ParticipantIDs []int64        `json:"participant_ids,omitempty"`
	Domains        []string       `json:"domains,omitempty"`
	MessageTypes   []string       `json:"message_types,omitempty"`
	After          *time.Time     `json:"after,omitempty"`
	Before         *time.Time     `json:"before,omitempty"`
	Deletion       DeletionFilter `json:"deletion,omitempty"`
}

type DeletionFilter string

const (
	DeletionAny     DeletionFilter = ""
	DeletionActive  DeletionFilter = "active"
	DeletionDeleted DeletionFilter = "deleted"
)

type SearchMode string

const (
	// MaxExploreCandidateMessageIDs is the shared cross-engine transfer ceiling
	// for ranked search candidates projected by DuckDB.
	MaxExploreCandidateMessageIDs = 10_000

	SearchNone     SearchMode = ""
	SearchFullText SearchMode = "full_text"
	SearchSemantic SearchMode = "semantic"
	SearchHybrid   SearchMode = "hybrid"
)

// SearchSpec carries candidates and index provenance resolved by the
// authoritative lexical/vector backends. DuckDB projects those candidates
// into logical entry rows; it does not scan SQLite as a fallback.
type SearchSpec struct {
	Mode                SearchMode `json:"mode,omitempty"`
	Query               string     `json:"query,omitempty"`
	CandidateMessageIDs []int64    `json:"-"`
	// LexicalCandidateMessageIDs is the complete exact FTS membership for a
	// hybrid query. CandidateMessageIDs remains the bounded fused ranking pool.
	LexicalCandidateMessageIDs []int64 `json:"-"`
	LexicalIndexRevision       string  `json:"-"`
	VectorGeneration           *int64  `json:"-"`
	// CandidatePoolSaturated reports that the authoritative search backend
	// matched more messages than could be transferred into the analytical
	// engine. Consumers must not present counts over this set as exact.
	CandidatePoolSaturated bool `json:"-"`
}

type GroupSpec struct {
	Dimension string `json:"dimension"`
}

type Presentation string

const (
	PresentationDefault  Presentation = ""
	PresentationTable    Presentation = "table"
	PresentationTimeline Presentation = "timeline"
	PresentationFiles    Presentation = "files"
)

type SortSpec struct {
	Field     string `json:"field"`
	Direction string `json:"direction"`
}

type PageSpec struct {
	Limit  int `json:"limit,omitempty"`
	Offset int `json:"offset,omitempty"`
}

type EntryKind string

const (
	EntryEmail        EntryKind = "email"
	EntryConversation EntryKind = "conversation"
	EntryEvent        EntryKind = "event"
	EntryMeeting      EntryKind = "meeting"
	EntryItem         EntryKind = "item"
)

type MatchSummary struct {
	LexicalMatchCount *int64   `json:"lexical_match_count,omitempty"`
	StrongestExcerpt  string   `json:"strongest_excerpt,omitempty"`
	SemanticScore     *float64 `json:"semantic_score,omitempty"`
}

type SearchProvenance struct {
	LexicalIndexRevision string `json:"lexical_index_revision,omitempty"`
	VectorGeneration     *int64 `json:"vector_generation,omitempty"`
}

// EntryRow is one logical archive row. Chat/text rows aggregate messages only
// after Context has been applied; other modalities retain durable item units.
type EntryRow struct {
	Key               string       `json:"key"`
	Kind              EntryKind    `json:"kind"`
	AnchorMessageID   *int64       `json:"anchor_message_id,omitempty"`
	ConversationID    *int64       `json:"conversation_id,omitempty"`
	OccurredAt        time.Time    `json:"occurred_at"`
	Match             MatchSummary `json:"match"`
	SourceID          int64        `json:"source_id"`
	SourceType        string       `json:"source_type"`
	SourceIdentifier  string       `json:"source_identifier"`
	MessageType       string       `json:"message_type"`
	ConversationType  string       `json:"conversation_type"`
	Title             string       `json:"title"`
	Preview           string       `json:"preview"`
	ParticipantIDs    []int64      `json:"participant_ids,omitempty"`
	ParticipantLabels []string     `json:"participant_labels,omitempty"`
	// StrongestMatchedMessageID is bounded internal semantic-ranking state.
	// It is never serialized and is nil for no-search and full-text rows.
	StrongestMatchedMessageID *int64 `json:"-"`
	MessageCount              int64  `json:"message_count"`
	HasAttachments            bool   `json:"has_attachments"`
	AttachmentCount           int64  `json:"attachment_count"`
	AttachmentSize            int64  `json:"attachment_size"`
	DeletedFromSource         bool   `json:"deleted_from_source"`
	// CounterpartParticipantID is the smallest participant ID on the entry
	// that is not the archive owner (owners resolved through the same
	// cluster-aware canon the Relationships ranking uses — see
	// buildExploreSQL). It is nil when the owner set is unknown (the
	// owner_participants dataset has no rows) or when every participant on
	// the entry is the owner: never guessed from participant_ids[0] alone.
	CounterpartParticipantID *int64 `json:"counterpart_participant_id,omitempty"`
}

type ExploreResponse struct {
	Rows             []EntryRow       `json:"rows"`
	TotalCount       int64            `json:"total_count"`
	CacheRevision    string           `json:"cache_revision"`
	SearchProvenance SearchProvenance `json:"search_provenance"`
}

// ExploreCoverageRequest selects the message-level population eligible for a
// semantic index. Coverage remains message-level because vector generations
// embed messages even when exploration later aggregates them into logical
// conversation rows.
type ExploreCoverageRequest struct {
	Context        Context `json:"context"`
	AfterMessageID int64   `json:"after_message_id,omitempty"`
	Limit          int     `json:"limit,omitempty"`
}

type ExploreCoverageResponse struct {
	MessageIDs         []int64 `json:"message_ids"`
	NextAfterMessageID *int64  `json:"next_after_message_id,omitempty"`
	CacheRevision      string  `json:"cache_revision"`
}

type ExploreGroupRequest struct {
	Explore   ExploreRequest `json:"explore"`
	Dimension string         `json:"dimension"`
	Sort      SortSpec       `json:"sort"`
	Page      PageSpec       `json:"page"`
}

type ExploreGroupRow struct {
	Key            string    `json:"key"`
	Label          string    `json:"label"`
	Count          int64     `json:"count"`
	EstimatedBytes int64     `json:"estimated_bytes"`
	LatestAt       time.Time `json:"latest_at"`
}

type ExploreGroupResponse struct {
	Rows             []ExploreGroupRow `json:"rows"`
	TotalCount       int64             `json:"total_count"`
	CacheRevision    string            `json:"cache_revision"`
	SearchProvenance SearchProvenance  `json:"search_provenance"`
}

type ExploreSelectionRequest struct {
	Explore                    ExploreRequest `json:"explore"`
	IncludedKeys               []string       `json:"included_keys,omitempty"`
	ExcludedKeys               []string       `json:"excluded_keys,omitempty"`
	IncludeDeletableMessageIDs bool           `json:"-"`
}

type ExploreSelectionStats struct {
	Count               int64            `json:"count"`
	EstimatedBytes      int64            `json:"estimated_bytes"`
	DeletableCount      int64            `json:"deletable_count"`
	FileCount           int64            `json:"file_count"`
	ExportableCount     int64            `json:"exportable_count"`
	OpenableCount       int64            `json:"openable_count"`
	CacheRevision       string           `json:"cache_revision"`
	SearchProvenance    SearchProvenance `json:"search_provenance"`
	DeletableMessageIDs []int64          `json:"-"`
	RawExportMessageID  *int64           `json:"-"`
}

type ExploreFilesRequest struct {
	Explore ExploreRequest `json:"explore"`
	Page    PageSpec       `json:"page"`
}

type ExploreFileFact struct {
	ID               int64     `json:"id"`
	Key              string    `json:"key"`
	EntryKey         string    `json:"entry_key"`
	MessageID        int64     `json:"message_id"`
	ConversationID   int64     `json:"conversation_id"`
	OccurredAt       time.Time `json:"occurred_at"`
	SourceID         int64     `json:"source_id"`
	SourceIdentifier string    `json:"source_identifier"`
	Title            string    `json:"title"`
	Filename         string    `json:"filename"`
	MimeType         string    `json:"mime_type"`
	Size             int64     `json:"size"`
}

type ExploreFilesResponse struct {
	Files            []ExploreFileFact `json:"files"`
	TotalCount       int64             `json:"total_count"`
	CacheRevision    string            `json:"cache_revision"`
	SearchProvenance SearchProvenance  `json:"search_provenance"`
}

type ExploreMatchCountsRequest struct {
	Explore ExploreRequest `json:"explore"`
	RowKeys []string       `json:"row_keys"`
}

type ExploreMatchCountsResponse struct {
	Counts           map[string]int64 `json:"counts"`
	CacheRevision    string           `json:"cache_revision"`
	SearchProvenance SearchProvenance `json:"search_provenance"`
}
