package query

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
// the query matches it against relationship_activity's alias-merged
// canonical_id, so no cluster member's alias messages are missed.
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
// Event and meeting rows appear only when one of the archive owner's
// identities participated, matching the relationship ranking's
// owner-participation rule in the relationship index — a shared-calendar
// event the owner never attended is not an interaction and would otherwise
// surface here despite contributing no ranking signal.
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

	// Cluster membership scopes the timeline, but any caller-supplied
	// participant filter (request.Context.ParticipantIDs and any
	// conjunctive AdditionalParticipantGroups, set via repeated "participant"
	// filter dimensions) must further restrict it rather than be silently
	// overwritten — the filter is part of the cursor hash and the caller has
	// no way to tell it was dropped. The subject cluster-membership
	// condition is its own AND'd predicate with different semantics (any
	// message touching the subject's cluster) than a caller participant
	// filter. That caller filter is first widened across its identity
	// cluster (like Explore/Files) so a secondary participant filter by
	// canonical ID also matches alias activity; the subject
	// cluster-membership predicate is already cluster-correct because
	// relationship_activity rows bake the alias-merged canonical_id for
	// every direct edge and conversation membership.
	explore, err := e.expandParticipantFilterClusters(ctx, ExploreRequest{Context: request.Context})
	if err != nil {
		return nil, err
	}
	// Caller filters render natively over relationship_activity facts
	// (buildIdentityFactConditions), so participant/domain filters become
	// edge semi-joins — the aggregated participant list columns of the
	// legacy view are never touched.
	conditions, factArgs := buildIdentityFactConditions(explore, e.identityActivityPath())
	args := make([]any, 0, len(factArgs)+4)
	args = append(args, request.CanonicalID)
	args = append(args, factArgs...)

	timezone := request.Timezone
	if timezone == "" {
		timezone = relationshipScoringTimezone
	}
	args = append(args, timezone)
	limit := request.Limit
	if limit == 0 {
		limit = defaultRelationshipTimelineLimit
	}
	args = append(args, limit, request.Offset)

	queryText := buildRelationshipTimelineSQL(
		conditions,
		e.identityActivityPath(),
		quoteIdentitySQLPath(e.parquetGlob()),
		quoteIdentitySQLPath(e.parquetPath(datasetConversations)),
	)
	rows, err := e.db.QueryContext(ctx, queryText, args...)
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

// buildRelationshipTimelineSQL builds the full timeline query. The
// population is index-native: subject_facts reads one counterpart's
// message facts straight from relationship_activity (which bakes
// occurred_at, entry_kind, chat classification, attachment counts, and
// deletion state per message), so the whole-archive analytical_entries
// assembly — which dominated person-click latency on production archives —
// never runs. Titles and previews resolve for the ≤limit page rows only,
// from the base messages/conversations Parquet directly (the same
// expressions the analytical_entries view uses); the view itself is never
// touched, because a join against it cannot push the page's ID filter
// through the view's aggregate subqueries and materializes them for the
// whole archive.
// Unlike Explore's per-conversation-lifetime chat grouping, chat messages
// are grouped per local calendar day so a years-long conversation doesn't
// collapse into a single burst. The "?" after the fact conditions binds
// the IANA timezone name once, in the "day_bucketed" CTE; both branches
// read the precomputed local_day column rather than re-evaluating
// timezone() per row.
//
// Event/meeting rows require owner participation, mirroring the
// relationship ranking exactly, including its person-level global-owner
// semantics: a subscribed or shared-calendar event the owner never attended
// is not an interaction with the counterpart, and the ranking already
// contributes no signal for it — the timeline must not display what the
// ranking ignores. with_owner comes from the member_owner CTE over the
// subject's messages, whose rows already carry the alias-merged
// canonical_id and owner-cluster is_owner flag the relationship index
// computed at build time, so owner participation under a clustered alias
// resolves identically on both surfaces. with_owner counts only direct
// owner edges: the ranking derives non-chat owner presence from direct
// participants, so a silent owner on a conversation roster must not make
// the timeline treat an event as attended. Email and chat rows are
// untouched: the timeline scopes by counterpart-cluster membership, not
// owner involvement, so a counterpart's email to a third party still
// appears.
//
// has_attachments comes from the baked per-message has_attachments flag
// (MIME structure at sync time), not attachment_count: extraction can lag
// or fail, leaving the flag true with a zero count, and the indicator must
// match what the message list shows for the same message.
func buildRelationshipTimelineSQL(conditions, activityGlob, messagesGlob, conversationsGlob string) string {
	activityScan := `read_parquet('` + activityGlob + `',
        hive_partitioning=true, union_by_name=true)`
	// The membership IN-subquery is a semi-join whose build side is the
	// subject's bare message IDs (compact even for archive-scale clusters);
	// the outer scan then folds per-message facts and owner presence into
	// ONE grouped hash over the subject's messages only. An explicit JOIN
	// against a DISTINCT-ids CTE reads the same, but the planner has built
	// the hash on the full edge scan side under that shape — which pins the
	// whole interactive memory budget regardless of subject size.
	return `
WITH subject_facts AS (
    SELECT a.message_id,
        any_value(a.conversation_id) AS conversation_id,
        any_value(a.source_id) AS source_id,
        any_value(a.occurred_at) AS occurred_at,
        any_value(a.entry_kind) AS entry_kind,
        any_value(a.is_chat) AS is_chat,
        any_value(a.has_attachments) AS has_attachments,
        bool_or(a.is_owner AND a.is_direct) AS with_owner
    FROM ` + activityScan + ` a
    WHERE a.message_id IN (
        SELECT f.message_id
        FROM ` + activityScan + ` f
        WHERE f.canonical_id = ? AND (` + conditions + `)
    )
    GROUP BY a.message_id
), day_bucketed AS (
    SELECT s.*,
        strftime(timezone(?, timezone('UTC', s.occurred_at)), '%Y-%m-%d') AS local_day
    FROM subject_facts s
), timeline_entries AS (
    SELECT
        'message:' || CAST(message_id AS VARCHAR) AS entry_key,
        entry_kind,
        message_id AS anchor_message_id,
        conversation_id,
        occurred_at,
        occurred_at AS first_at,
        source_id,
        1::BIGINT AS message_count,
        has_attachments
    FROM day_bucketed
    WHERE NOT is_chat
      AND NOT (entry_kind IN ('event','meeting') AND NOT with_owner)

    UNION ALL

    SELECT
        'burst:' || CAST(source_id AS VARCHAR) || ':' || CAST(conversation_id AS VARCHAR) || ':' || local_day AS entry_key,
        'chat_burst' AS entry_kind,
        arg_max(message_id, struct_pack(occurred_at := occurred_at, message_id := message_id)) AS anchor_message_id,
        conversation_id,
        MAX(occurred_at) AS occurred_at,
        MIN(occurred_at) AS first_at,
        source_id,
        COUNT(*)::BIGINT AS message_count,
        bool_or(has_attachments) AS has_attachments
    FROM day_bucketed
    WHERE is_chat
    GROUP BY source_id, conversation_id, local_day
), counted AS (
    SELECT *, COUNT(*) OVER () AS total_count FROM timeline_entries
), page AS (
    SELECT * FROM counted
    ORDER BY occurred_at DESC, entry_key ASC
    LIMIT ? OFFSET ?
)
SELECT
    p.entry_key, p.entry_kind, p.anchor_message_id, p.conversation_id, p.occurred_at, p.first_at,
    p.source_id,
    CASE WHEN p.entry_kind = 'chat_burst'
        THEN COALESCE(NULLIF(CAST(c.title AS VARCHAR), ''), 'Conversation')
        ELSE COALESCE(NULLIF(CAST(m.subject AS VARCHAR), ''),
            NULLIF(CAST(c.title AS VARCHAR), ''), CAST(m.snippet AS VARCHAR), '')
    END AS title,
    COALESCE(CAST(m.snippet AS VARCHAR), '') AS preview,
    p.message_count, p.has_attachments, p.total_count
FROM page p
LEFT JOIN read_parquet('` + messagesGlob + `',
    hive_partitioning=true, union_by_name=true) m ON m.id = p.anchor_message_id
LEFT JOIN read_parquet('` + conversationsGlob + `') c ON c.id = p.conversation_id
ORDER BY p.occurred_at DESC, p.entry_key ASC`
}
