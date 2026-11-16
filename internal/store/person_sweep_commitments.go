package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
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
	for _, source := range window.Sources {
		sources[source.ID] = source
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
	generation.ProgramID, generation.ProgramVersion = peoplesweep.CommitmentProgramID, peoplesweep.CommitmentProgramVersion
	generation.ProgramFingerprint = peoplesweep.CommitmentProgramFingerprint()
	generation.Claims, generation.EvidenceStatusChanges = nil, nil
	aligner := make(commitmentEvidenceAligner)
	evidence := make([]personfacts.EvidenceInput, len(result.Output.Promises))
	for i, promise := range result.Output.Promises {
		source, err := verify(promise.SourceID)
		if err != nil {
			return nil, err
		}
		evidence[i] = commitmentEvidence(personID, source, "commitment-promise", promise.Quote, promise.Start, promise.End)
	}
	for i, promise := range result.Output.Promises {
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
		key, err := personfacts.EvidenceKey(evidence[i])
		if err != nil {
			return nil, err
		}
		aligner[key] = evidence[i]
		generation.Claims = append(generation.Claims, personfacts.ProposedClaim{
			Target: descriptor, Relation: personfacts.RelationSupport, Origin: personfacts.OriginCommitment,
			SubmittedValue: value, Evidence: []personfacts.EvidenceInput{evidence[i]},
		})
		prepared.promises = append(prepared.promises, promise)
	}
	if len(generation.Claims) != 0 {
		var err error
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
	// Clear the oversized messages this window retried.
	for _, source := range request.Commitment.Window.Sources {
		if _, err := tx.ExecContext(ctx, `DELETE FROM person_commitment_oversized WHERE person_id = ? AND message_id = ?`, request.Lease.PersonID, source.MessageID); err != nil {
			return err
		}
	}
	return s.saveCommitmentFrontierTx(ctx, tx, request, prepared)
}

// Save discovery progress after the fenced apply.
func (s *Store) saveCommitmentFrontierTx(ctx context.Context, tx *loggedTx, request peoplesweep.ApplyRequest, prepared *preparedCommitmentSweep) error {
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
		if scope == prepared.scope {
			through = previous
		}
	}
	since := prepared.selectionAt.Add(-request.Commitment.Config.Window)
	if backfill == -1 {
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
