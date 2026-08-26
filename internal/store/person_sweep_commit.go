package store

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personfacts"
)

type personSweepApplyFailpointKey struct{}

type personSweepLockKind uint8

const (
	personSweepLockDailyUsage personSweepLockKind = iota + 1
	personSweepLockBatch
	personSweepLockWork
)

type personSweepLockCoordinate struct {
	kind     personSweepLockKind
	value    string
	ordinal  int
	personID int64
}

func personSweepUsageWorkLockPlan(
	coordinates []personSweepBatchCoordinate, personID int64,
) []personSweepLockCoordinate {
	days := make([]string, 0, len(coordinates))
	for _, coordinate := range coordinates {
		days = append(days, coordinate.day)
	}
	slices.Sort(days)
	days = slices.Compact(days)
	coordinates = append([]personSweepBatchCoordinate(nil), coordinates...)
	slices.SortFunc(coordinates, func(a, b personSweepBatchCoordinate) int { return a.ordinal - b.ordinal })
	plan := make([]personSweepLockCoordinate, 0, len(days)+len(coordinates)+1)
	for _, day := range days {
		plan = append(plan, personSweepLockCoordinate{kind: personSweepLockDailyUsage, value: day})
	}
	for _, coordinate := range coordinates {
		plan = append(plan, personSweepLockCoordinate{kind: personSweepLockBatch,
			value: coordinate.day, ordinal: coordinate.ordinal})
	}
	if personID > 0 {
		plan = append(plan, personSweepLockCoordinate{kind: personSweepLockWork, personID: personID})
	}
	return plan
}

func withPersonSweepApplyFailpoint(ctx context.Context, fail func(string) error) context.Context {
	return context.WithValue(ctx, personSweepApplyFailpointKey{}, fail)
}

func personSweepApplyStage(ctx context.Context, stage string) error {
	fail, _ := ctx.Value(personSweepApplyFailpointKey{}).(func(string) error)
	if fail == nil {
		return nil
	}
	return fail(stage)
}

func (s *Store) ApplyPersonSweep(
	ctx context.Context, request peoplesweep.ApplyRequest,
) (peoplesweep.ApplyResult, error) {
	return s.applyPersonSweepWithAligner(ctx, request, PersonSweepEvidenceAligner{Store: s})
}

func (s *Store) applyPersonSweepWithAligner(
	ctx context.Context, request peoplesweep.ApplyRequest, aligner personfacts.EvidenceAligner,
) (peoplesweep.ApplyResult, error) {
	if err := validatePersonSweepApplyRequest(request); err != nil {
		return peoplesweep.ApplyResult{}, err
	}
	if _, err := peoplesweep.ValidatePersonFactCursorBinding(
		request.Generation, request.CursorEnvelope, request.CursorAdvances); err != nil {
		return peoplesweep.ApplyResult{}, err
	}
	prepared, err := personfacts.PreparePersonFactGeneration(ctx, request.Generation, aligner)
	if err != nil {
		return peoplesweep.ApplyResult{}, fmt.Errorf("prepare person sweep generation: %w", err)
	}

	var result peoplesweep.ApplyResult
	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		attempt, err := s.lockPersonSweepBudgetAttempt(ctx, tx, request.AttemptID)
		if err != nil {
			return err
		}
		if attempt.runID != request.RunID || attempt.personID != request.Lease.PersonID ||
			attempt.leaseFence != request.Lease.Fence || attempt.status != peoplesweep.AttemptRunning {
			return peoplesweep.ErrLeaseLost
		}
		if err := s.requirePersonSweepBudgetRunRunning(ctx, tx, request.RunID); err != nil {
			return err
		}
		if err := s.verifyPersonSweepAttemptBindingTx(ctx, tx, request, attempt); err != nil {
			return err
		}
		coordinates, err := s.personSweepApplyBatchCoordinatesTx(ctx, tx, request)
		if err != nil {
			return err
		}
		current, err := s.lockPersonSweepUsageThenWorkTx(
			ctx, tx, request.AttemptID, coordinates, request.Lease)
		if err != nil {
			return err
		}
		if !current {
			return peoplesweep.ErrLeaseLost
		}
		if err := personSweepApplyStage(ctx, "locks"); err != nil {
			return err
		}

		if len(request.Batches) > 0 {
			active, err := s.hasActivePersonInferenceConsentTx(
				ctx, tx, request.Generation.Policy.ProviderPolicyFingerprint)
			if err != nil {
				return err
			}
			if !active {
				return peoplesweep.ErrPersonSweepConsentRevoked
			}
			if err := personSweepApplyStage(ctx, "consent"); err != nil {
				return err
			}
		}

		generation, facts, err := s.applyPreparedPersonFactGenerationDetailedTx(
			ctx, tx, prepared, func(stage string) error { return personSweepApplyStage(ctx, stage) })
		if err != nil {
			return err
		}
		result.Generation = *generation
		result.Mutations = peoplesweep.ApplyMutationMetadata{
			GenerationInserted:         facts.generationInserted,
			ClaimRowsInserted:          facts.claimRowsInserted,
			EvidenceStatusRowsInserted: facts.evidenceStatusRowsInserted,
			ResolutionRowsInserted:     facts.resolutionRowsInserted,
			DecisionRowsInserted:       facts.decisionRowsInserted,
			ProjectionRowsWritten:      facts.projectionRowsWritten,
			VCardRevisionBumped:        facts.vcardRevisionBumped,
		}

		batchCount, err := s.reconcilePersonSweepSuccessBatchesTx(ctx, tx, request)
		if err != nil {
			return err
		}
		result.Mutations.BatchRowsReconciled = batchCount
		if err := personSweepApplyStage(ctx, "usage"); err != nil {
			return err
		}

		cursorCount, err := s.advanceBoundPersonSweepCursorsTx(ctx, tx, request)
		if err != nil {
			return err
		}
		result.Mutations.CursorRowsAdvanced = cursorCount
		if err := personSweepApplyStage(ctx, "cursor_cas"); err != nil {
			return err
		}

		attemptResult, err := tx.ExecContext(ctx, `UPDATE person_sweep_attempts SET
			status = 'succeeded', failure_class = '', generation_id = ?, generation_key = ?,
			claim_count = ?, decision_count = ?, projected_write_count = ?, completed_at = ?
			WHERE id = ? AND status = 'running'`, generation.GenerationID,
			generation.GenerationKey, len(request.Generation.Claims), len(generation.Decisions),
			result.Mutations.ProjectionRowsWritten, s.dialect.TimestampParam(request.CompletedAt),
			request.AttemptID)
		if err != nil {
			return fmt.Errorf("complete person sweep attempt: %w", err)
		}
		changed, err := attemptResult.RowsAffected()
		if err != nil || changed != 1 {
			return peoplesweep.ErrLeaseLost
		}
		result.Mutations.AttemptRowsSucceeded = int(changed)
		if err := s.refreshPersonSweepAttemptAndRunUsage(ctx, tx, request.AttemptID, request.RunID); err != nil {
			return err
		}
		workCount, err := s.finishPersonSweepWorkTx(ctx, tx, request)
		if err != nil {
			return err
		}
		result.Mutations.WorkRowsUpdated = workCount
		return nil
	})
	if err != nil {
		return peoplesweep.ApplyResult{}, fmt.Errorf("apply person sweep: %w", err)
	}
	return result, nil
}

func validatePersonSweepApplyRequest(request peoplesweep.ApplyRequest) error {
	if request.Lease.PersonID <= 0 || strings.TrimSpace(request.Lease.WorkerID) == "" ||
		request.Lease.Fence < 0 || strings.TrimSpace(request.RunID) == "" ||
		strings.TrimSpace(request.AttemptID) == "" || request.CompletedAt.IsZero() ||
		request.Generation.PersonID != request.Lease.PersonID || len(request.CursorEnvelope) == 0 {
		return errors.New("apply person sweep requires complete lease, run, attempt, generation, cursors, and completion time")
	}
	if request.Usage.Requests < 0 || request.Usage.InputTokens < 0 ||
		request.Usage.OutputTokens < 0 || request.Usage.EstimatedCostMicroUSD < 0 {
		return errors.New("apply person sweep usage must not be negative")
	}
	statusOnly := len(request.Batches) == 0
	if statusOnly {
		if request.Usage != (peoplesweep.Usage{}) || len(request.Generation.Claims) != 0 ||
			request.Generation.Provider != peoplesweep.StatusOnlyProvider ||
			request.Generation.ProviderVersion != peoplesweep.StatusOnlyProviderVersion ||
			request.Generation.Model != peoplesweep.StatusOnlyModel ||
			request.Generation.ModelVersion != peoplesweep.StatusOnlyModelVersion {
			return errors.New("apply person sweep status-only generation has spoofed model identity or payload")
		}
		return nil
	}
	if request.Generation.Provider == peoplesweep.StatusOnlyProvider ||
		request.Generation.ProviderVersion == peoplesweep.StatusOnlyProviderVersion ||
		request.Generation.Model == peoplesweep.StatusOnlyModel ||
		request.Generation.ModelVersion == peoplesweep.StatusOnlyModelVersion {
		return errors.New("apply person sweep provider generation uses host-only identity")
	}
	if strings.TrimSpace(request.Generation.Provider) == "" ||
		strings.TrimSpace(request.Generation.ProviderVersion) == "" ||
		strings.TrimSpace(request.Generation.Model) == "" ||
		strings.TrimSpace(request.Generation.ModelVersion) == "" {
		return errors.New("apply person sweep provider generation has incomplete identity")
	}
	seen := make(map[int]struct{}, len(request.Batches))
	for _, batch := range request.Batches {
		if batch.Ordinal < 0 || batch.ReservationID == "" || batch.InputHash == "" ||
			batch.Usage.InputTokens < 0 || batch.Usage.OutputTokens < 0 ||
			batch.ActualCostMicroUSD < 0 || batch.Latency < 0 ||
			batch.ProviderVersion != request.Generation.ProviderVersion ||
			batch.ModelVersion != request.Generation.ModelVersion ||
			!peoplesweep.IsSafeProviderMetadata(batch.ProviderRequestID) {
			return errors.New("apply person sweep completed batch has invalid identity or usage")
		}
		if _, duplicate := seen[batch.Ordinal]; duplicate {
			return errors.New("apply person sweep completed batch ordinal is duplicated")
		}
		seen[batch.Ordinal] = struct{}{}
	}
	return nil
}

func (s *Store) verifyPersonSweepAttemptBindingTx(
	ctx context.Context, tx *loggedTx, request peoplesweep.ApplyRequest,
	attempt personSweepBudgetAttempt,
) error {
	var envelopeJSON, envelopeHash, program, catalog string
	err := tx.QueryRowContext(ctx, `SELECT cursor_envelope_json, envelope_hash,
		program_fingerprint, catalog_fingerprint FROM person_sweep_attempts WHERE id = ?`,
		request.AttemptID).Scan(&envelopeJSON, &envelopeHash, &program, &catalog)
	if err != nil {
		return fmt.Errorf("load person sweep attempt envelope: %w", err)
	}
	var durable []peoplesweep.GenerationCursor
	if err := json.Unmarshal([]byte(envelopeJSON), &durable); err != nil {
		return errors.New("person sweep attempt has invalid cursor envelope")
	}
	wantHash, err := personSweepRawEnvelopeHash(request.CursorEnvelope)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(durable, request.CursorEnvelope) || envelopeHash != wantHash ||
		program != request.Generation.ProgramFingerprint || catalog != request.Generation.CatalogFingerprint ||
		attempt.providerFingerprint != request.Generation.Policy.ProviderPolicyFingerprint {
		return errors.New("person sweep attempt identity does not match application")
	}
	var provider, model string
	var allowSensitive bool
	if err := tx.QueryRowContext(ctx, `SELECT provider_kind, model, allow_sensitive
		FROM person_inference_profiles WHERE fingerprint = ?`, attempt.providerFingerprint,
	).Scan(&provider, &model, &allowSensitive); err != nil {
		return fmt.Errorf("load person sweep provider profile identity: %w", err)
	}
	if allowSensitive != request.Generation.Policy.AllowSensitive {
		return errors.New("person sweep generation policy does not match durable profile")
	}
	if len(request.Batches) > 0 {
		if provider != request.Generation.Provider || model != request.Generation.Model ||
			allowSensitive != request.Generation.Policy.AllowSensitive {
			return errors.New("person sweep generation provider identity does not match durable profile")
		}
	}
	return nil
}

func (s *Store) personSweepApplyBatchCoordinatesTx(
	ctx context.Context, tx *loggedTx, request peoplesweep.ApplyRequest,
) ([]personSweepBatchCoordinate, error) {
	coordinates, err := listPersonSweepBatchCoordinatesTx(ctx, tx, request.AttemptID)
	if err != nil {
		return nil, err
	}
	if len(coordinates) != len(request.Batches) {
		return nil, errors.New("apply person sweep batches do not exactly cover durable reservations")
	}
	ordinals := make(map[int]struct{}, len(request.Batches))
	for _, batch := range request.Batches {
		ordinals[batch.Ordinal] = struct{}{}
	}
	for _, coordinate := range coordinates {
		if _, ok := ordinals[coordinate.ordinal]; !ok {
			return nil, errors.New("apply person sweep batches do not exactly cover durable reservations")
		}
	}
	return coordinates, nil
}

func (s *Store) lockPersonSweepUsageThenWorkTx(
	ctx context.Context, tx *loggedTx, attemptID string,
	coordinates []personSweepBatchCoordinate, lease peoplesweep.Lease,
) (bool, error) {
	current := false
	for _, coordinate := range personSweepUsageWorkLockPlan(coordinates, lease.PersonID) {
		switch coordinate.kind {
		case personSweepLockDailyUsage:
			if _, err := s.lockPersonSweepDailyUsage(ctx, tx, coordinate.value); err != nil {
				return false, err
			}
		case personSweepLockBatch:
			if _, found, err := loadPersonSweepBatchTx(
				ctx, tx, attemptID, coordinate.ordinal, s.IsPostgreSQL()); err != nil {
				return false, err
			} else if !found {
				return false, errors.New("person sweep lock plan batch is missing")
			}
		case personSweepLockWork:
			var err error
			current, err = s.lockPersonSweepWorkRowTx(ctx, tx, lease)
			if err != nil {
				return false, err
			}
		}
	}
	return current, nil
}

func (s *Store) lockPersonSweepWorkRowTx(
	ctx context.Context, tx *loggedTx, lease peoplesweep.Lease,
) (bool, error) {
	if !s.IsPostgreSQL() {
		if _, err := tx.ExecContext(ctx, `UPDATE person_sweep_work SET person_id = person_id
			WHERE person_id = ?`, lease.PersonID); err != nil {
			return false, err
		}
	}
	query := `SELECT lease_owner, lease_fence, COALESCE(lease_until > ` + s.dialect.Now() + `, FALSE)
		FROM person_sweep_work WHERE person_id = ?`
	if s.IsPostgreSQL() {
		query += " FOR UPDATE"
	}
	var owner string
	var fence int64
	var live bool
	if err := tx.QueryRowContext(ctx, query, lease.PersonID).Scan(&owner, &fence, &live); errors.Is(err, sql.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("lock person sweep work: %w", err)
	}
	return live && owner == lease.WorkerID && fence == lease.Fence, nil
}

func personSweepRawEnvelopeHash(cursors []peoplesweep.GenerationCursor) (string, error) {
	encoded, err := json.Marshal(cursors)
	if err != nil {
		return "", fmt.Errorf("encode person sweep attempt envelope: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func (s *Store) reconcilePersonSweepSuccessBatchesTx(
	ctx context.Context, tx *loggedTx, request peoplesweep.ApplyRequest,
) (int, error) {
	coordinates, err := listPersonSweepBatchCoordinatesTx(ctx, tx, request.AttemptID)
	if err != nil {
		return 0, err
	}
	if len(coordinates) != len(request.Batches) {
		return 0, errors.New("apply person sweep batches do not exactly cover durable reservations")
	}
	byOrdinal := make(map[int]peoplesweep.CompletedBatch, len(request.Batches))
	for _, completed := range request.Batches {
		byOrdinal[completed.Ordinal] = completed
	}
	for _, coordinate := range coordinates {
		if _, ok := byOrdinal[coordinate.ordinal]; !ok {
			return 0, errors.New("apply person sweep batches do not exactly cover durable reservations")
		}
	}
	actualTotal := peoplesweep.Usage{}
	for _, coordinate := range coordinates {
		completed := byOrdinal[coordinate.ordinal]
		batch, found, err := loadPersonSweepBatchTx(ctx, tx, request.AttemptID,
			coordinate.ordinal, s.IsPostgreSQL())
		if err != nil {
			return 0, err
		}
		if !found || batch.status != "running" || batch.reservationID != completed.ReservationID ||
			batch.inputHash != completed.InputHash {
			return 0, errors.New("apply person sweep completed batch is not the running reservation")
		}
		actual := peoplesweep.Usage{Requests: max(batch.reserved.Requests, 1),
			InputTokens:           max(batch.reserved.InputTokens, completed.Usage.InputTokens),
			OutputTokens:          max(batch.reserved.OutputTokens, completed.Usage.OutputTokens),
			EstimatedCostMicroUSD: max(batch.reserved.EstimatedCostMicroUSD, completed.ActualCostMicroUSD)}
		result, err := tx.ExecContext(ctx, `UPDATE person_sweep_batches SET status = 'succeeded',
			provider_request_id = ?, actual_requests = ?, actual_input_tokens = ?,
			actual_output_tokens = ?, actual_cost_micro_usd = ?, latency_milliseconds = ?,
			failure_class = '', completed_at = ? WHERE attempt_id = ? AND batch_ordinal = ?
			AND status = 'running'`, completed.ProviderRequestID, actual.Requests,
			actual.InputTokens, actual.OutputTokens, actual.EstimatedCostMicroUSD,
			completed.Latency.Milliseconds(), s.dialect.TimestampParam(request.CompletedAt),
			request.AttemptID, completed.Ordinal)
		if err != nil {
			return 0, err
		}
		changed, err := result.RowsAffected()
		if err != nil || changed != 1 {
			return 0, errors.New("apply person sweep batch lost running state")
		}
		if err := s.adjustPersonSweepDailyUsage(ctx, tx, batch.day,
			negatePersonSweepUsage(batch.reserved), actual); err != nil {
			return 0, err
		}
		actualTotal, err = addPersonSweepUsage(actualTotal, actual)
		if err != nil {
			return 0, err
		}
	}
	if actualTotal != request.Usage {
		return 0, errors.New("apply person sweep aggregate usage does not match completed batches")
	}
	return len(coordinates), nil
}

func (s *Store) advanceBoundPersonSweepCursorsTx(
	ctx context.Context, tx *loggedTx, request peoplesweep.ApplyRequest,
) (int, error) {
	advances := append([]peoplesweep.CursorAdvance(nil), request.CursorAdvances...)
	slices.SortFunc(advances, func(a, b peoplesweep.CursorAdvance) int {
		if a.Key.SourceLane != b.Key.SourceLane {
			return strings.Compare(string(a.Key.SourceLane), string(b.Key.SourceLane))
		}
		if order := personSweepCursorApplyOrder(a.Mode) - personSweepCursorApplyOrder(b.Mode); order != 0 {
			return order
		}
		if a.ExpectedSequence != b.ExpectedSequence {
			return cmp.Compare(a.ExpectedSequence, b.ExpectedSequence)
		}
		if a.ExpectedReconcileKey != b.ExpectedReconcileKey {
			return strings.Compare(a.ExpectedReconcileKey, b.ExpectedReconcileKey)
		}
		if a.ExpectedDocumentKey != b.ExpectedDocumentKey {
			return strings.Compare(a.ExpectedDocumentKey, b.ExpectedDocumentKey)
		}
		return strings.Compare(a.NextReconcileKey, b.NextReconcileKey)
	})
	count := 0
	for _, advance := range advances {
		var query string
		var args []any
		switch advance.Mode {
		case peoplesweep.GenerationCursorOptimistic:
			query = `UPDATE person_sweep_cursors SET optimistic_sequence = ?, optimistic_document_key = ?,
				updated_at = ` + s.dialect.Now() + `
				WHERE person_id = ? AND source_lane = ? AND program_fingerprint = ?
				AND catalog_fingerprint = ? AND optimistic_sequence = ? AND optimistic_document_key = ?`
			args = []any{advance.NextSequence, advance.NextDocumentKey, advance.Key.PersonID, advance.Key.SourceLane,
				advance.Key.ProgramFingerprint, advance.Key.CatalogFingerprint, advance.ExpectedSequence,
				advance.ExpectedDocumentKey}
		case peoplesweep.GenerationCursorReconciliation:
			queryUpper := `SELECT reconcile_upper_key FROM person_sweep_cursors
				WHERE person_id = ? AND source_lane = ? AND program_fingerprint = ?
				AND catalog_fingerprint = ?`
			if s.IsPostgreSQL() {
				queryUpper += " FOR UPDATE"
			}
			var upper string
			if err := tx.QueryRowContext(ctx, queryUpper, advance.Key.PersonID, advance.Key.SourceLane,
				advance.Key.ProgramFingerprint, advance.Key.CatalogFingerprint).Scan(&upper); err != nil {
				return 0, fmt.Errorf("lock person sweep reconciliation cursor: %w", err)
			}
			if (advance.NextReconcileKey >= upper && advance.NextDocumentKey == "") != advance.ReconciliationDone {
				return 0, errors.New("person sweep reconciliation completion does not match upper bound")
			}
			query = `UPDATE person_sweep_cursors SET reconcile_after_key = ?, reconcile_document_key = ?,
				reconciliation_complete = ?,
				updated_at = ` + s.dialect.Now() + ` WHERE person_id = ? AND source_lane = ?
				AND program_fingerprint = ? AND catalog_fingerprint = ?
				AND optimistic_sequence = ? AND reconcile_after_key = ? AND reconcile_document_key = ?
				AND ? <= reconcile_upper_key AND reconciliation_complete = FALSE`
			args = []any{advance.NextReconcileKey, advance.NextDocumentKey, advance.ReconciliationDone, advance.Key.PersonID,
				advance.Key.SourceLane, advance.Key.ProgramFingerprint, advance.Key.CatalogFingerprint,
				advance.ExpectedSequence, advance.ExpectedReconcileKey, advance.ExpectedDocumentKey,
				advance.NextReconcileKey}
		case peoplesweep.GenerationCursorBackstop:
			if advance.CapturedBackstopUpperKey == "" ||
				(advance.ExpectedBackstopUpperKey == "" && advance.ExpectedReconcileKey != "") ||
				(advance.ExpectedBackstopUpperKey != "" &&
					advance.ExpectedBackstopUpperKey != advance.CapturedBackstopUpperKey) ||
				!personSweepDocumentCoordinateAdvanced(advance.ExpectedReconcileKey,
					advance.ExpectedDocumentKey, advance.NextReconcileKey, advance.NextDocumentKey) ||
				advance.NextReconcileKey > advance.CapturedBackstopUpperKey ||
				advance.BackstopComplete != (advance.NextReconcileKey == advance.CapturedBackstopUpperKey &&
					advance.NextDocumentKey == "") {
				return 0, errors.New("person sweep backstop advance has invalid bounded progress")
			}
			nextUpper, nextAfter, nextDocument := advance.CapturedBackstopUpperKey,
				advance.NextReconcileKey, advance.NextDocumentKey
			if advance.BackstopComplete {
				nextUpper, nextAfter, nextDocument = "", "", ""
			}
			query = `UPDATE person_sweep_cursors SET backstop_upper_key = ?, backstop_after_key = ?,
				backstop_document_key = ?,
				last_backstop_at = CASE WHEN ? THEN ? ELSE last_backstop_at END,
				updated_at = ` + s.dialect.Now() + `
				WHERE person_id = ? AND source_lane = ? AND program_fingerprint = ?
				AND catalog_fingerprint = ? AND optimistic_sequence = ?
				AND backstop_upper_key = ? AND backstop_after_key = ? AND backstop_document_key = ?`
			args = []any{nextUpper, nextAfter, nextDocument, advance.BackstopComplete,
				s.dialect.TimestampParam(request.CompletedAt), advance.Key.PersonID,
				advance.Key.SourceLane, advance.Key.ProgramFingerprint, advance.Key.CatalogFingerprint,
				advance.ExpectedSequence, advance.ExpectedBackstopUpperKey, advance.ExpectedReconcileKey,
				advance.ExpectedDocumentKey}
		default:
			return 0, errors.New("apply person sweep has unsupported cursor advance mode")
		}
		result, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return 0, fmt.Errorf("advance bound person sweep cursor: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil || changed != 1 {
			return 0, errors.New("advance bound person sweep cursor compare-and-set failed")
		}
		count++
	}
	return count, nil
}

func personSweepDocumentCoordinateAdvanced(from, fromDocument, to, toDocument string) bool {
	return to > from || (to == from && toDocument > fromDocument)
}

func personSweepCursorApplyOrder(mode peoplesweep.GenerationCursorMode) int {
	switch mode {
	case peoplesweep.GenerationCursorReconciliation:
		return 0
	case peoplesweep.GenerationCursorBackstop:
		return 1
	case peoplesweep.GenerationCursorOptimistic:
		return 2
	default:
		return 3
	}
}

func (s *Store) finishPersonSweepWorkTx(
	ctx context.Context, tx *loggedTx, request peoplesweep.ApplyRequest,
) (int, error) {
	remaining := request.DeferredCursorWork
	var err error
	if !remaining {
		err = tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM person_sweep_cursors c WHERE c.person_id = ?
		AND c.program_fingerprint = ? AND c.catalog_fingerprint = ?
		AND (c.backstop_upper_key <> '' OR
		(c.reconciliation_complete = FALSE AND c.reconcile_after_key <> c.reconcile_upper_key) OR EXISTS (
			SELECT 1 FROM person_sweep_changes ch WHERE ch.person_id = c.person_id
			AND ch.source_lane = c.source_lane AND ch.sequence > c.optimistic_sequence)))`,
			request.Lease.PersonID, request.Generation.ProgramFingerprint,
			request.Generation.CatalogFingerprint).Scan(&remaining)
	}
	if err != nil {
		return 0, fmt.Errorf("check remaining person sweep work: %w", err)
	}
	var result sql.Result
	if remaining {
		result, err = tx.ExecContext(ctx, `UPDATE person_sweep_work SET available_at = `+s.dialect.Now()+`,
			lease_owner = '', lease_until = NULL, last_failure_class = '', updated_at = `+s.dialect.Now()+`
			WHERE person_id = ? AND lease_owner = ? AND lease_fence = ?`, request.Lease.PersonID,
			request.Lease.WorkerID, request.Lease.Fence)
	} else {
		result, err = tx.ExecContext(ctx, `DELETE FROM person_sweep_work
			WHERE person_id = ? AND lease_owner = ? AND lease_fence = ?`, request.Lease.PersonID,
			request.Lease.WorkerID, request.Lease.Fence)
	}
	if err != nil {
		return 0, fmt.Errorf("finish person sweep work: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		return 0, peoplesweep.ErrLeaseLost
	}
	return int(changed), nil
}
