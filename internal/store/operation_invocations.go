package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/operations"
)

var (
	ErrOperationInvocationNotFound = errors.New("operation invocation not found")
	ErrOperationInvocationTerminal = errors.New("operation invocation is already terminal")
)

var _ operations.Recorder = (*Store)(nil)

type operationInvocationLedger struct {
	table     string
	truncated bool
	skipped   bool
}

func (s *Store) Begin(ctx context.Context, spec operations.InvocationSpec) (operations.BeginResult, error) {
	return s.BeginOperationInvocation(ctx, spec)
}

func (s *Store) Checkpoint(ctx context.Context, id operations.StableID, counters operations.InvocationCounters) error {
	return s.CheckpointOperationInvocation(ctx, id, counters)
}

func (s *Store) Finish(ctx context.Context, id operations.StableID, counters operations.InvocationCounters,
	state operations.State, publicError *operations.PublicError,
) error {
	return s.FinishOperationInvocation(ctx, id, counters, state, publicError)
}

var operationInvocationLedgers = map[operations.Kind]operationInvocationLedger{
	operations.KindMessageEmbedding:   {table: "message_embedding_runs", truncated: true},
	operations.KindPersonEmbedding:    {table: "person_embedding_runs", truncated: true},
	operations.KindDocumentExtraction: {table: "document_extraction_runs"},
	operations.KindDocumentEmbedding:  {table: "document_embedding_runs"},
	operations.KindVisualEmbedding:    {table: "visual_embedding_runs", skipped: true},
}

func (s *Store) BeginOperationInvocation(
	ctx context.Context, spec operations.InvocationSpec,
) (operations.BeginResult, error) {
	if err := spec.Validate(); err != nil {
		return operations.BeginResult{}, err
	}
	spec = spec.Normalized()
	ledger, ok := operationInvocationLedgers[spec.Kind]
	if !ok {
		return operations.BeginResult{}, fmt.Errorf("operation kind %q has no invocation ledger", spec.Kind)
	}

	var result operations.BeginResult
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		var id int64
		err := tx.QueryRowContext(ctx, fmt.Sprintf(`
			INSERT INTO %s (invocation_key, trigger, state, started_at)
			VALUES (?, ?, 'running', ?)
			ON CONFLICT(invocation_key) DO NOTHING
			RETURNING id`, ledger.table), spec.Key, string(spec.Trigger),
			s.dialect.TimestampParam(spec.StartedAt)).Scan(&id)
		switch {
		case err == nil:
			stableID, idErr := operations.NewInt64ID(spec.Kind, id)
			if idErr != nil {
				return idErr
			}
			result = operations.BeginResult{ID: stableID, Disposition: operations.BeginCreated}
			return nil
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("insert operation invocation: %w", err)
		}

		run, readErr := scanOperationInvocation(spec.Kind, tx.QueryRowContext(ctx,
			operationInvocationSelect(ledger)+" WHERE invocation_key = ?", spec.Key))
		if readErr != nil {
			return fmt.Errorf("read existing operation invocation: %w", readErr)
		}
		result.ID = run.ID
		if run.State == operations.StateRunning {
			result.Disposition = operations.BeginActive
			return nil
		}
		result.Disposition = operations.BeginTerminal
		result.Terminal = &run
		return nil
	})
	return result, err
}

func (s *Store) CheckpointOperationInvocation(
	ctx context.Context, id operations.StableID, counters operations.InvocationCounters,
) error {
	ledger, err := operationInvocationLedgerForID(id)
	if err != nil {
		return err
	}
	return retryBusyWriteErr(ctx, s, "checkpoint operation invocation", func() error {
		return s.withTxContext(ctx, func(tx *loggedTx) error {
			run, readErr := scanOperationInvocation(id.Kind(), tx.QueryRowContext(ctx,
				operationInvocationSelect(ledger)+" WHERE id = ?"+s.dialect.SelectForUpdate(), mustOperationInt64ID(id)))
			if errors.Is(readErr, sql.ErrNoRows) {
				return ErrOperationInvocationNotFound
			}
			if readErr != nil {
				return fmt.Errorf("read operation invocation checkpoint: %w", readErr)
			}
			if run.State != operations.StateRunning {
				return ErrOperationInvocationTerminal
			}
			previous, convertErr := operations.InvocationCountersFromPublic(id.Kind(), run.Counters)
			if convertErr != nil {
				return convertErr
			}
			if validateErr := counters.ValidateCheckpoint(id.Kind(), previous); validateErr != nil {
				return validateErr
			}
			result, updateErr := tx.ExecContext(ctx, operationInvocationCounterUpdate(ledger),
				operationInvocationCounterArgs(ledger, counters, mustOperationInt64ID(id))...)
			if updateErr != nil {
				return fmt.Errorf("checkpoint operation invocation: %w", updateErr)
			}
			rows, rowsErr := result.RowsAffected()
			if rowsErr != nil {
				return fmt.Errorf("checkpoint operation invocation rows: %w", rowsErr)
			}
			if rows != 1 {
				return ErrOperationInvocationTerminal
			}
			return nil
		})
	})
}

func (s *Store) FinishOperationInvocation(
	ctx context.Context,
	id operations.StableID,
	counters operations.InvocationCounters,
	state operations.State,
	publicError *operations.PublicError,
) error {
	ledger, err := operationInvocationLedgerForID(id)
	if err != nil {
		return err
	}
	if state == operations.StateRunning || state == operations.StateQueued {
		return errors.New("operation invocation finish requires a terminal state")
	}
	if err := counters.ValidateFinal(id.Kind()); err != nil {
		return err
	}
	if publicError != nil {
		if err := publicError.Validate(); err != nil {
			return err
		}
	}
	derivedState, err := operations.DeriveInvocationState(id.Kind(), counters, publicError)
	if err != nil {
		return err
	}
	if state != derivedState {
		return fmt.Errorf("operation invocation state %q does not match derived state %q", state, derivedState)
	}

	return retryBusyWriteErr(ctx, s, "finish operation invocation", func() error {
		return s.withTxContext(ctx, func(tx *loggedTx) error {
			run, readErr := scanOperationInvocation(id.Kind(), tx.QueryRowContext(ctx,
				operationInvocationSelect(ledger)+" WHERE id = ?"+s.dialect.SelectForUpdate(), mustOperationInt64ID(id)))
			if errors.Is(readErr, sql.ErrNoRows) {
				return ErrOperationInvocationNotFound
			}
			if readErr != nil {
				return fmt.Errorf("read operation invocation finish: %w", readErr)
			}
			if run.State != operations.StateRunning {
				if sameOperationInvocationFinish(run, counters, state, publicError) {
					return nil
				}
				return ErrOperationInvocationTerminal
			}
			previous, convertErr := operations.InvocationCountersFromPublic(id.Kind(), run.Counters)
			if convertErr != nil {
				return convertErr
			}
			if err := counters.ValidateCheckpoint(id.Kind(), previous); err != nil {
				return err
			}
			finishedAt := time.Now().UTC()
			if finishedAt.Before(run.StartedAt) {
				finishedAt = run.StartedAt
			}
			candidate := operations.Run{
				ID: id, Lane: operationLane(id.Kind()), State: state,
				Trigger: run.Trigger, StartedAt: run.StartedAt, FinishedAt: &finishedAt,
				Counters: counters.PublicCounters(id.Kind()), Error: publicError,
			}
			if err := candidate.Validate(); err != nil {
				return err
			}
			errorCode := any(nil)
			if publicError != nil {
				errorCode = string(publicError.Code)
			}
			args := operationInvocationFinishArgs(ledger, state, finishedAt, errorCode, counters, mustOperationInt64ID(id))
			result, updateErr := tx.ExecContext(ctx, operationInvocationFinishUpdate(ledger), args...)
			if updateErr != nil {
				return fmt.Errorf("finish operation invocation: %w", updateErr)
			}
			rows, rowsErr := result.RowsAffected()
			if rowsErr != nil {
				return fmt.Errorf("finish operation invocation rows: %w", rowsErr)
			}
			if rows != 1 {
				return ErrOperationInvocationTerminal
			}
			return nil
		})
	})
}

func (s *Store) RecoverOperationInvocations(ctx context.Context, recoveredAt time.Time) error {
	if recoveredAt.IsZero() {
		return errors.New("operation invocation recovery time is required")
	}
	recoveredAt = recoveredAt.UTC()
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		for kind, ledger := range operationInvocationLedgers {
			rows, err := tx.QueryContext(ctx, operationInvocationSelect(ledger)+" WHERE state = 'running' ORDER BY id")
			if err != nil {
				return fmt.Errorf("list stale %s invocations: %w", kind, err)
			}
			var runs []operations.Run
			for rows.Next() {
				run, scanErr := scanOperationInvocation(kind, rows)
				if scanErr != nil {
					_ = rows.Close()
					return scanErr
				}
				runs = append(runs, run)
			}
			if err := rows.Err(); err != nil {
				_ = rows.Close()
				return err
			}
			if err := rows.Close(); err != nil {
				return err
			}
			for _, run := range runs {
				counters, err := operations.InvocationCountersFromPublic(kind, run.Counters)
				if err != nil {
					return err
				}
				accounted := counters.Succeeded + counters.Failed + counters.Skipped
				if counters.Attempted > accounted {
					counters.Failed += counters.Attempted - accounted
				} else if accounted > counters.Attempted {
					counters.Attempted = accounted
				}
				state := operations.StateFailed
				if counters.Succeeded > 0 {
					state = operations.StatePartial
				}
				finishedAt := recoveredAt
				if finishedAt.Before(run.StartedAt) {
					finishedAt = run.StartedAt
				}
				args := operationInvocationFinishArgs(ledger, state, finishedAt,
					string(operations.PublicErrorInvocationDaemonRestarted), counters, mustOperationInt64ID(run.ID))
				if _, err := tx.ExecContext(ctx, operationInvocationFinishUpdate(ledger), args...); err != nil {
					return fmt.Errorf("recover %s invocation: %w", kind, err)
				}
			}
		}
		return nil
	})
}

func operationInvocationLedgerForID(id operations.StableID) (operationInvocationLedger, error) {
	if err := id.Validate(); err != nil {
		return operationInvocationLedger{}, err
	}
	ledger, ok := operationInvocationLedgers[id.Kind()]
	if !ok {
		return operationInvocationLedger{}, fmt.Errorf("operation kind %q has no invocation ledger", id.Kind())
	}
	return ledger, nil
}

func mustOperationInt64ID(id operations.StableID) int64 {
	value, _ := id.Int64()
	return value
}

func operationInvocationSelect(ledger operationInvocationLedger) string {
	truncated := "0"
	if ledger.truncated {
		truncated = "truncated"
	}
	skipped := "0"
	if ledger.skipped {
		skipped = "skipped"
	}
	return fmt.Sprintf(`SELECT id, trigger, state, started_at, finished_at, error_code,
		attempted, succeeded, failed, %s, %s FROM %s`, truncated, skipped, ledger.table)
}

func scanOperationInvocation(kind operations.Kind, sc scanner) (operations.Run, error) {
	var (
		id        int64
		trigger   operations.Trigger
		state     operations.State
		started   requiredTimestamp
		finished  strictOperationNullableTimestamp
		errorCode sql.NullString
		counters  operations.InvocationCounters
	)
	if err := sc.Scan(&id, &trigger, &state, &started, &finished, &errorCode,
		&counters.Attempted, &counters.Succeeded, &counters.Failed, &counters.Truncated, &counters.Skipped); err != nil {
		return operations.Run{}, err
	}
	stableID, err := operations.NewInt64ID(kind, id)
	if err != nil {
		return operations.Run{}, err
	}
	run := operations.Run{
		ID: stableID, Lane: operationLane(kind), State: state, Trigger: &trigger,
		StartedAt: started.Time.UTC(), Counters: counters.PublicCounters(kind),
	}
	if finished.Valid {
		value := finished.Time.UTC()
		run.FinishedAt = &value
	}
	if errorCode.Valid {
		run.Error = operations.FixedPublicError(operations.PublicErrorCode(errorCode.String))
		if run.Error == nil {
			return operations.Run{}, fmt.Errorf("invalid stored operation public error code %q", errorCode.String)
		}
	}
	if err := run.Validate(); err != nil {
		return operations.Run{}, err
	}
	return run, nil
}

func operationLane(kind operations.Kind) operations.Lane {
	switch kind {
	case operations.KindMessageEmbedding:
		return operations.LaneMessages
	case operations.KindPersonEmbedding:
		return operations.LanePersonFacts
	case operations.KindDocumentExtraction, operations.KindDocumentEmbedding:
		return operations.LaneDocuments
	case operations.KindVisualEmbedding:
		return operations.LaneVisualAttachments
	default:
		return ""
	}
}

func operationInvocationCounterUpdate(ledger operationInvocationLedger) string {
	assignments := "attempted = ?, succeeded = ?, failed = ?"
	if ledger.truncated {
		assignments += ", truncated = ?"
	}
	if ledger.skipped {
		assignments += ", skipped = ?"
	}
	return fmt.Sprintf("UPDATE %s SET %s WHERE id = ? AND state = 'running'", ledger.table, assignments)
}

func operationInvocationCounterArgs(ledger operationInvocationLedger, counters operations.InvocationCounters, id int64) []any {
	args := []any{counters.Attempted, counters.Succeeded, counters.Failed}
	if ledger.truncated {
		args = append(args, counters.Truncated)
	}
	if ledger.skipped {
		args = append(args, counters.Skipped)
	}
	return append(args, id)
}

func operationInvocationFinishUpdate(ledger operationInvocationLedger) string {
	assignments := "state = ?, finished_at = ?, error_code = ?, attempted = ?, succeeded = ?, failed = ?"
	if ledger.truncated {
		assignments += ", truncated = ?"
	}
	if ledger.skipped {
		assignments += ", skipped = ?"
	}
	return fmt.Sprintf("UPDATE %s SET %s WHERE id = ? AND state = 'running'", ledger.table, assignments)
}

func operationInvocationFinishArgs(ledger operationInvocationLedger, state operations.State, finishedAt time.Time,
	errorCode any, counters operations.InvocationCounters, id int64,
) []any {
	args := []any{string(state), finishedAt.UTC(), errorCode, counters.Attempted, counters.Succeeded, counters.Failed}
	if ledger.truncated {
		args = append(args, counters.Truncated)
	}
	if ledger.skipped {
		args = append(args, counters.Skipped)
	}
	return append(args, id)
}

func sameOperationInvocationFinish(run operations.Run, counters operations.InvocationCounters,
	state operations.State, publicError *operations.PublicError,
) bool {
	existing, err := operations.InvocationCountersFromPublic(run.ID.Kind(), run.Counters)
	if err != nil || existing != counters || run.State != state {
		return false
	}
	if run.Error == nil || publicError == nil {
		return run.Error == nil && publicError == nil
	}
	return *run.Error == *publicError
}
