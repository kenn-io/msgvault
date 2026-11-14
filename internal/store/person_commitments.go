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
	ClaimID         *int64                      `json:"claim_id,omitempty"`
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
}

func (s *Store) ListPersonCommitmentsContext(ctx context.Context, personID int64, now time.Time, limit int, includeClosed bool) ([]PersonCommitment, error) {
	if personID < 0 || limit < 1 || limit > 201 {
		return nil, ErrInvalidActivityRequest
	}
	return s.listPersonCommitments(ctx, personID, now, limit, includeClosed)
}

// commitmentSourceCache serves one list. It shares raw source reads and each
// conversation scope, but every cited message still finds its own live
// conversation and message, and every citation validates its own version,
// author, time and exact rune range. Nothing outlives the list.
func (s *Store) commitmentSourceCache(owners loopOwners) func(context.Context, int64, int64) (peoplesweep.CommitmentSource, bool, error) {
	sources := make(map[[2]int64]peoplesweep.CommitmentSource)
	scopes := make(map[[2]int64][]LoopConversation)
	read := s.commitmentConversations(owners)
	conversations := func(ctx context.Context, personID, conversationID int64) ([]LoopConversation, error) {
		key := [2]int64{personID, conversationID}
		if scope, found := scopes[key]; found {
			return scope, nil
		}
		scope, err := read(ctx, personID, conversationID)
		if err == nil {
			scopes[key] = scope
		}
		return scope, err
	}
	return func(ctx context.Context, personID, messageID int64) (peoplesweep.CommitmentSource, bool, error) {
		key := [2]int64{personID, messageID}
		if source, found := sources[key]; found {
			return source, source.MessageID != 0, nil
		}
		source, live, err := s.currentCommitmentSourceWithOwners(ctx, personID, messageID, owners, conversations)
		if err == nil {
			sources[key] = source
		}
		return source, live, err
	}
}

func (s *Store) listPersonCommitments(ctx context.Context, personID int64, now time.Time, limit int, includeClosed bool) ([]PersonCommitment, error) {
	owners, err := s.loopOwnerIdentities(ctx)
	if err != nil {
		return nil, err
	}
	lookup := s.commitmentSourceCache(owners)
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
				c, visible, err := s.hydrateCommitmentCandidate(ctx, candidate, now, lookup)
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
		args = append(args, s.dialect.TimestampParam(now.Add(-90*24*time.Hour)), s.dialect.TimestampParam(now))
		if after.ID != 0 {
			query += ` AND (` + key + `, id) > (` + s.loopTimeKey("?") + `, ?)`
			args = append(args, s.dialect.TimestampParam(after.PromisedAt), after.ID)
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
		return fmt.Errorf("invalid commitment citation")
	}
	c.ClaimID = &claimID
	c.AnchorMessageID, c.Direction, c.Quote = messageID, value.Direction, promise.Input.Excerpt
	c.Start, c.End = *promise.Input.SpanStart, *promise.Input.SpanEnd
	c.Summary, c.DueOn, c.PromisedAt = value.Summary, value.DueOn, promise.Input.EventTime
	candidate.commitment, candidate.promise = c, promise
	return nil
}

func (s *Store) hydrateCommitmentCandidate(ctx context.Context, candidate commitmentCandidate, now time.Time, lookup func(context.Context, int64, int64) (peoplesweep.CommitmentSource, bool, error)) (PersonCommitment, bool, error) {
	c := candidate.commitment
	if candidate.promise.ID == 0 {
		return c, false, nil
	}
	promise, live, err := s.currentCommitmentCitation(ctx, c.PersonID, c.Direction, candidate.promise.Input, lookup)
	if err != nil || !live || promise.MessageID != c.AnchorMessageID || promise.Quote != c.Quote || promise.Start != c.Start || promise.End != c.End {
		return c, false, err
	}
	c.Freshness = describePersonFactFreshness(personfacts.TargetRef{Kind: personfacts.TargetCommitment}, nil, &c.PromisedAt, now).Status
	if c.State == LoopStateSnoozed && c.SnoozedUntil != nil && !c.SnoozedUntil.After(now) {
		c.State, c.SnoozedUntil = LoopStateOpen, nil
	}
	return c, true, nil
}

func (s *Store) currentCommitmentCitation(ctx context.Context, personID int64, direction string, input personfacts.EvidenceInput, lookup func(context.Context, int64, int64) (peoplesweep.CommitmentSource, bool, error)) (CommitmentCitation, bool, error) {
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
	source, live, err := lookup(ctx, personID, id)
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
	current, visible, err := s.hydrateCommitmentCandidate(ctx, candidate, now, s.currentCommitmentSource)
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
		previous := candidate.commitment
		var err error
		result, err = scanPersonCommitment(tx.QueryRowContext(ctx, `UPDATE person_commitments
			SET owner_state = ?, snoozed_until = ? WHERE id = ? AND owner_state = ?
			AND COALESCE((SELECT status.supported `+latestPersonFactEvidenceStatusSQL("?", "?", "?")+`), TRUE) = TRUE
			RETURNING `+commitmentColumns, state, deadline, id, previous.State,
			candidate.promise.Input.PersonID, candidate.promise.Key, candidate.promise.Input.SourceVersion))
		return err
	})
	if err == nil {
		result.ClaimID, result.AnchorMessageID, result.Direction, result.Quote = current.ClaimID, current.AnchorMessageID, current.Direction, current.Quote
		result.Start, result.End, result.Summary, result.DueOn, result.PromisedAt, result.Freshness = current.Start, current.End, current.Summary, current.DueOn, current.PromisedAt, current.Freshness
	}
	return result, err
}

// PersonCommitmentDiscoveryContext returns the saved discovery window and progress.
func (s *Store) PersonCommitmentDiscoveryContext(ctx context.Context, personID int64) (*time.Time, bool, error) {
	query := `SELECT f.discovery_since, f.backfill_message_id,
		EXISTS(SELECT 1 FROM person_sweep_changes c WHERE c.person_id = t.person_id
			AND c.sequence > CASE WHEN COALESCE(f.observed_sequence, 0) > f.through_sequence THEN f.observed_sequence ELSE f.through_sequence END)
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
		var pending bool
		if err := rows.Scan(&at, &backfill, &pending); err != nil {
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
		complete = complete && backfill.Valid && backfill.Int64 == -1 && !pending
	}
	return since, complete && since != nil, rows.Err()
}

func (s *Store) PersonCommitmentsDue(ctx context.Context, personID int64, now time.Time, interval, window time.Duration, scopeFingerprint string) (bool, error) {
	var processed requiredTimestamp
	var through, backfill int64
	var scope string
	var observed sql.NullInt64
	err := s.db.QueryRowContext(ctx, s.Rebind(`SELECT processed_at, through_sequence, observed_sequence, backfill_message_id,
		scope_fingerprint
		FROM person_commitment_frontiers WHERE person_id = ?`), personID).Scan(&processed, &through, &observed, &backfill, &scope)
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
	if backfill >= 0 || backfill < -2 {
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
	window := peoplesweep.CommitmentWindow{PersonID: personID, ScopeFingerprint: peoplesweep.CommitmentScopeFingerprint(profile, cfg), SelectionAt: now, Now: now, Timezone: timezone, Sources: []peoplesweep.CommitmentSource{}}
	if !profile.AllowSensitive || !slices.Contains(profile.AllowedSources, peoplesweep.SourceConversationText) {
		return window, nil
	}
	through, err := s.LatestPersonSweepChangeSequence(ctx)
	if err != nil {
		return window, err
	}
	window.SnapshotSequence = through
	var previous int64
	var scope string
	var since nullableTimestamp
	err = s.db.QueryRowContext(ctx, s.Rebind(`SELECT CASE WHEN observed_sequence > through_sequence THEN observed_sequence ELSE through_sequence END,
  backfill_message_id, scope_fingerprint, discovery_since FROM person_commitment_frontiers WHERE person_id = ?`), personID).
		Scan(&previous, &window.BackfillMessageID, &scope, &since)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return window, err
	}
	if window.BackfillMessageID != -1 && since.Valid && since.Time.Before(now.Add(-cfg.Window)) {
		window.SelectionAt = since.Time.Add(cfg.Window)
	}
	if scope != window.ScopeFingerprint {
		previous, window.BackfillMessageID = 0, 0
	}
	window.ThroughSequence = through
	inProfile := func(at time.Time) bool {
		day := at.UTC().Format(time.DateOnly)
		return day >= profile.SourceSince && (profile.SourceUntil == "" || day <= profile.SourceUntil)
	}
	_, err = s.loadCommitmentSources(ctx, &window, cfg, previous, inProfile)
	return window, err
}

func (s *Store) loadCommitmentSources(ctx context.Context, window *peoplesweep.CommitmentWindow, cfg peoplesweep.CommitmentConfig, previous int64, inProfile func(time.Time) bool) (int64, error) {
	personID, through := window.PersonID, window.ThroughSequence
	// -1 means complete, -2 means complete with an oversized gap, and lower
	// values encode the backfill position while such a gap remains.
	gap := window.BackfillMessageID <= -2
	if window.BackfillMessageID < -2 {
		window.BackfillMessageID = -(window.BackfillMessageID + 3)
	}
	defer func() {
		if gap {
			if window.BackfillMessageID < 0 {
				window.BackfillMessageID = -2
			} else {
				window.BackfillMessageID = -window.BackfillMessageID - 3
			}
		}
	}()
	backfillEnd := max(0, window.BackfillMessageID)
	seen, used := map[int64]bool{}, 0
	add := func(source peoplesweep.CommitmentSource) (bool, error) {
		if seen[source.MessageID] {
			return true, nil
		}
		if !inProfile(source.EventTime) {
			return true, nil
		}
		if len(source.Text) > cfg.MaxBytes {
			gap = true
			return true, nil
		}
		if len(window.Sources) >= cfg.MaxItems || used+len(source.Text) > cfg.MaxBytes {
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
				gap = true
			}
			return !fitsAlone, err
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
