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

	"go.kenn.io/msgvault/internal/jsonexact"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/vector/preprocess"
)

// ErrCommitmentChanged reports a decision that changed during an update.
var ErrCommitmentChanged = errors.New("commitment changed; retry")

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

func (s *Store) ListPersonCommitmentsContext(ctx context.Context, personID int64, now time.Time, limit, offset int, includeClosed bool) ([]PersonCommitment, error) {
	if personID < 0 || limit < 1 || limit > 201 || offset < 0 {
		return nil, ErrInvalidActivityRequest
	}
	result := make([]PersonCommitment, 0, limit)
	pageSize := min(32, max(8, limit))
	for _, fresh := range []bool{true, false} {
		var after PersonCommitment
		for {
			candidates, err := s.loadCommitmentCandidates(ctx, personID, now, includeClosed, after, pageSize, fresh)
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
				if offset > 0 {
					offset--
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

func (s *Store) loadCommitmentCandidates(ctx context.Context, personID int64, now time.Time, includeClosed bool, after PersonCommitment, pageSize int, fresh bool) ([]commitmentCandidate, error) {
	var candidates []commitmentCandidate
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		query := `SELECT ` + commitmentColumns + ` FROM person_commitments WHERE 1=1`
		var args []any
		if !includeClosed {
			query += ` AND owner_state <> 'done'`
			query += ` AND (owner_state <> 'snoozed' OR ` + s.loopTimeKey("snoozed_until") + ` <= ` + s.loopTimeKey("?") + `)`
			args = append(args, s.dialect.TimestampParam(now))
		}
		if personID > 0 {
			query += ` AND person_id = ?`
			args = append(args, personID)
		}
		promised := `(SELECT e.event_time FROM person_fact_claims c
				JOIN person_fact_generations g ON g.id = c.generation_id
				JOIN person_fact_claim_evidence ce ON ce.claim_id = c.id
				JOIN person_fact_evidence e ON e.id = ce.evidence_id
				WHERE c.person_id = person_commitments.person_id AND c.target_kind = 'commitment'
				AND c.target_key = person_commitments.target_key AND c.origin = 'commitment'
				AND c.relation = 'support' AND c.rejection_action IS NULL AND c.normalized_value_json IS NOT NULL
				AND COALESCE((SELECT status.supported ` + latestPersonFactEvidenceStatusSQL("e.person_id", "e.evidence_key", "e.source_version") + `), TRUE) = TRUE
				ORDER BY g.resolved_at DESC, c.id DESC, e.event_time DESC, e.id DESC LIMIT 1)`
		key := s.loopTimeKey(promised)
		query += ` AND ` + promised + ` IS NOT NULL`
		if fresh {
			query += ` AND ` + key + ` > ` + s.loopTimeKey("?") + ` AND ` + key + ` <= ` + s.loopTimeKey("?")
		} else {
			query += ` AND (` + key + ` <= ` + s.loopTimeKey("?") + ` OR ` + key + ` > ` + s.loopTimeKey("?") + `)`
		}
		args = append(args, s.dialect.TimestampParam(now.Add(-commitmentFreshnessDays*24*time.Hour)), s.dialect.TimestampParam(now))
		if after.ID != 0 {
			query += ` AND (` + key + `, id) > (` + s.loopTimeKey("?") + `, ?)`
			cursor := s.dialect.TimestampParam(after.PromisedAt)
			if !s.IsPostgreSQL() {
				cursor = after.PromisedAt.UTC().Format(time.RFC3339Nano)
			}
			args = append(args, cursor, after.ID)
		}
		query += ` ORDER BY ` + key + `, id`
		query += ` LIMIT ?`
		args = append(args, pageSize)
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
	if err := jsonexact.Validate([]byte(valueJSON), value); err != nil {
		return fmt.Errorf("decode commitment claim: %w", err)
	}
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
	candidate.commitment, candidate.promise = c, promise
	return nil
}

func (s *Store) hydrateCommitmentCandidate(ctx context.Context, candidate commitmentCandidate, now time.Time) (PersonCommitment, bool, error) {
	c := candidate.commitment
	if candidate.promise.ID == 0 {
		return c, false, nil
	}
	live, err := s.currentCommitmentCitation(ctx, c.PersonID, c.Direction, candidate.promise.Input)
	if err != nil || !live {
		return c, false, err
	}
	c.Freshness = describePersonFactFreshness(personfacts.TargetRef{Kind: personfacts.TargetCommitment}, nil, &c.PromisedAt, now).Status
	if c.State == LoopStateSnoozed && c.SnoozedUntil != nil && !c.SnoozedUntil.After(now) {
		c.State, c.SnoozedUntil = LoopStateOpen, nil
	}
	return c, true, nil
}

func (s *Store) currentCommitmentCitation(ctx context.Context, personID int64, direction string, input personfacts.EvidenceInput) (bool, error) {
	message, found := strings.CutPrefix(input.SourceRef, personfacts.CommitmentMessageRefPrefix)
	if !found {
		return false, nil
	}
	id, err := strconv.ParseInt(message, 10, 64)
	if err != nil {
		return false, fmt.Errorf("parse commitment citation message: %w", err)
	}
	if id <= 0 || input.SpanStart == nil || input.SpanEnd == nil {
		return false, nil
	}
	source, live, err := s.currentCommitmentSource(ctx, personID, id)
	if err != nil || !live || source.Direction != direction || source.SourceVersion != input.SourceVersion ||
		source.ContentSHA256 != input.ContentSHA256 ||
		!personFactPortableTime(source.EventTime).Equal(input.EventTime) ||
		!personFactPortableTime(source.RecordedTime).Equal(input.RecordedTime) {
		return false, err
	}
	return source.MatchesQuote(input.Excerpt, *input.SpanStart, *input.SpanEnd), nil
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
	result, err := s.updatePersonCommitmentDecision(ctx, candidate, state, until)
	if err == nil {
		current.State, current.SnoozedUntil = result.State, result.SnoozedUntil
		result = current
	}
	return result, err
}

// commitmentDeadline is the bind value for a snooze deadline; nil binds NULL.
func (s *Store) commitmentDeadline(until *time.Time) any {
	if until == nil {
		return nil
	}
	return s.dialect.TimestampParam(*until)
}

func (s *Store) updatePersonCommitmentDecision(ctx context.Context, candidate commitmentCandidate, state string, until *time.Time) (PersonCommitment, error) {
	var result PersonCommitment
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockSyncMaintenanceGate(ctx, tx); err != nil {
			return err
		}
		previous := candidate.commitment
		var err error
		result, err = scanPersonCommitment(tx.QueryRowContext(ctx, `UPDATE person_commitments
			SET owner_state = ?, snoozed_until = ? WHERE id = ? AND owner_state = ? AND snoozed_until IS NOT DISTINCT FROM ?
			AND COALESCE((SELECT status.supported `+latestPersonFactEvidenceStatusSQL("?", "?", "?")+`), TRUE) = TRUE
			RETURNING `+commitmentColumns, state, s.commitmentDeadline(until), previous.ID, previous.State, s.commitmentDeadline(previous.SnoozedUntil),
			candidate.promise.Input.PersonID, candidate.promise.Key, candidate.promise.Input.SourceVersion))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrCommitmentChanged
		}
		return err
	})
	return result, err
}

// PersonCommitmentDiscoveryContext returns the saved discovery window and progress.
func (s *Store) PersonCommitmentDiscoveryContext(ctx context.Context, personID int64, scopeFingerprint string) (*time.Time, bool, error) {
	query := `SELECT f.discovery_since, f.backfill_message_id,
		COALESCE(f.scope_fingerprint, ''),
		EXISTS(SELECT 1 FROM person_sweep_changes c WHERE c.person_id = t.person_id
			AND c.sequence > CASE WHEN COALESCE(f.observed_sequence, 0) > f.through_sequence THEN f.observed_sequence ELSE f.through_sequence END),
		EXISTS(SELECT 1 FROM person_commitment_oversized o JOIN messages m ON m.id = o.message_id WHERE o.person_id = t.person_id AND m.deleted_at IS NULL AND (COALESCE(m.sent_at, m.received_at, m.internal_date, m.archived_at) >= f.discovery_since OR m.archived_at >= f.discovery_since))
		FROM person_tracking t LEFT JOIN person_commitment_frontiers f ON f.person_id = t.person_id`
	var args []any
	if personID > 0 {
		query += ` WHERE t.person_id = ?`
		args = append(args, personID)
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
		var pending, oversized bool
		if err := rows.Scan(&at, &backfill, &scope, &pending, &oversized); err != nil {
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
		complete = complete && scope == scopeFingerprint && backfill.Valid && backfill.Int64 == -1 && !pending && !oversized
	}
	return since, complete && since != nil, rows.Err()
}

type commitmentDueState struct {
	processed         requiredTimestamp
	through, backfill int64
	observed          sql.NullInt64
	scope             string
}

func (s *Store) commitmentDueState(ctx context.Context, personID int64) (commitmentDueState, error) {
	var st commitmentDueState
	err := s.db.QueryRowContext(ctx, s.Rebind(`SELECT processed_at, through_sequence, observed_sequence, backfill_message_id,
		scope_fingerprint
		FROM person_commitment_frontiers WHERE person_id = ?`), personID).Scan(&st.processed, &st.through, &st.observed, &st.backfill, &st.scope)
	return st, err
}

// commitmentOversizedRetryDue reports whether a message was skipped under a
// byte limit below maxBytes in this scope, which the retry path will offer again.
func (s *Store) commitmentOversizedRetryDue(ctx context.Context, personID int64, scopeFingerprint string, maxBytes int) (bool, error) {
	var due bool
	err := s.db.QueryRowContext(ctx, s.Rebind(`SELECT EXISTS(SELECT 1 FROM person_commitment_oversized WHERE person_id = ? AND scope_fingerprint = ? AND max_bytes < ?)`), personID, scopeFingerprint, maxBytes).Scan(&due)
	return due, err
}

func (s *Store) PersonCommitmentsDue(ctx context.Context, personID int64, now time.Time, interval, window time.Duration, scopeFingerprint string, maxBytes int) (bool, error) {
	st, err := s.commitmentDueState(ctx, personID)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if scopeFingerprint != "" && st.scope != scopeFingerprint {
		return true, nil
	}
	if retry, err := s.commitmentOversizedRetryDue(ctx, personID, scopeFingerprint, maxBytes); retry || err != nil {
		return retry, err
	}
	if now.Before(st.processed.Time.Add(interval)) {
		return false, nil
	}
	if st.backfill >= 0 {
		return true, nil
	}
	if st.observed.Valid {
		if !now.Before(st.processed.Time.Add(window)) {
			return true, nil
		}
		st.through = max(st.through, st.observed.Int64)
	}
	return s.HasPersonSweepChangesAfter(ctx, personID, st.through)
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
	if err := s.db.QueryRowContext(ctx, s.Rebind(`SELECT archived_at FROM messages WHERE id = ?`), m.ID).Scan(&recorded); err != nil {
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
	return peoplesweep.CommitmentSource{ID: fmt.Sprintf("m%d", m.ID), MessageID: m.ID, SourceID: conv.SourceID,
		Direction: direction, Text: string(runes), Start: int64(first), EventTime: m.OccurredAt.UTC(), RecordedTime: recorded.Time.UTC(),
		SourceRef: personfacts.CommitmentMessageRefPrefix + strconv.FormatInt(m.ID, 10), SourceVersion: "commitment-body/v1:" + hash, ContentSHA256: hash}, true, nil
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

func (s *Store) LoadPersonCommitmentWindow(ctx context.Context, personID int64, profile peoplesweep.ProviderProfile, cfg peoplesweep.CommitmentConfig, now time.Time, timezone string) (peoplesweep.CommitmentWindow, error) {
	window := peoplesweep.CommitmentWindow{PersonID: personID, ScopeFingerprint: peoplesweep.CommitmentScopeFingerprint(profile, cfg), SelectionAt: now, Now: now, Timezone: timezone, Sources: []peoplesweep.CommitmentSource{}}
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
	if scope == window.ScopeFingerprint && window.BackfillMessageID != -1 && since.Valid && since.Time.Before(now.Add(-cfg.Window)) {
		window.SelectionAt = since.Time.Add(cfg.Window)
	}
	if scope != window.ScopeFingerprint {
		previous, window.BackfillMessageID = 0, 0
		if _, err := s.db.ExecContext(ctx, s.Rebind(`DELETE FROM person_commitment_oversized WHERE person_id = ? AND scope_fingerprint != ?`), personID, window.ScopeFingerprint); err != nil {
			return window, err
		}
	}
	window.SnapshotSequence, window.ThroughSequence = through, through
	inProfile := func(at time.Time) bool {
		day := at.UTC().Format(time.DateOnly)
		return day >= profile.SourceSince && (profile.SourceUntil == "" || day <= profile.SourceUntil)
	}
	err = s.loadCommitmentSources(ctx, &window, cfg, previous, window.ScopeFingerprint, inProfile)
	return window, err
}

func (s *Store) loadCommitmentSources(ctx context.Context, window *peoplesweep.CommitmentWindow, cfg peoplesweep.CommitmentConfig, previous int64, baseScope string, inProfile func(time.Time) bool) error {
	personID, through := window.PersonID, window.ThroughSequence
	seen, used := map[int64]bool{}, 0
	add := func(source peoplesweep.CommitmentSource) (bool, error) {
		sourceBytes, err := peoplesweep.CommitmentSourceBytes(source.Text)
		if err != nil {
			return false, err
		}
		if sourceBytes > cfg.MaxBytes {
			err = s.recordOversizedCommitment(ctx, personID, source.MessageID, baseScope, cfg.MaxBytes)
			return true, err
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
				err = s.recordOversizedCommitment(ctx, personID, source.MessageID, baseScope, cfg.MaxBytes)
			}
			return !fitsAlone, err
		}
		seen[source.MessageID] = true
		used += sourceBytes
		return true, nil
	}
	if stop, err := s.retryOversizedCommitments(ctx, window, cfg, previous, baseScope, inProfile, add); stop || err != nil {
		return err
	}
	changes, err := s.ScanPersonSweepChanges(ctx, personID, previous, maxPersonSweepChangeScan)
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
		if !ok || !inProfile(source.EventTime) ||
			(source.EventTime.Before(cutoff) && source.RecordedTime.Before(cutoff) && change.RecordedAt.Before(cutoff)) {
			continue
		}
		added, err := add(source)
		if err != nil {
			return err
		}
		if !added {
			window.ThroughSequence = change.Sequence - 1
			return nil
		}
	}
	if window.BackfillMessageID < 0 {
		return nil
	}
	const backfillPage = 256
	ids, err := s.commitmentBackfillMessages(ctx, personID, window.BackfillMessageID, cutoff, backfillPage)
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
func (s *Store) commitmentBackfillMessages(ctx context.Context, personID, after int64, cutoff time.Time, limit int) ([]int64, error) {
	scoped, args := s.loopConversationQuery(LoopQuery{PersonID: personID})
	at := s.dialect.TimestampParam(cutoff)
	args = append(args, after, at, at, limit)
	query := `WITH scoped AS (` + scoped + `)
		SELECT m.id FROM messages m JOIN scoped c ON c.conversation_id = m.conversation_id
		JOIN sources src ON src.id = m.source_id
		WHERE m.id > ? AND ` + s.loopLiveMessageSQL() + `
		AND (` + s.loopTimeKey("m.sent_at") + ` >= ` + s.loopTimeKey("?") + `
			OR ` + s.loopTimeKey("m.archived_at") + ` >= ` + s.loopTimeKey("?") + `)
		ORDER BY m.id LIMIT ?`
	rows, err := s.db.QueryContext(ctx, s.Rebind(query), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanInt64Rows(rows)
}

func (s *Store) recordOversizedCommitment(ctx context.Context, personID, messageID int64, baseScope string, maxBytes int) error {
	_, err := s.db.ExecContext(ctx, s.Rebind(`INSERT INTO person_commitment_oversized (person_id, message_id, scope_fingerprint, max_bytes) VALUES (?, ?, ?, ?) ON CONFLICT (person_id, message_id) DO UPDATE SET scope_fingerprint = excluded.scope_fingerprint, max_bytes = excluded.max_bytes`), personID, messageID, baseScope, maxBytes)
	return err
}

// retryOversizedCommitments offers the messages skipped under a smaller
// max_bytes before any new journal or backfill work. It reports whether the
// window is full, in which case discovery resumes from the previous frontier.
func (s *Store) retryOversizedCommitments(ctx context.Context, window *peoplesweep.CommitmentWindow, cfg peoplesweep.CommitmentConfig, previous int64, baseScope string, inProfile func(time.Time) bool, add func(peoplesweep.CommitmentSource) (bool, error)) (bool, error) {
	personID := window.PersonID
	const retryPage = 256
	rows, err := s.db.QueryContext(ctx, s.Rebind(`SELECT message_id FROM person_commitment_oversized WHERE person_id = ? AND scope_fingerprint = ? AND max_bytes < ? ORDER BY message_id LIMIT ?`), personID, baseScope, cfg.MaxBytes, retryPage)
	if err != nil {
		return false, err
	}
	ids, err := scanInt64Rows(rows)
	_ = rows.Close()
	if err != nil {
		return false, err
	}
	for _, id := range ids {
		source, ok, err := s.currentCommitmentSource(ctx, personID, id)
		if err != nil {
			return false, err
		}
		if !ok || !inProfile(source.EventTime) {
			if _, err := s.db.ExecContext(ctx, s.Rebind(`DELETE FROM person_commitment_oversized WHERE person_id = ? AND message_id = ?`), personID, id); err != nil {
				return false, err
			}
			continue
		}
		added, err := add(source)
		if err != nil {
			return false, err
		}
		if !added {
			window.ThroughSequence = previous
			return true, nil
		}
	}
	if len(ids) == retryPage {
		window.ThroughSequence = previous
		return true, nil
	}
	return false, nil
}
