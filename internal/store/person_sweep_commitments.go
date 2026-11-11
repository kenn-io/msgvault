package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"time"

	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personfacts"
)

// commitmentPrepareError is a preparation outcome that fails only the
// optional commitment lane. The apply keeps ordinary work and retries the lane.
type commitmentPrepareError peoplesweep.FailureClass

func (f commitmentPrepareError) Error() string { return "commitment lane failed: " + string(f) }

const (
	// The archive changed between the provider window and the fenced apply.
	errCommitmentWindowChanged = commitmentPrepareError(peoplesweep.FailureArchiveGap)
	// The provider output cannot be stored or read back faithfully: an ambiguous
	// edited locator, or an expiry beyond the four-digit years of stored and
	// public timestamps.
	errCommitmentInvalidOutput = commitmentPrepareError(peoplesweep.FailureInvalidOutput)
)

type preparedCommitmentPromise struct {
	existingID int64
	replay     bool
	replace    bool
	promise    peoplesweep.CommitmentPromise
	source     peoplesweep.CommitmentSource
	expires    time.Time
}

type preparedCommitmentFulfilment struct {
	change            peoplesweep.CommitmentFulfilment
	anchor            peoplesweep.CommitmentAnchor
	input             personfacts.EvidenceInput
	replaceEvidenceID int64
	messageID         int64
}

type preparedCommitmentSweep struct {
	generation        personfacts.PreparedGeneration
	promises          []preparedCommitmentPromise
	fulfilments       []preparedCommitmentFulfilment
	through           int64
	snapshot          int64
	autoFulfil        bool
	observedOnly      bool
	backfillMessageID int64
	anchorAfterID     int64
	anchorBackfillID  int64
	scope             string
	selectionAt       time.Time
	statuses          []personfacts.EvidenceStatusChange
	maintenanceIDs    []int64
	maintenanceFrom   int64
}

func commitmentEvidence(personID int64, source peoplesweep.CommitmentSource, role, quote string, start, end int64) personfacts.EvidenceInput {
	subject := &personID
	if source.Direction == peoplesweep.CommitmentOwedBy {
		subject = nil
		role = personfacts.CommitmentOwnerSubjectRef
	}
	return personfacts.EvidenceInput{
		PersonID: personID, SubjectPersonID: subject, SubjectRef: role,
		SourceClass: personfacts.EvidenceArchive, Directness: personfacts.DirectSelf,
		Authority: personfacts.AuthorityAuthoritative, SourceRef: source.SourceRef,
		SourceVersion: source.SourceVersion, ContentSHA256: source.ContentSHA256,
		EventTime: personFactPortableTime(source.EventTime), RecordedTime: personFactPortableTime(source.RecordedTime),
		Excerpt: quote, SpanStart: &start, SpanEnd: &end,
	}
}

// The only commitment aligner is backed by sources verified against the local
// archive during preparation of a fenced sweep application.
type commitmentEvidenceAligner map[string]personfacts.EvidenceInput

func (a commitmentEvidenceAligner) Align(_ context.Context, input personfacts.EvidenceInput) (personfacts.AlignmentResult, error) {
	key, err := personfacts.EvidenceKey(input)
	if err != nil {
		return personfacts.AlignmentResult{}, err
	}
	if _, ok := a[key]; !ok {
		return personfacts.AlignmentResult{}, errors.New("commitment evidence was not verified")
	}
	return personfacts.AlignmentResult{Accepted: true, SourceVersion: input.SourceVersion, ContentSHA256: input.ContentSHA256}, nil
}

// Match an exact locator first. A unique overlapping re-quote is another
// description of the existing promise. A span covering several promises is
// ambiguous and cannot replace one of their owner decisions. With replacements,
// a stale row is a candidate only until a live promise owns the range; the first
// live match discards stale candidates and later ones.
func (s *Store) matchCommitmentLocator(ctx context.Context, reader contextRowsQuerier, personID, messageID int64, promise peoplesweep.CommitmentPromise, source peoplesweep.CommitmentSource, replacements bool) (PersonCommitment, bool, error) {
	rows, err := reader.QueryContext(ctx, `SELECT `+commitmentColumns+` FROM person_commitments
		WHERE person_id = ? AND anchor_message_id = ? AND direction = ? ORDER BY id`, personID, messageID, promise.Direction)
	if err != nil {
		return PersonCommitment{}, false, err
	}
	defer func() { _ = rows.Close() }()
	var matches []PersonCommitment
	for rows.Next() {
		existing, err := scanPersonCommitment(rows)
		if err != nil {
			return PersonCommitment{}, false, err
		}
		existing, live := locateCommitmentQuote(source, existing)
		overlaps := existing.Start < promise.End && existing.End > promise.Start
		if !overlaps || (!live && !replacements) {
			continue
		}
		if live && replacements {
			matches, replacements = nil, false
		}
		matches = append(matches, existing)
	}
	if err := rows.Err(); err != nil || len(matches) == 0 {
		return PersonCommitment{}, false, err
	}
	for _, match := range matches {
		if match.Start == promise.Start && match.End == promise.End {
			return match, false, nil
		}
	}
	return matches[0], len(matches) > 1, nil
}

func (s *Store) prepareCommitmentSweep(ctx context.Context, request peoplesweep.ApplyRequest) (*preparedCommitmentSweep, error) {
	result := request.Commitment
	window := result.Window
	prepared := &preparedCommitmentSweep{through: window.ThroughSequence, snapshot: window.SnapshotSequence, observedOnly: len(window.Sources) == 0, backfillMessageID: window.BackfillMessageID, anchorAfterID: window.AnchorAfterID, anchorBackfillID: window.AnchorBackfillID, autoFulfil: result.Config.AutoFulfil, scope: window.ScopeFingerprint, selectionAt: window.SelectionAt}
	if prepared.observedOnly {
		err := s.prepareCommitmentStatuses(ctx, request, prepared, nil)
		return prepared, err
	}
	output, err := json.Marshal(result.Output)
	if err != nil {
		return nil, err
	}
	parsed, err := peoplesweep.ParseCommitments(output, window)
	if err != nil {
		return nil, err
	}
	location, err := time.LoadLocation(window.Timezone)
	if err != nil {
		return nil, fmt.Errorf("load commitment timezone: %w", err)
	}
	sources := make(map[string]peoplesweep.CommitmentSource, len(window.Sources))
	for _, source := range window.Sources {
		sources[source.ID] = source
	}
	anchors := make(map[int64]peoplesweep.CommitmentAnchor, len(window.Anchors))
	for _, anchor := range window.Anchors {
		anchors[anchor.ID] = anchor
	}
	personID := request.Lease.PersonID
	verify := func(sourceID string) (peoplesweep.CommitmentSource, error) {
		expected := sources[sourceID]
		source, ok, err := s.currentCommitmentSource(ctx, personID, expected.MessageID)
		if err != nil {
			return peoplesweep.CommitmentSource{}, err
		}
		if !ok || source != expected {
			return peoplesweep.CommitmentSource{}, errCommitmentWindowChanged
		}
		return source, nil
	}
	generation := request.Generation
	generation.ProgramID = peoplesweep.CommitmentProgramID
	generation.ProgramVersion = peoplesweep.CommitmentProgramVersion
	generation.ProgramFingerprint = peoplesweep.CommitmentProgramFingerprint()
	generation.Claims = nil
	generation.EvidenceStatusChanges = nil
	aligner := make(commitmentEvidenceAligner)
	type matchedPromise struct {
		promise  peoplesweep.CommitmentPromise
		source   peoplesweep.CommitmentSource
		existing PersonCommitment
	}
	matches := make([]matchedPromise, 0, len(parsed.Promises))
	preferred := make(map[int64]int)
	for _, promise := range parsed.Promises {
		source, err := verify(promise.SourceID)
		if err != nil {
			return nil, err
		}
		existing, ambiguous, err := s.matchCommitmentLocator(ctx, s.db, personID, source.MessageID, promise, source, true)
		if err != nil {
			return nil, err
		}
		if ambiguous {
			return nil, errCommitmentInvalidOutput
		}
		if existing.ID != 0 {
			if _, matched := preferred[existing.ID]; !matched || (existing.Start == promise.Start && existing.End == promise.End) {
				preferred[existing.ID] = len(matches)
			}
		}
		matches = append(matches, matchedPromise{promise: promise, source: source, existing: existing})
	}
	for index, match := range matches {
		promise := match.promise
		if match.existing.ID != 0 && preferred[match.existing.ID] != index {
			selected := matches[preferred[match.existing.ID]].promise
			if promise.Start < selected.End && promise.End > selected.Start {
				continue
			}
			// Distinct disjoint promises may split one older broad citation.
			// Keep its decision on the preferred match and admit the other anew.
			match.existing = PersonCommitment{}
		}
		_, same := locateCommitmentQuote(match.source, match.existing)
		if same && match.existing.ClaimID != nil {
			promise.TargetKey = match.existing.TargetKey
		}
		source := match.source
		expires := source.EventTime.Add(result.Config.MaxDuration)
		if promise.DueOn != nil {
			// ParseCommitments above already parsed this date in this location.
			day, _ := time.ParseInLocation(time.DateOnly, *promise.DueOn, location)
			expires = day.AddDate(0, 0, 1).Add(result.Config.ExpireAfterDue).UTC()
		}
		if expires.Year() > 9999 {
			return nil, errCommitmentInvalidOutput
		}
		descriptor := personfacts.TargetDescriptor{Kind: personfacts.TargetCommitment,
			Key: promise.TargetKey, Slug: "commitment", ValueType: personfacts.ValueText,
			Cardinality: personfacts.CardinalitySingle, Sensitive: true, MaxLength: 240}
		descriptor.Revision, err = personfacts.DescriptorRevision(descriptor)
		if err != nil {
			return nil, err
		}
		value, err := json.Marshal(promise.Summary)
		if err != nil {
			return nil, err
		}
		evidence := commitmentEvidence(personID, source, "commitment-promise", promise.Quote, promise.Start, promise.End)
		key, err := personfacts.EvidenceKey(evidence)
		if err != nil {
			return nil, err
		}
		aligner[key] = evidence
		generation.Claims = append(generation.Claims, personfacts.ProposedClaim{
			Target: descriptor, Relation: personfacts.RelationSupport, Origin: personfacts.OriginCommitment,
			SubmittedValue: value, ValidUntil: &expires, Evidence: []personfacts.EvidenceInput{evidence},
		})
		current, err := s.commitmentClaimCurrent(ctx, match.existing, source, request.Generation.EvidenceStatusChanges)
		if err != nil {
			return nil, err
		}
		prepared.promises = append(prepared.promises, preparedCommitmentPromise{existingID: match.existing.ID, replace: !same, replay: !same || match.existing.ClaimID == nil || (!current && anchors[match.existing.ID].ID == 0), promise: promise, source: source, expires: expires})
	}
	for _, fulfilment := range parsed.Fulfilments {
		source, err := verify(fulfilment.SourceID)
		if err != nil {
			return nil, err
		}
		input := commitmentEvidence(personID, source, "commitment-fulfilment", fulfilment.Quote, fulfilment.Start, fulfilment.End)
		key, err := personfacts.EvidenceKey(input)
		if err != nil {
			return nil, err
		}
		aligner[key] = input
		anchor := anchors[fulfilment.CommitmentID]
		replace, err := s.withdrawnCommitmentFulfilment(ctx, personID, anchor)
		if err != nil {
			return nil, err
		}
		prepared.fulfilments = append(prepared.fulfilments, preparedCommitmentFulfilment{
			change: fulfilment, anchor: anchor, input: input, replaceEvidenceID: replace, messageID: source.MessageID,
		})
	}
	if len(generation.Claims) != 0 {
		prepared.generation, err = personfacts.PreparePersonFactGeneration(ctx, generation, aligner)
		if err != nil {
			return nil, err
		}
		for _, claim := range prepared.generation.Claims() {
			if claim.Failure != nil {
				return nil, fmt.Errorf("invalid prepared commitment: %s", claim.Failure.Reason)
			}
		}
	}
	if err := s.prepareCommitmentStatuses(ctx, request, prepared, aligner); err != nil {
		return nil, err
	}
	return prepared, nil
}

// A newer proof may replace a withdrawn or pre-reopen fulfilment. The
// transaction only replaces the exact ID inspected here.
func (s *Store) withdrawnCommitmentFulfilment(ctx context.Context, personID int64, anchor peoplesweep.CommitmentAnchor) (int64, error) {
	var evidence personfacts.Evidence
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		var id sql.NullInt64
		err := tx.QueryRowContext(ctx, `SELECT fulfilment_evidence_id FROM person_commitments
			WHERE id = ? AND person_id = ? AND target_key = ?`, anchor.ID, personID, anchor.TargetKey).Scan(&id)
		if err != nil || !id.Valid {
			return err
		}
		evidence, err = s.loadCommitmentFulfilmentTx(ctx, tx, id.Int64)
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return 0, errCommitmentWindowChanged
	}
	if err != nil || evidence.ID == 0 {
		return 0, err
	}
	if !commitmentFulfilmentCurrent(evidence, anchor.PromisedAt, anchor.ReopenedAt) {
		return evidence.ID, nil
	}
	_, live, err := s.currentCommitmentCitation(ctx, personID, anchor.Direction, evidence.Input, s.currentCommitmentSource)
	if err == nil && !live {
		return evidence.ID, nil
	}
	return 0, err
}

func (s *Store) applyCommitmentSweepTx(ctx context.Context, tx *loggedTx, request peoplesweep.ApplyRequest, prepared *preparedCommitmentSweep) error {
	personID := request.Lease.PersonID
	if err := s.applyCommitmentStatusesTx(ctx, tx, request, prepared); err != nil {
		return err
	}
	if prepared.observedOnly {
		return s.saveCommitmentFrontierTx(ctx, tx, request, prepared, false)
	}
	if err := s.vacateCommitmentLocatorsTx(ctx, tx, personID, prepared.promises); err != nil {
		return err
	}
	newAnchorNeedsReplay := false
	if len(prepared.promises) > 0 {
		generation, err := s.applyPreparedPersonFactGenerationTx(ctx, tx, prepared.generation)
		if err != nil {
			return err
		}
		for _, p := range prepared.promises {
			newAnchorNeedsReplay = newAnchorNeedsReplay || p.replay
			var claimID int64
			if err := tx.QueryRowContext(ctx, `SELECT id FROM person_fact_claims
				WHERE generation_id = ? AND target_key = ? AND rejection_action IS NULL`,
				generation.GenerationID, p.promise.TargetKey).Scan(&claimID); err != nil {
				return err
			}
			if p.existingID != 0 {
				result, err := tx.ExecContext(ctx, `UPDATE person_commitments SET current_claim_id = ?, target_key = ?,
					quote = ?, quote_start = ?, quote_end = ?, summary = ?, due_on = ?, promised_at = ?, expires_at = ?,
					owner_state = CASE WHEN ? THEN 'open' ELSE owner_state END,
					snoozed_until = CASE WHEN ? THEN NULL ELSE snoozed_until END,
					reopened_at = CASE WHEN ? THEN NULL ELSE reopened_at END,
					fulfilment_evidence_id = CASE WHEN ? THEN NULL ELSE fulfilment_evidence_id END,
					fulfilment_confirmed = CASE WHEN ? THEN 0 ELSE fulfilment_confirmed END
					WHERE id = ? AND person_id = ? AND anchor_message_id = ? AND direction = ?`,
					claimID, p.promise.TargetKey, p.promise.Quote, p.promise.Start, p.promise.End, p.promise.Summary, p.promise.DueOn,
					s.dialect.TimestampParam(p.source.EventTime), s.dialect.TimestampParam(p.expires), p.replace, p.replace, p.replace, p.replace, p.replace,
					p.existingID, personID, p.source.MessageID, p.promise.Direction)
				if err != nil {
					return fmt.Errorf("refresh commitment lifecycle: %w", err)
				}
				if count, err := result.RowsAffected(); err != nil || count != 1 {
					return errCommitmentWindowChanged
				}
				continue
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO person_commitments
				(person_id, current_claim_id, target_key, anchor_message_id, direction, quote, quote_start,
				 quote_end, summary, due_on, promised_at, expires_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT (person_id, anchor_message_id, direction, quote_start, quote_end) DO UPDATE SET
				 current_claim_id = excluded.current_claim_id, target_key = excluded.target_key,
				 quote = excluded.quote, summary = excluded.summary, due_on = excluded.due_on,
				 promised_at = excluded.promised_at, expires_at = excluded.expires_at`,
				personID, claimID, p.promise.TargetKey, p.source.MessageID, p.promise.Direction,
				p.promise.Quote, p.promise.Start, p.promise.End, p.promise.Summary, p.promise.DueOn,
				s.dialect.TimestampParam(p.source.EventTime), s.dialect.TimestampParam(p.expires))
			if err != nil {
				return fmt.Errorf("persist commitment lifecycle: %w", err)
			}
		}
	}
	for _, f := range prepared.fulfilments {
		key, err := personfacts.EvidenceKey(f.input)
		if err != nil {
			return err
		}
		evidence, err := s.insertPersonFactEvidenceTx(ctx, tx, personID, key, f.input)
		if err != nil {
			return err
		}
		confirmed, err := s.commitmentFulfilmentConfirmedTx(ctx, tx, prepared.autoFulfil, f)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE person_commitments SET
			fulfilment_evidence_id = ?, fulfilment_confirmed = ?
			WHERE id = ? AND person_id = ? AND target_key = ? AND current_claim_id IS NOT NULL
			AND promised_at < ? AND (reopened_at IS NULL OR reopened_at < ?)
			AND (fulfilment_evidence_id IS NULL OR fulfilment_evidence_id = ? OR (fulfilment_confirmed = 0 AND EXISTS (
			  SELECT 1 FROM person_fact_evidence e WHERE e.id = person_commitments.fulfilment_evidence_id
			  AND e.event_time <= ?)))`, evidence.ID, confirmed, f.anchor.ID, personID, f.anchor.TargetKey,
			s.dialect.TimestampParam(f.input.EventTime), s.dialect.TimestampParam(f.input.EventTime), f.replaceEvidenceID, s.dialect.TimestampParam(f.input.EventTime))
		if err != nil {
			return fmt.Errorf("persist commitment fulfilment: %w", err)
		}
	}
	return s.saveCommitmentFrontierTx(ctx, tx, request, prepared, newAnchorNeedsReplay)
}

// Automatic fulfilment needs the owner's opt-in and the model's confirmation.
// An owner's completion email also needs the Sent provenance the loop reader
// trusts, so an inbox copy stays an exact proposal. Returns the stored 0 or 1.
func (s *Store) commitmentFulfilmentConfirmedTx(ctx context.Context, tx *loggedTx, autoFulfil bool, f preparedCommitmentFulfilment) (int, error) {
	if !autoFulfil || !f.change.Confirmed {
		return 0, nil
	}
	if f.anchor.Direction != peoplesweep.CommitmentOwedBy {
		return 1, nil
	}
	var trusted bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM messages m
		JOIN sources src ON src.id = m.source_id JOIN conversations c ON c.id = m.conversation_id
		WHERE m.id = ? AND (c.conversation_type <> 'email_thread' OR `+sentConfirmedSQL(s.dialect, "", "m", "src")+`))`,
		f.messageID).Scan(&trusted)
	if err != nil {
		return 0, fmt.Errorf("check commitment completion provenance: %w", err)
	}
	return boolInt(trusted), nil
}

// Vacate every refreshed locator, and any other row still occupying a final
// destination, before writing final coordinates. Exact authored promises can
// swap positions; ordering their final writes cannot break that unique-key
// cycle. Refreshed rows are rewritten in this transaction. A displaced omitted
// row keeps its identity, decision and history; its parked range lies beyond
// every quote, and its quote still relocates by text, so a later extraction
// can reattach it.
func (s *Store) vacateCommitmentLocatorsTx(ctx context.Context, tx *loggedTx, personID int64, promises []preparedCommitmentPromise) error {
	if len(promises) == 0 {
		return nil
	}
	var end int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(quote_end), 0) FROM person_commitments WHERE person_id = ?`, personID).Scan(&end); err != nil {
		return err
	}
	for _, p := range promises {
		end = max(end, p.promise.End)
	}
	for _, p := range promises {
		// Adding the row ID keeps every parked range distinct.
		if _, err := tx.ExecContext(ctx, `UPDATE person_commitments SET quote_start = ? + id, quote_end = ? + id
			WHERE person_id = ? AND (id = ? OR (anchor_message_id = ? AND direction = ? AND quote_start = ? AND quote_end = ?))`,
			end+1, end+2, personID, p.existingID, p.source.MessageID, p.promise.Direction, p.promise.Start, p.promise.End); err != nil {
			return fmt.Errorf("vacate commitment locator: %w", err)
		}
	}
	return nil
}

// One frontier carries both recent-history discovery and fair anchor pages.
// New or reattached promises restart recent-history discovery, which also
// visits completion messages consumed before their earlier promise arrived.
func (s *Store) saveCommitmentFrontierTx(ctx context.Context, tx *loggedTx, request peoplesweep.ApplyRequest, prepared *preparedCommitmentSweep, replay bool) error {
	through, backfill := prepared.through, prepared.backfillMessageID
	after, anchorThrough, anchorBackfill := int64(0), int64(0), int64(0)
	var observed any
	var previousThrough int64
	var previousScope string
	if prepared.observedOnly || replay || prepared.anchorAfterID > 0 {
		err := tx.QueryRowContext(ctx, `SELECT through_sequence, scope_fingerprint FROM person_commitment_frontiers WHERE person_id = ?`, request.Lease.PersonID).Scan(&previousThrough, &previousScope)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if previousScope != prepared.scope {
			previousThrough = 0
		}
	}
	if prepared.observedOnly {
		observed = through
		// Observations do not consume the model's journal frontier.
		through = previousThrough
	} else if replay {
		// Finish this frozen discovery cycle before admitting newer journal work.
		backfill, through, anchorThrough = 0, previousThrough, prepared.through
	} else if prepared.anchorAfterID > 0 {
		after, anchorThrough, anchorBackfill = prepared.anchorAfterID, prepared.through, prepared.anchorBackfillID
		// Keep the previously consumed journal prefix while paging anchors.
		through = previousThrough
	} else if backfill >= 0 {
		// More recent-history pages remain within this cycle's snapshot.
		anchorThrough = prepared.snapshot
	}
	var selected any
	if backfill >= 0 || anchorThrough > 0 || after > 0 {
		selected = s.dialect.TimestampParam(prepared.selectionAt)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO person_commitment_frontiers
		(person_id, through_sequence, processed_at, observed_sequence, backfill_message_id,
		 anchor_after_id, anchor_through_sequence, anchor_backfill_id, scope_fingerprint, selection_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (person_id) DO UPDATE SET
		through_sequence = excluded.through_sequence, processed_at = excluded.processed_at,
		observed_sequence = excluded.observed_sequence, backfill_message_id = excluded.backfill_message_id,
		anchor_after_id = excluded.anchor_after_id, anchor_through_sequence = excluded.anchor_through_sequence,
		anchor_backfill_id = excluded.anchor_backfill_id, scope_fingerprint = excluded.scope_fingerprint,
		selection_at = excluded.selection_at`, request.Lease.PersonID, through, s.dialect.TimestampParam(request.CompletedAt), observed,
		backfill, after, anchorThrough, anchorBackfill, prepared.scope, selected)
	return err
}

func (s *Store) currentCommitmentSource(ctx context.Context, personID, messageID int64) (peoplesweep.CommitmentSource, bool, error) {
	owners, err := s.loopOwnerIdentities(ctx)
	if err != nil {
		return peoplesweep.CommitmentSource{}, false, err
	}
	return s.currentCommitmentSourceWithOwners(ctx, personID, messageID, owners)
}

func (s *Store) currentCommitmentSourceWithOwners(ctx context.Context, personID, messageID int64, owners loopOwners) (peoplesweep.CommitmentSource, bool, error) {
	var conversationID int64
	err := s.db.QueryRowContext(ctx, s.Rebind(`SELECT m.conversation_id
		FROM messages m JOIN sources src ON src.id = m.source_id
		WHERE m.id = ? AND m.conversation_id IS NOT NULL AND m.deleted_from_source_at IS NULL AND `+s.loopLiveMessageSQL()), messageID).Scan(&conversationID)
	if errors.Is(err, sql.ErrNoRows) {
		return peoplesweep.CommitmentSource{}, false, nil
	}
	if err != nil {
		return peoplesweep.CommitmentSource{}, false, err
	}
	heads, err := s.loopConversationHeads(ctx, LoopQuery{PersonID: personID, ConversationID: conversationID})
	if err != nil {
		return peoplesweep.CommitmentSource{}, false, err
	}
	conversations, err := s.loopChunk(ctx, heads, owners)
	if err != nil {
		return peoplesweep.CommitmentSource{}, false, err
	}
	for _, conversation := range conversations {
		messages, err := s.loopMessages(ctx, []int64{messageID}, owners)
		if err != nil {
			return peoplesweep.CommitmentSource{}, false, err
		}
		if len(messages) == 1 {
			return s.commitmentMessage(ctx, personID, conversation, messages[0])
		}
	}
	return peoplesweep.CommitmentSource{}, false, nil
}

func validatePersonSweepApplyCommitment(request peoplesweep.ApplyRequest) error {
	for _, claim := range request.Generation.Claims {
		if claim.Origin == personfacts.OriginCommitment || claim.Target.Kind == personfacts.TargetCommitment {
			return errors.New("ordinary sweep claims cannot carry commitments")
		}
	}
	hasCall := false
	for _, batch := range request.Batches {
		if request.CommitmentOnly && !personSweepCommitmentPurpose(batch.Purpose) {
			return errors.New("commitment-only sweep cannot apply ordinary provider calls")
		}
		hasCall = hasCall || batch.Purpose == peoplesweep.ProviderCallPurposeCommitment
	}
	if request.CommitmentOnly && (request.Brief != nil || len(request.Generation.Claims) != 0) {
		return errors.New("commitment-only sweep cannot apply ordinary provider output")
	}
	if request.CommitmentFailureClass != "" {
		if !validPersonSweepFailureClass(request.CommitmentFailureClass) ||
			!request.CommitmentRetryAt.After(request.CompletedAt) || request.Commitment != nil {
			return errors.New("invalid failed commitment result")
		}
	} else if !request.CommitmentRetryAt.IsZero() {
		return errors.New("commitment retry requires a failure")
	}
	result := request.Commitment
	if result == nil {
		if hasCall && request.CommitmentFailureClass == "" {
			return errors.New("completed commitment call requires a result or failure")
		}
		return nil
	}
	window := result.Window
	if !request.Generation.Policy.AllowSensitive || !result.Config.Enabled ||
		window.PersonID != request.Lease.PersonID || !window.Now.Equal(request.Generation.ResolvedAt) ||
		window.BackfillMessageID < -1 || window.AnchorAfterID < 0 || window.AnchorBackfillID < 0 || window.ThroughSequence < 0 || window.SnapshotSequence < window.ThroughSequence ||
		len(window.Sources) > result.Config.MaxItems || len(window.Anchors) > result.Config.MaxOpenAnchors {
		return errors.New("commitment result does not match the attempt")
	}
	if window.SelectionAt.IsZero() || window.SelectionAt.After(window.Now) || len(window.ScopeFingerprint) != 64 {
		return errors.New("invalid commitment selection bounds")
	}
	fits, err := peoplesweep.CommitmentInputFits(window, false)
	if err != nil || !fits {
		return errors.Join(errors.New("commitment input exceeds wire limit"), err)
	}
	if window.AnchorAfterID > 0 && (len(window.Anchors) == 0 || window.Anchors[len(window.Anchors)-1].ID != window.AnchorAfterID) {
		return errors.New("commitment continuation must match the last open anchor")
	}
	if hasCall == (len(window.Sources) == 0) ||
		(!hasCall && (len(window.Anchors) != 0 || len(result.Output.Promises) != 0 || len(result.Output.Fulfilments) != 0)) {
		return errors.New("commitment result does not match its provider call")
	}
	if err := result.Config.Validate(request.Budget); err != nil {
		return err
	}
	size := 0
	for _, source := range window.Sources {
		size += len(source.Text)
		if size > result.Config.MaxBytes {
			return errors.New("commitment window exceeds its byte limit")
		}
	}
	return nil
}

// The change journal covers message content and person identity changes between
// source alignment and the fenced apply. A stale window can be retried without
// committing its frontier or discarding the ordinary result.
func (s *Store) commitmentWindowCurrentTx(ctx context.Context, tx *loggedTx, request peoplesweep.ApplyRequest, prepared *preparedCommitmentSweep) (bool, error) {
	var latest int64
	if err := tx.QueryRowContext(ctx, `SELECT sequence FROM person_sweep_change_clock WHERE singleton = TRUE`).Scan(&latest); err != nil || prepared.snapshot > latest {
		return false, err
	}
	ids := append([]int64(nil), prepared.maintenanceIDs...)
	for _, source := range request.Commitment.Window.Sources {
		ids = append(ids, source.MessageID)
	}
	marks, binds := sortedIDPlaceholders(ids)
	if marks != "" {
		var changed bool
		args := append([]any{request.Lease.PersonID, prepared.snapshot, peoplesweep.SourceConversationText}, binds...)
		err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM person_sweep_changes
			WHERE person_id = ? AND sequence > ? AND source_lane = ?
			AND (message_id IS NULL OR message_id IN (`+marks+`)))`, args...).Scan(&changed)
		if err != nil || changed {
			return false, err
		}
	}
	var allowSensitive bool
	var sourceSince, sourceJSON string
	var sourceUntil sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT allow_sensitive, source_since, source_until, CAST(allowed_sources AS TEXT)
		FROM person_inference_profiles WHERE fingerprint = ?`,
		request.Generation.Policy.ProviderPolicyFingerprint).Scan(&allowSensitive, &sourceSince, &sourceUntil, &sourceJSON)
	if err != nil {
		return false, err
	}
	var lanes []peoplesweep.SourceClass
	if err := json.Unmarshal([]byte(sourceJSON), &lanes); err != nil {
		return false, err
	}
	if !allowSensitive || !slices.Contains(lanes, peoplesweep.SourceConversationText) {
		return false, nil
	}
	profile := peoplesweep.ProviderProfile{AllowSensitive: allowSensitive, SourceSince: sourceSince, SourceUntil: sourceUntil.String, AllowedSources: lanes}
	if prepared.scope != peoplesweep.CommitmentScopeFingerprint(profile, request.Commitment.Config) {
		return false, nil
	}
	for _, source := range request.Commitment.Window.Sources {
		day := source.EventTime.Format(time.DateOnly)
		if day < sourceSince || (sourceUntil.Valid && day > sourceUntil.String) {
			return false, nil
		}
	}
	return true, nil
}

// Identical extraction converges even when a promise cannot be its own older
// anchor. Changed, detached or newly supported citations need matching replay.
func (s *Store) commitmentClaimCurrent(ctx context.Context, c PersonCommitment, source peoplesweep.CommitmentSource, changes []personfacts.EvidenceStatusChange) (bool, error) {
	if c.ClaimID == nil || !c.PromisedAt.Equal(source.EventTime) {
		return false, nil
	}
	var key string
	var supported bool
	err := s.db.QueryRowContext(ctx, s.Rebind(`SELECT e.evidence_key,
		COALESCE((SELECT status.supported `+latestPersonFactEvidenceStatusSQL("e.person_id", "e.evidence_key", "e.source_version")+`), TRUE)
		FROM person_fact_evidence e WHERE e.source_version = ? AND EXISTS
		(SELECT 1 FROM person_fact_claim_evidence ce WHERE ce.claim_id = ? AND ce.evidence_id = e.id) LIMIT 1`), source.SourceVersion, *c.ClaimID).Scan(&key, &supported)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	for _, change := range changes {
		if change.EvidenceKey == key && !change.Supported {
			supported = false
		}
	}
	return supported, err
}

func (s *Store) applyCommitmentStatusesTx(ctx context.Context, tx *loggedTx, request peoplesweep.ApplyRequest, sweep *preparedCommitmentSweep) error {
	pending, err := personSweepPendingStatusChangesTx(ctx, tx, request.Lease.PersonID, sweep.statuses)
	if err != nil || len(pending) == 0 {
		return err
	}
	input := request.Generation
	input.ProgramID, input.ProgramVersion = peoplesweep.CommitmentProgramID, peoplesweep.CommitmentProgramVersion
	input.ProgramFingerprint = peoplesweep.CommitmentProgramFingerprint()
	input.Claims, input.EvidenceStatusChanges = nil, pending
	// Bind status identity to the existing commitment journal interval, even
	// when the ordinary provider cursors have not advanced.
	input.SourceCursors = []personfacts.SourceCursor{{Lane: peoplesweep.CommitmentProgramID,
		Start: fmt.Sprintf("%s:%d", sweep.scope, sweep.maintenanceFrom),
		End:   fmt.Sprintf("%s:%d", sweep.scope, sweep.through)}}
	prepared, err := personfacts.PreparePersonFactGeneration(ctx, input, nil)
	if err != nil {
		return err
	}
	_, err = s.applyPreparedPersonFactGenerationTx(ctx, tx, prepared)
	return err
}
