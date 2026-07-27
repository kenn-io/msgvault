package query

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/bits"
	"sort"
	"strconv"
	"time"

	"go.kenn.io/msgvault/internal/identityindex"
)

const (
	relationshipWeightSent     = 2.0
	relationshipWeightMeetings = 3.0
	relationshipWeightReceived = 1.0
	relationshipBreadthStep    = 0.25
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
//
// Unfiltered requests read compact anchored rollups. Filtered requests retain
// exact logical-entry semantics by reducing the narrow identity fact and edge
// datasets; neither path reconstructs the legacy wide analytical entry graph.
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
	// Widen any participant filter across its whole identity cluster before
	// rendering conditions, so ranking a canonical person credits activity
	// recorded under a linked alias — matching Explore/Files.
	explore, err := e.expandParticipantFilterClusters(ctx, ExploreRequest{Context: request.Context})
	if err != nil {
		return nil, err
	}
	anchor, err := relationshipAnchorDate(state.RelationshipAnchorDate)
	if err != nil {
		return nil, err
	}
	candidates, err := e.queryRelationshipCandidates(
		ctx,
		explore,
		request.ShowAll,
		now.UTC(),
		anchor,
	)
	if err != nil {
		return nil, err
	}

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

// queryRelationshipCandidates runs either the compact rollup query or the
// narrow filtered reduction, then applies the shared gate, score, and total
// ordering in Go.
func (e *DuckDBEngine) queryRelationshipCandidates(
	ctx context.Context,
	explore ExploreRequest,
	showAll bool,
	now time.Time,
	anchor time.Time,
) ([]RelationshipRow, error) {
	var queryText string
	var queryArgs []any
	unfiltered := identityRequestIsUnfiltered(explore)
	if unfiltered {
		queryText, queryArgs = buildRelationshipRollupSQL(now)
		queryText = e.resolveIdentityPathPlaceholders(queryText)
	} else {
		queryText, queryArgs = e.buildFilteredRelationshipsSQL(explore, now)
	}

	rows, err := e.db.QueryContext(ctx, queryText, queryArgs...)
	if err != nil {
		return nil, fmt.Errorf("query indexed relationships: %w", err)
	}
	defer func() { _ = rows.Close() }()

	candidates := make([]RelationshipRow, 0)
	for rows.Next() {
		var row RelationshipRow
		var memberIDsJSON string
		var rowAnchor time.Time
		var modalityMask uint8
		if err := rows.Scan(
			&row.CanonicalID, &row.DisplayLabel, &memberIDsJSON, &rowAnchor,
			&row.Signals.SentToThem, &row.Signals.SentCount,
			&row.Signals.ReceivedFromThem, &row.Signals.MeetingsTogether, &row.Signals.MeetingCount,
			&modalityMask, &row.LastAt,
		); err != nil {
			return nil, fmt.Errorf("scan indexed relationship: %w", err)
		}
		if unfiltered && !rowAnchor.Equal(anchor) {
			return nil, fmt.Errorf(
				"%w: relationship rollup anchor %s does not match marker anchor %s",
				errInvalidCacheState,
				rowAnchor.UTC().Format(time.DateOnly),
				anchor.Format(time.DateOnly),
			)
		}
		if err := json.Unmarshal([]byte(memberIDsJSON), &row.MemberIDs); err != nil {
			return nil, fmt.Errorf("decode relationship member IDs: %w", err)
		}
		row.Signals.Modalities = modalitiesFromMask(modalityMask)
		row.Signals.LastInteractionAt = row.LastAt
		row.Score = RelationshipScore(row.Signals)
		if !showAll && row.Signals.SentCount < 1 && row.Signals.MeetingCount < 1 {
			continue
		}
		candidates = append(candidates, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate indexed relationships: %w", err)
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Score != candidates[j].Score {
			return candidates[i].Score > candidates[j].Score
		}
		if !candidates[i].LastAt.Equal(candidates[j].LastAt) {
			return candidates[i].LastAt.After(candidates[j].LastAt)
		}
		if candidates[i].DisplayLabel != candidates[j].DisplayLabel {
			return candidates[i].DisplayLabel < candidates[j].DisplayLabel
		}
		// CanonicalID is the unique final tie-breaker: without it, rows with
		// identical score, timestamp, and label keep whatever order the
		// database returned them in, which can differ across memo evictions
		// or restarts and duplicate/omit rows across offset-based pages.
		return candidates[i].CanonicalID < candidates[j].CanonicalID
	})
	return candidates, nil
}

func relationshipAnchorDate(value string) (time.Time, error) {
	anchor, err := time.Parse(time.DateOnly, value)
	if err != nil || anchor.Format(time.DateOnly) != value {
		return time.Time{}, fmt.Errorf(
			"%w: invalid relationship anchor date %q",
			errInvalidCacheState,
			value,
		)
	}
	return anchor.UTC(), nil
}

func modalitiesFromMask(mask uint8) int {
	return bits.OnesCount8(mask & (identityindex.ModalityEmail |
		identityindex.ModalityChat | identityindex.ModalityMeeting))
}

func buildRelationshipRollupSQL(now time.Time) (string, []any) {
	rollups := quoteIdentityPathPlaceholder(identityindex.DatasetRelationships)
	future := quoteIdentityPathPlaceholder(identityindex.DatasetRelationshipFuture)
	directory := quoteIdentityPathPlaceholder(identityindex.DatasetDirectory)
	queryText := `
WITH request_clock AS (
	SELECT ?::DOUBLE AS decay_rate, CAST(? AS DATE) AS request_date
), future_signals AS (
	SELECT f.canonical_id,
	       sum(f.sent_units * exp(-c.decay_rate * greatest(
	           0, date_diff('day', f.event_date, c.request_date))))::DOUBLE
	           AS sent_decayed,
	       sum(f.received_units * exp(-c.decay_rate * greatest(
	           0, date_diff('day', f.event_date, c.request_date))))::DOUBLE
	           AS received_decayed,
	       sum(f.meeting_units * exp(-c.decay_rate * greatest(
	           0, date_diff('day', f.event_date, c.request_date))))::DOUBLE
	           AS meetings_decayed
	FROM read_parquet('` + future + `') f
	CROSS JOIN request_clock c
	GROUP BY f.canonical_id
), indexed_relationships AS (
	SELECT r.canonical_id,
	       r.anchor_date,
	       r.sent_decayed * exp(-c.decay_rate * greatest(
	           0, date_diff('day', r.anchor_date, c.request_date))) +
	           coalesce(f.sent_decayed, 0) AS sent_decayed,
	       r.sent_count,
	       r.received_decayed * exp(-c.decay_rate * greatest(
	           0, date_diff('day', r.anchor_date, c.request_date))) +
	           coalesce(f.received_decayed, 0) AS received_decayed,
	       r.meetings_decayed * exp(-c.decay_rate * greatest(
	           0, date_diff('day', r.anchor_date, c.request_date))) +
	           coalesce(f.meetings_decayed, 0) AS meetings_decayed,
	       r.meeting_count,
	       r.modality_mask,
	       r.last_at
	FROM read_parquet('` + rollups + `') r
	CROSS JOIN request_clock c
	LEFT JOIN future_signals f USING (canonical_id)
)
SELECT r.canonical_id,
       d.display_label,
       CAST(to_json(d.member_ids) AS VARCHAR) AS member_ids,
       r.anchor_date,
       r.sent_decayed,
       r.sent_count,
       r.received_decayed,
       r.meetings_decayed,
       r.meeting_count,
       r.modality_mask,
       r.last_at
FROM indexed_relationships r
JOIN read_parquet('` + directory + `') d USING (canonical_id)
WHERE NOT d.is_owner`
	return queryText, []any{
		identityindex.RelationshipDecayRate,
		duckDBDateParam(now),
	}
}

func (e *DuckDBEngine) buildFilteredRelationshipsSQL(
	explore ExploreRequest,
	now time.Time,
) (string, []any) {
	logicalSQL, args := e.buildIdentityLogicalSQL(explore, "")
	directory := quoteIdentitySQLPath(
		e.identityDatasetPath(identityindex.DatasetDirectory),
	)
	queryText := logicalSQL + `,
relationship_interactions AS (
	SELECT p.*,
	       CASE WHEN p.is_from_me
	                 AND p.entry_kind IN ('email','conversation','item')
	            THEN 1::BIGINT ELSE 0::BIGINT END AS sent_units,
	       CASE WHEN NOT p.is_from_me
	                 AND (p.entry_kind = 'conversation'
	                      OR (p.entry_kind IN ('email','item') AND p.is_author))
	            THEN 1::BIGINT ELSE 0::BIGINT END AS received_units,
	       CASE WHEN p.entry_kind IN ('event','meeting') AND p.with_owner
	            THEN 1::BIGINT ELSE 0::BIGINT END AS meeting_units,
	       CASE
	           WHEN p.entry_kind IN ('event','meeting') AND p.with_owner
	               THEN ` + strconv.FormatUint(uint64(identityindex.ModalityMeeting), 10) + `::UTINYINT
	           WHEN p.entry_kind = 'conversation'
	               THEN ` + strconv.FormatUint(uint64(identityindex.ModalityChat), 10) + `::UTINYINT
	           WHEN p.entry_kind IN ('email','item')
	               THEN ` + strconv.FormatUint(uint64(identityindex.ModalityEmail), 10) + `::UTINYINT
	           ELSE 0::UTINYINT
	       END AS modality_mask,
	       exp(-? * greatest(
	           0, date_diff('day', p.occurred_at, CAST(? AS TIMESTAMP)))) AS decay
	FROM logical_people p
	WHERE NOT p.is_owner
	  AND NOT (p.entry_kind IN ('event','meeting') AND NOT p.with_owner)
), aggregated AS (
	SELECT canonical_id,
	       sum(sent_units * decay)::DOUBLE AS sent_decayed,
	       sum(sent_units)::BIGINT AS sent_count,
	       sum(received_units * decay)::DOUBLE AS received_decayed,
	       sum(meeting_units * decay)::DOUBLE AS meetings_decayed,
	       sum(meeting_units)::BIGINT AS meeting_count,
	       bit_or(modality_mask)::UTINYINT AS modality_mask,
	       max(occurred_at)::TIMESTAMP AS last_at
	FROM relationship_interactions
	GROUP BY canonical_id
)
SELECT a.canonical_id,
       d.display_label,
       CAST(to_json(d.member_ids) AS VARCHAR) AS member_ids,
       CAST(? AS DATE) AS anchor_date,
       a.sent_decayed,
       a.sent_count,
       a.received_decayed,
       a.meetings_decayed,
       a.meeting_count,
       a.modality_mask,
       a.last_at
FROM aggregated a
JOIN read_parquet('` + directory + `') d USING (canonical_id)`
	args = append(args,
		identityindex.RelationshipDecayRate,
		duckDBDateParam(now),
		duckDBDateParam(now),
	)
	return queryText, args
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
