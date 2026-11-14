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
	// invalid commitment output.
	errCommitmentInvalidOutput = commitmentPrepareError(peoplesweep.FailureInvalidOutput)
)

type preparedCommitmentPromise struct {
	promise peoplesweep.CommitmentPromise
}

type preparedCommitmentSweep struct {
	generation        personfacts.PreparedGeneration
	promises          []preparedCommitmentPromise
	through           int64
	snapshot          int64
	observedOnly      bool
	backfillMessageID int64
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

func (s *Store) prepareCommitmentSweep(ctx context.Context, request peoplesweep.ApplyRequest) (*preparedCommitmentSweep, error) {
	result := request.Commitment
	window := result.Window
	prepared := &preparedCommitmentSweep{through: window.ThroughSequence, snapshot: window.SnapshotSequence, observedOnly: len(window.Sources) == 0, backfillMessageID: window.BackfillMessageID, scope: window.ScopeFingerprint, selectionAt: window.SelectionAt}
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
	generation.ProgramID = peoplesweep.CommitmentProgramID
	generation.ProgramVersion = peoplesweep.CommitmentProgramVersion
	generation.ProgramFingerprint = peoplesweep.CommitmentProgramFingerprint()
	generation.Claims = nil
	generation.EvidenceStatusChanges = nil
	aligner := make(commitmentEvidenceAligner)
	for _, promise := range parsed.Promises {
		source, err := verify(promise.SourceID)
		if err != nil {
			return nil, err
		}
		descriptor := personfacts.TargetDescriptor{Kind: personfacts.TargetCommitment,
			Key: promise.TargetKey, Slug: "commitment", ValueType: personfacts.ValueCommitment,
			Cardinality: personfacts.CardinalitySingle, Sensitive: true, MaxLength: 240}
		descriptor.Revision, err = personfacts.DescriptorRevision(descriptor)
		if err != nil {
			return nil, err
		}
		value, err := json.Marshal(personfacts.CommitmentValue{Direction: promise.Direction, Summary: promise.Summary, DueOn: promise.DueOn})
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
			SubmittedValue: value, Evidence: []personfacts.EvidenceInput{evidence},
		})
		prepared.promises = append(prepared.promises, preparedCommitmentPromise{promise: promise})
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

func (s *Store) applyCommitmentSweepTx(ctx context.Context, tx *loggedTx, request peoplesweep.ApplyRequest, prepared *preparedCommitmentSweep) error {
	personID := request.Lease.PersonID
	if err := s.applyCommitmentStatusesTx(ctx, tx, request, prepared); err != nil {
		return err
	}
	if prepared.observedOnly {
		return s.saveCommitmentFrontierTx(ctx, tx, request, prepared)
	}
	if len(prepared.promises) > 0 {
		_, err := s.applyPreparedPersonFactGenerationTx(ctx, tx, prepared.generation)
		if err != nil {
			return err
		}
		for _, p := range prepared.promises {
			_, err := tx.ExecContext(ctx, `INSERT INTO person_commitments (person_id, target_key) VALUES (?, ?) ON CONFLICT (person_id, target_key) DO NOTHING`, personID, p.promise.TargetKey)
			if err != nil {
				return fmt.Errorf("persist commitment decision: %w", err)
			}
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
	return s.currentCommitmentSourceWithOwners(ctx, personID, messageID, owners, s.commitmentConversations(owners))
}

// commitmentConversationReader returns a person's live conversation scope.
// It never depends on which message a promise cites.
type commitmentConversationReader func(ctx context.Context, personID, conversationID int64) ([]LoopConversation, error)

func (s *Store) commitmentConversations(owners loopOwners) commitmentConversationReader {
	return func(ctx context.Context, personID, conversationID int64) ([]LoopConversation, error) {
		heads, err := s.loopConversationHeads(ctx, LoopQuery{PersonID: personID, ConversationID: conversationID})
		if err != nil {
			return nil, err
		}
		return s.loopChunk(ctx, heads, owners)
	}
}

func (s *Store) currentCommitmentSourceWithOwners(ctx context.Context, personID, messageID int64, owners loopOwners, read commitmentConversationReader) (peoplesweep.CommitmentSource, bool, error) {
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
	conversations, err := read(ctx, personID, conversationID)
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
		window.BackfillMessageID < -1 || window.ThroughSequence < 0 || window.SnapshotSequence < window.ThroughSequence ||
		len(window.Sources) > result.Config.MaxItems {
		return errors.New("commitment result does not match the attempt")
	}
	if window.SelectionAt.IsZero() || window.SelectionAt.After(window.Now) || len(window.ScopeFingerprint) != 64 {
		return errors.New("invalid commitment selection bounds")
	}
	fits, err := peoplesweep.CommitmentInputFits(window)
	if err != nil || !fits {
		return errors.Join(errors.New("commitment input exceeds wire limit"), err)
	}
	if hasCall == (len(window.Sources) == 0) ||
		(!hasCall && (len(result.Output.Promises) != 0)) {
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

// Window sources and person-level changes are watched in the conversation lane.
// A maintained citation can live in any lane, so its message is watched in all.
func (s *Store) commitmentCitedChangedTx(ctx context.Context, tx *loggedTx, request peoplesweep.ApplyRequest, prepared *preparedCommitmentSweep) (bool, error) {
	ids := append([]int64(nil), prepared.maintenanceIDs...)
	for _, source := range request.Commitment.Window.Sources {
		ids = append(ids, source.MessageID)
	}
	marks, binds := sortedIDPlaceholders(ids)
	if marks == "" {
		return false, nil
	}
	cited, citedBinds := sortedIDPlaceholders(prepared.maintenanceIDs)
	if cited == "" {
		cited = "NULL"
	}
	var changed bool
	args := append([]any{request.Lease.PersonID, prepared.snapshot, peoplesweep.SourceConversationText}, binds...)
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM person_sweep_changes
		WHERE person_id = ? AND sequence > ? AND ((source_lane = ?
		AND (message_id IS NULL OR message_id IN (`+marks+`))) OR message_id IN (`+cited+`)))`,
		append(args, citedBinds...)...).Scan(&changed)
	return changed, err
}

// The change journal covers message content and person identity changes between
// source alignment and the fenced apply. A stale window can be retried without
// committing its frontier or discarding the ordinary result.
func (s *Store) commitmentWindowCurrentTx(ctx context.Context, tx *loggedTx, request peoplesweep.ApplyRequest, prepared *preparedCommitmentSweep) (bool, error) {
	var latest int64
	if err := tx.QueryRowContext(ctx, `SELECT sequence FROM person_sweep_change_clock WHERE singleton = TRUE`).Scan(&latest); err != nil || prepared.snapshot > latest {
		return false, err
	}
	if changed, err := s.commitmentCitedChangedTx(ctx, tx, request, prepared); err != nil || changed {
		return false, err
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
