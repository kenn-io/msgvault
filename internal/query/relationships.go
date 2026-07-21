package query

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"
)

const (
	relationshipWeightSent     = 2.0
	relationshipWeightMeetings = 3.0
	relationshipWeightReceived = 1.0
	relationshipBreadthStep    = 0.25
	relationshipHalfLifeDays   = 365.0
)

const (
	defaultRelationshipsLimit = 100
	maxRelationshipsLimit     = 500
)

// RelationshipSignals holds the decayed interaction sums and raw counts that
// feed RelationshipScore. Decay is applied in SQL (see (*DuckDBEngine).
// Relationships), not here.
type RelationshipSignals struct {
	SentToThem        float64   `json:"sent_to_them"`
	ReceivedFromThem  float64   `json:"received_from_them"`
	MeetingsTogether  float64   `json:"meetings_together"`
	SentCount         int64     `json:"sent_count"`
	MeetingCount      int64     `json:"meeting_count"`
	Modalities        int       `json:"modalities"`
	LastInteractionAt time.Time `json:"last_interaction_at"`
}

// RelationshipScore applies the spec's weights, decay having been applied in
// SQL: score = (2.0*sent + 3.0*meetings + 1.0*ln(1+received)) * (1 + 0.25*(modalities-1)).
//
// The received term is log-compressed (ln(1+received_decayed), via
// math.Log1p) while sent and meetings stay linear. On real archives, inbound
// volume (mailing lists, CI/notification bots, co-workers whose tickets you
// merely receive) grows far faster than genuine reciprocal contact, so a
// linear received term let high-volume one-way senders outrank people with
// real back-and-forth or shared meetings. Log-compression keeps received
// mail a positive signal without letting its raw volume dominate the score.
// received_from_them in RelationshipSignals and the API response still
// reports the raw (pre-log) decayed value — only this score composition
// changes.
func RelationshipScore(s RelationshipSignals) float64 {
	base := relationshipWeightSent*s.SentToThem +
		relationshipWeightMeetings*s.MeetingsTogether +
		relationshipWeightReceived*math.Log1p(s.ReceivedFromThem)
	breadth := 1.0
	if s.Modalities > 1 {
		breadth = 1.0 + relationshipBreadthStep*float64(s.Modalities-1)
	}
	return base * breadth
}

// RelationshipRow is one ranked counterpart: a canonical identity cluster
// scored against the archive owner's interactions with it.
type RelationshipRow struct {
	CanonicalID  int64               `json:"canonical_id"`
	DisplayLabel string              `json:"display_label"`
	MemberIDs    []int64             `json:"member_ids"`
	Score        float64             `json:"score"`
	Signals      RelationshipSignals `json:"signals"`
	LastAt       time.Time           `json:"last_at"`
}

// RelationshipsRequest scopes and pages a relationship ranking query. Now is
// injected so decay is deterministic in tests; if left zero, the engine
// defaults it to time.Now().UTC() (API callers always set it explicitly so
// results are reproducible across a paginated request).
type RelationshipsRequest struct {
	Context Context
	ShowAll bool
	Limit   int
	Offset  int
	Now     time.Time
}

// RelationshipsResponse is the ranked page plus the cache/identity revisions
// it was computed against, for cursor-drift detection.
type RelationshipsResponse struct {
	Rows             []RelationshipRow
	TotalCount       int64
	CacheRevision    string
	IdentityRevision int64
}

// RelationshipAnalyzer is separate from Engine so relationship ranking can
// only be served from a committed canonical cache snapshot with resolved
// identity clusters.
type RelationshipAnalyzer interface {
	Relationships(ctx context.Context, request RelationshipsRequest) (*RelationshipsResponse, error)

	// RelationshipTimeline returns one counterpart's interactions as a
	// modality-neutral timeline, with chat messages bucketed into local-day
	// bursts. See relationship_timeline.go.
	RelationshipTimeline(ctx context.Context, request RelationshipTimelineRequest) (*RelationshipTimelineResponse, error)

	// ResolveCanonicalParticipant maps any participant ID to its canonical
	// identity cluster ID (itself, if it belongs to no recorded cluster),
	// via the committed participant_clusters dataset. Callers (e.g. the
	// relationship timeline route) use this to accept any member ID in a
	// URL path while scoping queries by the single canonical ID.
	ResolveCanonicalParticipant(ctx context.Context, participantID int64) (int64, error)
}

// Relationships ranks every non-owner canonical identity the archive owner
// has interacted with, by a reciprocity-weighted, time-decayed score. Owner
// clusters are excluded (you don't rank yourself); by default only
// counterparts with at least one sent message or one shared meeting are
// returned (the reciprocity gate), filtering out inbound-only newsletters.
func (e *DuckDBEngine) Relationships(ctx context.Context, request RelationshipsRequest) (*RelationshipsResponse, error) {
	if e.analyticsDir == "" {
		return nil, &CacheUnavailableError{Readiness: CacheAbsent}
	}
	if err := validateRelationshipsRequest(request); err != nil {
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

	now := request.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	conditions, args := buildExploreConditions(ExploreRequest{Context: request.Context})
	queryText := buildRelationshipsSQL(conditions, e.parquetPath(datasetParticipantClusters), e.parquetPath(datasetOwnerParticipants))
	args = append(args, math.Ln2/relationshipHalfLifeDays, now.UTC())

	rows, err := e.db.QueryContext(ctx, queryText, args...)
	if err != nil {
		return nil, fmt.Errorf("query analytical relationships: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var candidates []RelationshipRow
	for rows.Next() {
		var row RelationshipRow
		var memberIDsJSON string
		if err := rows.Scan(
			&row.CanonicalID, &row.DisplayLabel, &memberIDsJSON,
			&row.Signals.SentToThem, &row.Signals.SentCount,
			&row.Signals.ReceivedFromThem, &row.Signals.MeetingsTogether, &row.Signals.MeetingCount,
			&row.Signals.Modalities, &row.LastAt,
		); err != nil {
			return nil, fmt.Errorf("scan analytical relationship: %w", err)
		}
		if err := json.Unmarshal([]byte(memberIDsJSON), &row.MemberIDs); err != nil {
			return nil, fmt.Errorf("decode relationship member IDs: %w", err)
		}
		row.Signals.LastInteractionAt = row.LastAt
		row.Score = RelationshipScore(row.Signals)
		if !request.ShowAll && row.Signals.SentCount < 1 && row.Signals.MeetingCount < 1 {
			continue
		}
		candidates = append(candidates, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate analytical relationships: %w", err)
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Score != candidates[j].Score {
			return candidates[i].Score > candidates[j].Score
		}
		if !candidates[i].LastAt.Equal(candidates[j].LastAt) {
			return candidates[i].LastAt.After(candidates[j].LastAt)
		}
		return candidates[i].DisplayLabel < candidates[j].DisplayLabel
	})

	limit := request.Limit
	if limit == 0 {
		limit = defaultRelationshipsLimit
	}
	response := &RelationshipsResponse{
		Rows:             make([]RelationshipRow, 0),
		TotalCount:       int64(len(candidates)),
		CacheRevision:    state.Revision(),
		IdentityRevision: state.IdentityRevision,
	}
	if request.Offset < len(candidates) {
		end := min(request.Offset+limit, len(candidates))
		response.Rows = candidates[request.Offset:end]
	}
	return response, nil
}

func validateRelationshipsRequest(request RelationshipsRequest) error {
	if request.Offset < 0 || request.Limit < 0 || request.Limit > maxRelationshipsLimit {
		return fmt.Errorf("%w: relationships page is outside the supported range", ErrInvalidExploreRequest)
	}
	if request.Context.Deletion != DeletionAny && request.Context.Deletion != DeletionActive && request.Context.Deletion != DeletionDeleted {
		return fmt.Errorf("%w: unknown deletion filter %q", ErrInvalidExploreRequest, request.Context.Deletion)
	}
	return nil
}

// buildRelationshipsSQL builds the full relationship-ranking query. Decay is
// computed in SQL from two trailing bound parameters (decay rate, now); the
// gate, score, and sort order are applied in Go so RelationshipScore stays
// the single source of truth for the weights.
//
// Owners may themselves be clustered, so "owner_canon" resolves every owner
// participant to its cluster canonical ID and interactions exclude any
// canonical identity that appears there — you never rank yourself.
// "owner_participant_ids" expands owner_canon back out to every raw
// participant ID sharing an owner's canonical cluster (not just the raw
// owner_participants rows), so a meeting attended under an alias linked only
// via participant_clusters still counts as "together" (with_owner, computed
// once per entry before the UNNEST that fans an entry out per participant).
//
// A single logical entry can list several raw participant IDs that resolve
// to the same canonical cluster (e.g. cc'ing a contact's work and personal
// addresses); the DISTINCT in "interactions" collapses those back to one row
// per (entry, canonical_id) so aggregation doesn't double-count the entry.
//
// A meeting/event the owner did not attend contributes no signal for any
// attendee (no sent/received count, no modality — see "aggregated" below),
// so "interactions" excludes such rows entirely rather than merely zeroing
// their contribution: left in, they would still feed MAX(occurred_at) and
// inflate LastAt/LastInteractionAt with a meeting the owner never attended.
//
// received_from_them credits only the AUTHOR of an incoming entry
// ("author_links": the recipient_type='from' participant, or messages.
// sender_id for sources that record senders directly), never co-recipients.
// Without that restriction a mailing-list address accumulates one received
// unit for every subscription message it merely appears on as a recipient,
// which ranked lists and bots above real people. Chat conversations
// ('conversation' entries) are exempt: a grouped chat has no single author,
// so its per-conversation credit for every non-owner member is unchanged.
// Non-author co-recipient rows stay in "interactions" so LastAt keeps
// matching the timeline's "all shared messages" scope.
func buildRelationshipsSQL(conditions, clustersGlob, ownersGlob string) string {
	return buildExploreLogicalSQL(conditions) + fmt.Sprintf(`
), clusters AS (
    SELECT participant_id, canonical_id FROM read_parquet('%s')
), owners AS (
    SELECT DISTINCT participant_id FROM read_parquet('%s')
), canon AS (
    SELECT p.id AS participant_id, COALESCE(c.canonical_id, p.id) AS canonical_id
    FROM participants p LEFT JOIN clusters c ON c.participant_id = p.id
), owner_canon AS (
    SELECT DISTINCT cn.canonical_id FROM owners o JOIN canon cn ON cn.participant_id = o.participant_id
), owner_participant_ids AS (
    SELECT DISTINCT cn.participant_id FROM canon cn
    WHERE cn.canonical_id IN (SELECT canonical_id FROM owner_canon)
), le_with_owner AS (
    SELECT le.*, list_has_any(le.participant_ids, (SELECT list(participant_id) FROM owner_participant_ids)) AS with_owner
    FROM logical_entries le
), author_links AS (
    SELECT mr.message_id, cn.canonical_id
    FROM message_recipients mr
    JOIN canon cn ON cn.participant_id = mr.participant_id
    WHERE mr.recipient_type = 'from'
    UNION
    SELECT m.id AS message_id, cn.canonical_id
    FROM messages m
    JOIN canon cn ON cn.participant_id = m.sender_id
    WHERE m.sender_id IS NOT NULL
), interactions AS (
    SELECT DISTINCT
        le.entry_key,
        cn.canonical_id,
        le.entry_kind,
        le.occurred_at,
        le.is_from_me,
        le.with_owner,
        EXISTS (SELECT 1 FROM author_links al
                WHERE al.message_id = le.anchor_message_id
                  AND al.canonical_id = cn.canonical_id) AS is_author,
        exp(-? * date_diff('day', le.occurred_at, ?)) AS decay
    FROM le_with_owner le
    CROSS JOIN UNNEST(le.participant_ids) AS pid(participant_id)
    JOIN canon cn ON cn.participant_id = pid.participant_id
    WHERE cn.canonical_id NOT IN (SELECT canonical_id FROM owner_canon)
      AND NOT (le.entry_kind IN ('event','meeting') AND NOT le.with_owner)
), aggregated AS (
    SELECT
        canonical_id,
        SUM(CASE WHEN is_from_me AND entry_kind IN ('email','conversation','item') THEN decay ELSE 0 END) AS sent_decayed,
        COUNT(CASE WHEN is_from_me AND entry_kind IN ('email','conversation','item') THEN 1 END) AS sent_count,
        SUM(CASE WHEN NOT is_from_me
                  AND (entry_kind = 'conversation' OR (entry_kind IN ('email','item') AND is_author))
                 THEN decay ELSE 0 END) AS received_decayed,
        SUM(CASE WHEN entry_kind IN ('event','meeting') AND with_owner THEN decay ELSE 0 END) AS meetings_decayed,
        COUNT(CASE WHEN entry_kind IN ('event','meeting') AND with_owner THEN 1 END) AS meeting_count,
        COUNT(DISTINCT CASE
            WHEN entry_kind IN ('event','meeting') AND with_owner THEN 'meeting'
            WHEN entry_kind IN ('event','meeting') THEN NULL
            WHEN entry_kind = 'conversation' THEN 'chat'
            ELSE 'email' END) AS modalities,
        MAX(occurred_at) AS last_at
    FROM interactions
    GROUP BY canonical_id
)
SELECT
    a.canonical_id,
    (SELECT COALESCE(NULLIF(TRIM(p2.display_name), ''), NULLIF(TRIM(p2.phone_number), ''), NULLIF(TRIM(p2.email_address), ''),
        (SELECT COALESCE(NULLIF(TRIM(pi.display_value), ''), pi.identifier_value) FROM participant_identifiers pi
         WHERE pi.participant_id = p2.id ORDER BY pi.is_primary DESC, pi.identifier_type, pi.identifier_value LIMIT 1),
        'Unknown person #' || CAST(p2.id AS VARCHAR))
     FROM participants p2 WHERE p2.id = a.canonical_id) AS display_label,
    CAST(COALESCE((SELECT to_json(list(cn2.participant_id ORDER BY cn2.participant_id))
        FROM canon cn2 WHERE cn2.canonical_id = a.canonical_id), '[]') AS VARCHAR) AS member_ids,
    a.sent_decayed, a.sent_count, a.received_decayed, a.meetings_decayed, a.meeting_count, a.modalities, a.last_at
FROM aggregated a`, clustersGlob, ownersGlob)
}
