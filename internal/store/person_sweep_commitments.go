package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strconv"
	"strings"
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
)

type preparedCommitmentSweep struct {
	generation        personfacts.PreparedGeneration
	promises          []peoplesweep.CommitmentPromise
	through           int64
	snapshot          int64
	observedOnly      bool
	backfillMessageID int64
	scope             string
	selectionAt       time.Time
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

func (s *Store) prepareCommitmentSweep(ctx context.Context, request peoplesweep.ApplyRequest) (*preparedCommitmentSweep, error) {
	result := request.Commitment
	window := result.Window
	prepared := &preparedCommitmentSweep{through: window.ThroughSequence, snapshot: window.SnapshotSequence, observedOnly: len(window.Sources) == 0, backfillMessageID: window.BackfillMessageID, scope: window.ScopeFingerprint, selectionAt: window.SelectionAt}
	if prepared.observedOnly {
		return prepared, nil
	}
	sources := make(map[string]peoplesweep.CommitmentSource, len(window.Sources))
	personID := request.Lease.PersonID
	for _, expected := range window.Sources {
		source, ok, err := s.currentCommitmentSource(ctx, personID, expected.MessageID)
		if err != nil {
			return nil, err
		}
		if !ok || source != expected {
			return nil, errCommitmentWindowChanged
		}
		sources[source.ID] = source
	}
	generation := request.Generation
	generation.ProgramID, generation.ProgramVersion = peoplesweep.CommitmentProgramID, peoplesweep.CommitmentProgramVersion
	generation.ProgramFingerprint = peoplesweep.CommitmentProgramFingerprint()
	generation.SourceCursors = []personfacts.SourceCursor{{Lane: "commitments", Start: strconv.FormatInt(window.BackfillMessageID, 10), End: fmt.Sprintf("%d:%d", window.ThroughSequence, window.SnapshotSequence)}}
	generation.Claims, generation.EvidenceStatusChanges = nil, nil
	promises, err := s.matchCommitmentCitations(ctx, personID, sources, result.Output.Promises)
	if err != nil {
		return nil, err
	}
	aligner := make(commitmentEvidenceAligner)
	for _, promise := range promises {
		source, ok := sources[promise.SourceID]
		if !ok {
			return nil, errCommitmentWindowChanged
		}
		evidence := commitmentEvidence(personID, source, "commitment-promise", promise.Quote, promise.Start, promise.End)
		descriptor := personfacts.TargetDescriptor{Kind: personfacts.TargetCommitment,
			Key: promise.TargetKey, Slug: "commitment", ValueType: personfacts.ValueCommitment,
			Cardinality: personfacts.CardinalitySingle, Sensitive: true, MaxLength: 240}
		revision, err := personfacts.DescriptorRevision(descriptor)
		if err != nil {
			return nil, err
		}
		descriptor.Revision = revision
		value, err := json.Marshal(personfacts.CommitmentValue{Direction: promise.Direction, Summary: promise.Summary, DueOn: promise.DueOn})
		if err != nil {
			return nil, err
		}
		key, err := personfacts.EvidenceKey(evidence)
		if err != nil {
			return nil, err
		}
		aligner[key] = evidence
		generation.Claims = append(generation.Claims, personfacts.ProposedClaim{
			Target: descriptor, Relation: personfacts.RelationSupport, Origin: personfacts.OriginCommitment,
			SubmittedValue: value, Evidence: []personfacts.EvidenceInput{evidence},
		})
		prepared.promises = append(prepared.promises, promise)
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
	return prepared, nil
}

func (s *Store) matchCommitmentCitations(ctx context.Context, personID int64, sources map[string]peoplesweep.CommitmentSource, promises []peoplesweep.CommitmentPromise) ([]peoplesweep.CommitmentPromise, error) {
	if len(promises) == 0 {
		return promises, nil
	}
	args := []any{personID}
	for _, source := range sources {
		args = append(args, source.SourceRef)
	}
	if len(args) == 1 {
		return nil, errCommitmentWindowChanged
	}
	rows, err := s.db.QueryContext(ctx, s.Rebind(`SELECT d.target_key, e.source_ref, e.source_version, e.subject_ref, e.span_start, e.span_end, e.excerpt, e.recorded_time
		FROM person_commitments d JOIN person_fact_claims c ON c.person_id = d.person_id AND c.target_kind = 'commitment' AND c.target_key = d.target_key AND c.origin = 'commitment'
		JOIN person_fact_claim_evidence ce ON ce.claim_id = c.id JOIN person_fact_evidence e ON e.id = ce.evidence_id
		WHERE d.person_id = ? AND c.relation = 'support' AND c.rejection_action IS NULL AND c.normalized_value_json IS NOT NULL
		AND e.source_ref IN (`+strings.Repeat("?,", len(args)-2)+`?)`), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	matches := make([]map[string]bool, len(promises))
	oldMatches := make([]map[string]bool, len(promises))
	for rows.Next() {
		var key, ref, version, subject, quote string
		var start, end sql.NullInt64
		var recorded requiredTimestamp
		if err := rows.Scan(&key, &ref, &version, &subject, &start, &end, &quote, &recorded); err != nil {
			return nil, err
		}
		for i, promise := range promises {
			source := sources[promise.SourceID]
			role := "commitment-promise"
			if source.Direction == peoplesweep.CommitmentOwedBy {
				role = personfacts.CommitmentOwnerSubjectRef
			}
			identity := fmt.Sprintf("commitment-body/v1:%d:%q:", source.SourceID, source.SourceMessageID)
			if source.SourceRef != ref || !strings.HasPrefix(version, identity) || !personFactPortableTime(source.RecordedTime).Equal(recorded.Time) || subject != role || !start.Valid || !end.Valid ||
				(source.SourceVersion != version && promise.Quote != quote) || (source.SourceVersion == version && (promise.Start >= end.Int64 || start.Int64 >= promise.End)) || peoplesweep.CommitmentQuoteRepeated(source.Text, quote) || peoplesweep.CommitmentQuoteRepeated(source.Text, promise.Quote) {
				continue
			}
			target := &matches[i]
			if source.SourceVersion != version {
				target = &oldMatches[i]
			}
			if *target == nil {
				*target = make(map[string]bool)
			}
			(*target)[key] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range matches {
		if len(matches[i]) == 0 && len(oldMatches[i]) == 1 {
			matches[i] = oldMatches[i]
		}
	}
	return reconcileCommitmentCitations(promises, matches), nil
}

func reconcileCommitmentCitations(promises []peoplesweep.CommitmentPromise, matches []map[string]bool) []peoplesweep.CommitmentPromise {
	uses := make(map[string]int)
	for _, match := range matches {
		for key := range match {
			uses[key]++
		}
	}
	result := make([]peoplesweep.CommitmentPromise, 0, len(promises))
	for i, match := range matches {
		if len(match) > 1 {
			continue
		}
		promise := promises[i]
		if len(match) == 1 {
			for key := range match {
				if uses[key] == 1 {
					promise.TargetKey = key
				}
			}
		}
		result = append(result, promise)
	}
	return result
}

func (s *Store) applyCommitmentSweepTx(ctx context.Context, tx *loggedTx, request peoplesweep.ApplyRequest, prepared *preparedCommitmentSweep) error {
	if len(prepared.promises) > 0 {
		_, err := s.applyPreparedPersonFactGenerationTx(ctx, tx, prepared.generation)
		if err != nil {
			return err
		}
		for _, p := range prepared.promises {
			_, err := tx.ExecContext(ctx, `INSERT INTO person_commitments (person_id, target_key) VALUES (?, ?) ON CONFLICT (person_id, target_key) DO NOTHING`, request.Lease.PersonID, p.TargetKey)
			if err != nil {
				return fmt.Errorf("persist commitment decision: %w", err)
			}
		}
	}
	sources := request.Commitment.Window.Sources
	if len(sources) > 0 {
		byRef := make(map[string]peoplesweep.CommitmentSource, len(sources))
		args := []any{request.Lease.PersonID}
		for _, source := range sources {
			byRef[source.SourceRef] = source
			args = append(args, source.SourceRef)
		}
		// A formerly unique citation cannot retain its decision after it repeats.
		rows, err := tx.QueryContext(ctx, `SELECT d.id, d.target_key, e.source_ref, e.excerpt,
			(SELECT e0.excerpt FROM person_fact_claims c0
			 JOIN person_fact_claim_evidence ce0 ON ce0.claim_id = c0.id
			 JOIN person_fact_evidence e0 ON e0.id = ce0.evidence_id
			 WHERE c0.person_id = d.person_id AND c0.target_kind = 'commitment' AND c0.target_key = d.target_key
			 AND c0.origin = 'commitment' AND e0.source_ref = e.source_ref ORDER BY c0.id, e0.id LIMIT 1)
			FROM person_commitments d
			JOIN person_fact_claims c ON c.person_id = d.person_id AND c.target_kind = 'commitment' AND c.target_key = d.target_key AND c.origin = 'commitment'
			JOIN person_fact_claim_evidence ce ON ce.claim_id = c.id
			JOIN person_fact_evidence e ON e.id = ce.evidence_id
			WHERE d.person_id = ? AND d.owner_state <> 'open' AND e.source_ref IN (`+strings.Repeat("?,", len(sources)-1)+`?)`, args...)
		if err != nil {
			return err
		}
		ids := make(map[int64]bool)
		for rows.Next() {
			var id int64
			var key, ref, quote, originalQuote string
			if err := rows.Scan(&id, &key, &ref, &quote, &originalQuote); err != nil {
				_ = rows.Close()
				return err
			}
			source := byRef[ref]
			if peoplesweep.CommitmentQuoteRepeated(source.Text, quote) && key == peoplesweep.UniqueCommitmentTargetKey(request.Lease.PersonID, source, originalQuote) {
				ids[id] = true
			}
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return err
		}
		for id := range ids {
			if _, err := tx.ExecContext(ctx, `UPDATE person_commitments SET owner_state = 'open', snoozed_until = NULL WHERE id = ?`, id); err != nil {
				return fmt.Errorf("clear ambiguous commitment decision: %w", err)
			}
		}
	}
	return s.saveCommitmentFrontierTx(ctx, tx, request, prepared)
}

// Save discovery progress after the fenced apply.
func (s *Store) saveCommitmentFrontierTx(ctx context.Context, tx *loggedTx, request peoplesweep.ApplyRequest, prepared *preparedCommitmentSweep) error {
	window := request.Commitment.Window
	if window.ResetSkipped {
		if _, err := tx.ExecContext(ctx, `DELETE FROM person_commitment_skipped WHERE person_id = ?`, request.Lease.PersonID); err != nil {
			return err
		}
	}
	for _, id := range window.RetriedMessageIDs {
		if _, err := tx.ExecContext(ctx, `DELETE FROM person_commitment_skipped WHERE person_id = ? AND message_id = ?`, request.Lease.PersonID, id); err != nil {
			return err
		}
	}
	for _, id := range window.SkippedMessageIDs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO person_commitment_skipped (person_id, message_id, max_bytes) VALUES (?, ?, ?) ON CONFLICT (person_id, message_id) DO UPDATE SET max_bytes = excluded.max_bytes`, request.Lease.PersonID, id, request.Commitment.Config.MaxBytes); err != nil {
			return err
		}
	}
	through, backfill := prepared.through, prepared.backfillMessageID
	var observed any
	if prepared.observedOnly {
		var previous int64
		var scope string
		err := tx.QueryRowContext(ctx, `SELECT through_sequence, scope_fingerprint FROM person_commitment_frontiers WHERE person_id = ?`, request.Lease.PersonID).Scan(&previous, &scope)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		observed = through
		through = 0
		if len(scope) == 64 && scope[:56] == prepared.scope[:56] {
			through = previous
		}
	}
	since := prepared.selectionAt.Add(-request.Commitment.Config.Window)
	if backfill == -1 && prepared.through >= prepared.snapshot {
		since = request.Commitment.Window.Now.Add(-request.Commitment.Config.Window)
	}
	var profileSince string
	if err := tx.QueryRowContext(ctx, `SELECT source_since FROM person_inference_profiles WHERE fingerprint = ?`, request.Generation.Policy.ProviderPolicyFingerprint).Scan(&profileSince); err != nil {
		return err
	}
	if profileStart, err := time.Parse(time.DateOnly, profileSince); err == nil && profileStart.After(since) {
		since = profileStart
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO person_commitment_frontiers
 (person_id, through_sequence, processed_at, observed_sequence, backfill_message_id, scope_fingerprint, discovery_since)
 VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT (person_id) DO UPDATE SET
 through_sequence = excluded.through_sequence, processed_at = excluded.processed_at,
 observed_sequence = excluded.observed_sequence, backfill_message_id = excluded.backfill_message_id,
 scope_fingerprint = excluded.scope_fingerprint, discovery_since = excluded.discovery_since`, request.Lease.PersonID, through, s.dialect.TimestampParam(request.CompletedAt), observed,
		backfill, prepared.scope, s.dialect.TimestampParam(since))
	return err
}

func (s *Store) currentCommitmentSource(ctx context.Context, personID, messageID int64) (peoplesweep.CommitmentSource, bool, error) {
	owners, err := s.loopOwnerIdentities(ctx)
	if err != nil {
		return peoplesweep.CommitmentSource{}, false, err
	}
	var conversationID int64
	err = s.db.QueryRowContext(ctx, s.Rebind(`SELECT m.conversation_id
		FROM messages m JOIN sources src ON src.id = m.source_id
		WHERE m.id = ? AND m.conversation_id IS NOT NULL AND `+s.loopLiveMessageSQL()), messageID).Scan(&conversationID)
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
	if len(conversations) > 0 {
		messages, err := s.loopMessages(ctx, []int64{messageID}, owners)
		if err != nil {
			return peoplesweep.CommitmentSource{}, false, err
		}
		if len(messages) == 1 {
			return s.commitmentMessage(ctx, personID, conversations[0], messages[0], owners)
		}
	}
	return peoplesweep.CommitmentSource{}, false, nil
}

func validatePersonSweepApplyCommitment(request peoplesweep.ApplyRequest) error {
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
		window.ThroughSequence < 0 || window.SnapshotSequence < window.ThroughSequence ||
		len(window.Sources) > result.Config.MaxItems {
		return errors.New("commitment result does not match the attempt")
	}
	if window.SelectionAt.IsZero() || window.SelectionAt.After(window.Now) || len(window.ScopeFingerprint) != 64 {
		return errors.New("invalid commitment selection bounds")
	}
	if hasCall == (len(window.Sources) == 0) ||
		(!hasCall && (len(result.Output.Promises) != 0)) {
		return errors.New("commitment result does not match its provider call")
	}
	return result.Config.Validate(request.Budget)
}

// The change journal covers message content and person identity changes between
// source alignment and the fenced apply. Window sources and person-level changes
// are watched in the conversation lane. A stale window can be retried without
// committing its frontier or discarding the ordinary result.
func (s *Store) commitmentWindowCurrentTx(ctx context.Context, tx *loggedTx, request peoplesweep.ApplyRequest, prepared *preparedCommitmentSweep) (bool, error) {
	if _, err := tx.ExecContext(ctx, s.dialect.InsertOrIgnore(`INSERT OR IGNORE INTO archive_metadata (key, value) VALUES (?, '0')`), accountIdentityRevisionKey); err != nil {
		return false, err
	}
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT CAST(value AS INTEGER) FROM archive_metadata WHERE key = ?`+s.dialect.SelectForUpdate(), accountIdentityRevisionKey).Scan(&revision); err != nil {
		return false, err
	}
	if revision != request.Commitment.Window.IdentityRevision {
		return false, nil
	}
	var latest int64
	if err := tx.QueryRowContext(ctx, `SELECT sequence FROM person_sweep_change_clock WHERE singleton = TRUE`).Scan(&latest); err != nil || prepared.snapshot > latest {
		return false, err
	}
	var ids []int64
	for _, source := range request.Commitment.Window.Sources {
		ids = append(ids, source.MessageID)
	}
	marks, binds := sortedIDPlaceholders(ids)
	if marks == "" {
		return true, nil
	}
	var changed bool
	args := append([]any{request.Lease.PersonID, prepared.snapshot, peoplesweep.SourceConversationText}, binds...)
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM person_sweep_changes
		WHERE person_id = ? AND sequence > ? AND (source_lane = ?
		AND (message_id IS NULL OR message_id IN (`+marks+`))))`, args...).Scan(&changed)
	return !changed, err
}
