package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/operations"
)

var ErrOperationHistoryConsistencyConflict = errors.New("operation history consistency conflict")

const personEnrichmentOperationRunColumns = `id, kind, state, started_at, completed_at,
	requested_count, started_count, succeeded_count, failed_count,
	suppressed_count, identity_rejected_count, COALESCE(failure_class, '')`

func (s *Store) listOperationHistorySnapshot(
	ctx context.Context, query operations.Query,
) (operations.HistorySnapshot, error) {
	if err := query.Validate(); err != nil {
		return operations.HistorySnapshot{}, fmt.Errorf("list operation runs: %w", err)
	}
	selected, err := selectedDurableOperationKinds(query.Kinds)
	if err != nil {
		return operations.HistorySnapshot{}, err
	}
	fetchLimit := query.Limit + 1
	snapshot := operations.HistorySnapshot{
		Runs:             make([]operations.Run, 0, len(selected)*fetchLimit),
		AvailableKinds:   make([]operations.Kind, 0, len(selected)),
		UnavailableKinds: make([]operations.Kind, 0),
	}
	err = s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		initialRevision, revisionErr := operationMembershipRevisionTx(ctx, tx)
		if revisionErr != nil {
			return fmt.Errorf("read operation membership revision: %w", revisionErr)
		}
		snapshot.MembershipRevision = initialRevision

		for index, kind := range selected {
			savepoint := fmt.Sprintf("operation_adapter_%d", index)
			if _, savepointErr := tx.ExecContext(ctx, `SAVEPOINT `+savepoint); savepointErr != nil {
				return fmt.Errorf("create operation adapter savepoint: %w", savepointErr)
			}
			runs, listErr := s.listOperationRunsForKindFrom(ctx, tx, kind, query, fetchLimit)
			if listErr != nil {
				if ctx.Err() != nil || errors.Is(listErr, driver.ErrBadConn) {
					return fmt.Errorf("list operation runs for %q: %w", kind, listErr)
				}
				if _, rollbackErr := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT `+savepoint); rollbackErr != nil {
					return fmt.Errorf("rollback failed operation adapter %q: %w", kind, rollbackErr)
				}
				if _, releaseErr := tx.ExecContext(ctx, `RELEASE SAVEPOINT `+savepoint); releaseErr != nil {
					return fmt.Errorf("release failed operation adapter %q: %w", kind, releaseErr)
				}
				snapshot.UnavailableKinds = append(snapshot.UnavailableKinds, kind)
				continue
			}
			if _, releaseErr := tx.ExecContext(ctx, `RELEASE SAVEPOINT `+savepoint); releaseErr != nil {
				return fmt.Errorf("release operation adapter %q: %w", kind, releaseErr)
			}
			snapshot.AvailableKinds = append(snapshot.AvailableKinds, kind)
			snapshot.Runs = append(snapshot.Runs, runs...)
			if s.operationHistoryAfterAdapterReadHook != nil {
				s.operationHistoryAfterAdapterReadHook(string(kind))
			}
		}

		finalRevision, revisionErr := operationMembershipRevisionTx(ctx, tx)
		if revisionErr != nil {
			return fmt.Errorf("re-read operation membership revision: %w", revisionErr)
		}
		if finalRevision != initialRevision {
			return fmt.Errorf("%w: membership revision changed from %d to %d",
				ErrOperationHistoryConsistencyConflict, initialRevision, finalRevision)
		}
		return nil
	})
	if err != nil {
		return operations.HistorySnapshot{}, fmt.Errorf("list operation history snapshot: %w", err)
	}
	operations.SortRuns(snapshot.Runs)
	if len(snapshot.Runs) > query.Limit {
		lastExposed := snapshot.Runs[query.Limit-1]
		snapshot.Position = &operations.Position{StartedAt: lastExposed.StartedAt, ID: lastExposed.ID}
	}
	if len(snapshot.Runs) > fetchLimit {
		snapshot.Runs = snapshot.Runs[:fetchLimit]
	}
	return snapshot, nil
}

func operationMembershipRevisionTx(ctx context.Context, tx *loggedTx) (int64, error) {
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT membership_revision
		FROM operation_history_state WHERE singleton = 1`).Scan(&revision); err != nil {
		return 0, err
	}
	if revision < 0 {
		return 0, errors.New("operation membership revision is negative")
	}
	return revision, nil
}

func (s *Store) listOperationRunsForKindFrom(
	ctx context.Context,
	queryer contextRowsQuerier,
	kind operations.Kind,
	query operations.Query,
	fetchLimit int,
) ([]operations.Run, error) {
	switch kind {
	case operations.KindCardDAVSync:
		return s.listCardDAVOperationRunsFrom(ctx, queryer, query, fetchLimit)
	case operations.KindDocumentEmbedding, operations.KindDocumentExtraction,
		operations.KindMessageEmbedding, operations.KindPersonEmbedding,
		operations.KindVisualEmbedding:
		return s.listInvocationOperationRunsFrom(ctx, queryer, kind, query, fetchLimit)
	case operations.KindPersonEnrichment:
		return s.listPersonEnrichmentOperationRunsFrom(ctx, queryer, query, fetchLimit)
	case operations.KindPersonSweep:
		return s.listPersonSweepOperationRunsFrom(ctx, queryer, query, fetchLimit)
	case operations.KindSourceSync:
		return s.listSourceOperationRunsFrom(ctx, queryer, query, fetchLimit)
	default:
		return nil, fmt.Errorf("operation kind %q: %w", kind, ErrOperationHistoryUnavailable)
	}
}

func (s *Store) listInvocationOperationRunsFrom(
	ctx context.Context,
	queryer contextRowsQuerier,
	kind operations.Kind,
	query operations.Query,
	fetchLimit int,
) ([]operations.Run, error) {
	if err := query.Validate(); err != nil {
		return nil, fmt.Errorf("list operation runs: %w", err)
	}
	if fetchLimit < 1 || fetchLimit > query.Limit+1 {
		return nil, errors.New("invocation operation fetch limit must be between one and query limit plus one")
	}
	ledger, ok := operationInvocationLedgers[kind]
	if !ok {
		return nil, fmt.Errorf("operation kind %q has no invocation history", kind)
	}

	conditions := make([]string, 0, 4)
	args := make([]any, 0, 8)
	if len(query.States) > 0 {
		condition, stateArgs := directOperationStateCondition(query.States, false)
		conditions = append(conditions, condition)
		args = append(args, stateArgs...)
	}
	if query.StartedFrom != nil {
		conditions = append(conditions, `started_at >= ?`)
		args = append(args, s.dialect.TimestampParam(*query.StartedFrom))
	}
	if query.StartedBefore != nil {
		conditions = append(conditions, `started_at < ?`)
		args = append(args, s.dialect.TimestampParam(*query.StartedBefore))
	}
	if query.Position != nil {
		condition, positionArgs, err := s.numericOperationPositionCondition(
			kind, *query.Position, time.Millisecond)
		if err != nil {
			return nil, fmt.Errorf("list operation runs: %w", err)
		}
		conditions = append(conditions, condition)
		args = append(args, positionArgs...)
	}

	statement := operationInvocationSelect(ledger)
	if len(conditions) > 0 {
		statement += ` WHERE ` + strings.Join(conditions, ` AND `)
	}
	statement += ` ORDER BY started_at DESC, id DESC LIMIT ?`
	args = append(args, fetchLimit)
	rows, err := queryer.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("list %s operation runs: %w", kind, err)
	}
	defer func() { _ = rows.Close() }()
	runs := make([]operations.Run, 0, fetchLimit)
	for rows.Next() {
		run, scanErr := scanOperationInvocation(kind, rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan %s operation run: %w", kind, scanErr)
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate %s operation runs: %w", kind, err)
	}
	return runs, nil
}

func (s *Store) listPersonEnrichmentOperationRunsFrom(
	ctx context.Context,
	queryer contextRowsQuerier,
	query operations.Query,
	fetchLimit int,
) ([]operations.Run, error) {
	if err := query.Validate(); err != nil {
		return nil, fmt.Errorf("list operation runs: %w", err)
	}
	if fetchLimit < 1 || fetchLimit > query.Limit+1 {
		return nil, errors.New("person enrichment operation fetch limit must be between one and query limit plus one")
	}
	conditions := []string{`started_at IS NOT NULL`}
	args := make([]any, 0, 8)
	if len(query.States) > 0 {
		condition, stateArgs := directOperationStateCondition(query.States, true)
		conditions = append(conditions, condition)
		args = append(args, stateArgs...)
	}
	if query.StartedFrom != nil {
		conditions = append(conditions, `started_at >= ?`)
		args = append(args, query.StartedFrom.UTC())
	}
	if query.StartedBefore != nil {
		conditions = append(conditions, `started_at < ?`)
		args = append(args, query.StartedBefore.UTC())
	}
	if query.Position != nil {
		condition, positionArgs, err := s.numericOperationPositionConditionWithTimestamp(
			operations.KindPersonEnrichment, *query.Position, time.Millisecond,
			query.Position.StartedAt.UTC())
		if err != nil {
			return nil, fmt.Errorf("list operation runs: %w", err)
		}
		conditions = append(conditions, condition)
		args = append(args, positionArgs...)
	}
	statement := `SELECT ` + personEnrichmentOperationRunColumns + `
		FROM person_enrichment_runs WHERE ` + strings.Join(conditions, ` AND `) + `
		ORDER BY started_at DESC, id DESC LIMIT ?`
	args = append(args, fetchLimit)
	rows, err := queryer.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("list person enrichment operation runs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	runs := make([]operations.Run, 0, fetchLimit)
	for rows.Next() {
		run, scanErr := scanPersonEnrichmentOperationRun(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan person enrichment operation run: %w", scanErr)
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate person enrichment operation runs: %w", err)
	}
	return runs, nil
}

func directOperationStateCondition(states []operations.State, queued bool) (string, []any) {
	args := make([]any, 0, len(states))
	placeholders := make([]string, 0, len(states))
	for _, state := range states {
		if state == operations.StateQueued && !queued {
			continue
		}
		placeholders = append(placeholders, `?`)
		args = append(args, string(state))
	}
	if len(placeholders) == 0 {
		return `FALSE`, nil
	}
	return `state IN (` + strings.Join(placeholders, `, `) + `)`, args
}

func (s *Store) numericOperationPositionCondition(
	kind operations.Kind, position operations.Position, sqlitePrecision time.Duration,
) (string, []any, error) {
	return s.numericOperationPositionConditionWithTimestamp(
		kind, position, sqlitePrecision, s.dialect.TimestampParam(position.StartedAt))
}

func (s *Store) numericOperationPositionConditionWithTimestamp(
	kind operations.Kind, position operations.Position, sqlitePrecision time.Duration, timestamp any,
) (string, []any, error) {
	if s.sqliteOperationPositionFinerThan(position.StartedAt, sqlitePrecision) {
		return `started_at <= ?`, []any{timestamp}, nil
	}
	switch strings.Compare(string(kind), string(position.ID.Kind())) {
	case -1:
		return `started_at < ?`, []any{timestamp}, nil
	case 1:
		return `started_at <= ?`, []any{timestamp}, nil
	default:
		positionID, ok := position.ID.Int64()
		if !ok {
			return "", nil, fmt.Errorf("operation kind %q position requires a numeric run ID", kind)
		}
		return `(started_at < ? OR (started_at = ? AND id < ?))`,
			[]any{timestamp, timestamp, positionID}, nil
	}
}

func (s *Store) getInvocationOperationRun(
	ctx context.Context, id operations.StableID,
) (operations.Run, error) {
	ledger, err := operationInvocationLedgerForID(id)
	if err != nil {
		return operations.Run{}, fmt.Errorf("get operation run: %w", err)
	}
	run, err := scanOperationInvocation(id.Kind(), s.db.QueryRowContext(ctx,
		operationInvocationSelect(ledger)+` WHERE id = ?`, mustOperationInt64ID(id)))
	if errors.Is(err, sql.ErrNoRows) {
		return operations.Run{}, fmt.Errorf("get operation run: %w", ErrOperationRunNotFound)
	}
	if err != nil {
		return operations.Run{}, fmt.Errorf("get %s operation run: %w", id.Kind(), err)
	}
	return run, nil
}

func (s *Store) getPersonEnrichmentOperationRun(
	ctx context.Context, id operations.StableID,
) (operations.Run, error) {
	if err := id.Validate(); err != nil {
		return operations.Run{}, fmt.Errorf("get operation run: %w", err)
	}
	if id.Kind() != operations.KindPersonEnrichment {
		return operations.Run{}, fmt.Errorf("get operation run: %w", ErrOperationRunNotFound)
	}
	runID, ok := id.Int64()
	if !ok {
		return operations.Run{}, fmt.Errorf("get operation run: %w", ErrOperationRunNotFound)
	}
	run, err := scanPersonEnrichmentOperationRun(s.db.QueryRowContext(ctx,
		`SELECT `+personEnrichmentOperationRunColumns+`
		 FROM person_enrichment_runs WHERE id = ? AND started_at IS NOT NULL`, runID))
	if errors.Is(err, sql.ErrNoRows) {
		return operations.Run{}, fmt.Errorf("get operation run: %w", ErrOperationRunNotFound)
	}
	if err != nil {
		return operations.Run{}, fmt.Errorf("get person enrichment operation run: %w", err)
	}
	return run, nil
}

func scanPersonEnrichmentOperationRun(sc scanner) (operations.Run, error) {
	var (
		runID                                      int64
		durableTrigger, durableState, failureClass string
		startedAt                                  requiredTimestamp
		completedAt                                strictOperationNullableTimestamp
		counters                                   operations.InvocationCounters
	)
	if err := sc.Scan(&runID, &durableTrigger, &durableState, &startedAt, &completedAt,
		&counters.Requested, &counters.Started, &counters.Succeeded, &counters.Failed,
		&counters.Suppressed, &counters.IdentityRejected, &failureClass); err != nil {
		return operations.Run{}, err
	}
	id, err := operations.NewInt64ID(operations.KindPersonEnrichment, runID)
	if err != nil {
		return operations.Run{}, fmt.Errorf("person enrichment operation stable ID: %w", err)
	}
	trigger := operations.Trigger(durableTrigger)
	if err := trigger.Validate(); err != nil {
		return operations.Run{}, fmt.Errorf("person enrichment operation trigger: %w", err)
	}
	state := operations.State(durableState)
	if err := state.Validate(); err != nil {
		return operations.Run{}, fmt.Errorf("person enrichment operation state: %w", err)
	}
	run := operations.Run{
		ID: id, Lane: operations.LanePersonFacts, State: state, Trigger: &trigger,
		StartedAt: startedAt.Time.UTC(), Counters: counters.PublicCounters(operations.KindPersonEnrichment),
	}
	if completedAt.Valid {
		finishedAt := completedAt.Time.UTC()
		run.FinishedAt = &finishedAt
	}
	if state == operations.StatePartial || state == operations.StateFailed {
		run.Error = projectPersonEnrichmentOperationFailure(failureClass)
	}
	if err := run.Validate(); err != nil {
		return operations.Run{}, fmt.Errorf("validate person enrichment operation run: %w", err)
	}
	return run, nil
}

func projectPersonEnrichmentOperationFailure(class string) *operations.PublicError {
	var code operations.PublicErrorCode
	switch class {
	case "":
		return nil
	case "rate_limited":
		code = operations.PublicErrorInvocationRateLimited
	case "invalid_output":
		code = operations.PublicErrorInvocationInvalidOutput
	case "uncertain_start":
		code = operations.PublicErrorInvocationDaemonRestarted
	case "transient", "terminal":
		code = operations.PublicErrorInvocationUpstreamFailed
	default:
		code = operations.PublicErrorInvocationInternal
	}
	return operations.FixedPublicError(code)
}

func (s *Store) genericOperationLaneStatus(
	ctx context.Context, kind operations.Kind,
) (operations.LaneHistoryStatus, error) {
	if !slices.Contains(durableOperationKinds, kind) {
		return operations.LaneHistoryStatus{}, fmt.Errorf("operation kind %q: %w",
			kind, ErrOperationHistoryUnavailable)
	}
	status := operations.LaneHistoryStatus{
		Kind: kind, Lane: operationHistoryLane(kind), HistoryAvailability: operations.HistoryAvailable,
	}
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		activeStates := []operations.State{operations.StateRunning}
		if kind == operations.KindPersonEnrichment {
			activeStates = []operations.State{operations.StateQueued, operations.StateRunning}
		}
		var err error
		status.Active, err = s.firstOperationStatusRun(ctx, tx, kind, activeStates)
		if err != nil {
			return fmt.Errorf("read active %s operation run: %w", kind, err)
		}
		if s.operationHistoryStatusAfterActiveHook != nil {
			s.operationHistoryStatusAfterActiveHook(string(kind))
		}
		status.Latest, err = s.firstOperationStatusRun(ctx, tx, kind, nil)
		if err != nil {
			return fmt.Errorf("read latest %s operation run: %w", kind, err)
		}
		status.LatestSuccessful, err = s.firstOperationStatusRun(
			ctx, tx, kind, []operations.State{operations.StateSucceeded})
		if err != nil {
			return fmt.Errorf("read latest successful %s operation run: %w", kind, err)
		}
		return nil
	})
	if err != nil {
		return operations.LaneHistoryStatus{}, fmt.Errorf("read %s operation status: %w", kind, err)
	}
	if err := status.Validate(); err != nil {
		return operations.LaneHistoryStatus{}, fmt.Errorf("validate %s operation status: %w", kind, err)
	}
	return status, nil
}

func (s *Store) firstOperationStatusRun(
	ctx context.Context, tx *loggedTx, kind operations.Kind, states []operations.State,
) (*operations.Run, error) {
	runs, err := s.listOperationRunsForKindFrom(ctx, tx, kind, operations.Query{
		Kinds: []operations.Kind{kind}, States: states, Limit: 1,
	}, 1)
	if err != nil {
		return nil, err
	}
	if len(runs) == 0 {
		return nil, nil //nolint:nilnil // No matching run is a valid empty status role.
	}
	return &runs[0], nil
}

func operationHistoryLane(kind operations.Kind) operations.Lane {
	switch kind {
	case operations.KindSourceSync, operations.KindMessageEmbedding:
		return operations.LaneMessages
	case operations.KindPersonSweep, operations.KindPersonEmbedding, operations.KindPersonEnrichment:
		return operations.LanePersonFacts
	case operations.KindCardDAVSync:
		return operations.LaneContacts
	case operations.KindDocumentExtraction, operations.KindDocumentEmbedding:
		return operations.LaneDocuments
	case operations.KindVisualEmbedding:
		return operations.LaneVisualAttachments
	default:
		return ""
	}
}
