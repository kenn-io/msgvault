package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

var ErrPersonUIDGone = errors.New("person UID retired without a surviving person")

// MessagingRouteEvidence is captured by provider sync, never by discovery.
// The roster IDs bind route metadata to the exact persisted roster snapshot.
type MessagingRouteEvidence struct {
	ChatID             string    `json:"chat_id"`
	AccountID          string    `json:"account_id"`
	Network            string    `json:"network"`
	NetworkLabel       string    `json:"network_label"`
	ProviderType       string    `json:"provider_type"`
	ObservedAt         time.Time `json:"observed_at"`
	MembershipComplete bool      `json:"membership_complete"`
	ParticipantIDs     []int64   `json:"participant_ids"`
	SelfParticipantIDs []int64   `json:"self_participant_ids"`
	MemberChatIDs      []string  `json:"member_chat_ids"`
	MergedIntoChatID   string    `json:"merged_into_chat_id"`
	Merged             bool      `json:"merged"`
	Truncated          bool      `json:"truncated"`
	Failure            string    `json:"failure"`
}

type PersonMessagingRouteQuery struct {
	PersonUID           string
	Network             string
	SourceID            int64
	Limit               int
	AfterConversationID int64
	AfterContactPointID int64
	AfterObservationID  int64
	AfterSuggestionID   int64
}

type MessagingRoute struct {
	ConversationID       int64      `json:"conversation_id"`
	ProviderChatID       string     `json:"provider_chat_id"`
	SourceID             int64      `json:"source_id"`
	SourceType           string     `json:"source_type"`
	AccountID            string     `json:"account_id"`
	Network              string     `json:"network"`
	NetworkLabel         string     `json:"network_label"`
	ConversationType     string     `json:"conversation_type"`
	Status               string     `json:"status" enum:"archive_verified,unresolved,group_context,merged_container"`
	Reasons              []string   `json:"reasons"`
	BoundParticipantIDs  []int64    `json:"bound_participant_ids"`
	EvidenceTruncated    bool       `json:"evidence_truncated"`
	MembershipComplete   bool       `json:"membership_complete"`
	ObservedAt           *time.Time `json:"observed_at,omitempty"`
	SourceLastSyncAt     *time.Time `json:"source_last_sync_at,omitempty"`
	MemberChatIDs        []string   `json:"member_chat_ids"`
	MissingMemberChatIDs []string   `json:"missing_member_chat_ids"`
	MergedIntoChatID     string     `json:"merged_into_chat_id"`
}

type ContactPointPage struct {
	Items       []PersonContactPoint `json:"items"`
	HasMore     bool                 `json:"has_more"`
	NextAfterID int64                `json:"next_after_id"`
}
type ContactObservationPage struct {
	Items       []ParticipantContactObservation `json:"items"`
	HasMore     bool                            `json:"has_more"`
	NextAfterID int64                           `json:"next_after_id"`
}

// IdentityRouteSuggestion deliberately omits unbounded candidate evidence.
type IdentityRouteSuggestion struct {
	ID        int64  `json:"id"`
	LeftKind  string `json:"left_kind"`
	LeftID    int64  `json:"left_id"`
	RightKind string `json:"right_kind"`
	RightID   int64  `json:"right_id"`
	Basis     string `json:"basis"`
	State     string `json:"state"`
}
type IdentityRouteSuggestionPage struct {
	Items       []IdentityRouteSuggestion `json:"items"`
	HasMore     bool                      `json:"has_more"`
	NextAfterID int64                     `json:"next_after_id"`
}
type MessagingRoutePage struct {
	Items       []MessagingRoute `json:"items"`
	HasMore     bool             `json:"has_more"`
	NextAfterID int64            `json:"next_after_id"`
}
type PersonMessagingRoutesPage struct {
	RequestedUID          string                      `json:"requested_uid"`
	PersonUID             string                      `json:"person_uid"`
	PersonID              int64                       `json:"person_id"`
	PersonRevision        int64                       `json:"person_revision"`
	IdentityRevision      int64                       `json:"identity_revision"`
	AliasReason           string                      `json:"alias_reason"`
	Freshness             string                      `json:"freshness"`
	CheckedAt             time.Time                   `json:"checked_at"`
	Routes                MessagingRoutePage          `json:"routes"`
	ContactPoints         ContactPointPage            `json:"contact_points"`
	Observations          ContactObservationPage      `json:"observations"`
	UnreviewedSuggestions IdentityRouteSuggestionPage `json:"unreviewed_suggestions"`
	Warnings              []string                    `json:"warnings"`
}

const maxRouteEvidenceIDs = 100
const routeEvidenceMaxAge = 7 * 24 * time.Hour

func ValidatePersonMessagingRouteQuery(q PersonMessagingRouteQuery) error {
	validUID := strings.TrimSpace(q.PersonUID) != "" && utf8.ValidString(q.PersonUID) && len(q.PersonUID) <= 256
	negativeID := q.SourceID < 0 || q.AfterConversationID < 0 || q.AfterContactPointID < 0 ||
		q.AfterObservationID < 0 || q.AfterSuggestionID < 0
	if !validUID || q.Limit < 0 || q.Limit > 100 || negativeID {
		return fmt.Errorf("%w: person_uid required; limit 1..100; IDs nonnegative", ErrInvalidContactLookup)
	}
	if len(q.Network) > 64 {
		return ErrInvalidContactLookup
	}
	for _, r := range q.Network {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return fmt.Errorf("%w: network must be a lowercase service name", ErrInvalidContactLookup)
		}
	}
	return nil
}

// GetPersonMessagingRoutesContext reads one consistent archive snapshot.
// Separate keyset cursors bound every section; paging between calls is live.
func (s *Store) GetPersonMessagingRoutesContext(ctx context.Context, q PersonMessagingRouteQuery) (*PersonMessagingRoutesPage, error) {
	if err := ValidatePersonMessagingRouteQuery(q); err != nil {
		return nil, err
	}
	limit := q.Limit
	if limit == 0 {
		limit = 20
	}
	page := &PersonMessagingRoutesPage{
		RequestedUID:          q.PersonUID,
		Freshness:             "archive_only",
		CheckedAt:             time.Now().UTC(),
		Warnings:              []string{},
		Routes:                MessagingRoutePage{Items: []MessagingRoute{}},
		ContactPoints:         ContactPointPage{Items: []PersonContactPoint{}},
		Observations:          ContactObservationPage{Items: []ParticipantContactObservation{}},
		UnreviewedSuggestions: IdentityRouteSuggestionPage{Items: []IdentityRouteSuggestion{}},
	}
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		if err := resolveMessagingPersonTx(ctx, tx, q.PersonUID, page); err != nil {
			return err
		}
		var err error
		page.IdentityRevision, err = s.currentIdentityRevisionTxContext(ctx, tx)
		if err != nil {
			return err
		}
		if err := readMessagingContacts(ctx, tx, q, page, limit); err != nil {
			return err
		}
		if err := readMessagingSuggestions(ctx, tx, q, page, limit); err != nil {
			return err
		}
		return s.readMessagingRoutes(ctx, tx, q, page, limit)
	})
	if err != nil {
		return nil, fmt.Errorf("get person messaging routes: %w", err)
	}
	if len(page.Routes.Items) == 0 {
		if !page.Routes.HasMore && q.AfterConversationID == 0 && q.Network == "" && q.SourceID == 0 {
			page.Warnings = append(page.Warnings, "no_archived_routes")
		} else {
			page.Warnings = append(page.Warnings, "no_routes_on_page")
		}
	}
	page.Warnings = append(page.Warnings, "archive_evidence_is_not_live_send_authorization", "paging_is_live_between_requests")
	return page, nil
}

func resolveMessagingPersonTx(ctx context.Context, tx *loggedTx, uid string, page *PersonMessagingRoutesPage) error {
	const columns = `SELECT id,vcard_uid,revision FROM persons WHERE `
	err := tx.QueryRowContext(ctx, columns+`vcard_uid=?`, uid).Scan(&page.PersonID, &page.PersonUID, &page.PersonRevision)
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var survivor sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT surviving_person_id,reason FROM person_uid_aliases WHERE retired_uid=?`, uid).Scan(&survivor, &page.AliasReason); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrPersonNotFound
		}
		return err
	}
	if !survivor.Valid {
		return ErrPersonUIDGone
	}
	err = tx.QueryRowContext(ctx, columns+`id=?`, survivor.Int64).Scan(&page.PersonID, &page.PersonUID, &page.PersonRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrPersonUIDGone
	}
	return err
}

func readContactLookupRows[T any](ctx context.Context, tx *loggedTx, query string, scan func(scanner) (*T, error), args ...any) ([]T, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	items := []T{}
	for rows.Next() {
		item, err := scan(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, *item)
	}
	return items, rows.Err()
}

func readMessagingContacts(ctx context.Context, tx *loggedTx, q PersonMessagingRouteQuery, page *PersonMessagingRoutesPage, limit int) error {
	points, err := readContactLookupRows(ctx, tx, personContactPointSelect+`
  WHERE p.person_id=? AND p.active_until IS NULL AND p.superseded_at IS NULL AND p.id>? ORDER BY p.id LIMIT ?`, scanPersonContactPoint, page.PersonID, q.AfterContactPointID, limit+1)
	if err != nil {
		return fmt.Errorf("read route contact points: %w", err)
	}
	if len(points) > limit {
		page.ContactPoints.HasMore = true
		points = points[:limit]
	}
	page.ContactPoints.Items = points
	if len(points) > 0 {
		page.ContactPoints.NextAfterID = points[len(points)-1].Envelope.ID
	}
	observations, err := readContactLookupRows(ctx, tx, participantObservationSelect+`
  WHERE o.active_until IS NULL AND o.superseded_at IS NULL AND o.id>?
  AND EXISTS (SELECT 1 FROM person_participants pp WHERE pp.person_id=? AND pp.participant_id=o.participant_id)
  ORDER BY o.id LIMIT ?`, scanParticipantObservation, q.AfterObservationID, page.PersonID, limit+1)
	if err != nil {
		return fmt.Errorf("read route observations: %w", err)
	}
	if len(observations) > limit {
		page.Observations.HasMore = true
		observations = observations[:limit]
	}
	page.Observations.Items = observations
	if len(observations) > 0 {
		page.Observations.NextAfterID = observations[len(observations)-1].Envelope.ID
	}
	return nil
}

func suggestionTouchesPerson(side string) string {
	return `(` + side + `_kind='person' AND ` + side + `_id=?) OR
 (` + side + `_kind='participant' AND EXISTS (SELECT 1 FROM person_participants pp WHERE pp.person_id=? AND pp.participant_id=c.` + side + `_id)) OR
 (` + side + `_kind='contact_point' AND EXISTS (SELECT 1 FROM person_contact_points cp WHERE cp.person_id=? AND cp.id=c.` + side + `_id)) OR
 (` + side + `_kind='observation' AND EXISTS (SELECT 1 FROM participant_contact_observations o WHERE o.id=c.` + side + `_id AND EXISTS (SELECT 1 FROM person_participants pp WHERE pp.person_id=? AND pp.participant_id=o.participant_id))) OR
 (` + side + `_kind='carddav_resource' AND EXISTS (SELECT 1 FROM carddav_resources cr WHERE cr.person_id=? AND cr.id=c.` + side + `_id))`
}
func readMessagingSuggestions(ctx context.Context, tx *loggedTx, q PersonMessagingRouteQuery, page *PersonMessagingRoutesPage, limit int) error {
	args := []any{q.AfterSuggestionID}
	// Five existing identity endpoint kinds on each side of the candidate.
	for range 10 {
		args = append(args, page.PersonID)
	}
	args = append(args, limit+1)
	items, err := readContactLookupRows(ctx, tx, `SELECT c.id,c.left_kind,c.left_id,c.right_kind,c.right_id,c.basis,c.state
  FROM identity_match_candidates c WHERE c.id>? AND (c.state IN ('candidate','conflict') OR (c.state='accepted' AND c.application_pending=TRUE))
  AND (`+suggestionTouchesPerson("left")+` OR `+suggestionTouchesPerson("right")+`) ORDER BY c.id LIMIT ?`, func(row scanner) (*IdentityRouteSuggestion, error) {
		item := &IdentityRouteSuggestion{}
		err := row.Scan(&item.ID, &item.LeftKind, &item.LeftID, &item.RightKind, &item.RightID, &item.Basis, &item.State)
		return item, err
	}, args...)
	if err != nil {
		return fmt.Errorf("read route suggestions: %w", err)
	}
	if len(items) > limit {
		page.UnreviewedSuggestions.HasMore = true
		items = items[:limit]
	}
	page.UnreviewedSuggestions.Items = items
	if len(items) > 0 {
		page.UnreviewedSuggestions.NextAfterID = items[len(items)-1].ID
	}
	return nil
}

type archivedMessagingRoute struct {
	route         MessagingRoute
	metadata      string
	sourceFailure string
	oversized     bool
	deleted       bool
}

// nativeRouteSourceTypes identify their service independently of labels or
// participant identifiers. They still need roster/self proof to verify.
var nativeRouteSourceTypes = []string{"whatsapp", "slack", sourceTypeDiscord, "matrix", "teams"}

func (s *Store) readMessagingRoutes(
	ctx context.Context, tx *loggedTx, q PersonMessagingRouteQuery, page *PersonMessagingRoutesPage, limit int,
) error {
	metadataBytes := `LENGTH(CAST(c.metadata AS BLOB))`
	if s.IsPostgreSQL() {
		metadataBytes = `OCTET_LENGTH(CAST(c.metadata AS TEXT))`
	}
	query := `SELECT c.id,COALESCE(c.source_conversation_id,''),c.source_id,src.source_type,src.identifier,
 c.conversation_type,
 COALESCE(src.last_sync_at,(SELECT MAX(sr.completed_at) FROM sync_runs sr
  WHERE sr.source_id=src.id AND sr.status='completed' AND sr.errors_count=0)),
 CASE WHEN ` + metadataBytes + `<=65536 THEN COALESCE(CAST(c.metadata AS TEXT),'') ELSE '' END,
 COALESCE(` + metadataBytes + `,0)>65536,
 EXISTS(SELECT 1 FROM messages m WHERE m.conversation_id=c.id)
  AND NOT EXISTS(SELECT 1 FROM messages m WHERE m.conversation_id=c.id AND m.deleted_from_source_at IS NULL),
 COALESCE(srf.failure,'')
 FROM conversations c JOIN sources src ON src.id=c.source_id
 LEFT JOIN source_messaging_route_failures srf ON srf.source_id=src.id
 WHERE c.id>? AND c.conversation_type<>'email_thread'
 AND EXISTS(SELECT 1 FROM conversation_participants cp JOIN person_participants pp ON pp.participant_id=cp.participant_id
  WHERE cp.conversation_id=c.id AND cp.left_at IS NULL AND pp.person_id=?)`
	args := []any{q.AfterConversationID, page.PersonID}
	if q.SourceID > 0 {
		query += ` AND c.source_id=?`
		args = append(args, q.SourceID)
	}
	if q.Network != "" {
		// Native sources carry their network in the source type, so other
		// networks are skipped in SQL. Beeper networks come from route
		// metadata and are filtered after evaluation; unknown ones stay visible.
		query += ` AND (src.source_type NOT IN (` + sqlPlaceholders(len(nativeRouteSourceTypes)) + `) OR src.source_type=?)`
		for _, sourceType := range nativeRouteSourceTypes {
			args = append(args, sourceType)
		}
		args = append(args, q.Network)
	}
	// Bound the number of inspected rows as well as emitted rows.
	query += ` ORDER BY c.id LIMIT ?`
	args = append(args, limit+1)
	archived, err := readContactLookupRows(ctx, tx, query, scanArchivedMessagingRoute, args...)
	if err != nil {
		return fmt.Errorf("read route conversations: %w", err)
	}
	if len(archived) > limit {
		page.Routes.HasMore = true
		archived = archived[:limit]
	}
	for _, item := range archived {
		page.Routes.NextAfterID = item.route.ConversationID
		if err := evaluateMessagingRoute(ctx, tx, page.PersonID, page.CheckedAt, &item); err != nil {
			return err
		}
		if q.Network != "" && item.route.Network != "" && item.route.Network != q.Network {
			continue
		}
		page.Routes.Items = append(page.Routes.Items, item.route)
	}
	return nil
}

func scanArchivedMessagingRoute(row scanner) (*archivedMessagingRoute, error) {
	item := &archivedMessagingRoute{}
	var synced nullableTimestamp
	r := &item.route
	err := row.Scan(&r.ConversationID, &r.ProviderChatID, &r.SourceID, &r.SourceType, &r.AccountID,
		&r.ConversationType, &synced, &item.metadata, &item.oversized, &item.deleted, &item.sourceFailure)
	if synced.Valid {
		r.SourceLastSyncAt = &synced.Time
	}
	return item, err
}

func messagingRoster(ctx context.Context, tx *loggedTx, conversationID, personID int64) (roster, bound []int64, err error) {
	rows, err := tx.QueryContext(ctx, `SELECT cp.participant_id,
 EXISTS(SELECT 1 FROM person_participants pp WHERE pp.participant_id=cp.participant_id AND pp.person_id=?)
 FROM conversation_participants cp WHERE cp.conversation_id=? AND cp.left_at IS NULL
 ORDER BY cp.participant_id LIMIT ?`, personID, conversationID, maxRouteEvidenceIDs+1)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()
	roster = []int64{}
	bound = []int64{}
	for rows.Next() {
		var id int64
		var belongs bool
		if err := rows.Scan(&id, &belongs); err != nil {
			return nil, nil, err
		}
		roster = append(roster, id)
		if belongs {
			bound = append(bound, id)
		}
	}
	return roster, bound, rows.Err()
}

// routeMetadata is the part of conversation metadata that route proof reads.
type routeMetadata struct {
	Route              *MessagingRouteEvidence `json:"messaging_route"`
	MemberCount        *int                    `json:"member_count"`
	MemberCountUnknown bool                    `json:"member_count_unknown"`
}

func evaluateMessagingRoute(
	ctx context.Context, tx *loggedTx, personID int64, now time.Time, item *archivedMessagingRoute,
) error {
	r := &item.route
	r.Status = "unresolved"
	r.Reasons = []string{}
	r.MemberChatIDs = []string{}
	r.MissingMemberChatIDs = []string{}
	roster, bound, err := messagingRoster(ctx, tx, r.ConversationID, personID)
	if err != nil {
		return fmt.Errorf("read route roster: %w", err)
	}
	r.BoundParticipantIDs = bound
	if slices.Contains(nativeRouteSourceTypes, r.SourceType) {
		r.Network = r.SourceType
	}
	if len(roster) > maxRouteEvidenceIDs {
		r.EvidenceTruncated = true
		r.Reasons = append(r.Reasons, "evidence_truncated")
		r.BoundParticipantIDs = bound[:min(len(bound), maxRouteEvidenceIDs)]
	}
	var metadata routeMetadata
	if item.oversized {
		r.Reasons = append(r.Reasons, "route_metadata_oversized")
	}
	if item.metadata != "" {
		if err := json.Unmarshal([]byte(item.metadata), &metadata); err != nil {
			r.Reasons = append(r.Reasons, "route_metadata_invalid")
		}
	}
	if metadata.MemberCountUnknown {
		r.MembershipComplete = false
		r.Reasons = append(r.Reasons, "membership_incomplete")
	}
	if metadata.Route == nil {
		r.Reasons = append(r.Reasons, "route_metadata_missing")
	} else if err := applyRouteEvidence(ctx, tx, personID, now, r, metadata, roster); err != nil {
		return err
	}
	appendRouteSourceReasons(r, item, now)
	finalizeRouteStatus(r)
	return nil
}

func applyRouteEvidence(
	ctx context.Context, tx *loggedTx, personID int64, now time.Time,
	r *MessagingRoute, metadata routeMetadata, roster []int64,
) error {
	e := metadata.Route
	r.NetworkLabel = e.NetworkLabel
	if r.SourceType == "beeper" {
		r.Network = e.Network
	}
	r.MergedIntoChatID = e.MergedIntoChatID
	r.MembershipComplete = e.MembershipComplete && !metadata.MemberCountUnknown
	if !e.ObservedAt.IsZero() {
		r.ObservedAt = &e.ObservedAt
	}
	checkRouteBindings(r, e)
	checkRouteRoster(r, e, metadata.MemberCount, roster)
	if e.ObservedAt.IsZero() || now.Sub(e.ObservedAt) > routeEvidenceMaxAge || e.ObservedAt.After(now.Add(time.Minute)) {
		r.Reasons = append(r.Reasons, "evidence_stale")
	}
	switch e.Failure {
	case "":
	case "account_lookup_failed", "account_not_connected", "account_binding_mismatch",
		"chat_binding_mismatch", "membership_fetch_failed", "network_unverified":
		r.Reasons = append(r.Reasons, e.Failure)
	default:
		r.Reasons = append(r.Reasons, "provider_metadata_unavailable")
	}
	if !e.Merged {
		return nil
	}
	return checkMergedContainer(ctx, tx, personID, r, e)
}

func checkRouteBindings(r *MessagingRoute, e *MessagingRouteEvidence) {
	if e.AccountID != r.AccountID {
		r.Reasons = append(r.Reasons, "account_binding_mismatch")
	}
	if e.ChatID != r.ProviderChatID {
		r.Reasons = append(r.Reasons, "chat_binding_mismatch")
	}
	if e.ProviderType != "single" && e.ProviderType != "group" {
		r.Reasons = append(r.Reasons, "conversation_type_unverified")
	}
	directMismatch := e.ProviderType == "single" && r.ConversationType != "direct_chat"
	groupMismatch := e.ProviderType == "group" && r.ConversationType != "group_chat"
	if !e.Merged && (directMismatch || groupMismatch) {
		r.Reasons = append(r.Reasons, "conversation_type_mismatch")
	}
}

// checkRouteRoster compares the archived roster with the captured snapshot.
// A later member-count write without a matching roster capture, such as a
// media refresh, also invalidates the snapshot.
func checkRouteRoster(r *MessagingRoute, e *MessagingRouteEvidence, memberCount *int, roster []int64) {
	if !e.MembershipComplete {
		r.Reasons = append(r.Reasons, "membership_incomplete")
	}
	ids := slices.Clone(e.ParticipantIDs)
	slices.Sort(ids)
	if !slices.Equal(roster, ids) || memberCount != nil && !e.Truncated && *memberCount != len(ids) {
		r.Reasons = append(r.Reasons, "roster_changed")
	}
	if e.Truncated || len(ids) > maxRouteEvidenceIDs || len(e.SelfParticipantIDs) > maxRouteEvidenceIDs ||
		len(e.MemberChatIDs) > maxRouteEvidenceIDs {
		r.EvidenceTruncated = true
		r.Reasons = append(r.Reasons, "evidence_truncated")
	}
	nonSelf := []int64{}
	for _, id := range r.BoundParticipantIDs {
		if !slices.Contains(e.SelfParticipantIDs, id) {
			nonSelf = append(nonSelf, id)
		}
	}
	if len(nonSelf) == 0 {
		r.Reasons = append(r.Reasons, "self_only")
	}
	// A direct endpoint belongs to the person only when nobody else is in it.
	if r.ConversationType == "direct_chat" {
		for _, id := range roster {
			if !slices.Contains(e.SelfParticipantIDs, id) && !slices.Contains(r.BoundParticipantIDs, id) {
				r.Reasons = append(r.Reasons, "direct_chat_has_unbound_member")
				break
			}
		}
	}
	r.BoundParticipantIDs = nonSelf
}

func checkMergedContainer(
	ctx context.Context, tx *loggedTx, personID int64, r *MessagingRoute, e *MessagingRouteEvidence,
) error {
	r.Status = "merged_container"
	r.Reasons = append(r.Reasons, "merged_container_not_network_endpoint")
	r.MemberChatIDs = append(r.MemberChatIDs, e.MemberChatIDs[:min(len(e.MemberChatIDs), maxRouteEvidenceIDs)]...)
	if len(r.MemberChatIDs) == 0 {
		return nil
	}
	args := make([]any, 0, len(r.MemberChatIDs)+1)
	for _, chat := range r.MemberChatIDs {
		args = append(args, chat)
	}
	args = append(args, personID)
	// Member chats may belong to other Beeper accounts, so the lookup spans sources.
	counts, err := readContactLookupRows(ctx, tx, `SELECT c.source_conversation_id,COUNT(*)
 FROM conversations c JOIN sources src ON src.id=c.source_id
 WHERE c.source_conversation_id IN (`+sqlPlaceholders(len(r.MemberChatIDs))+`) AND src.source_type='beeper'
 AND EXISTS(SELECT 1 FROM conversation_participants cp JOIN person_participants pp ON pp.participant_id=cp.participant_id
  WHERE cp.conversation_id=c.id AND cp.left_at IS NULL AND pp.person_id=?)
 GROUP BY c.source_conversation_id`, func(row scanner) (*mergedMemberCount, error) {
		item := &mergedMemberCount{}
		return item, row.Scan(&item.chatID, &item.count)
	}, args...)
	if err != nil {
		return fmt.Errorf("read merged route members: %w", err)
	}
	for _, chat := range r.MemberChatIDs {
		matched := slices.ContainsFunc(counts, func(c mergedMemberCount) bool { return c.chatID == chat && c.count == 1 })
		if !matched {
			r.MissingMemberChatIDs = append(r.MissingMemberChatIDs, chat)
		}
	}
	if len(r.MissingMemberChatIDs) > 0 {
		r.Reasons = append(r.Reasons, "member_route_evidence_missing_or_ambiguous")
	}
	return nil
}

type mergedMemberCount struct {
	chatID string
	count  int
}

func appendRouteSourceReasons(r *MessagingRoute, item *archivedMessagingRoute, now time.Time) {
	if item.sourceFailure != "" {
		r.Reasons = append(r.Reasons, item.sourceFailure)
	}
	if r.Network == "" {
		r.Reasons = append(r.Reasons, "network_unverified")
	}
	if r.ProviderChatID == "" {
		r.Reasons = append(r.Reasons, "provider_chat_id_missing")
	}
	if r.SourceLastSyncAt == nil {
		r.Reasons = append(r.Reasons, "source_never_synced")
	} else if now.Sub(*r.SourceLastSyncAt) > routeEvidenceMaxAge {
		r.Reasons = append(r.Reasons, "source_stale")
	}
	if item.deleted {
		r.Reasons = append(r.Reasons, "source_messages_deleted")
	}
}

func finalizeRouteStatus(r *MessagingRoute) {
	if r.Status != "merged_container" && (r.ConversationType == "group_chat" || r.ConversationType == "channel") {
		r.Status = "group_context"
		r.Reasons = append(r.Reasons, "group_membership_is_not_a_direct_endpoint")
	}
	uniqueReasons := r.Reasons[:0]
	for _, reason := range r.Reasons {
		if !slices.Contains(uniqueReasons, reason) {
			uniqueReasons = append(uniqueReasons, reason)
		}
	}
	r.Reasons = uniqueReasons
	if len(r.Reasons) == 0 && r.ConversationType == "direct_chat" {
		r.Status = "archive_verified"
	}
}
