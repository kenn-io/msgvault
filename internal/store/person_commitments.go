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
	ID                   int64               `json:"id"`
	PersonID             int64               `json:"person_id"`
	ClaimID              *int64              `json:"claim_id,omitempty"`
	TargetKey            string              `json:"target_key"`
	AnchorMessageID      int64               `json:"anchor_message_id"`
	Direction            string              `json:"direction"`
	Quote                string              `json:"quote"`
	Start                int64               `json:"quote_start"`
	End                  int64               `json:"quote_end"`
	Summary              string              `json:"summary"`
	DueOn                *string             `json:"due_on,omitempty"`
	PromisedAt           time.Time           `json:"promised_at"`
	ExpiresAt            time.Time           `json:"expires_at"`
	State                string              `json:"state"`
	SnoozedUntil         *time.Time          `json:"snoozed_until,omitempty"`
	ReopenedAt           *time.Time          `json:"reopened_at,omitempty"`
	FulfilmentEvidenceID *int64              `json:"fulfilment_evidence_id,omitempty"`
	Fulfilment           *CommitmentCitation `json:"fulfilment,omitempty"`
	FulfilmentConfirmed  bool                `json:"fulfilment_confirmed"`
}

const commitmentColumns = `id, person_id, current_claim_id, target_key, anchor_message_id,
 direction, quote, quote_start, quote_end, summary, due_on, promised_at, expires_at,
 owner_state, snoozed_until, reopened_at, fulfilment_evidence_id, fulfilment_confirmed`

func scanPersonCommitment(row scanner) (PersonCommitment, error) {
	var c PersonCommitment
	var claim, fulfil sql.NullInt64
	var due sql.NullString
	var promised, expires requiredTimestamp
	var snooze, reopened nullableTimestamp
	err := row.Scan(&c.ID, &c.PersonID, &claim, &c.TargetKey, &c.AnchorMessageID, &c.Direction, &c.Quote, &c.Start, &c.End,
		&c.Summary, &due, &promised, &expires, &c.State, &snooze, &reopened, &fulfil, &c.FulfilmentConfirmed)
	if claim.Valid {
		c.ClaimID = &claim.Int64
	}
	if fulfil.Valid {
		c.FulfilmentEvidenceID = &fulfil.Int64
	}
	if due.Valid {
		c.DueOn = &due.String
	}
	if snooze.Valid {
		c.SnoozedUntil = &snooze.Time
	}
	if reopened.Valid {
		c.ReopenedAt = &reopened.Time
	}
	c.PromisedAt, c.ExpiresAt = promised.Time, expires.Time
	return c, err
}

// CommitmentCitation locates an exact quote in an original message body.
type CommitmentCitation struct {
	MessageID int64
	Quote     string
	Start     int64
	End       int64
}

type commitmentCandidate struct {
	commitment PersonCommitment
	promise    personfacts.Evidence
	fulfilment *personfacts.Evidence
}

// CommitmentStateFulfilled is derived from live, newer evidence on an open promise.
const CommitmentStateFulfilled = "fulfilled"

func (s *Store) ListPersonCommitmentsContext(ctx context.Context, personID int64, now time.Time, limit int, includeClosed bool) ([]PersonCommitment, error) {
	return s.listPersonCommitments(ctx, commitmentListFilter{personID: personID, includeClosed: includeClosed}, now, limit)
}

type commitmentListFilter struct {
	personID, id                  int64
	includeSnoozed, includeClosed bool
	agendaCandidates              bool
	agendaDeferred                bool
	afterID                       *int64
	eligible                      func(PersonCommitment) bool
}

// GetOpenPersonCommitmentContext verifies a single promise even while it is snoozed.
func (s *Store) GetOpenPersonCommitmentContext(ctx context.Context, id int64, now time.Time) (PersonCommitment, error) {
	if id <= 0 {
		return PersonCommitment{}, ErrInvalidActivityRequest
	}
	items, err := s.listPersonCommitments(ctx, commitmentListFilter{id: id, includeSnoozed: true}, now, 1)
	if err != nil {
		return PersonCommitment{}, err
	}
	if len(items) == 0 {
		return PersonCommitment{}, sql.ErrNoRows
	}
	return items[0], nil
}

func (s *Store) ListCommitmentAgendaCandidatesContext(ctx context.Context, now time.Time, limit int) ([]PersonCommitment, error) {
	filter := commitmentListFilter{agendaCandidates: true}
	items, err := s.listPersonCommitments(ctx, filter, now, limit)
	if err != nil || len(items) == limit {
		return items, err
	}
	// Prefer promises that have never failed preparation. A permanently
	// unpreparable first batch must not fill each scheduled run.
	filter.agendaDeferred = true
	retries, err := s.listPersonCommitments(ctx, filter, now, limit-len(items))
	return append(items, retries...), err
}

func (s *Store) listPersonCommitments(ctx context.Context, filter commitmentListFilter, now time.Time, limit int) ([]PersonCommitment, error) {
	if filter.personID < 0 || limit < 1 || limit > 201 {
		return nil, ErrInvalidActivityRequest
	}
	result := make([]PersonCommitment, 0, limit)
	var after PersonCommitment
	if filter.afterID != nil {
		after.ID = *filter.afterID
	}
	pageSize := min(32, max(8, limit))
	for {
		candidates, err := s.loadCommitmentCandidates(ctx, filter, now, after, pageSize)
		if err != nil {
			return nil, err
		}
		for _, candidate := range candidates {
			if filter.eligible != nil && !filter.eligible(candidate.commitment) {
				continue
			}
			c, visible, err := s.hydrateCommitmentCandidate(ctx, candidate, now)
			if err != nil {
				return nil, err
			}
			if !visible || !filter.includeClosed && (c.State == CommitmentStateFulfilled || filter.includeSnoozed && c.FulfilmentConfirmed) {
				continue
			}
			result = append(result, c)
			if len(result) == limit {
				return result, nil
			}
		}
		if len(candidates) < pageSize {
			return result, nil
		}
		after = candidates[len(candidates)-1].commitment
	}
}

func (s *Store) loadCommitmentCandidates(ctx context.Context, filter commitmentListFilter, now time.Time, after PersonCommitment, pageSize int) ([]commitmentCandidate, error) {
	var candidates []commitmentCandidate
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		query := `SELECT ` + commitmentColumns + ` FROM person_commitments
			WHERE current_claim_id IS NOT NULL AND ` + s.loopTimeKey("expires_at") + ` > ` + s.loopTimeKey("?")
		args := []any{s.dialect.TimestampParam(now)}
		if !filter.includeClosed {
			query += ` AND owner_state <> 'done'`
			if !filter.includeSnoozed {
				query += ` AND (owner_state <> 'snoozed' OR ` + s.loopTimeKey("snoozed_until") + ` <= ` + s.loopTimeKey("?") + `)`
				args = append(args, s.dialect.TimestampParam(now))
			}
		}
		if filter.personID > 0 {
			query += ` AND person_id = ?`
			args = append(args, filter.personID)
		}
		if filter.id > 0 {
			query += ` AND id = ?`
			args = append(args, filter.id)
		}
		if filter.agendaCandidates {
			query += ` AND NOT EXISTS (SELECT 1 FROM person_commitment_agenda a
				WHERE a.commitment_id = person_commitments.id AND (a.state <> 'retired'
					OR (a.attempted_at IS NOT NULL AND (a.target_key <> person_commitments.target_key OR a.quote <> person_commitments.quote))))`
			const deferred = ` (SELECT 1 FROM person_commitment_agenda a
				WHERE a.commitment_id = person_commitments.id AND a.deferred_at IS NOT NULL`
			if filter.agendaDeferred {
				query += ` AND EXISTS` + deferred + ` AND ` + s.loopTimeKey("a.deferred_at") + ` <= ` + s.loopTimeKey("?") + `)`
				args = append(args, s.dialect.TimestampParam(now.Add(-commitmentAgendaDeferFor)))
			} else {
				query += ` AND NOT EXISTS` + deferred + `)`
			}
		}
		promisedAt := s.loopTimeKey("promised_at")
		if filter.afterID != nil {
			query += ` AND id > ? ORDER BY id LIMIT ?`
			args = append(args, after.ID)
		} else if filter.agendaDeferred {
			const deferredAt = `(SELECT a.deferred_at FROM person_commitment_agenda a WHERE a.commitment_id = `
			rank := s.loopTimeKey(deferredAt + "person_commitments.id)")
			if after.ID != 0 {
				query += ` AND (` + rank + `, id) > (` + s.loopTimeKey(deferredAt+"?)") + `, ?)`
				args = append(args, after.ID, after.ID)
			}
			query += ` ORDER BY ` + rank + `, id LIMIT ?`
		} else {
			if after.ID != 0 {
				query += ` AND (` + promisedAt + `, id) > (` + s.loopTimeKey("?") + `, ?)`
				args = append(args, s.dialect.TimestampParam(after.PromisedAt), after.ID)
			}
			query += ` ORDER BY ` + promisedAt + `, id LIMIT ?`
		}
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
	if c.ClaimID == nil {
		return nil
	}
	// A dated promise can remain open after its ledger freshness horizon.
	// Only currently supported evidence can make it visible until expires_at.
	promise, err := scanPersonFactEvidence(tx.QueryRowContext(ctx, `SELECT `+personFactEvidenceColumns+`
		FROM person_fact_evidence e WHERE EXISTS (
			SELECT 1 FROM person_fact_claim_evidence ce WHERE ce.claim_id = ? AND ce.evidence_id = e.id)
		AND COALESCE((SELECT status.supported `+latestPersonFactEvidenceStatusSQL("e.person_id", "e.evidence_key", "e.source_version")+`), TRUE) = TRUE
		ORDER BY e.id LIMIT 1`, *c.ClaimID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	candidate.promise = promise
	if c.FulfilmentEvidenceID != nil {
		evidence, err := s.loadCommitmentFulfilmentTx(ctx, tx, *c.FulfilmentEvidenceID)
		if err != nil {
			return err
		}
		candidate.fulfilment = &evidence
	}
	return nil
}

func (s *Store) hydrateCommitmentCandidate(ctx context.Context, candidate commitmentCandidate, now time.Time) (PersonCommitment, bool, error) {
	c := candidate.commitment
	if candidate.promise.ID == 0 || !c.ExpiresAt.After(now) {
		return c, false, nil
	}
	promise, live, err := s.currentCommitmentCitation(ctx, c.PersonID, c.Direction, candidate.promise.Input)
	if err != nil || !live || promise.MessageID != c.AnchorMessageID || promise.Quote != c.Quote || promise.Start != c.Start || promise.End != c.End {
		return c, false, err
	}
	if evidence := candidate.fulfilment; evidence != nil && evidence.Supported &&
		(c.ReopenedAt == nil || evidence.Input.EventTime.After(*c.ReopenedAt)) {
		fulfilment, live, err := s.currentCommitmentCitation(ctx, c.PersonID, c.Direction, evidence.Input)
		if err != nil {
			return c, false, err
		}
		if live {
			c.Fulfilment = &fulfilment
		}
	}
	if c.State == LoopStateSnoozed && c.SnoozedUntil != nil && !c.SnoozedUntil.After(now) {
		c.State, c.SnoozedUntil = LoopStateOpen, nil
	}
	if c.Fulfilment != nil && c.FulfilmentConfirmed && c.State == LoopStateOpen {
		c.State = CommitmentStateFulfilled
	}
	if c.Fulfilment == nil {
		c.FulfilmentEvidenceID = nil
		c.FulfilmentConfirmed = false
	}
	return c, true, nil
}

func (s *Store) loadCommitmentFulfilmentTx(ctx context.Context, tx *loggedTx, id int64) (personfacts.Evidence, error) {
	evidence, err := scanPersonFactEvidence(tx.QueryRowContext(ctx, `SELECT `+personFactEvidenceColumns+` FROM person_fact_evidence e WHERE e.id = ?`, id))
	if err != nil {
		return evidence, err
	}
	page := []personfacts.Evidence{evidence}
	err = s.hydratePersonFactEvidenceStatusTx(ctx, tx, page)
	return page[0], err
}

func (s *Store) currentCommitmentCitation(ctx context.Context, personID int64, direction string, input personfacts.EvidenceInput) (CommitmentCitation, bool, error) {
	message, found := strings.CutPrefix(input.SourceRef, personfacts.CommitmentMessageRefPrefix)
	if !found {
		return CommitmentCitation{}, false, nil
	}
	id, err := strconv.ParseInt(message, 10, 64)
	if err != nil {
		return CommitmentCitation{}, false, fmt.Errorf("parse commitment citation message: %w", err)
	}
	if id <= 0 || input.SpanStart == nil || input.SpanEnd == nil {
		return CommitmentCitation{}, false, nil
	}
	source, live, err := s.currentCommitmentSource(ctx, personID, id)
	if err != nil || !live || source.Direction != direction || source.SourceVersion != input.SourceVersion ||
		source.ContentSHA256 != input.ContentSHA256 ||
		!personFactPortableTime(source.EventTime).Equal(input.EventTime) ||
		!personFactPortableTime(source.RecordedTime).Equal(input.RecordedTime) {
		return CommitmentCitation{}, false, err
	}
	if !source.MatchesQuote(input.Excerpt, *input.SpanStart, *input.SpanEnd) {
		return CommitmentCitation{}, false, nil
	}
	return CommitmentCitation{MessageID: id, Quote: input.Excerpt, Start: *input.SpanStart, End: *input.SpanEnd}, true, nil
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
	if state == LoopStateOpen && current.State == LoopStateOpen {
		return current, nil
	}
	var result PersonCommitment
	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockSyncMaintenanceGate(ctx, tx); err != nil {
			return err
		}
		var deadline any
		if until != nil {
			deadline = s.dialect.TimestampParam(*until)
		}
		var reopened any
		if state == LoopStateOpen {
			reopened = s.dialect.TimestampParam(now)
		}
		previous := candidate.commitment
		var fulfilmentID int64
		if previous.FulfilmentEvidenceID != nil {
			fulfilmentID = *previous.FulfilmentEvidenceID
		}
		var err error
		result, err = scanPersonCommitment(tx.QueryRowContext(ctx, `UPDATE person_commitments
			SET owner_state = ?, snoozed_until = ?, reopened_at = COALESCE(?, reopened_at)
			WHERE id = ? AND current_claim_id = ? AND owner_state = ?
			AND COALESCE(fulfilment_evidence_id, 0) = ? AND (fulfilment_confirmed = 1) = ?
			AND COALESCE((SELECT status.supported `+latestPersonFactEvidenceStatusSQL("?", "?", "?")+`), TRUE) = TRUE
			RETURNING `+commitmentColumns, state, deadline, reopened, id, *previous.ClaimID,
			previous.State, fulfilmentID, previous.FulfilmentConfirmed,
			candidate.promise.Input.PersonID, candidate.promise.Key, candidate.promise.Input.SourceVersion))
		return err
	})
	return result, err
}

func (s *Store) PersonCommitmentsDue(ctx context.Context, personID int64, now time.Time, interval, window time.Duration, scopeFingerprint string) (bool, error) {
	var processed requiredTimestamp
	var through, backfill int64
	var anchorPagePending bool
	var scope string
	var observed sql.NullInt64
	err := s.db.QueryRowContext(ctx, s.Rebind(`SELECT processed_at, through_sequence, observed_sequence, backfill_message_id,
		(anchor_after_id > 0 OR anchor_through_sequence > 0 OR anchor_backfill_id > 0), scope_fingerprint
		FROM person_commitment_frontiers WHERE person_id = ?`), personID).Scan(&processed, &through, &observed, &backfill, &anchorPagePending, &scope)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if scopeFingerprint != "" && scope != scopeFingerprint {
		return true, nil
	}
	if now.Before(processed.Time.Add(interval)) {
		return false, nil
	}
	if backfill >= 0 || anchorPagePending {
		return true, nil
	}
	if observed.Valid {
		if !now.Before(processed.Time.Add(window)) {
			return true, nil
		}
		through = max(through, observed.Int64)
	}
	return s.HasPersonSweepChangesAfter(ctx, personID, through)
}

// commitmentMessage admits both sides only when participant identities prove a
// direct owner/person exchange. Email without verified owner participation,
// third-party recipients, bulk mail and List-Id are refused.
func (s *Store) commitmentMessage(ctx context.Context, personID int64, conv LoopConversation, m LoopMessage) (peoplesweep.CommitmentSource, bool, error) {
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
		owner, person, err = s.commitmentChatMembers(ctx, conv.ConversationID, personID, owner, person)
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
	// Begin the bounded excerpt at its first authored character. The offset
	// remains a rune coordinate in the original message body.
	first := 0
	for first < len(runes) && (runes[first] == peoplesweep.CommitmentQuoteMask || unicode.IsSpace(runes[first])) {
		first++
	}
	if first == len(runes) {
		return peoplesweep.CommitmentSource{}, false, nil
	}
	runes = runes[first:min(first+personfacts.MaxEvidenceExcerptRunes, len(runes))]
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
func (s *Store) commitmentChatMembers(ctx context.Context, conversationID, personID int64, owner, person bool) (bool, bool, error) {
	owners, err := s.loopOwnerIdentities(ctx)
	if err != nil {
		return false, false, err
	}
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
	window := peoplesweep.CommitmentWindow{PersonID: personID, ScopeFingerprint: peoplesweep.CommitmentScopeFingerprint(profile, cfg), SelectionAt: now, Now: now, Timezone: timezone, Sources: []peoplesweep.CommitmentSource{}, Anchors: []peoplesweep.CommitmentAnchor{}}
	if !profile.AllowSensitive || !slices.Contains(profile.AllowedSources, peoplesweep.SourceConversationText) {
		return window, nil
	}
	through, err := s.LatestPersonSweepChangeSequence(ctx)
	if err != nil {
		return window, err
	}
	window.SnapshotSequence = through
	var previous, anchorAfter, anchorThrough, anchorBackfill int64
	var scope string
	var selection nullableTimestamp
	err = s.db.QueryRowContext(ctx, s.Rebind(`SELECT CASE WHEN observed_sequence > through_sequence THEN observed_sequence ELSE through_sequence END,
		backfill_message_id, anchor_after_id, anchor_through_sequence, anchor_backfill_id, scope_fingerprint, selection_at FROM person_commitment_frontiers WHERE person_id = ?`), personID).
		Scan(&previous, &window.BackfillMessageID, &anchorAfter, &anchorThrough, &anchorBackfill, &scope, &selection)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return window, err
	}
	if scope != window.ScopeFingerprint {
		previous, window.BackfillMessageID, anchorAfter, anchorThrough, anchorBackfill = 0, 0, 0, 0, 0
	} else if selection.Valid && (window.BackfillMessageID >= 0 || anchorAfter > 0 || anchorThrough > 0 || anchorBackfill > 0) {
		window.SelectionAt = selection.Time
	}
	var backfillCap *int64
	if anchorAfter > 0 || anchorThrough > 0 || anchorBackfill > 0 {
		through = min(through, anchorThrough)
		if anchorAfter > 0 {
			backfillCap = &anchorBackfill
		}
		window.SnapshotSequence = through
	}
	window.ThroughSequence = through
	inProfile := func(at time.Time) bool {
		day := at.UTC().Format(time.DateOnly)
		return day >= profile.SourceSince && (profile.SourceUntil == "" || day <= profile.SourceUntil)
	}
	backfillEnd, err := s.loadCommitmentSources(ctx, &window, cfg, previous, backfillCap, inProfile)
	if err != nil || len(window.Sources) == 0 {
		return window, err
	}

	var newest time.Time
	for _, source := range window.Sources {
		if source.EventTime.After(newest) {
			newest = source.EventTime
		}
	}
	// Give every eligible anchor this bounded batch before advancing its
	// evidence frontier. Anchor quotes suffice to describe the earlier promise.
	open, err := s.listPersonCommitments(ctx, commitmentListFilter{
		personID: personID, includeSnoozed: true, afterID: &anchorAfter,
		eligible: func(c PersonCommitment) bool { return c.PromisedAt.Before(newest) && inProfile(c.PromisedAt) },
	}, now, cfg.MaxOpenAnchors+1)
	if err != nil {
		return window, err
	}
	window.AnchorBackfillID = backfillEnd
	for index, c := range open {
		if len(window.Anchors) == cfg.MaxOpenAnchors {
			window.AnchorAfterID = window.Anchors[len(window.Anchors)-1].ID
			break
		}
		anchor := peoplesweep.CommitmentAnchor{ID: c.ID, TargetKey: c.TargetKey, Direction: c.Direction, Quote: c.Quote, PromisedAt: c.PromisedAt, ReopenedAt: c.ReopenedAt}
		window.Anchors = append(window.Anchors, anchor)
		fits, err := peoplesweep.CommitmentInputFits(window, false)
		if err != nil {
			return window, err
		}
		if !fits {
			window.Anchors = window.Anchors[:len(window.Anchors)-1]
			if index == 0 {
				return window, errors.New("commitment input cannot fit one bounded anchor")
			}
			window.AnchorAfterID = window.Anchors[len(window.Anchors)-1].ID
			break
		}
	}
	return window, nil
}

func (s *Store) loadCommitmentSources(ctx context.Context, window *peoplesweep.CommitmentWindow, cfg peoplesweep.CommitmentConfig, previous int64, backfillCap *int64, inProfile func(time.Time) bool) (int64, error) {
	personID, through := window.PersonID, window.ThroughSequence
	backfillEnd := max(0, window.BackfillMessageID)
	seen, used := map[int64]bool{}, 0
	add := func(source peoplesweep.CommitmentSource) (bool, error) {
		if seen[source.MessageID] {
			return true, nil
		}
		if !inProfile(source.EventTime) || len(window.Sources) >= cfg.MaxItems || used+len(source.Text) > cfg.MaxBytes {
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
		fits, err := peoplesweep.CommitmentInputFits(*window, true)
		if err != nil || !fits {
			window.Sources = window.Sources[:len(window.Sources)-1]
			return false, err
		}
		seen[source.MessageID] = true
		used += len(source.Text)
		return true, nil
	}
	changes, err := s.ScanPersonSweepChanges(ctx, personID, previous, maxPersonSweepChangeScan)
	if err != nil {
		return backfillEnd, err
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
			return backfillEnd, err
		}
		if !ok || !inProfile(source.EventTime) ||
			(source.EventTime.Before(cutoff) && source.RecordedTime.Before(cutoff) && change.RecordedAt.Before(cutoff)) {
			continue
		}
		added, err := add(source)
		if err != nil {
			return backfillEnd, err
		}
		if !added {
			window.ThroughSequence = change.Sequence - 1
			return backfillEnd, nil
		}
	}
	if window.BackfillMessageID < 0 {
		return backfillEnd, nil
	}
	const backfillPage = 256
	ids, err := s.commitmentBackfillMessages(ctx, personID, window.BackfillMessageID, cutoff, backfillPage)
	if err != nil {
		return backfillEnd, err
	}
	for _, id := range ids {
		if backfillCap != nil && id > *backfillCap {
			return backfillEnd, nil
		}
		if !seen[id] {
			source, ok, err := s.currentCommitmentSource(ctx, personID, id)
			if err != nil {
				return backfillEnd, err
			}
			if ok && inProfile(source.EventTime) {
				added, err := add(source)
				if err != nil || !added {
					return backfillEnd, err
				}
			}
		}
		window.BackfillMessageID = id
		backfillEnd = id
	}
	if len(ids) < backfillPage {
		window.BackfillMessageID = -1
	}
	return backfillEnd, nil
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
	var claimID any
	if selected != "" {
		id, err := loadPersonFactClaimIDByKeyTx(ctx, tx, generation.PersonID, selected)
		if err != nil {
			return false, err
		}
		claimID = id
	}
	_, err = tx.ExecContext(ctx, `UPDATE person_commitments SET current_claim_id = ? WHERE person_id = ? AND target_key = ?`, claimID, generation.PersonID, descriptor.Key)
	return false, err
}

// The journal handles changes to every imported source. This keyset covers
// older live messages that predate a person's journal and detached merge
// anchors, one bounded page at a time. Merges reset the backfill cursor.
func (s *Store) commitmentBackfillMessages(ctx context.Context, personID, after int64, cutoff time.Time, limit int) ([]int64, error) {
	scoped, args := s.loopConversationQuery(LoopQuery{PersonID: personID})
	at := s.dialect.TimestampParam(cutoff)
	args = append(args, after, at, at, personID, limit)
	query := `WITH scoped AS (` + scoped + `)
		SELECT m.id FROM messages m JOIN scoped c ON c.conversation_id = m.conversation_id
		JOIN sources src ON src.id = m.source_id
		WHERE m.id > ? AND ` + s.loopLiveMessageSQL() + `
		AND ((` + s.loopTimeKey("m.sent_at") + ` >= ` + s.loopTimeKey("?") + `
			OR ` + s.loopTimeKey("m.archived_at") + ` >= ` + s.loopTimeKey("?") + `)
			OR EXISTS (SELECT 1 FROM person_commitments detached
				WHERE detached.person_id = ? AND detached.anchor_message_id = m.id AND detached.current_claim_id IS NULL))
		ORDER BY m.id LIMIT ?`
	rows, err := s.db.QueryContext(ctx, s.Rebind(query), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
