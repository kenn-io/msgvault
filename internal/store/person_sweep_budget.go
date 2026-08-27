package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"math"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/peoplesweep"
)

func (s *Store) StartPersonSweepRun(
	ctx context.Context, input peoplesweep.StartRun,
) (peoplesweep.Run, error) {
	if err := validatePersonSweepStartRun(input); err != nil {
		return peoplesweep.Run{}, err
	}
	_, err := s.db.ExecContext(ctx, s.Rebind(`
		INSERT INTO person_sweep_runs
			(id, kind, mode, status, program_fingerprint, catalog_fingerprint,
			 provider_fingerprint, started_at)
		VALUES (?, ?, ?, 'running', ?, ?, ?, ?)`), input.ID, input.Kind, input.Mode,
		input.ProgramFingerprint, input.CatalogFingerprint, input.ProviderFingerprint,
		s.dialect.TimestampParam(input.StartedAt))
	if err != nil {
		return peoplesweep.Run{}, fmt.Errorf("start person sweep run: %w", err)
	}
	return peoplesweep.Run{StartRun: input, Status: peoplesweep.RunRunning}, nil
}

func validatePersonSweepStartRun(input peoplesweep.StartRun) error {
	if strings.TrimSpace(input.ID) == "" || strings.TrimSpace(input.ProgramFingerprint) == "" ||
		strings.TrimSpace(input.CatalogFingerprint) == "" ||
		strings.TrimSpace(input.ProviderFingerprint) == "" || input.StartedAt.IsZero() {
		return errors.New("start person sweep run: complete identity and start time are required")
	}
	if input.Kind != peoplesweep.RunScheduled && input.Kind != peoplesweep.RunManual {
		return errors.New("start person sweep run: invalid kind")
	}
	if !validPersonSweepRunMode(input.Mode) {
		return errors.New("start person sweep run: invalid mode")
	}
	return nil
}

func (s *Store) FinishPersonSweepRun(
	ctx context.Context, runID string, status peoplesweep.RunStatus, completedAt time.Time,
) error {
	if strings.TrimSpace(runID) == "" || completedAt.IsZero() ||
		(status != peoplesweep.RunSucceeded && status != peoplesweep.RunPartial &&
			status != peoplesweep.RunFailed) {
		return errors.New("finish person sweep run: valid ID, terminal status, and time are required")
	}
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockPersonSweepBudgetRun(ctx, tx, runID); err != nil {
			return err
		}
		var durableStatus peoplesweep.RunStatus
		if err := tx.QueryRowContext(ctx,
			`SELECT status FROM person_sweep_runs WHERE id = ?`, runID).Scan(&durableStatus); err != nil {
			return fmt.Errorf("load person sweep run status: %w", err)
		}
		if durableStatus != peoplesweep.RunRunning {
			if durableStatus != status {
				return errors.New("finish person sweep run: terminal status replay mismatch")
			}
			var reclaimed int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM person_sweep_attempts
				WHERE run_id = ? AND failure_class = ?`, runID,
				peoplesweep.FailureLeaseLost).Scan(&reclaimed); err != nil {
				return fmt.Errorf("check reclaimed person sweep run: %w", err)
			}
			if reclaimed > 0 {
				return nil
			}
			var exact int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM person_sweep_runs
				WHERE id = ? AND completed_at = ?`, runID,
				s.dialect.TimestampParam(completedAt)).Scan(&exact); err != nil {
				return fmt.Errorf("verify person sweep run completion replay: %w", err)
			}
			if exact != 1 {
				return errors.New("finish person sweep run: terminal completion time replay mismatch")
			}
			return nil
		}
		var activeAttempts int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM person_sweep_attempts
			WHERE run_id = ? AND status = 'running'`, runID).Scan(&activeAttempts); err != nil {
			return fmt.Errorf("count active person sweep attempts: %w", err)
		}
		if activeAttempts != 0 {
			return errors.New("finish person sweep run: running attempts remain")
		}
		var activeBatches int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*)
			FROM person_sweep_batches b
			JOIN person_sweep_attempts a ON a.id = b.attempt_id
			WHERE a.run_id = ? AND b.status IN ('reserved', 'running')`, runID).Scan(&activeBatches); err != nil {
			return fmt.Errorf("count active person sweep batches: %w", err)
		}
		if activeBatches != 0 {
			return errors.New("finish person sweep run: nonterminal batches remain")
		}
		_, err := tx.ExecContext(ctx, `UPDATE person_sweep_runs
			SET status = ?, completed_at = ? WHERE id = ? AND status = 'running'`, status,
			s.dialect.TimestampParam(completedAt), runID)
		return err
	})
	if err != nil {
		return fmt.Errorf("finish person sweep run: %w", err)
	}
	return nil
}

func (s *Store) StartPersonSweepAttempt(
	ctx context.Context, input peoplesweep.StartAttempt,
) error {
	if strings.TrimSpace(input.ID) == "" || strings.TrimSpace(input.RunID) == "" ||
		input.PersonID <= 0 || input.LeaseFence < 0 || input.StartedAt.IsZero() ||
		!validPersonSweepRunMode(input.Mode) || len(input.CursorEnvelope) == 0 {
		return errors.New("start person sweep attempt: complete valid input is required")
	}
	envelope, err := json.Marshal(input.CursorEnvelope)
	if err != nil {
		return fmt.Errorf("start person sweep attempt: encode cursor envelope: %w", err)
	}
	digest := sha256.Sum256(envelope)
	if input.EnvelopeHash != hex.EncodeToString(digest[:]) {
		return errors.New("start person sweep attempt: cursor envelope hash mismatch")
	}
	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		var program, catalog, provider string
		var runMode peoplesweep.RunMode
		var runStatus peoplesweep.RunStatus
		query := `SELECT mode, status, program_fingerprint, catalog_fingerprint,
		                 provider_fingerprint FROM person_sweep_runs WHERE id = ?`
		if s.IsPostgreSQL() {
			query += " FOR UPDATE"
		} else if _, lockErr := tx.ExecContext(ctx,
			`UPDATE person_sweep_runs SET id = id WHERE id = ?`, input.RunID); lockErr != nil {
			return fmt.Errorf("lock person sweep run: %w", lockErr)
		}
		if scanErr := tx.QueryRowContext(ctx, query, input.RunID).Scan(
			&runMode, &runStatus, &program, &catalog, &provider); scanErr != nil {
			return fmt.Errorf("load person sweep run: %w", scanErr)
		}
		if runStatus != peoplesweep.RunRunning || runMode != input.Mode {
			return errors.New("start person sweep attempt: run is not compatible and running")
		}
		for _, cursor := range input.CursorEnvelope {
			if keyErr := validatePersonSweepCursorKey(cursor.Key); keyErr != nil {
				return fmt.Errorf("validate person sweep attempt cursor: %w", keyErr)
			}
			if cursor.Key.PersonID != input.PersonID ||
				cursor.Key.ProgramFingerprint != program ||
				cursor.Key.CatalogFingerprint != catalog {
				return errors.New("start person sweep attempt: cursor identity mismatch")
			}
		}
		_, insertErr := tx.ExecContext(ctx, `
			INSERT INTO person_sweep_attempts
				(id, run_id, person_id, lease_fence, mode, status,
				 cursor_envelope_json, envelope_hash, program_fingerprint,
				 catalog_fingerprint, provider_fingerprint, started_at)
			VALUES (?, ?, ?, ?, ?, 'running', ?, ?, ?, ?, ?, ?)`, input.ID,
			input.RunID, input.PersonID, input.LeaseFence, input.Mode, string(envelope),
			input.EnvelopeHash, program, catalog, provider,
			s.dialect.TimestampParam(input.StartedAt))
		if insertErr != nil {
			return fmt.Errorf("insert person sweep attempt: %w", insertErr)
		}
		_, updateErr := tx.ExecContext(ctx, `
			UPDATE person_sweep_runs SET attempt_count = attempt_count + 1 WHERE id = ?`, input.RunID)
		return updateErr
	})
	if err != nil {
		return fmt.Errorf("start person sweep attempt: %w", err)
	}
	return nil
}

func validPersonSweepRunMode(mode peoplesweep.RunMode) bool {
	return mode == peoplesweep.RunIncremental || mode == peoplesweep.RunBackstop
}

func (s *Store) ReservePersonSweepBudget(
	ctx context.Context, input peoplesweep.BudgetReservationRequest,
) (peoplesweep.BudgetReservation, error) {
	if err := validatePersonSweepReservationRequest(input); err != nil {
		return peoplesweep.BudgetReservation{}, err
	}
	reservation := peoplesweep.BudgetReservation{ID: personSweepReservationID(input), Request: input}
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		attempt, err := s.lockPersonSweepBudgetAttempt(ctx, tx, input.AttemptID)
		if err != nil {
			return err
		}
		if attempt.runID != input.RunID || attempt.personID != input.PersonID ||
			attempt.providerFingerprint != input.ProviderFingerprint ||
			attempt.status != peoplesweep.AttemptRunning {
			return errors.New("reserve person sweep budget: attempt identity is not current")
		}
		if err := s.requirePersonSweepBudgetRunRunning(ctx, tx, input.RunID); err != nil {
			return err
		}
		dayToLock := input.UTCDate
		if coordinate, found, err := loadPersonSweepBatchCoordinateTx(ctx, tx,
			input.AttemptID, input.BatchOrdinal); err != nil {
			return err
		} else if found {
			dayToLock = coordinate.day
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
			INSERT INTO person_sweep_daily_usage (utc_day, updated_at)
			VALUES (?, %s) ON CONFLICT (utc_day) DO NOTHING`, s.dialect.Now()),
			dayToLock); err != nil {
			return fmt.Errorf("create person sweep daily usage: %w", err)
		}
		day, err := s.lockPersonSweepDailyUsage(ctx, tx, dayToLock)
		if err != nil {
			return err
		}
		if existing, found, err := loadPersonSweepBatchTx(ctx, tx, input.AttemptID,
			input.BatchOrdinal, s.IsPostgreSQL()); err != nil {
			return err
		} else if found {
			if existing.matches(input) && existing.status == "reserved" {
				return nil
			}
			return errors.New("reserve person sweep budget: batch ordinal already has different metadata")
		}
		personUsage, err := personSweepBudgetUsageTx(ctx, tx,
			`SELECT b.status, b.reserved_requests, b.reserved_input_tokens, b.reserved_output_tokens,
			        b.reserved_cost_micro_usd, b.actual_requests, b.actual_input_tokens,
			        b.actual_output_tokens, b.actual_cost_micro_usd
			 FROM person_sweep_batches b
			 JOIN person_sweep_attempts a ON a.id = b.attempt_id
			 WHERE a.run_id = ? AND a.person_id = ?`, input.RunID, input.PersonID)
		if err != nil {
			return fmt.Errorf("sum person sweep run-person budget: %w", err)
		}
		runUsage, err := personSweepBudgetUsageTx(ctx, tx,
			`SELECT b.status, b.reserved_requests, b.reserved_input_tokens,
			        b.reserved_output_tokens, b.reserved_cost_micro_usd,
			        b.actual_requests, b.actual_input_tokens, b.actual_output_tokens,
			        b.actual_cost_micro_usd
			 FROM person_sweep_batches b
			 JOIN person_sweep_attempts a ON a.id = b.attempt_id WHERE a.run_id = ?`, input.RunID)
		if err != nil {
			return fmt.Errorf("sum person sweep run budget: %w", err)
		}
		estimate := peoplesweep.Usage{Requests: input.EstimatedRequests,
			InputTokens: input.EstimatedInputTokens, OutputTokens: input.EstimatedOutputTokens,
			EstimatedCostMicroUSD: input.EstimatedCostMicroUSD}
		if err := enforcePersonSweepBudget(personUsage, runUsage, day, estimate, input.Budget); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, fmt.Sprintf(`
			INSERT INTO person_sweep_batches
				(attempt_id, batch_ordinal, utc_day, reservation_id, budget_fingerprint,
				 input_hash, item_count, status,
				 reserved_requests, reserved_input_tokens, reserved_output_tokens,
				 reserved_cost_micro_usd, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, 'reserved', ?, ?, ?, ?, %s)`, s.dialect.Now()),
			input.AttemptID, input.BatchOrdinal, input.UTCDate, reservation.ID,
			personSweepBudgetFingerprint(input.Budget), input.InputHash,
			input.ItemCount, input.EstimatedRequests, input.EstimatedInputTokens,
			input.EstimatedOutputTokens, input.EstimatedCostMicroUSD)
		if err != nil {
			return fmt.Errorf("insert person sweep budget reservation: %w", err)
		}
		_, err = tx.ExecContext(ctx, fmt.Sprintf(`
			UPDATE person_sweep_daily_usage
			SET reserved_requests = reserved_requests + ?,
			    reserved_input_tokens = reserved_input_tokens + ?,
			    reserved_output_tokens = reserved_output_tokens + ?,
			    reserved_cost_micro_usd = reserved_cost_micro_usd + ?,
			    updated_at = %s WHERE utc_day = ?`, s.dialect.Now()),
			input.EstimatedRequests, input.EstimatedInputTokens,
			input.EstimatedOutputTokens, input.EstimatedCostMicroUSD, input.UTCDate)
		return err
	})
	if err != nil {
		return peoplesweep.BudgetReservation{}, fmt.Errorf("reserve person sweep budget: %w", err)
	}
	return reservation, nil
}

type personSweepBudgetAttempt struct {
	runID               string
	personID            int64
	leaseFence          int64
	providerFingerprint string
	status              peoplesweep.AttemptStatus
	failureClass        peoplesweep.FailureClass
}

func (s *Store) lockPersonSweepBudgetAttempt(
	ctx context.Context, tx *loggedTx, attemptID string,
) (personSweepBudgetAttempt, error) {
	if !s.IsPostgreSQL() {
		if _, err := tx.ExecContext(ctx,
			`UPDATE person_sweep_attempts SET id = id WHERE id = ?`, attemptID); err != nil {
			return personSweepBudgetAttempt{}, fmt.Errorf("lock person sweep attempt: %w", err)
		}
	}
	query := `SELECT run_id, person_id, lease_fence, provider_fingerprint, status, failure_class
	          FROM person_sweep_attempts WHERE id = ?`
	if s.IsPostgreSQL() {
		query += " FOR UPDATE"
	}
	var attempt personSweepBudgetAttempt
	if err := tx.QueryRowContext(ctx, query, attemptID).Scan(&attempt.runID,
		&attempt.personID, &attempt.leaseFence, &attempt.providerFingerprint,
		&attempt.status, &attempt.failureClass); err != nil {
		return personSweepBudgetAttempt{}, fmt.Errorf("load person sweep attempt: %w", err)
	}
	return attempt, nil
}

func (s *Store) lockPersonSweepBudgetRun(ctx context.Context, tx *loggedTx, runID string) error {
	if !s.IsPostgreSQL() {
		_, err := tx.ExecContext(ctx, `UPDATE person_sweep_runs SET id = id WHERE id = ?`, runID)
		return err
	}
	var locked string
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM person_sweep_runs WHERE id = ? FOR UPDATE`, runID).Scan(&locked); err != nil {
		return fmt.Errorf("lock person sweep run: %w", err)
	}
	return nil
}

func (s *Store) requirePersonSweepBudgetRunRunning(
	ctx context.Context, tx *loggedTx, runID string,
) error {
	if err := s.lockPersonSweepBudgetRun(ctx, tx, runID); err != nil {
		return err
	}
	var status peoplesweep.RunStatus
	if err := tx.QueryRowContext(ctx,
		`SELECT status FROM person_sweep_runs WHERE id = ?`, runID).Scan(&status); err != nil {
		return fmt.Errorf("load person sweep run status: %w", err)
	}
	if status != peoplesweep.RunRunning {
		return errors.New("person sweep run is terminal")
	}
	return nil
}

type personSweepDailyUsage struct{ reserved, actual peoplesweep.Usage }

func (s *Store) lockPersonSweepDailyUsage(
	ctx context.Context, tx *loggedTx, day string,
) (personSweepDailyUsage, error) {
	query := `SELECT reserved_requests, reserved_input_tokens, reserved_output_tokens,
	                 reserved_cost_micro_usd, actual_requests, actual_input_tokens,
	                 actual_output_tokens, actual_cost_micro_usd
	          FROM person_sweep_daily_usage WHERE utc_day = ?`
	if s.IsPostgreSQL() {
		query += " FOR UPDATE"
	}
	var usage personSweepDailyUsage
	err := tx.QueryRowContext(ctx, query, day).Scan(&usage.reserved.Requests,
		&usage.reserved.InputTokens, &usage.reserved.OutputTokens,
		&usage.reserved.EstimatedCostMicroUSD, &usage.actual.Requests,
		&usage.actual.InputTokens, &usage.actual.OutputTokens,
		&usage.actual.EstimatedCostMicroUSD)
	if err != nil {
		return personSweepDailyUsage{}, fmt.Errorf("lock person sweep daily usage: %w", err)
	}
	return usage, nil
}

func personSweepBudgetUsageTx(
	ctx context.Context, tx *loggedTx, query string, args ...any,
) (peoplesweep.Usage, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return peoplesweep.Usage{}, err
	}
	defer func() { _ = rows.Close() }()
	var total peoplesweep.Usage
	for rows.Next() {
		var status string
		var reserved, actual peoplesweep.Usage
		if err := rows.Scan(&status, &reserved.Requests, &reserved.InputTokens,
			&reserved.OutputTokens, &reserved.EstimatedCostMicroUSD, &actual.Requests,
			&actual.InputTokens, &actual.OutputTokens, &actual.EstimatedCostMicroUSD); err != nil {
			return peoplesweep.Usage{}, err
		}
		if status == "reserved" || status == "running" {
			actual = reserved
		}
		var addErr error
		total, addErr = addPersonSweepUsage(total, actual)
		if addErr != nil {
			return peoplesweep.Usage{}, addErr
		}
	}
	return total, rows.Err()
}

func enforcePersonSweepBudget(attempt, run peoplesweep.Usage, day personSweepDailyUsage,
	estimate peoplesweep.Usage, budget peoplesweep.BudgetConfig,
) error {
	personTotal, err := addPersonSweepUsage(attempt, estimate)
	if err != nil {
		return err
	}
	runTotal, err := addPersonSweepUsage(run, estimate)
	if err != nil {
		return err
	}
	dayCurrent, err := addPersonSweepUsage(day.reserved, day.actual)
	if err != nil {
		return err
	}
	dayTotal, err := addPersonSweepUsage(dayCurrent, estimate)
	if err != nil {
		return err
	}
	if exceedsPersonSweepUsage(personTotal, budget.MaxRequestsPerPerson,
		budget.MaxInputTokensPerPerson, budget.MaxOutputTokensPerPerson, 0) ||
		exceedsPersonSweepUsage(runTotal, budget.MaxRequestsPerRun,
			budget.MaxInputTokensPerRun, budget.MaxOutputTokensPerRun,
			budget.MaxEstimatedCostMicroUSDPerRun) ||
		exceedsPersonSweepUsage(dayTotal, budget.MaxRequestsPerDay,
			budget.MaxInputTokensPerDay, budget.MaxOutputTokensPerDay,
			budget.MaxEstimatedCostMicroUSDPerDay) {
		return peoplesweep.ErrBudgetExceeded
	}
	return nil
}

func exceedsPersonSweepUsage(usage peoplesweep.Usage, requests int, input, output, cost int64) bool {
	return usage.Requests > requests || usage.InputTokens > input || usage.OutputTokens > output ||
		(cost > 0 && usage.EstimatedCostMicroUSD > cost)
}

func addPersonSweepUsage(left, right peoplesweep.Usage) (peoplesweep.Usage, error) {
	if right.Requests < 0 || right.InputTokens < 0 || right.OutputTokens < 0 ||
		right.EstimatedCostMicroUSD < 0 || left.Requests > math.MaxInt-right.Requests ||
		left.InputTokens > math.MaxInt64-right.InputTokens ||
		left.OutputTokens > math.MaxInt64-right.OutputTokens ||
		left.EstimatedCostMicroUSD > math.MaxInt64-right.EstimatedCostMicroUSD {
		return peoplesweep.Usage{}, peoplesweep.ErrBudgetOverflow
	}
	return peoplesweep.Usage{Requests: left.Requests + right.Requests,
		InputTokens:           left.InputTokens + right.InputTokens,
		OutputTokens:          left.OutputTokens + right.OutputTokens,
		EstimatedCostMicroUSD: left.EstimatedCostMicroUSD + right.EstimatedCostMicroUSD}, nil
}

func validatePersonSweepReservationRequest(input peoplesweep.BudgetReservationRequest) error {
	if strings.TrimSpace(input.RunID) == "" || strings.TrimSpace(input.AttemptID) == "" ||
		input.BatchOrdinal < 0 || input.PersonID <= 0 || strings.TrimSpace(input.ProviderFingerprint) == "" ||
		input.ItemCount < 0 || input.ItemCount > math.MaxInt32 || input.EstimatedRequests <= 0 ||
		input.EstimatedRequests > math.MaxInt32 || input.EstimatedInputTokens <= 0 ||
		input.EstimatedOutputTokens <= 0 || input.EstimatedCostMicroUSD < 0 {
		return errors.New("reserve person sweep budget: complete positive estimates are required")
	}
	if parsed, err := time.Parse("2006-01-02", input.UTCDate); err != nil || parsed.Format("2006-01-02") != input.UTCDate {
		return errors.New("reserve person sweep budget: canonical UTC date is required")
	}
	if len(input.InputHash) != sha256.Size*2 {
		return errors.New("reserve person sweep budget: SHA-256 input hash is required")
	}
	if decoded, err := hex.DecodeString(input.InputHash); err != nil || len(decoded) != sha256.Size ||
		input.InputHash != strings.ToLower(input.InputHash) {
		return errors.New("reserve person sweep budget: canonical SHA-256 input hash is required")
	}
	if err := peoplesweep.ValidateBudgetConfig(input.Budget); err != nil {
		return fmt.Errorf("reserve person sweep budget: %w", err)
	}
	cost, err := peoplesweep.EstimateCostMicroUSD(peoplesweep.TokenUsage{
		InputTokens: input.EstimatedInputTokens, OutputTokens: input.EstimatedOutputTokens,
	}, input.Budget)
	if err != nil {
		return fmt.Errorf("reserve person sweep budget: %w", err)
	}
	if cost != input.EstimatedCostMicroUSD {
		return errors.New("reserve person sweep budget: estimated cost does not match token prices")
	}
	return nil
}

func personSweepReservationID(input peoplesweep.BudgetReservationRequest) string {
	hash := sha256.New()
	writeString := func(value string) {
		writePersonSweepIdentityInt(hash, int64(len(value)))
		_, _ = hash.Write([]byte(value))
	}
	writeString(input.RunID)
	writeString(input.AttemptID)
	writePersonSweepIdentityInt(hash, int64(input.BatchOrdinal))
	writePersonSweepIdentityInt(hash, input.PersonID)
	writeString(input.ProviderFingerprint)
	writeString(input.UTCDate)
	writeString(input.InputHash)
	writePersonSweepIdentityInt(hash, int64(input.ItemCount))
	writePersonSweepIdentityInt(hash, int64(input.EstimatedRequests))
	writePersonSweepIdentityInt(hash, input.EstimatedInputTokens)
	writePersonSweepIdentityInt(hash, input.EstimatedOutputTokens)
	writePersonSweepIdentityInt(hash, input.EstimatedCostMicroUSD)
	writePersonSweepBudgetIdentity(hash, input.Budget)
	return hex.EncodeToString(hash.Sum(nil))
}

func personSweepBudgetFingerprint(budget peoplesweep.BudgetConfig) string {
	digest := sha256.New()
	writePersonSweepBudgetIdentity(digest, budget)
	return hex.EncodeToString(digest.Sum(nil))
}

func writePersonSweepBudgetIdentity(digest hash.Hash, budget peoplesweep.BudgetConfig) {
	writePersonSweepIdentityInt(digest, int64(budget.MaxRequestsPerPerson))
	writePersonSweepIdentityInt(digest, budget.MaxInputTokensPerPerson)
	writePersonSweepIdentityInt(digest, budget.MaxOutputTokensPerPerson)
	writePersonSweepIdentityInt(digest, int64(budget.MaxRequestsPerRun))
	writePersonSweepIdentityInt(digest, budget.MaxInputTokensPerRun)
	writePersonSweepIdentityInt(digest, budget.MaxOutputTokensPerRun)
	writePersonSweepIdentityInt(digest, budget.MaxEstimatedCostMicroUSDPerRun)
	writePersonSweepIdentityInt(digest, int64(budget.MaxRequestsPerDay))
	writePersonSweepIdentityInt(digest, budget.MaxInputTokensPerDay)
	writePersonSweepIdentityInt(digest, budget.MaxOutputTokensPerDay)
	writePersonSweepIdentityInt(digest, budget.MaxEstimatedCostMicroUSDPerDay)
	writePersonSweepIdentityInt(digest, budget.InputCostMicroUSDPerMillionTokens)
	writePersonSweepIdentityInt(digest, budget.OutputCostMicroUSDPerMillionTokens)
}

func writePersonSweepIdentityInt(digest hash.Hash, value int64) {
	var encoded [8]byte
	// #nosec G115 -- the two's-complement bit pattern is the intended
	// deterministic encoding for every signed reservation field.
	binary.BigEndian.PutUint64(encoded[:], uint64(value))
	_, _ = digest.Write(encoded[:])
}

type personSweepBatch struct {
	day               string
	reservationID     string
	budgetFingerprint string
	inputHash         string
	itemCount         int
	status            string
	reserved          peoplesweep.Usage
	actual            peoplesweep.Usage
	requestID         string
	latencyMS         int64
	completed         nullableTimestamp
}

func loadPersonSweepBatchTx(ctx context.Context, tx *loggedTx, attemptID string,
	ordinal int, postgres bool,
) (personSweepBatch, bool, error) {
	query := `SELECT utc_day, reservation_id, budget_fingerprint, input_hash,
	                 item_count, status, reserved_requests,
	                 reserved_input_tokens, reserved_output_tokens, reserved_cost_micro_usd,
	                 actual_requests, actual_input_tokens, actual_output_tokens,
	                 actual_cost_micro_usd, provider_request_id, latency_milliseconds,
	                 completed_at
	          FROM person_sweep_batches WHERE attempt_id = ? AND batch_ordinal = ?`
	if postgres {
		query += " FOR UPDATE"
	}
	var batch personSweepBatch
	err := tx.QueryRowContext(ctx, query, attemptID, ordinal).Scan(&batch.day,
		&batch.reservationID, &batch.budgetFingerprint, &batch.inputHash,
		&batch.itemCount, &batch.status, &batch.reserved.Requests,
		&batch.reserved.InputTokens, &batch.reserved.OutputTokens,
		&batch.reserved.EstimatedCostMicroUSD, &batch.actual.Requests,
		&batch.actual.InputTokens, &batch.actual.OutputTokens,
		&batch.actual.EstimatedCostMicroUSD, &batch.requestID, &batch.latencyMS,
		&batch.completed)
	if errors.Is(err, sql.ErrNoRows) {
		return personSweepBatch{}, false, nil
	}
	if err != nil {
		return personSweepBatch{}, false, fmt.Errorf("load person sweep batch: %w", err)
	}
	return batch, true, nil
}

func (batch personSweepBatch) matches(input peoplesweep.BudgetReservationRequest) bool {
	return batch.day == input.UTCDate &&
		batch.reservationID == personSweepReservationID(input) &&
		batch.budgetFingerprint == personSweepBudgetFingerprint(input.Budget) &&
		batch.inputHash == input.InputHash &&
		batch.itemCount == input.ItemCount && batch.reserved == (peoplesweep.Usage{
		Requests: input.EstimatedRequests, InputTokens: input.EstimatedInputTokens,
		OutputTokens:          input.EstimatedOutputTokens,
		EstimatedCostMicroUSD: input.EstimatedCostMicroUSD})
}

func (s *Store) ReleasePersonSweepBudget(
	ctx context.Context, reservation peoplesweep.BudgetReservation,
) error {
	if reservation.ID != personSweepReservationID(reservation.Request) {
		return errors.New("release person sweep budget: reservation identity mismatch")
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		attempt, err := s.lockPersonSweepBudgetAttempt(ctx, tx, reservation.Request.AttemptID)
		if err != nil {
			return err
		}
		if attempt.runID != reservation.Request.RunID ||
			attempt.personID != reservation.Request.PersonID ||
			attempt.providerFingerprint != reservation.Request.ProviderFingerprint {
			return errors.New("release person sweep budget: attempt identity mismatch")
		}
		if err := s.lockPersonSweepBudgetRun(ctx, tx, attempt.runID); err != nil {
			return err
		}
		coordinate, found, err := loadPersonSweepBatchCoordinateTx(ctx, tx,
			reservation.Request.AttemptID, reservation.Request.BatchOrdinal)
		if err != nil {
			return err
		}
		if !found {
			return errors.New("release person sweep budget: reservation is missing")
		}
		if _, err := s.lockPersonSweepDailyUsage(ctx, tx, coordinate.day); err != nil {
			return err
		}
		batch, found, err := loadPersonSweepBatchTx(ctx, tx, reservation.Request.AttemptID,
			reservation.Request.BatchOrdinal, s.IsPostgreSQL())
		if err != nil {
			return err
		}
		if !found || !batch.matches(reservation.Request) {
			return errors.New("release person sweep budget: reservation is not authentic")
		}
		if batch.status == "cancelled" {
			return nil
		}
		if batch.status != "reserved" {
			return errors.New("release person sweep budget: provider work already started")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE person_sweep_batches
			SET status = 'cancelled', completed_at = ? WHERE attempt_id = ? AND batch_ordinal = ?`,
			s.dialect.TimestampParam(time.Now().UTC()), reservation.Request.AttemptID,
			reservation.Request.BatchOrdinal); err != nil {
			return err
		}
		return s.adjustPersonSweepDailyUsage(ctx, tx, batch.day, negatePersonSweepUsage(batch.reserved),
			peoplesweep.Usage{})
	})
}

func negatePersonSweepUsage(usage peoplesweep.Usage) peoplesweep.Usage {
	return peoplesweep.Usage{Requests: -usage.Requests, InputTokens: -usage.InputTokens,
		OutputTokens: -usage.OutputTokens, EstimatedCostMicroUSD: -usage.EstimatedCostMicroUSD}
}

func (s *Store) adjustPersonSweepDailyUsage(ctx context.Context, tx *loggedTx, day string,
	reservedDelta, actualDelta peoplesweep.Usage,
) error {
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE person_sweep_daily_usage
		SET reserved_requests = reserved_requests + ?,
		    reserved_input_tokens = reserved_input_tokens + ?,
		    reserved_output_tokens = reserved_output_tokens + ?,
		    reserved_cost_micro_usd = reserved_cost_micro_usd + ?,
		    actual_requests = actual_requests + ?,
		    actual_input_tokens = actual_input_tokens + ?,
		    actual_output_tokens = actual_output_tokens + ?,
		    actual_cost_micro_usd = actual_cost_micro_usd + ?, updated_at = %s
		WHERE utc_day = ?`, s.dialect.Now()), reservedDelta.Requests,
		reservedDelta.InputTokens, reservedDelta.OutputTokens,
		reservedDelta.EstimatedCostMicroUSD, actualDelta.Requests,
		actualDelta.InputTokens, actualDelta.OutputTokens,
		actualDelta.EstimatedCostMicroUSD, day)
	if err != nil {
		return fmt.Errorf("adjust person sweep daily usage: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		return errors.New("adjust person sweep daily usage: daily row is missing")
	}
	return nil
}

// MarkPersonSweepBudgetStarted closes the only release-before-network window.
// Task 8 calls it immediately before sending the immutable reserved bytes.
func (s *Store) MarkPersonSweepBudgetStarted(
	ctx context.Context, reservation peoplesweep.BudgetReservation,
) error {
	if reservation.ID != personSweepReservationID(reservation.Request) {
		return errors.New("mark person sweep budget started: reservation identity mismatch")
	}
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		attempt, err := s.lockPersonSweepBudgetAttempt(ctx, tx, reservation.Request.AttemptID)
		if err != nil {
			return err
		}
		if attempt.runID != reservation.Request.RunID ||
			attempt.personID != reservation.Request.PersonID ||
			attempt.providerFingerprint != reservation.Request.ProviderFingerprint ||
			attempt.status != peoplesweep.AttemptRunning {
			return errors.New("mark person sweep budget started: attempt identity is not current")
		}
		if err := s.requirePersonSweepBudgetRunRunning(ctx, tx, attempt.runID); err != nil {
			return err
		}
		coordinate, found, err := loadPersonSweepBatchCoordinateTx(ctx, tx,
			reservation.Request.AttemptID, reservation.Request.BatchOrdinal)
		if err != nil {
			return err
		}
		if !found {
			return errors.New("mark person sweep budget started: reservation is missing")
		}
		if _, err := s.lockPersonSweepDailyUsage(ctx, tx, coordinate.day); err != nil {
			return err
		}
		batch, found, err := loadPersonSweepBatchTx(ctx, tx,
			reservation.Request.AttemptID, reservation.Request.BatchOrdinal, s.IsPostgreSQL())
		if err != nil {
			return err
		}
		if !found || !batch.matches(reservation.Request) {
			return errors.New("mark person sweep budget started: reservation is not authentic")
		}
		if batch.status == "running" {
			return nil
		}
		if batch.status != "reserved" {
			return errors.New("mark person sweep budget started: reservation is terminal")
		}
		_, err = tx.ExecContext(ctx, `UPDATE person_sweep_batches SET status = 'running'
			WHERE attempt_id = ? AND batch_ordinal = ?`, reservation.Request.AttemptID,
			reservation.Request.BatchOrdinal)
		return err
	})
	if err != nil {
		return fmt.Errorf("mark person sweep budget started: %w", err)
	}
	return nil
}

func (s *Store) FinalizePersonSweepFailure(
	ctx context.Context, input peoplesweep.FailureFinalization,
) error {
	if strings.TrimSpace(input.AttemptID) == "" || input.RetryAt.IsZero() ||
		input.FinalizedAt.IsZero() || !validPersonSweepFailureClass(input.Class) {
		return errors.New("finalize person sweep failure: complete valid input is required")
	}
	reservations := append([]peoplesweep.BudgetReservation(nil), input.Reservations...)
	slices.SortFunc(reservations, func(a, b peoplesweep.BudgetReservation) int {
		return a.Request.BatchOrdinal - b.Request.BatchOrdinal
	})
	completed := make(map[int]peoplesweep.CompletedUsage, len(input.Completed))
	for _, usage := range input.Completed {
		if usage.BatchOrdinal < 0 || usage.Usage.InputTokens < 0 || usage.Usage.OutputTokens < 0 ||
			usage.Latency < 0 {
			return errors.New("finalize person sweep failure: invalid completed usage")
		}
		if !peoplesweep.IsSafeProviderMetadata(usage.ProviderRequestID) {
			return errors.New("finalize person sweep failure: unsafe provider request ID")
		}
		if _, duplicate := completed[usage.BatchOrdinal]; duplicate {
			return errors.New("finalize person sweep failure: duplicate completed batch")
		}
		completed[usage.BatchOrdinal] = usage
	}
	seen := make(map[int]struct{}, len(reservations))
	for _, reservation := range reservations {
		if reservation.Request.AttemptID != input.AttemptID ||
			reservation.ID != personSweepReservationID(reservation.Request) {
			return errors.New("finalize person sweep failure: reservation identity mismatch")
		}
		if _, duplicate := seen[reservation.Request.BatchOrdinal]; duplicate {
			return errors.New("finalize person sweep failure: duplicate reservation")
		}
		seen[reservation.Request.BatchOrdinal] = struct{}{}
	}
	for ordinal := range completed {
		if _, ok := seen[ordinal]; !ok {
			return errors.New("finalize person sweep failure: completed batch has no reservation")
		}
	}

	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		attempt, err := s.lockPersonSweepBudgetAttempt(ctx, tx, input.AttemptID)
		if err != nil {
			return err
		}
		if attempt.status != peoplesweep.AttemptRunning && attempt.status != peoplesweep.AttemptFailed {
			return errors.New("finalize person sweep failure: attempt is not failure-finalizable")
		}
		if input.Lease.PersonID != attempt.personID || input.Lease.Fence != attempt.leaseFence {
			return errors.New("finalize person sweep failure: lease does not match durable attempt")
		}
		if attempt.status == peoplesweep.AttemptFailed &&
			attempt.failureClass == peoplesweep.FailureLeaseLost {
			return nil
		}
		if err := s.lockPersonSweepBudgetRun(ctx, tx, attempt.runID); err != nil {
			return err
		}
		for _, reservation := range reservations {
			if reservation.Request.RunID != attempt.runID ||
				reservation.Request.PersonID != attempt.personID ||
				reservation.Request.ProviderFingerprint != attempt.providerFingerprint {
				return errors.New("finalize person sweep failure: reservation attempt identity mismatch")
			}
		}
		durableBatches, err := listPersonSweepBatchCoordinatesTx(ctx, tx, input.AttemptID)
		if err != nil {
			return err
		}
		if len(durableBatches) != len(seen) {
			return errors.New("finalize person sweep failure: reservations do not cover every durable batch")
		}
		for _, batch := range durableBatches {
			if _, covered := seen[batch.ordinal]; !covered {
				return errors.New("finalize person sweep failure: reservations do not cover every durable batch")
			}
		}
		if _, err := s.lockPersonSweepUsageThenWorkTx(
			ctx, tx, input.AttemptID, durableBatches, input.Lease); err != nil {
			return err
		}
		for _, reservation := range reservations {
			batch, found, err := loadPersonSweepBatchTx(ctx, tx, input.AttemptID,
				reservation.Request.BatchOrdinal, s.IsPostgreSQL())
			if err != nil {
				return err
			}
			if !found || !batch.matches(reservation.Request) {
				return errors.New("finalize person sweep failure: reservation is not authentic")
			}
			usage, didComplete := completed[reservation.Request.BatchOrdinal]
			if batch.status == "succeeded" {
				if didComplete {
					actual, actualErr := completedPersonSweepUsage(batch.reserved, usage,
						reservation.Request.Budget)
					if actualErr != nil {
						return actualErr
					}
					if batch.actual != actual || batch.requestID != usage.ProviderRequestID ||
						batch.latencyMS != usage.Latency.Milliseconds() {
						return errors.New("finalize person sweep failure: completed batch replay mismatch")
					}
				}
				continue
			}
			if batch.status == "failed" {
				if didComplete {
					return errors.New("finalize person sweep failure: failed batch cannot become completed")
				}
				continue
			}
			if batch.status == "cancelled" {
				if didComplete {
					return errors.New("finalize person sweep failure: cancelled batch cannot become completed")
				}
				continue
			}
			if didComplete || batch.status == "running" {
				actual, err := completedPersonSweepUsage(batch.reserved, usage, reservation.Request.Budget)
				if err != nil {
					return err
				}
				status := "failed"
				if didComplete {
					status = "succeeded"
				}
				_, err = tx.ExecContext(ctx, `UPDATE person_sweep_batches
					SET status = ?, provider_request_id = ?, actual_requests = ?,
					    actual_input_tokens = ?, actual_output_tokens = ?,
					    actual_cost_micro_usd = ?, latency_milliseconds = ?,
					    failure_class = ?, completed_at = ?
					WHERE attempt_id = ? AND batch_ordinal = ?`, status,
					usage.ProviderRequestID, actual.Requests, actual.InputTokens,
					actual.OutputTokens, actual.EstimatedCostMicroUSD,
					usage.Latency.Milliseconds(), input.Class,
					s.dialect.TimestampParam(input.FinalizedAt), input.AttemptID,
					reservation.Request.BatchOrdinal)
				if err != nil {
					return err
				}
				if err := s.adjustPersonSweepDailyUsage(ctx, tx, batch.day,
					negatePersonSweepUsage(batch.reserved), actual); err != nil {
					return err
				}
			} else {
				_, err = tx.ExecContext(ctx, `UPDATE person_sweep_batches
					SET status = 'cancelled', failure_class = ?, completed_at = ?
					WHERE attempt_id = ? AND batch_ordinal = ?`, input.Class,
					s.dialect.TimestampParam(input.FinalizedAt), input.AttemptID,
					reservation.Request.BatchOrdinal)
				if err != nil {
					return err
				}
				if err := s.adjustPersonSweepDailyUsage(ctx, tx, batch.day,
					negatePersonSweepUsage(batch.reserved), peoplesweep.Usage{}); err != nil {
					return err
				}
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE person_sweep_attempts
			SET status = 'failed', failure_class = ?, retry_at = ?, completed_at = ?
			WHERE id = ? AND status = 'running'`, input.Class,
			s.dialect.TimestampParam(input.RetryAt), s.dialect.TimestampParam(input.FinalizedAt),
			input.AttemptID); err != nil {
			return err
		}
		if err := s.refreshPersonSweepAttemptAndRunUsage(ctx, tx, input.AttemptID, attempt.runID); err != nil {
			return err
		}
		if input.Lease.PersonID > 0 {
			_, err = tx.ExecContext(ctx, fmt.Sprintf(`UPDATE person_sweep_work
				SET available_at = ?, attempt_count = attempt_count + 1,
				    last_failure_class = ?, lease_owner = '', lease_until = NULL,
				    updated_at = %s
				WHERE person_id = ? AND lease_owner = ? AND lease_fence = ?
				  AND lease_until > %s`, s.dialect.Now(), s.dialect.Now()),
				s.dialect.TimestampParam(input.RetryAt), input.Class, input.Lease.PersonID,
				input.Lease.WorkerID, input.Lease.Fence)
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("finalize person sweep failure: %w", err)
	}
	return nil
}

type personSweepBatchCoordinate struct {
	ordinal int
	day     string
}

func loadPersonSweepBatchCoordinateTx(
	ctx context.Context, tx *loggedTx, attemptID string, ordinal int,
) (personSweepBatchCoordinate, bool, error) {
	coordinate := personSweepBatchCoordinate{ordinal: ordinal}
	err := tx.QueryRowContext(ctx, `SELECT utc_day FROM person_sweep_batches
		WHERE attempt_id = ? AND batch_ordinal = ?`, attemptID, ordinal).Scan(&coordinate.day)
	if errors.Is(err, sql.ErrNoRows) {
		return personSweepBatchCoordinate{}, false, nil
	}
	if err != nil {
		return personSweepBatchCoordinate{}, false,
			fmt.Errorf("load person sweep batch coordinate: %w", err)
	}
	return coordinate, true, nil
}

func listPersonSweepBatchCoordinatesTx(
	ctx context.Context, tx *loggedTx, attemptID string,
) ([]personSweepBatchCoordinate, error) {
	rows, err := tx.QueryContext(ctx, `SELECT batch_ordinal, utc_day
		FROM person_sweep_batches WHERE attempt_id = ? ORDER BY batch_ordinal`, attemptID)
	if err != nil {
		return nil, fmt.Errorf("list person sweep batch coordinates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	coordinates := make([]personSweepBatchCoordinate, 0)
	for rows.Next() {
		var coordinate personSweepBatchCoordinate
		if err := rows.Scan(&coordinate.ordinal, &coordinate.day); err != nil {
			return nil, fmt.Errorf("scan person sweep batch coordinate: %w", err)
		}
		coordinates = append(coordinates, coordinate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate person sweep batch coordinates: %w", err)
	}
	return coordinates, nil
}

func validPersonSweepFailureClass(class peoplesweep.FailureClass) bool {
	return class == peoplesweep.FailurePolicy || class == peoplesweep.FailureBudget ||
		class == peoplesweep.FailureLeaseLost || class == peoplesweep.FailureRateLimited ||
		class == peoplesweep.FailureTimeout || class == peoplesweep.FailureProviderHTTP ||
		class == peoplesweep.FailureInvalidOutput || class == peoplesweep.FailureArchiveGap ||
		class == peoplesweep.FailureInternal
}

func completedPersonSweepUsage(reserved peoplesweep.Usage, completed peoplesweep.CompletedUsage,
	budget peoplesweep.BudgetConfig,
) (peoplesweep.Usage, error) {
	actualTokens := peoplesweep.TokenUsage{InputTokens: max(reserved.InputTokens,
		completed.Usage.InputTokens), OutputTokens: max(reserved.OutputTokens,
		completed.Usage.OutputTokens)}
	cost, err := peoplesweep.EstimateCostMicroUSD(actualTokens, budget)
	if err != nil {
		return peoplesweep.Usage{}, err
	}
	return peoplesweep.Usage{Requests: max(reserved.Requests, 1),
		InputTokens: actualTokens.InputTokens, OutputTokens: actualTokens.OutputTokens,
		EstimatedCostMicroUSD: max(reserved.EstimatedCostMicroUSD, cost)}, nil
}

func (s *Store) refreshPersonSweepAttemptAndRunUsage(
	ctx context.Context, tx *loggedTx, attemptID, runID string,
) error {
	if _, err := tx.ExecContext(ctx, `UPDATE person_sweep_attempts SET
		request_count = COALESCE((SELECT SUM(actual_requests) FROM person_sweep_batches WHERE attempt_id = ?), 0),
		input_tokens = COALESCE((SELECT SUM(actual_input_tokens) FROM person_sweep_batches WHERE attempt_id = ?), 0),
		output_tokens = COALESCE((SELECT SUM(actual_output_tokens) FROM person_sweep_batches WHERE attempt_id = ?), 0),
		estimated_cost_micro_usd = COALESCE((SELECT SUM(actual_cost_micro_usd) FROM person_sweep_batches WHERE attempt_id = ?), 0),
		latency_milliseconds = COALESCE((SELECT SUM(latency_milliseconds) FROM person_sweep_batches WHERE attempt_id = ?), 0),
		provider_request_id = COALESCE((SELECT provider_request_id FROM person_sweep_batches
			WHERE attempt_id = ? AND provider_request_id <> '' ORDER BY batch_ordinal DESC LIMIT 1), '')
		WHERE id = ?`, attemptID, attemptID, attemptID, attemptID, attemptID,
		attemptID, attemptID); err != nil {
		return fmt.Errorf("refresh person sweep attempt usage: %w", err)
	}
	_, err := tx.ExecContext(ctx, `UPDATE person_sweep_runs SET
		attempt_count = (SELECT COUNT(*) FROM person_sweep_attempts WHERE run_id = ?),
		success_count = (SELECT COUNT(*) FROM person_sweep_attempts WHERE run_id = ? AND status = 'succeeded'),
		failure_count = (SELECT COUNT(*) FROM person_sweep_attempts WHERE run_id = ? AND status = 'failed'),
		projected_write_count = COALESCE((SELECT SUM(projected_write_count) FROM person_sweep_attempts WHERE run_id = ?), 0),
		actual_requests = COALESCE((SELECT SUM(request_count) FROM person_sweep_attempts WHERE run_id = ?), 0),
		actual_input_tokens = COALESCE((SELECT SUM(input_tokens) FROM person_sweep_attempts WHERE run_id = ?), 0),
		actual_output_tokens = COALESCE((SELECT SUM(output_tokens) FROM person_sweep_attempts WHERE run_id = ?), 0),
		actual_cost_micro_usd = COALESCE((SELECT SUM(estimated_cost_micro_usd) FROM person_sweep_attempts WHERE run_id = ?), 0)
		WHERE id = ?`, runID, runID, runID, runID, runID, runID, runID, runID, runID)
	if err != nil {
		return fmt.Errorf("refresh person sweep run usage: %w", err)
	}
	return nil
}
