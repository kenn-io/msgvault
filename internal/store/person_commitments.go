package store

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/vector/preprocess"
)

// PersonCommitment keeps mutable owner decisions separate from immutable facts.
type PersonCommitment struct {
	ID              int64                       `json:"id"`
	PersonID        int64                       `json:"person_id"`
	TargetKey       string                      `json:"target_key"`
	AnchorMessageID int64                       `json:"anchor_message_id"`
	Direction       string                      `json:"direction"`
	Quote           string                      `json:"quote"`
	Start           int64                       `json:"quote_start"`
	End             int64                       `json:"quote_end"`
	Summary         string                      `json:"summary"`
	DueOn           *string                     `json:"due_on,omitempty"`
	PromisedAt      time.Time                   `json:"promised_at"`
	Freshness       personfacts.FreshnessStatus `json:"freshness"`
	State           string                      `json:"state"`
	SnoozedUntil    *time.Time                  `json:"snoozed_until,omitempty"`
}

const commitmentColumns = `id, person_id, target_key, owner_state, snoozed_until`

func scanPersonCommitment(row scanner) (PersonCommitment, error) {
	var c PersonCommitment
	var snooze nullableTimestamp
	err := row.Scan(&c.ID, &c.PersonID, &c.TargetKey, &c.State, &snooze)
	if snooze.Valid {
		c.SnoozedUntil = &snooze.Time
	}
	return c, err
}

type commitmentCandidate struct {
	commitment PersonCommitment
	promise    personfacts.Evidence
}

func (s *Store) ListPersonCommitmentsContext(ctx context.Context, personID int64, now time.Time, limit int, includeClosed bool) ([]PersonCommitment, error) {
	if personID < 0 || limit < 1 || limit > 201 {
		return nil, ErrInvalidActivityRequest
	}
	result := make([]PersonCommitment, 0, limit)
	pageSize := min(32, max(8, limit))
	for _, fresh := range []bool{true, false} {
		var after PersonCommitment
		for {
			candidates, err := s.loadCommitmentCandidates(ctx, personID, now, after, pageSize, fresh, includeClosed)
			if err != nil {
				return nil, err
			}
			for _, candidate := range candidates {
				c, visible, err := s.hydrateCommitmentCandidate(ctx, candidate, now)
				if err != nil {
					return nil, err
				}
				if !visible {
					continue
				}
				result = append(result, c)
				if len(result) == limit {
					return result, nil
				}
			}
			if len(candidates) < pageSize {
				break
			}
			after = candidates[len(candidates)-1].commitment
		}
	}
	return result, nil
}

// commitmentLatestClaimSQL selects column from the latest supported claim and
// evidence row of the current commitment.
func commitmentLatestClaimSQL(column string) string {
	return `(SELECT ` + column + ` FROM person_fact_claims c
				JOIN person_fact_generations g ON g.id = c.generation_id
				JOIN person_fact_claim_evidence ce ON ce.claim_id = c.id
				JOIN person_fact_evidence e ON e.id = ce.evidence_id
				WHERE c.person_id = person_commitments.person_id AND c.target_kind = 'commitment'
				AND c.target_key = person_commitments.target_key AND c.origin = 'commitment'
				AND c.relation = 'support' AND c.rejection_action IS NULL AND c.normalized_value_json IS NOT NULL
				AND COALESCE((SELECT status.supported ` + latestPersonFactEvidenceStatusSQL("e.person_id", "e.evidence_key", "e.source_version") + `), TRUE) = TRUE
				ORDER BY g.resolved_at DESC, c.id DESC, e.event_time DESC, e.id DESC LIMIT 1)`
}

// commitmentCandidateQuery pages one freshness group, overdue promises first,
// then oldest promised first, keyed by (overdue rank, promised time, id).
func (s *Store) commitmentCandidateQuery(personID int64, now time.Time, after PersonCommitment, pageSize int, fresh, includeClosed bool) (string, []any) {
	query := `SELECT ` + commitmentColumns + ` FROM person_commitments WHERE 1=1`
	var args []any
	if !includeClosed {
		query += ` AND (owner_state = 'open' OR (owner_state = 'snoozed' AND ` + s.loopTimeKey("snoozed_until") + ` <= ` + s.loopTimeKey("?") + `))`
		args = append(args, s.dialect.TimestampParam(now))
	}
	if personID > 0 {
		query += ` AND person_id = ?`
		args = append(args, personID)
	}
	promised := commitmentLatestClaimSQL(`e.event_time`)
	key := s.loopTimeKey(promised)
	today := now.Format(time.DateOnly)
	rank := `(CASE WHEN ` + commitmentLatestClaimSQL(`c.normalized_value_json ->> 'due_on'`) + ` < ? THEN 0 ELSE 1 END)`
	query += ` AND ` + promised + ` IS NOT NULL`
	if fresh {
		query += ` AND ` + key + ` > ` + s.loopTimeKey("?") + ` AND ` + key + ` <= ` + s.loopTimeKey("?")
	} else {
		query += ` AND (` + key + ` <= ` + s.loopTimeKey("?") + ` OR ` + key + ` > ` + s.loopTimeKey("?") + `)`
	}
	args = append(args, s.dialect.TimestampParam(now.Add(-commitmentFreshnessDays*24*time.Hour)), s.dialect.TimestampParam(now))
	if after.ID != 0 {
		query += ` AND (` + rank + `, ` + key + `, id) > (?, ` + s.loopTimeKey("?") + `, ?)`
		cursor := s.dialect.TimestampParam(after.PromisedAt)
		if !s.IsPostgreSQL() {
			cursor = after.PromisedAt.UTC().Format(time.RFC3339Nano)
		}
		afterRank := 1
		if after.DueOn != nil && *after.DueOn < today {
			afterRank = 0
		}
		args = append(args, today, afterRank, cursor, after.ID)
	}
	query += ` ORDER BY ` + rank + `, ` + key + `, id LIMIT ?`
	return query, append(args, today, pageSize)
}

func (s *Store) loadCommitmentCandidates(ctx context.Context, personID int64, now time.Time, after PersonCommitment, pageSize int, fresh, includeClosed bool) ([]commitmentCandidate, error) {
	var candidates []commitmentCandidate
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		query, args := s.commitmentCandidateQuery(personID, now, after, pageSize, fresh, includeClosed)
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			c, err := scanPersonCommitment(rows)
			if err != nil {
				_ = rows.Close()
				return err
			}
			candidates = append(candidates, commitmentCandidate{commitment: c})
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return err
		}
		for i := range candidates {
			if err := s.loadCommitmentCandidateEvidenceTx(ctx, tx, &candidates[i]); err != nil {
				return err
			}
		}
		return nil
	})
	return candidates, err
}

func (s *Store) loadCommitmentCandidateEvidenceTx(ctx context.Context, tx *loggedTx, candidate *commitmentCandidate) error {
	c := candidate.commitment
	var claimID int64
	var valueJSON string
	err := tx.QueryRowContext(ctx, `SELECT c.id, c.normalized_value_json
		FROM person_fact_claims c JOIN person_fact_generations g ON g.id = c.generation_id
		WHERE c.person_id = ? AND c.target_kind = 'commitment' AND c.target_key = ?
		AND c.origin = 'commitment' AND c.relation = 'support' AND c.rejection_action IS NULL
		AND c.normalized_value_json IS NOT NULL AND EXISTS (
		 SELECT 1 FROM person_fact_claim_evidence ce JOIN person_fact_evidence e ON e.id = ce.evidence_id
		 WHERE ce.claim_id = c.id AND COALESCE((SELECT status.supported `+latestPersonFactEvidenceStatusSQL("e.person_id", "e.evidence_key", "e.source_version")+`), TRUE) = TRUE)
		ORDER BY g.resolved_at DESC, c.id DESC LIMIT 1`, c.PersonID, c.TargetKey).Scan(&claimID, &valueJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var value personfacts.CommitmentValue
	if err := json.Unmarshal([]byte(valueJSON), &value); err != nil {
		return fmt.Errorf("decode commitment claim: %w", err)
	}
	promise, err := scanPersonFactEvidence(tx.QueryRowContext(ctx, `SELECT `+personFactEvidenceColumns+`
		FROM person_fact_evidence e WHERE EXISTS (
		 SELECT 1 FROM person_fact_claim_evidence ce WHERE ce.claim_id = ? AND ce.evidence_id = e.id)
		AND COALESCE((SELECT status.supported `+latestPersonFactEvidenceStatusSQL("e.person_id", "e.evidence_key", "e.source_version")+`), TRUE) = TRUE
		ORDER BY e.event_time DESC, e.id DESC LIMIT 1`, claimID))
	if err != nil {
		return err
	}
	messageID, err := strconv.ParseInt(strings.TrimPrefix(promise.Input.SourceRef, personfacts.CommitmentMessageRefPrefix), 10, 64)
	if err != nil || promise.Input.SpanStart == nil || promise.Input.SpanEnd == nil {
		return errors.New("invalid commitment citation")
	}
	c.AnchorMessageID, c.Direction, c.Quote = messageID, value.Direction, promise.Input.Excerpt
	c.Start, c.End = *promise.Input.SpanStart, *promise.Input.SpanEnd
	c.Summary, c.DueOn, c.PromisedAt = value.Summary, value.DueOn, promise.Input.EventTime
	candidate.commitment = c
	candidate.promise = promise
	return nil
}

func (s *Store) hydrateCommitmentCandidate(ctx context.Context, candidate commitmentCandidate, now time.Time) (PersonCommitment, bool, error) {
	c, input := candidate.commitment, candidate.promise.Input
	if candidate.promise.ID == 0 {
		return c, false, nil
	}
	source, live, err := s.currentCommitmentSource(ctx, c.PersonID, c.AnchorMessageID)
	if err != nil || !live || source.Direction != c.Direction || source.SourceVersion != input.SourceVersion ||
		!personFactPortableTime(source.EventTime).Equal(input.EventTime) ||
		!personFactPortableTime(source.RecordedTime).Equal(input.RecordedTime) ||
		!source.MatchesQuote(c.Quote, c.Start, c.End) {
		return c, false, err
	}
	c.Freshness = describePersonFactFreshness(personfacts.TargetRef{Kind: personfacts.TargetCommitment}, nil, &c.PromisedAt, now).Status
	if c.State == LoopStateSnoozed && c.SnoozedUntil != nil && !c.SnoozedUntil.After(now) {
		c.State, c.SnoozedUntil = LoopStateOpen, nil
	}
	return c, true, nil
}

func (s *Store) DecidePersonCommitmentContext(ctx context.Context, id int64, state string, until *time.Time, now time.Time) (PersonCommitment, error) {
	if id <= 0 || (state != LoopStateDone && state != LoopStateSnoozed && state != LoopStateOpen) || (state == LoopStateSnoozed && (until == nil || !until.After(now))) || (state != LoopStateSnoozed && until != nil) {
		return PersonCommitment{}, ErrInvalidActivityRequest
	}
	var candidate commitmentCandidate
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		c, err := scanPersonCommitment(tx.QueryRowContext(ctx, `SELECT `+commitmentColumns+` FROM person_commitments WHERE id = ?`, id))
		if err != nil {
			return err
		}
		candidate.commitment = c
		return s.loadCommitmentCandidateEvidenceTx(ctx, tx, &candidate)
	})
	if err != nil {
		return PersonCommitment{}, err
	}
	current, visible, err := s.hydrateCommitmentCandidate(ctx, candidate, now)
	if err != nil {
		return PersonCommitment{}, err
	}
	if !visible {
		return PersonCommitment{}, sql.ErrNoRows
	}
	var result PersonCommitment
	var deadline any
	if until != nil {
		deadline = s.dialect.TimestampParam(*until)
	}
	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockSyncMaintenanceGate(ctx, tx); err != nil {
			return err
		}
		var err error
		result, err = scanPersonCommitment(tx.QueryRowContext(ctx, `UPDATE person_commitments
			SET owner_state = ?, snoozed_until = ? WHERE id = ? RETURNING `+commitmentColumns,
			state, deadline, id))
		return err
	})
	if err == nil {
		current.State, current.SnoozedUntil = result.State, result.SnoozedUntil
		result = current
	}
	return result, err
}

// PersonCommitmentDiscoveryContext returns the saved discovery window and progress.
func (s *Store) PersonCommitmentDiscoveryContext(ctx context.Context, personID int64, scopeFingerprint string) (*time.Time, bool, error) {
	personColumn := "t.person_id"
	from := `person_tracking t LEFT JOIN person_commitment_frontiers f ON f.person_id = t.person_id`
	var args []any
	if personID > 0 {
		personColumn = "f.person_id"
		from = `person_commitment_frontiers f`
		args = append(args, personID)
	}
	query := `SELECT f.discovery_since, f.backfill_message_id,
		COALESCE(f.scope_fingerprint, ''),
		EXISTS(SELECT 1 FROM person_sweep_changes c WHERE c.person_id = ` + personColumn + `
			AND c.sequence > CASE WHEN COALESCE(f.observed_sequence, 0) > f.through_sequence THEN f.observed_sequence ELSE f.through_sequence END)
		FROM ` + from
	if personID > 0 {
		query += ` WHERE f.person_id = ?`
	}
	rows, err := s.db.QueryContext(ctx, s.Rebind(query), args...)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rows.Close() }()
	var since *time.Time
	complete := true
	for rows.Next() {
		var at nullableTimestamp
		var backfill sql.NullInt64
		var scope string
		var pending bool
		if err := rows.Scan(&at, &backfill, &scope, &pending); err != nil {
			return nil, false, err
		}
		if !at.Valid {
			complete = false
			continue
		}
		if since == nil || at.Time.After(*since) {
			value := at.Time
			since = &value
		}
		complete = complete && scope == scopeFingerprint && backfill.Valid && backfill.Int64 == -1 && !pending
	}
	return since, complete && since != nil, rows.Err()
}

func (s *Store) PersonCommitmentsDue(ctx context.Context, personID int64, now time.Time, interval, window time.Duration, scopeFingerprint string) (bool, error) {
	var processed requiredTimestamp
	var through, backfill int64
	var observed sql.NullInt64
	var scope string
	err := s.db.QueryRowContext(ctx, s.Rebind(`SELECT processed_at, through_sequence, observed_sequence, backfill_message_id, scope_fingerprint
		FROM person_commitment_frontiers WHERE person_id = ?`), personID).Scan(&processed, &through, &observed, &backfill, &scope)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if (scopeFingerprint != "" && scope != scopeFingerprint) || backfill >= 0 || backfill < -2 {
		return true, nil
	}
	if observed.Valid {
		if !now.Before(processed.Time.Add(window)) {
			return true, nil
		}
		through = max(through, observed.Int64)
	}
	if now.Before(processed.Time.Add(interval)) {
		var pending bool
		err := s.db.QueryRowContext(ctx, s.Rebind(`SELECT EXISTS(SELECT 1 FROM person_sweep_changes WHERE person_id = ? AND sequence > ? AND `+s.loopTimeKey("recorded_at")+` <= `+s.loopTimeKey("?")+`)`), personID, through, s.dialect.TimestampParam(processed.Time)).Scan(&pending)
		return pending, err
	}
	return s.HasPersonSweepChangesAfter(ctx, personID, through)
}

// commitmentMessage admits both sides only when participant identities prove a
// direct owner/person exchange. Email without verified owner participation,
// third-party recipients, bulk mail and List-Id are refused.
func (s *Store) commitmentMessage(ctx context.Context, personID int64, conv LoopConversation, m LoopMessage, owners loopOwners) (peoplesweep.CommitmentSource, bool, error) {
	if conv.GroupChat || (conv.ConversationType != string(MessageExportConversationDirectChat) && conv.ConversationType != "email_thread") || m.ListID != "" {
		return peoplesweep.CommitmentSource{}, false, nil
	}
	var direction string
	if m.Sender.IsOwner && m.Direction == DirectionOutbound {
		direction = peoplesweep.CommitmentOwedBy
	} else if m.Sender.PersonID == personID && !m.Sender.IsOwner && m.Direction == DirectionInbound {
		direction = peoplesweep.CommitmentOwedTo
	} else {
		return peoplesweep.CommitmentSource{}, false, nil
	}
	owner, person := m.Sender.IsOwner, m.Sender.PersonID == personID
	for _, r := range m.Recipients {
		if !r.IsOwner && r.PersonID != personID {
			return peoplesweep.CommitmentSource{}, false, nil
		}
		owner = owner || r.IsOwner
		person = person || r.PersonID == personID
	}
	if conv.ConversationType == string(MessageExportConversationDirectChat) {
		var err error
		owner, person, err = s.commitmentChatMembers(ctx, conv.ConversationID, personID, owner, person, owners)
		if err != nil {
			return peoplesweep.CommitmentSource{}, false, err
		}
	}
	if !owner || !person {
		return peoplesweep.CommitmentSource{}, false, nil
	}
	if conv.ConversationType == "email_thread" {
		signals, _, unreadable, err := s.LoopOriginalEvidenceContext(ctx, m.ID)
		if err != nil {
			return peoplesweep.CommitmentSource{}, false, err
		}
		if unreadable || signals.AutoSubmitted || signals.PrecedenceBulk || signals.ListID || signals.ListUnsubscribe || signals.CalendarPart || signals.ReplyToNoReply {
			return peoplesweep.CommitmentSource{}, false, nil
		}
	}
	body, err := s.GetMessageBodyText(ctx, m.ID)
	if err != nil {
		return peoplesweep.CommitmentSource{}, false, err
	}
	if strings.TrimSpace(body) == "" {
		return peoplesweep.CommitmentSource{}, false, nil
	}
	digest := sha256.Sum256([]byte(body))
	hash := hex.EncodeToString(digest[:])
	var recorded requiredTimestamp
	var sourceMessageID string
	if err := s.db.QueryRowContext(ctx, s.Rebind(`SELECT archived_at, COALESCE(source_message_id, '') FROM messages WHERE id = ?`), m.ID).Scan(&recorded, &sourceMessageID); err != nil {
		return peoplesweep.CommitmentSource{}, false, err
	}
	runes := []rune(body)
	if conv.ConversationType == "email_thread" {
		runes = commitmentEmailRunes(body)
	}
	// Keep the original rune coordinates while skipping leading masked text.
	first := 0
	for first < len(runes) && (runes[first] == peoplesweep.CommitmentQuoteMask || unicode.IsSpace(runes[first])) {
		first++
	}
	if first == len(runes) {
		return peoplesweep.CommitmentSource{}, false, nil
	}
	runes = runes[first:]
	return peoplesweep.CommitmentSource{ID: fmt.Sprintf("m%d", m.ID), MessageID: m.ID, SourceID: conv.SourceID, SourceMessageID: sourceMessageID,
		Direction: direction, Text: string(runes), Start: int64(first), EventTime: m.OccurredAt.UTC(), RecordedTime: recorded.Time.UTC(),
		SourceRef: personfacts.CommitmentMessageRefPrefix + strconv.FormatInt(m.ID, 10), SourceVersion: fmt.Sprintf("commitment-body/v1:%d:%q:%s", conv.SourceID, sourceMessageID, hash), ContentSHA256: hash}, true, nil
}

// Reuse the reply detector's byte mask while retaining a single original
// coordinate for every rune, including non-ASCII text in quoted history.
func commitmentEmailRunes(body string) []rune {
	masked := preprocess.MaskReplyQuotes(body)
	runes := make([]rune, 0, utf8.RuneCountInString(body))
	for offset, r := range body {
		_, width := utf8.DecodeRuneInString(body[offset:])
		if body[offset:offset+width] != masked[offset:offset+width] {
			r = peoplesweep.CommitmentQuoteMask
		}
		runes = append(runes, r)
	}
	return runes
}

// A chat importer may record recipients only at the conversation level.
func (s *Store) commitmentChatMembers(ctx context.Context, conversationID, personID int64, owner, person bool, owners loopOwners) (bool, bool, error) {
	rows, err := s.db.QueryContext(ctx, s.Rebind(`SELECT cp.participant_id, COALESCE(pp.person_id, 0)
		FROM conversation_participants cp LEFT JOIN person_participants pp ON pp.participant_id = cp.participant_id
		WHERE cp.conversation_id = ?`), conversationID)
	if err != nil {
		return false, false, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var participant, id int64
		if err := rows.Scan(&participant, &id); err != nil {
			return false, false, err
		}
		_, ownerParticipant := owners.participants[participant]
		_, ownerPerson := owners.persons[id]
		switch {
		case ownerParticipant || ownerPerson:
			owner = true
		case id == personID:
			person = true
		default:
			return false, false, nil
		}
	}
	return owner, person, rows.Err()
}

func (s *Store) LoadPersonCommitmentWindow(ctx context.Context, personID int64, profile peoplesweep.ProviderProfile, cfg peoplesweep.CommitmentConfig, now time.Time) (peoplesweep.CommitmentWindow, error) {
	window := peoplesweep.CommitmentWindow{PersonID: personID, ScopeFingerprint: peoplesweep.CommitmentScopeFingerprint(profile, cfg), SelectionAt: now, Now: now, Timezone: cfg.Timezone, Sources: []peoplesweep.CommitmentSource{}}
	revision, err := s.AccountIdentityRevisionContext(ctx)
	if err != nil {
		return window, err
	}
	window.IdentityRevision = revision
	if !profile.AllowSensitive || !slices.Contains(profile.AllowedSources, peoplesweep.SourceConversationText) {
		return window, nil
	}
	through, err := s.LatestPersonSweepChangeSequence(ctx)
	if err != nil {
		return window, err
	}
	var previous int64
	var scope string
	var since nullableTimestamp
	err = s.db.QueryRowContext(ctx, s.Rebind(`SELECT CASE WHEN observed_sequence > through_sequence THEN observed_sequence ELSE through_sequence END,
  backfill_message_id, scope_fingerprint, discovery_since FROM person_commitment_frontiers WHERE person_id = ?`), personID).
		Scan(&previous, &window.BackfillMessageID, &scope, &since)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return window, err
	}
	sameEligibility := len(scope) == 64 && scope[:56] == window.ScopeFingerprint[:56]
	if sameEligibility && (window.BackfillMessageID != -1 || previous < through) && since.Valid && since.Time.Before(now.Add(-cfg.Window)) {
		window.SelectionAt = since.Time.Add(cfg.Window)
	}
	if !sameEligibility {
		previous, window.BackfillMessageID = 0, 0
		window.ResetSkipped = true
	}
	window.SnapshotSequence, window.ThroughSequence = through, through
	inProfile := func(at time.Time) bool {
		day := at.UTC().Format(time.DateOnly)
		return day >= profile.SourceSince && (profile.SourceUntil == "" || day <= profile.SourceUntil)
	}
	err = s.loadCommitmentSources(ctx, &window, cfg, previous, inProfile)

	return window, err
}

// Keep journal positions for the scan cursor, but defer each message to its
// last change in the snapshot so its current body is sent only once.
func (s *Store) scanCommitmentChanges(ctx context.Context, personID, previous, through int64) ([]peoplesweep.ArchiveChange, error) {
	rows, err := s.db.QueryContext(ctx, s.Rebind(`WITH page AS (
		SELECT sequence, source_lane, message_id, recorded_at FROM person_sweep_changes
		WHERE person_id = ? AND sequence > ? ORDER BY sequence LIMIT ?
	) SELECT p.sequence, p.source_lane, COALESCE(p.message_id, 0), p.recorded_at,
		EXISTS(SELECT 1 FROM person_sweep_changes c WHERE c.person_id = ? AND c.message_id = p.message_id
			AND c.source_lane = 'conversation_text' AND c.sequence > p.sequence AND c.sequence <= ?)
		FROM page p ORDER BY p.sequence`), personID, previous, maxPersonSweepChangeScan, personID, through)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var changes []peoplesweep.ArchiveChange
	for rows.Next() {
		var change peoplesweep.ArchiveChange
		var recordedAt requiredTimestamp
		var later bool
		if err := rows.Scan(&change.Sequence, &change.SourceLane, &change.MessageID, &recordedAt, &later); err != nil {
			return nil, err
		}
		change.RecordedAt = recordedAt.Time.UTC()
		if later && change.SourceLane == peoplesweep.SourceConversationText {
			change.MessageID = 0
		}
		changes = append(changes, change)
	}
	return changes, rows.Err()
}

func (s *Store) loadCommitmentSources(ctx context.Context, window *peoplesweep.CommitmentWindow, cfg peoplesweep.CommitmentConfig, previous int64, inProfile func(time.Time) bool) error {
	personID, through := window.PersonID, window.ThroughSequence
	// Negative values below -1 carry a pending gap and the backfill cursor.
	pending := map[int64]bool{}
	if window.BackfillMessageID < -2 {
		window.BackfillMessageID = -(window.BackfillMessageID + 3)
	} else if window.BackfillMessageID == -2 {
		window.BackfillMessageID = -1
	}
	defer func() {
		if len(pending) > 0 {
			if window.BackfillMessageID < 0 {
				window.BackfillMessageID = -2
			} else {
				window.BackfillMessageID = -window.BackfillMessageID - 3
			}
		}
	}()
	seen, used := map[int64]bool{}, 0
	add := func(source peoplesweep.CommitmentSource) (bool, error) {
		sourceBytes, err := peoplesweep.CommitmentSourceBytes(source.Text)
		if err != nil {
			return false, err
		}
		if sourceBytes > cfg.MaxBytes {
			pending[source.MessageID] = true
			seen[source.MessageID] = true
			window.SkippedMessageIDs = append(window.SkippedMessageIDs, source.MessageID)
			return true, nil
		}
		if len(window.Sources) >= cfg.MaxItems || used+sourceBytes > cfg.MaxBytes {
			return false, nil
		}
		var beyond bool
		if err := s.db.QueryRowContext(ctx, s.Rebind(`SELECT EXISTS(SELECT 1 FROM person_sweep_changes WHERE person_id = ? AND message_id = ? AND sequence > ?)`), personID, source.MessageID, window.SnapshotSequence).Scan(&beyond); err != nil {
			return false, err
		}
		if beyond {
			// A newer journal entry will bring this source back in a later batch.
			return true, nil
		}
		window.Sources = append(window.Sources, source)
		fits, err := peoplesweep.CommitmentInputFits(*window)
		if err != nil || !fits {
			window.Sources = window.Sources[:len(window.Sources)-1]
			if err != nil {
				return false, err
			}
			single := *window
			single.Sources = []peoplesweep.CommitmentSource{source}
			fitsAlone, err := peoplesweep.CommitmentInputFits(single)
			if !fitsAlone && err == nil {
				pending[source.MessageID] = true
				seen[source.MessageID] = true
				window.SkippedMessageIDs = append(window.SkippedMessageIDs, source.MessageID)
			}
			return !fitsAlone, err
		}
		seen[source.MessageID] = true
		used += sourceBytes
		return true, nil
	}
	if !window.ResetSkipped {
		rows, err := s.db.QueryContext(ctx, s.Rebind(`SELECT message_id, max_bytes FROM person_commitment_skipped WHERE person_id = ? ORDER BY message_id`), personID)
		if err != nil {
			return err
		}
		type skippedMessage struct {
			id       int64
			maxBytes int
		}
		var skipped []skippedMessage
		for rows.Next() {
			var item skippedMessage
			if err := rows.Scan(&item.id, &item.maxBytes); err != nil {
				_ = rows.Close()
				return err
			}
			skipped = append(skipped, item)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return err
		}
		for _, item := range skipped {
			pending[item.id] = true
		}
		for _, item := range skipped {
			if item.maxBytes >= cfg.MaxBytes {
				continue
			}
			source, ok, err := s.currentCommitmentSource(ctx, personID, item.id)
			if err != nil {
				return err
			}
			if ok && inProfile(source.EventTime) {
				delete(pending, item.id)
				added, err := add(source)
				if err != nil || !added {
					pending[item.id] = true
					window.ThroughSequence = previous
					return err
				}
			}
			if !ok || !inProfile(source.EventTime) {
				delete(pending, item.id)
			}
			window.RetriedMessageIDs = append(window.RetriedMessageIDs, item.id)
		}
	}
	changes, err := s.scanCommitmentChanges(ctx, personID, previous, through)
	if err != nil {
		return err
	}
	if len(changes) == maxPersonSweepChangeScan {
		window.ThroughSequence = min(through, changes[len(changes)-1].Sequence)
	}
	cutoff := window.SelectionAt.Add(-cfg.Window)
	for _, change := range changes {
		if change.Sequence > through {
			break
		}
		if change.SourceLane != peoplesweep.SourceConversationText || change.MessageID == 0 || seen[change.MessageID] {
			continue
		}
		source, ok, err := s.currentCommitmentSource(ctx, personID, change.MessageID)
		if err != nil {
			return err
		}
		if !ok || !inProfile(source.EventTime) || !commitmentInWindow(source, !change.RecordedAt.Before(cutoff), cutoff) {
			if pending[change.MessageID] {
				delete(pending, change.MessageID)
				window.RetriedMessageIDs = append(window.RetriedMessageIDs, change.MessageID)
			}
			continue
		}
		wasSkipped := pending[source.MessageID]
		delete(pending, source.MessageID)
		added, err := add(source)
		if err != nil {
			return err
		}
		if !added {
			if wasSkipped {
				pending[source.MessageID] = true
			}
			window.ThroughSequence = change.Sequence - 1
			return nil
		}
		if wasSkipped {
			window.RetriedMessageIDs = append(window.RetriedMessageIDs, source.MessageID)
		}
	}
	if window.ThroughSequence < through || window.BackfillMessageID < 0 {
		return nil
	}
	const backfillPage = 256
	ids, err := s.commitmentBackfillMessages(ctx, personID, window.BackfillMessageID, previous, cutoff, backfillPage)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if !seen[id] {
			source, ok, err := s.currentCommitmentSource(ctx, personID, id)
			if err != nil {
				return err
			}
			if ok && inProfile(source.EventTime) {
				added, err := add(source)
				if err != nil || !added {
					return err
				}
			}
		}
		window.BackfillMessageID = id
	}
	if len(ids) < backfillPage {
		window.BackfillMessageID = -1
	}
	return nil
}

// resolvePersonCommitmentTargetTx has no confidence score: only a currently
// supported commitment-origin claim can be selected, with deterministic latest
// generation order. Owner decisions live outside this immutable resolution.
func (s *Store) resolvePersonCommitmentTargetTx(ctx context.Context, tx *loggedTx, generation personfacts.Generation, descriptor personfacts.TargetDescriptor, failures map[string]*personfacts.ValidationFailure) (bool, error) {
	claims, chronology, err := s.loadPersonFactResolvedClaimsTx(ctx, tx, generation.PersonID, descriptor, failures)
	if err != nil {
		return false, err
	}
	slices.SortFunc(claims, func(a, b personfacts.ResolvedClaim) int {
		x, y := chronology[a.ClaimKey], chronology[b.ClaimKey]
		if c := y.ResolvedAt.Compare(x.ResolvedAt); c != 0 {
			return c
		}
		return cmp.Compare(y.GenerationID, x.GenerationID)
	})
	decisions := make([]personfacts.Decision, 0, len(claims))
	selected := ""
	for _, c := range claims {
		action, reason := personfacts.DecisionInvalid, personfacts.ReasonEvidenceUnsupported
		supported := false
		for _, e := range c.Evidence {
			supported = supported || e.Supported
		}
		if c.Failure == nil && c.Claim.Origin == personfacts.OriginCommitment && c.Normalized != nil && supported && c.Claim.Relation == personfacts.RelationSupport {
			if selected == "" {
				selected = c.ClaimKey
				action, reason = personfacts.DecisionApplied, personfacts.ReasonAppliedProjection
			} else {
				action, reason = personfacts.DecisionRetained, personfacts.ReasonAppliedProjection
			}
		}
		decisions = append(decisions, personfacts.Decision{ClaimKey: c.ClaimKey, Action: action, Reason: reason})
	}
	// Identical support decisions can recur after withdrawal and restoration.
	// Bind their ledger identity to the immutable generation being resolved.
	encoded, err := json.Marshal(struct {
		GenerationKey string                 `json:"generation_key"`
		Decisions     []personfacts.Decision `json:"decisions"`
	}{generation.GenerationKey, decisions}, json.Deterministic(true))
	if err != nil {
		return false, err
	}
	digest := sha256.Sum256(encoded)
	_, err = s.insertPersonFactResolutionTx(ctx, tx, generation, personfacts.Resolution{Target: personfacts.TargetRef{Kind: descriptor.Kind, Key: descriptor.Key, Revision: descriptor.Revision}, ResolverVersion: "commitment-supported-v1", InputFingerprint: hex.EncodeToString(digest[:]), ResolvedAt: generation.ResolvedAt, Decisions: decisions}, generation.Policy.ProviderPolicyFingerprint)
	if err != nil {
		return false, err
	}
	return false, nil
}

// The journal handles changes to every imported source. This keyset covers
// older live messages that predate a person's journal, one page at a time.
func (s *Store) commitmentBackfillMessages(ctx context.Context, personID, after, processed int64, cutoff time.Time, limit int) ([]int64, error) {
	scoped, args := s.loopConversationQuery(LoopQuery{PersonID: personID})
	at := s.dialect.TimestampParam(cutoff)
	args = append(args, after, at, at, personID, processed, limit)
	// The journal has already covered these messages in the current scope.
	query := `WITH scoped AS (` + scoped + `)
		SELECT m.id FROM messages m JOIN scoped c ON c.conversation_id = m.conversation_id
		JOIN sources src ON src.id = m.source_id
		WHERE m.id > ? AND ` + s.loopLiveMessageSQL() + `
		AND (` + s.loopTimeKey("m.sent_at") + ` >= ` + s.loopTimeKey("?") + `
			OR ` + s.loopTimeKey("m.archived_at") + ` >= ` + s.loopTimeKey("?") + `)
		AND NOT EXISTS (SELECT 1 FROM person_sweep_changes c WHERE c.person_id = ? AND c.message_id = m.id AND c.source_lane = 'conversation_text' AND c.sequence <= ?)
		ORDER BY m.id LIMIT ?`
	rows, err := s.db.QueryContext(ctx, s.Rebind(query), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanInt64Rows(rows)
}

// commitmentInWindow reports whether a source is inside the discovery window:
// its event or archive time, or a journal entry, is no older than cutoff.
func commitmentInWindow(source peoplesweep.CommitmentSource, journalRecent bool, cutoff time.Time) bool {
	return journalRecent || !source.EventTime.Before(cutoff) || !source.RecordedTime.Before(cutoff)
}
