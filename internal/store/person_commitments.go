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

	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personfacts"
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

func (s *Store) ListPersonCommitmentsContext(ctx context.Context, personID int64, now time.Time, limit int) ([]PersonCommitment, error) {
	return s.listPersonCommitments(ctx, commitmentListFilter{personID: personID}, now, limit)
}

type commitmentListFilter struct {
	personID, id                   int64
	includeSnoozed, awaitingAgenda bool
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
	return s.listPersonCommitments(ctx, commitmentListFilter{awaitingAgenda: true}, now, limit)
}

func (s *Store) listPersonCommitments(ctx context.Context, filter commitmentListFilter, now time.Time, limit int) ([]PersonCommitment, error) {
	if filter.personID < 0 || limit < 1 || limit > 201 {
		return nil, ErrInvalidActivityRequest
	}
	var candidates []commitmentCandidate
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		query := `SELECT ` + commitmentColumns + ` FROM person_commitments
			WHERE current_claim_id IS NOT NULL AND owner_state <> 'done'
			AND ` + s.loopTimeKey("expires_at") + ` > ` + s.loopTimeKey("?")
		args := []any{s.dialect.TimestampParam(now)}
		if !filter.includeSnoozed {
			query += ` AND (owner_state <> 'snoozed' OR ` + s.loopTimeKey("snoozed_until") + ` <= ` + s.loopTimeKey("?") + `)`
			args = append(args, s.dialect.TimestampParam(now))
		}
		if filter.personID > 0 {
			query += ` AND person_id = ?`
			args = append(args, filter.personID)
		}
		if filter.id > 0 {
			query += ` AND id = ?`
			args = append(args, filter.id)
		}
		if filter.awaitingAgenda {
			query += ` AND NOT EXISTS (SELECT 1 FROM person_commitment_agenda a WHERE a.commitment_id = person_commitments.id)`
		}
		query += ` ORDER BY ` + s.loopTimeKey("promised_at") + `, id LIMIT 1000`
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
		claims := make([]personfacts.Claim, 0, len(candidates))
		for _, candidate := range candidates {
			claim, err := scanPersonFactClaim(tx.QueryRowContext(ctx, `SELECT `+personFactClaimColumns+` FROM person_fact_claims c WHERE c.id = ?`, *candidate.commitment.ClaimID))
			if err != nil {
				return err
			}
			claims = append(claims, claim)
		}
		if err := hydratePersonFactFreshnessTx(ctx, tx, claims, now); err != nil {
			return err
		}
		for i := range candidates {
			c := &candidates[i]
			if claims[i].Freshness.Status != personfacts.StatusFresh {
				continue
			}
			promise, err := scanPersonFactEvidence(tx.QueryRowContext(ctx, `SELECT `+personFactEvidenceColumns+`
				FROM person_fact_evidence e WHERE EXISTS (
					SELECT 1 FROM person_fact_claim_evidence ce WHERE ce.claim_id = ? AND ce.evidence_id = e.id)
				ORDER BY e.id LIMIT 1`, *c.commitment.ClaimID))
			if err != nil {
				return err
			}
			c.promise = promise
			if c.commitment.FulfilmentEvidenceID == nil {
				continue
			}
			evidence, err := s.loadCommitmentFulfilmentTx(ctx, tx, *c.commitment.FulfilmentEvidenceID)
			if err != nil {
				return err
			}
			c.fulfilment = &evidence
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	result := make([]PersonCommitment, 0, min(limit, len(candidates)))
	for _, candidate := range candidates {
		c := candidate.commitment
		if candidate.promise.ID == 0 {
			continue
		}
		promise, err := s.currentCommitmentCitation(ctx, c.PersonID, c.Direction, candidate.promise.Input)
		if err != nil {
			return nil, err
		}
		if promise == nil || promise.MessageID != c.AnchorMessageID || promise.Quote != c.Quote || promise.Start != c.Start || promise.End != c.End {
			continue
		}
		if evidence := candidate.fulfilment; evidence != nil && evidence.Supported &&
			(c.ReopenedAt == nil || evidence.Input.EventTime.After(*c.ReopenedAt)) {
			c.Fulfilment, err = s.currentCommitmentCitation(ctx, c.PersonID, c.Direction, evidence.Input)
			if err != nil {
				return nil, err
			}
		}
		if c.Fulfilment != nil && c.FulfilmentConfirmed {
			continue
		}
		if c.Fulfilment == nil {
			c.FulfilmentEvidenceID = nil
			c.FulfilmentConfirmed = false
		}
		result = append(result, c)
		if len(result) == limit {
			break
		}
	}
	return result, nil
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

func (s *Store) currentCommitmentCitation(ctx context.Context, personID int64, direction string, input personfacts.EvidenceInput) (*CommitmentCitation, error) {
	message, found := strings.CutPrefix(input.SourceRef, "commitment-message:")
	id, err := strconv.ParseInt(message, 10, 64)
	if !found || err != nil || id <= 0 || input.SpanStart == nil || input.SpanEnd == nil {
		return nil, nil
	}
	source, live, err := s.currentCommitmentSource(ctx, personID, id)
	if err != nil || !live || source.Direction != direction || source.SourceVersion != input.SourceVersion ||
		source.ContentSHA256 != input.ContentSHA256 {
		return nil, err
	}
	runes := []rune(source.Text)
	start, end := *input.SpanStart-source.Start, *input.SpanEnd-source.Start
	if start < 0 || end <= start || end > int64(len(runes)) || string(runes[start:end]) != input.Excerpt {
		return nil, nil
	}
	return &CommitmentCitation{MessageID: id, Quote: input.Excerpt, Start: *input.SpanStart, End: *input.SpanEnd}, nil
}

func (s *Store) DecidePersonCommitmentContext(ctx context.Context, id int64, state string, until *time.Time, now time.Time) (PersonCommitment, error) {
	if id <= 0 || (state != LoopStateDone && state != LoopStateSnoozed && state != LoopStateOpen) || (state == LoopStateSnoozed && (until == nil || !until.After(now))) || (state != LoopStateSnoozed && until != nil) {
		return PersonCommitment{}, ErrInvalidActivityRequest
	}
	var result PersonCommitment
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
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
		_, err := tx.ExecContext(ctx, `UPDATE person_commitments SET owner_state = ?, snoozed_until = ?,
   reopened_at = COALESCE(?,reopened_at) WHERE id = ?`, state, deadline, reopened, id)
		if err != nil {
			return err
		}
		result, err = scanPersonCommitment(tx.QueryRowContext(ctx, `SELECT `+commitmentColumns+` FROM person_commitments WHERE id = ?`, id))
		return err
	})
	return result, err
}

func (s *Store) PersonCommitmentsDue(ctx context.Context, personID int64, now time.Time, interval time.Duration) (bool, error) {
	var processed requiredTimestamp
	var through int64
	err := s.db.QueryRowContext(ctx, s.Rebind(`SELECT processed_at, through_sequence FROM person_commitment_frontiers WHERE person_id = ?`), personID).Scan(&processed, &through)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if now.Before(processed.Time.Add(interval)) {
		return false, nil
	}
	return s.HasPersonSweepChangesAfter(ctx, personID, through)
}

// commitmentMessage admits both sides only when participant identities prove a
// direct owner/person exchange. Email without verified owner participation,
// third-party recipients, bulk mail and List-Id are refused.
func (s *Store) commitmentMessage(ctx context.Context, personID int64, conv LoopConversation, m LoopMessage) (peoplesweep.CommitmentSource, bool, error) {
	if conv.GroupChat || (conv.ConversationType != "direct_chat" && conv.ConversationType != "email_thread") || m.ListID != "" {
		return peoplesweep.CommitmentSource{}, false, nil
	}
	direction := ""
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
	if conv.ConversationType == "direct_chat" && (!owner || !person) {
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
	if len(runes) > 2000 {
		runes = runes[:2000]
	}
	return peoplesweep.CommitmentSource{ID: fmt.Sprintf("m%d", m.ID), MessageID: m.ID, SourceID: conv.SourceID,
		Direction: direction, Text: string(runes), EventTime: m.OccurredAt.UTC(), RecordedTime: recorded.Time.UTC(),
		SourceRef: "commitment-message:" + strconv.FormatInt(m.ID, 10), SourceVersion: "commitment-body/v1:" + hash, ContentSHA256: hash}, true, nil
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
	window := peoplesweep.CommitmentWindow{PersonID: personID, Now: now, Timezone: timezone, Sources: []peoplesweep.CommitmentSource{}, Anchors: []peoplesweep.CommitmentAnchor{}}
	if !profile.AllowSensitive || !slices.Contains(profile.AllowedSources, peoplesweep.SourceConversationText) {
		return window, nil
	}
	through, err := s.LatestPersonSweepChangeSequence(ctx)
	if err != nil {
		return window, err
	}
	window.SnapshotSequence = through
	window.ThroughSequence = through
	var open []PersonCommitment
	if anchorLimit := min(cfg.MaxOpenAnchors, cfg.MaxItems-1); anchorLimit > 0 {
		open, err = s.ListPersonCommitmentsContext(ctx, personID, now, anchorLimit)
		if err != nil {
			return window, err
		}
	}
	seen, scanned, used := map[int64]bool{}, 0, 0
	inProfile := func(at time.Time) bool {
		day := at.UTC().Format(time.DateOnly)
		return day >= profile.SourceSince && (profile.SourceUntil == "" || day <= profile.SourceUntil)
	}
	add := func(source peoplesweep.CommitmentSource) (bool, error) {
		if seen[source.MessageID] {
			return true, nil
		}
		if !inProfile(source.EventTime) || len(window.Sources) >= cfg.MaxItems || used+len(source.Text) > cfg.MaxBytes {
			return false, nil
		}
		var beyond bool
		if err := s.db.QueryRowContext(ctx, s.Rebind(`SELECT EXISTS(SELECT 1 FROM person_sweep_changes WHERE person_id = ? AND message_id = ? AND sequence > ?)`), personID, source.MessageID, through).Scan(&beyond); err != nil || beyond {
			return false, err
		}
		seen[source.MessageID] = true
		used += len(source.Text)
		window.Sources = append(window.Sources, source)
		return true, nil
	}
	for _, c := range open {
		source, ok, err := s.currentCommitmentSource(ctx, personID, c.AnchorMessageID)
		if err != nil {
			return window, err
		}
		if !ok {
			continue
		}
		added, err := add(source)
		if err != nil {
			return window, err
		}
		if added {
			window.Anchors = append(window.Anchors, peoplesweep.CommitmentAnchor{ID: c.ID, TargetKey: c.TargetKey, Direction: c.Direction, Quote: c.Quote, PromisedAt: c.PromisedAt, ReopenedAt: c.ReopenedAt})
		}
	}
	var previous int64
	err = s.db.QueryRowContext(ctx, s.Rebind(`SELECT through_sequence FROM person_commitment_frontiers WHERE person_id = ?`), personID).Scan(&previous)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return window, err
	}
	changes, err := s.ScanPersonSweepChanges(ctx, personID, previous, maxPersonSweepChangeScan)
	if err != nil {
		return window, err
	}
	if len(changes) == maxPersonSweepChangeScan {
		window.ThroughSequence = min(through, changes[len(changes)-1].Sequence)
	}
	cutoff := now.Add(-cfg.Window)
	for _, change := range changes {
		if change.Sequence > through {
			break
		}
		if change.SourceLane != peoplesweep.SourceConversationText || change.MessageID == 0 || seen[change.MessageID] {
			continue
		}
		source, ok, err := s.currentCommitmentSource(ctx, personID, change.MessageID)
		if err != nil {
			return window, err
		}
		if !ok || !inProfile(source.EventTime) || len(source.Text) > cfg.MaxBytes ||
			(source.EventTime.Before(cutoff) && source.RecordedTime.Before(cutoff) && change.RecordedAt.Before(cutoff)) {
			continue
		}
		added, err := add(source)
		if err != nil {
			return window, err
		}
		if !added {
			window.ThroughSequence = change.Sequence - 1
			return window, nil
		}
	}
	rows, err := s.db.QueryContext(ctx, s.Rebind(`SELECT anchor_message_id FROM person_commitments
		WHERE person_id = ? AND current_claim_id IS NULL ORDER BY id LIMIT ?`), personID, cfg.MaxOpenAnchors)
	if err != nil {
		return window, err
	}
	var detached []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return window, err
		}
		detached = append(detached, id)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return window, err
	}
	for _, id := range detached {
		source, ok, err := s.currentCommitmentSource(ctx, personID, id)
		if err != nil {
			return window, err
		}
		if ok {
			if _, err := add(source); err != nil {
				return window, err
			}
		}
	}
	for conv, err := range s.LoopConversationsContext(ctx, LoopQuery{PersonID: personID}) {
		if err != nil {
			return window, err
		}
		for m, err := range conv.Walk() {
			if err != nil {
				return window, err
			}
			scanned++
			if scanned > 2000 {
				return window, nil
			}
			if seen[m.ID] {
				continue
			}
			if !inProfile(m.OccurredAt) {
				continue
			}
			source, ok, err := s.commitmentMessage(ctx, personID, conv, m)
			if err != nil {
				return window, err
			}
			if !ok || (source.EventTime.Before(cutoff) && source.RecordedTime.Before(cutoff)) {
				continue
			}
			if _, err := add(source); err != nil {
				return window, err
			}
			if len(window.Sources) >= cfg.MaxItems {
				return window, nil
			}
		}
	}
	return window, nil
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
	encoded, err := json.Marshal(decisions, json.Deterministic(true))
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
