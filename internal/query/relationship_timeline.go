package query

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	defaultRelationshipTimelineLimit = 100
	maxRelationshipTimelineLimit     = 500
)

// TimelineRow is one entry in a relationship timeline: either a single
// message (email, event, meeting) or a chat_burst summarizing every chat
// message one counterpart's cluster exchanged with the owner on a single
// local day in one (source, conversation).
//
// FirstAt is only meaningful for chat_burst rows (the earliest message in
// the burst); for single-message rows it equals OccurredAt.
type TimelineRow struct {
	Key        string    `json:"key"`  // message:<id> | burst:<source>:<conversation>:<yyyy-mm-dd>
	Kind       string    `json:"kind"` // email | event | meeting | item | chat_burst
	Title      string    `json:"title"`
	Preview    string    `json:"preview"`
	OccurredAt time.Time `json:"occurred_at"` // burst: latest message time
	// FirstAt's omitempty never actually omits (time.Time is a struct, not a
	// pointer) — kept anyway to match the task's binding contract verbatim.
	FirstAt         time.Time `json:"first_at,omitempty"` //nolint:modernize // see comment above
	MessageCount    int64     `json:"message_count"`
	SourceID        int64     `json:"source_id"`
	ConversationID  *int64    `json:"conversation_id,omitempty"`
	AnchorMessageID *int64    `json:"anchor_message_id,omitempty"`
	HasAttachments  bool      `json:"has_attachments"`
}

// RelationshipTimelineRequest scopes and pages one counterpart's timeline.
// CanonicalID must already be resolved (see ResolveCanonicalParticipant);
// the query then expands it back out to every member of that cluster so no
// member's alias messages are missed.
type RelationshipTimelineRequest struct {
	CanonicalID int64
	Timezone    string // validated IANA name; "" = UTC
	Context     Context
	Limit       int
	Offset      int
}

// RelationshipTimelineResponse is the ranked page plus the cache/identity
// revisions it was computed against, for cursor-drift detection.
type RelationshipTimelineResponse struct {
	Rows             []TimelineRow
	TotalCount       int64
	CacheRevision    string
	IdentityRevision int64
}

// RelationshipTimeline returns a canonical identity cluster's interactions
// as a modality-neutral timeline. Email, calendar, and meeting entries are
// one row per message; chat messages are grouped into "bursts" — one row
// per (source, conversation, local calendar day) — because a chat
// conversation can contain thousands of short back-and-forth messages that
// would otherwise drown out the rarer email/meeting entries.
//
// The local day boundary is computed by DuckDB's ICU-backed timezone()
// function (statically linked into the go-duckdb prebuilt binary — see
// TestDuckDBTimezoneConversionUsesBundledICU), not by decay-style date_diff
// arithmetic: occurred_at is a naive TIMESTAMP holding a UTC instant, so the
// column is first anchored to UTC and then re-expressed in the requested
// zone's wall-clock time before formatting the date.
func (e *DuckDBEngine) RelationshipTimeline(ctx context.Context, request RelationshipTimelineRequest) (*RelationshipTimelineResponse, error) {
	if e.analyticsDir == "" {
		return nil, &CacheUnavailableError{Readiness: CacheAbsent}
	}
	if err := validateRelationshipTimelineRequest(request); err != nil {
		return nil, err
	}
	release, err := e.acquireQuerySlot(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	state, err := ReadCacheSyncState(e.analyticsDir)
	if err != nil {
		return nil, fmt.Errorf("read committed cache state: %w", err)
	}

	members, err := e.clusterMembers(ctx, request.CanonicalID)
	if err != nil {
		return nil, err
	}
	// Cluster membership scopes the timeline, but any caller-supplied
	// participant filter (request.Context.ParticipantIDs, set via a
	// "participant" filter dimension) must further restrict it rather than
	// be silently overwritten — the filter is part of the cursor hash and
	// the caller has no way to tell it was dropped. buildExploreConditions
	// only expresses one participant-membership OR-group per Context, so
	// the cluster-membership condition is built separately here and AND'd
	// onto whatever buildExploreConditions produced for the caller's own
	// Context (including any participant filter already in it).
	conditions, args := buildExploreConditions(ExploreRequest{Context: request.Context})
	membershipCondition, membershipArgs := participantMembershipCondition(members)
	if membershipCondition != "" {
		if conditions == "true" {
			conditions = membershipCondition
		} else {
			conditions += " AND " + membershipCondition
		}
		args = append(args, membershipArgs...)
	}

	timezone := request.Timezone
	if timezone == "" {
		timezone = "UTC"
	}
	args = append(args, timezone)
	limit := request.Limit
	if limit == 0 {
		limit = defaultRelationshipTimelineLimit
	}
	args = append(args, limit, request.Offset)

	rows, err := e.db.QueryContext(ctx, buildRelationshipTimelineSQL(conditions), args...)
	if err != nil {
		return nil, fmt.Errorf("query relationship timeline: %w", err)
	}
	defer func() { _ = rows.Close() }()

	response := &RelationshipTimelineResponse{
		Rows:             make([]TimelineRow, 0),
		CacheRevision:    state.Revision(),
		IdentityRevision: state.IdentityRevision,
	}
	for rows.Next() {
		var row TimelineRow
		var key string
		var anchorMessageID, conversationID sql.NullInt64
		if err := rows.Scan(
			&key, &row.Kind, &anchorMessageID, &conversationID, &row.OccurredAt, &row.FirstAt,
			&row.SourceID, &row.Title, &row.Preview, &row.MessageCount, &row.HasAttachments,
			&response.TotalCount,
		); err != nil {
			return nil, fmt.Errorf("scan relationship timeline row: %w", err)
		}
		row.Key = key
		if row.Title == row.Preview {
			row.Title = FlattenSnippet(row.Title)
		}
		row.Preview = FlattenSnippet(row.Preview)
		if anchorMessageID.Valid {
			row.AnchorMessageID = &anchorMessageID.Int64
		}
		if conversationID.Valid {
			row.ConversationID = &conversationID.Int64
		}
		response.Rows = append(response.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate relationship timeline: %w", err)
	}
	return response, nil
}

// ResolveCanonicalParticipant maps a participant ID to its canonical
// identity cluster ID via the committed participant_clusters dataset. A
// participant absent from that dataset belongs to no recorded cluster and
// is its own single-member canonical ID.
func (e *DuckDBEngine) ResolveCanonicalParticipant(ctx context.Context, participantID int64) (int64, error) {
	if e.analyticsDir == "" {
		return 0, &CacheUnavailableError{Readiness: CacheAbsent}
	}
	release, err := e.acquireQuerySlot(ctx)
	if err != nil {
		return 0, err
	}
	defer release()

	queryText := fmt.Sprintf("SELECT canonical_id FROM read_parquet('%s') WHERE participant_id = ?", e.parquetPath(datasetParticipantClusters))
	var canonicalID int64
	err = e.db.QueryRowContext(ctx, queryText, participantID).Scan(&canonicalID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return participantID, nil
	case err != nil:
		return 0, fmt.Errorf("resolve canonical participant: %w", err)
	default:
		return canonicalID, nil
	}
}

// clusterMembers returns every participant ID whose canonical identity is
// canonicalID, via the committed participant_clusters dataset. LinkCluster
// (and Store.ParticipantClusters, which it mirrors) writes a self-row for
// the canonical participant too, so a genuine cluster's members are found
// with no participants-table join. A canonicalID with no rows at all is a
// single-member cluster of itself (never linked).
func (e *DuckDBEngine) clusterMembers(ctx context.Context, canonicalID int64) ([]int64, error) {
	queryText := fmt.Sprintf("SELECT participant_id FROM read_parquet('%s') WHERE canonical_id = ?", e.parquetPath(datasetParticipantClusters))
	rows, err := e.db.QueryContext(ctx, queryText, canonicalID)
	if err != nil {
		return nil, fmt.Errorf("resolve cluster members: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var members []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan cluster member: %w", err)
		}
		members = append(members, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate cluster members: %w", err)
	}
	if len(members) == 0 {
		members = []int64{canonicalID}
	}
	return members, nil
}

// participantMembershipCondition builds the OR-of-any-member SQL fragment
// matching buildExploreConditions' ParticipantIDs shape (sender, entry
// participant, or conversation participant), for the cluster-membership
// scope RelationshipTimeline AND's onto — rather than substitutes for — any
// caller-supplied Context.ParticipantIDs filter. Returns ("", nil) for an
// empty member list (never happens in practice: clusterMembers always
// returns at least the canonical ID itself).
func participantMembershipCondition(members []int64) (string, []any) {
	if len(members) == 0 {
		return "", nil
	}
	parts := make([]string, len(members))
	args := make([]any, 0, len(members)*3)
	for i, id := range members {
		parts[i] = "(sender_id = ? OR list_contains(participant_ids, ?) OR list_contains(conversation_participant_ids, ?))"
		args = append(args, id, id, id)
	}
	return "(" + strings.Join(parts, " OR ") + ")", args
}

func validateRelationshipTimelineRequest(request RelationshipTimelineRequest) error {
	if request.Offset < 0 || request.Limit < 0 || request.Limit > maxRelationshipTimelineLimit {
		return fmt.Errorf("%w: timeline page is outside the supported range", ErrInvalidExploreRequest)
	}
	if request.Context.Deletion != DeletionAny && request.Context.Deletion != DeletionActive && request.Context.Deletion != DeletionDeleted {
		return fmt.Errorf("%w: unknown deletion filter %q", ErrInvalidExploreRequest, request.Context.Deletion)
	}
	if request.Timezone != "" {
		if _, err := time.LoadLocation(request.Timezone); err != nil {
			return fmt.Errorf("%w: invalid timezone %q: %w", ErrInvalidExploreRequest, request.Timezone, err)
		}
	}
	return nil
}

// buildRelationshipTimelineSQL builds the full timeline query. It shares
// the "filtered"/"classified" CTEs with buildExploreLogicalSQLWithCandidateRank
// (see buildExploreFilteredClassifiedCTE) but defines its own final CTE:
// unlike Explore's per-conversation-lifetime chat grouping, chat messages
// here are grouped per local calendar day so a years-long conversation
// doesn't collapse into a single burst. The trailing "?" binds the IANA
// timezone name once, in the "day_bucketed" CTE; both branches read the
// precomputed local_day column rather than re-evaluating timezone() per row.
func buildRelationshipTimelineSQL(conditions string) string {
	return buildExploreFilteredClassifiedCTE(conditions, "NULL::BIGINT") + `
, day_bucketed AS (
    SELECT *, strftime(timezone(?, timezone('UTC', occurred_at)), '%Y-%m-%d') AS local_day
    FROM classified
), timeline_entries AS (
    SELECT
        'message:' || CAST(message_id AS VARCHAR) AS entry_key,
        entry_kind,
        message_id AS anchor_message_id,
        conversation_id,
        occurred_at,
        occurred_at AS first_at,
        source_id,
        COALESCE(NULLIF(subject, ''), NULLIF(conversation_title, ''), snippet, '') AS title,
        snippet AS preview,
        1::BIGINT AS message_count,
        has_attachments
    FROM day_bucketed
    WHERE NOT is_chat

    UNION ALL

    SELECT
        'burst:' || CAST(source_id AS VARCHAR) || ':' || CAST(conversation_id AS VARCHAR) || ':' || local_day AS entry_key,
        'chat_burst' AS entry_kind,
        arg_max(message_id, struct_pack(occurred_at := occurred_at, message_id := message_id)) AS anchor_message_id,
        conversation_id,
        MAX(occurred_at) AS occurred_at,
        MIN(occurred_at) AS first_at,
        source_id,
        COALESCE(NULLIF(MAX(conversation_title), ''), 'Conversation') AS title,
        arg_max(snippet, struct_pack(occurred_at := occurred_at, message_id := message_id)) AS preview,
        COUNT(*)::BIGINT AS message_count,
        bool_or(has_attachments) AS has_attachments
    FROM day_bucketed
    WHERE is_chat
    GROUP BY source_id, conversation_id, local_day
), counted AS (
    SELECT *, COUNT(*) OVER () AS total_count FROM timeline_entries
)
SELECT
    entry_key, entry_kind, anchor_message_id, conversation_id, occurred_at, first_at,
    source_id, title, preview, message_count, has_attachments, total_count
FROM counted
ORDER BY occurred_at DESC, entry_key ASC
LIMIT ? OFFSET ?`
}
