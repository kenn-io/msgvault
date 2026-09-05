package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/peoplesweep"
)

// ErrOperationRunNotFound reports that a valid operation run ID has no row in
// the durable history owned by this store.
var (
	ErrOperationRunNotFound        = errors.New("operation run not found")
	ErrOperationHistoryUnavailable = errors.New("operation history unavailable")
)

var _ operations.HistoryReader = (*Store)(nil)

var durableOperationKinds = []operations.Kind{
	operations.KindCardDAVSync,
	operations.KindDocumentEmbedding,
	operations.KindDocumentExtraction,
	operations.KindMessageEmbedding,
	operations.KindPersonEmbedding,
	operations.KindPersonEnrichment,
	operations.KindPersonSweep,
	operations.KindSourceSync,
	operations.KindVisualEmbedding,
}

const (
	sourceOperationRunColumns = `id, started_at, completed_at, status,
		messages_processed, messages_added, messages_updated, errors_count`
	cardDAVOperationRunColumns = `id, trigger, state, started_at, finished_at,
		books, created, updated, removed,
		CASE WHEN state IN ('failed', 'cancelled', 'partial') THEN error_code ELSE '' END`
	operationSQLiteTimestampLayout = "2006-01-02 15:04:05.999999999"
)

func personSweepOperationRunColumns(textCollation string) string {
	return `r.id, r.kind, r.status,
		r.attempt_count, r.success_count, r.failure_count, r.projected_write_count,
		r.started_at, r.completed_at,
		COALESCE((
			SELECT a.failure_class
			FROM person_sweep_attempts a
			WHERE a.run_id = r.id AND a.failure_class <> ''
			ORDER BY COALESCE(a.completed_at, a.started_at) DESC, a.id` + textCollation + ` DESC
			LIMIT 1
		), '')`
}

// strictOperationNullableTimestamp distinguishes SQL NULL from malformed
// non-null values. The shared nullableTimestamp intentionally does not, but
// public operation history must fail closed on durable ledger corruption.
type strictOperationNullableTimestamp struct {
	Time  time.Time
	Valid bool
}

func (n *strictOperationNullableTimestamp) Scan(src any) error {
	if src == nil {
		n.Time = time.Time{}
		n.Valid = false
		return nil
	}
	var required requiredTimestamp
	if err := required.Scan(src); err != nil {
		return fmt.Errorf("operation completion timestamp: %w", err)
	}
	n.Time = required.Time
	n.Valid = true
	return nil
}

// listSourceOperationRuns projects source sync rows using only the durable
// columns allowed by the normalized operations contract.
func (s *Store) listSourceOperationRuns(
	ctx context.Context, query operations.Query, fetchLimit int,
) ([]operations.Run, error) {
	return s.listSourceOperationRunsFrom(ctx, s.db, query, fetchLimit)
}

func (s *Store) listSourceOperationRunsFrom(
	ctx context.Context, queryer contextRowsQuerier, query operations.Query, fetchLimit int,
) ([]operations.Run, error) {
	if err := query.Validate(); err != nil {
		return nil, fmt.Errorf("list operation runs: %w", err)
	}
	if fetchLimit < 1 || fetchLimit > query.Limit+1 {
		return nil, errors.New("source operation fetch limit must be between one and query limit plus one")
	}

	conditions := make([]string, 0, 4)
	args := make([]any, 0, 6)
	if len(query.States) > 0 {
		conditions = append(conditions, sourceOperationStateCondition(query.States))
	}
	if query.StartedFrom != nil {
		conditions = append(conditions, `started_at >= ?`)
		args = append(args, s.operationTimestampParam(*query.StartedFrom))
	}
	if query.StartedBefore != nil {
		conditions = append(conditions, `started_at < ?`)
		args = append(args, s.operationTimestampParam(*query.StartedBefore))
	}
	if query.Position != nil {
		condition, positionArgs, err := s.sourceOperationPositionCondition(*query.Position)
		if err != nil {
			return nil, fmt.Errorf("list operation runs: %w", err)
		}
		conditions = append(conditions, condition)
		args = append(args, positionArgs...)
	}

	statement := `SELECT ` + sourceOperationRunColumns + ` FROM sync_runs`
	if len(conditions) > 0 {
		statement += ` WHERE ` + strings.Join(conditions, ` AND `)
	}
	statement += ` ORDER BY started_at DESC, id DESC LIMIT ?`
	args = append(args, fetchLimit)

	rows, err := queryer.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("list source operation runs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	runs := make([]operations.Run, 0, fetchLimit)
	for rows.Next() {
		run, scanErr := scanSourceOperationRun(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan source operation run: %w", scanErr)
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate source operation runs: %w", err)
	}
	return runs, nil
}

// getSourceOperationRun returns one safe source operation projection. The
// typed stable ID is validated before it can select a durable table.
func (s *Store) getSourceOperationRun(
	ctx context.Context, id operations.StableID,
) (operations.Run, error) {
	if err := id.Validate(); err != nil {
		return operations.Run{}, fmt.Errorf("get operation run: %w", err)
	}
	if id.Kind() != operations.KindSourceSync {
		return operations.Run{}, fmt.Errorf("get operation run: %w", ErrOperationRunNotFound)
	}
	runID, ok := id.Int64()
	if !ok {
		return operations.Run{}, fmt.Errorf("get operation run: %w", ErrOperationRunNotFound)
	}

	run, err := scanSourceOperationRun(s.db.QueryRowContext(ctx,
		`SELECT `+sourceOperationRunColumns+` FROM sync_runs WHERE id = ?`, runID))
	if errors.Is(err, sql.ErrNoRows) {
		return operations.Run{}, fmt.Errorf("get operation run: %w", ErrOperationRunNotFound)
	}
	if err != nil {
		return operations.Run{}, fmt.Errorf("get source operation run: %w", err)
	}
	return run, nil
}

// sourceOperationLaneStatus returns a consistent source-lane snapshot. Each
// query reads only the safe run columns and all three roles share one database
// snapshot.
func (s *Store) sourceOperationLaneStatus(
	ctx context.Context,
) (operations.LaneHistoryStatus, error) {
	status := operations.LaneHistoryStatus{
		Kind:                operations.KindSourceSync,
		Lane:                operations.LaneMessages,
		HistoryAvailability: operations.HistoryAvailable,
	}
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		var err error
		status.Active, err = sourceOperationStatusRun(
			ctx, tx, `status = 'running'`)
		if err != nil {
			return fmt.Errorf("read active source operation run: %w", err)
		}
		if s.operationHistoryStatusAfterActiveHook != nil {
			s.operationHistoryStatusAfterActiveHook(string(operations.KindSourceSync))
		}
		status.Latest, err = sourceOperationStatusRun(ctx, tx, `TRUE`)
		if err != nil {
			return fmt.Errorf("read latest source operation run: %w", err)
		}
		status.LatestSuccessful, err = sourceOperationStatusRun(
			ctx, tx, `status = 'completed' AND errors_count = 0`)
		if err != nil {
			return fmt.Errorf("read latest successful source operation run: %w", err)
		}
		return nil
	})
	if err != nil {
		return operations.LaneHistoryStatus{}, fmt.Errorf("read source operation status: %w", err)
	}
	if err := status.Validate(); err != nil {
		return operations.LaneHistoryStatus{}, fmt.Errorf("validate source operation status: %w", err)
	}
	return status, nil
}

func sourceOperationStateCondition(states []operations.State) string {
	conditions := make([]string, 0, len(states))
	for _, state := range states {
		switch state {
		case operations.StateRunning:
			conditions = append(conditions, `status = 'running'`)
		case operations.StateSucceeded:
			conditions = append(conditions, `(status = 'completed' AND errors_count = 0)`)
		case operations.StatePartial:
			conditions = append(conditions, `((status = 'completed' AND errors_count > 0) OR `+
				`(status = 'failed' AND (messages_added > 0 OR messages_updated > 0)))`)
		case operations.StateFailed:
			conditions = append(conditions, `(status = 'failed' AND messages_added = 0 AND messages_updated = 0)`)
		case operations.StateCancelled:
			conditions = append(conditions, `status = 'cancelled'`)
		case operations.StateQueued:
			// Source sync has no durable queued rows.
		}
	}
	if len(conditions) == 0 {
		return `FALSE`
	}
	return `(` + strings.Join(conditions, ` OR `) + `)`
}

func (s *Store) sourceOperationPositionCondition(
	position operations.Position,
) (string, []any, error) {
	timestamp := s.operationTimestampParam(position.StartedAt)
	if s.sqliteOperationPositionFinerThan(position.StartedAt, time.Second) {
		return `started_at <= ?`, []any{timestamp}, nil
	}
	switch cmp.Compare(operations.KindSourceSync, position.ID.Kind()) {
	case -1:
		return `started_at < ?`, []any{timestamp}, nil
	case 1:
		return `started_at <= ?`, []any{timestamp}, nil
	default:
		positionID, ok := position.ID.Int64()
		if !ok {
			return "", nil, errors.New("source operation position requires a numeric source run ID")
		}
		return `(started_at < ? OR (started_at = ? AND id < ?))`,
			[]any{timestamp, timestamp, positionID}, nil
	}
}

// operationTimestampParam renders a run position timestamp the way the
// source and CardDAV run tables store started_at on each backend.
func (s *Store) operationTimestampParam(value time.Time) any {
	if s.IsPostgreSQL() {
		return s.dialect.TimestampParam(value)
	}
	return value.UTC().Format(operationSQLiteTimestampLayout)
}

func sourceOperationStatusQuery(condition string) string {
	return `SELECT ` + sourceOperationRunColumns + ` FROM sync_runs WHERE ` + condition +
		` ORDER BY started_at DESC, id DESC LIMIT 1`
}

func sourceOperationStatusRun(
	ctx context.Context, queryer *loggedTx, condition string,
) (*operations.Run, error) {
	run, err := scanSourceOperationRun(queryer.QueryRowContext(ctx, sourceOperationStatusQuery(condition)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil //nolint:nilnil // No matching source run is a valid empty status.
	}
	if err != nil {
		return nil, err
	}
	return &run, nil
}

func scanSourceOperationRun(sc scanner) (operations.Run, error) {
	var (
		runID        int64
		startedAt    requiredTimestamp
		completedAt  strictOperationNullableTimestamp
		durableState string
		processed    int64
		added        int64
		updated      int64
		itemErrors   int64
	)
	if err := sc.Scan(
		&runID, &startedAt, &completedAt, &durableState,
		&processed, &added, &updated, &itemErrors,
	); err != nil {
		return operations.Run{}, err
	}

	id, err := operations.NewInt64ID(operations.KindSourceSync, runID)
	if err != nil {
		return operations.Run{}, fmt.Errorf("source operation stable ID: %w", err)
	}
	state, publicError, err := operations.ProjectSourceState(
		durableState, itemErrors, added, updated)
	if err != nil {
		return operations.Run{}, err
	}
	run := operations.Run{
		ID:        id,
		Lane:      operations.LaneMessages,
		State:     state,
		StartedAt: startedAt.Time.UTC(),
		Counters: []operations.PublicCounter{
			{Name: operations.CounterProcessed, Unit: operations.CounterUnitMessages, Value: processed},
			{Name: operations.CounterAdded, Unit: operations.CounterUnitMessages, Value: added},
			{Name: operations.CounterUpdated, Unit: operations.CounterUnitMessages, Value: updated},
			{Name: operations.CounterItemErrors, Unit: operations.CounterUnitMessages, Value: itemErrors},
		},
		Error: publicError,
	}
	if completedAt.Valid {
		finishedAt := completedAt.Time.UTC()
		run.FinishedAt = &finishedAt
	}
	if err := run.Validate(); err != nil {
		return operations.Run{}, fmt.Errorf("validate source operation run: %w", err)
	}
	return run, nil
}

// listPersonSweepOperationRuns projects only safe run-level sweep metadata and
// one allow-listed aggregate failure class. It never hydrates person, attempt,
// evidence, fingerprint, provider, model, or usage fields.
func (s *Store) listPersonSweepOperationRuns(
	ctx context.Context, query operations.Query, fetchLimit int,
) ([]operations.Run, error) {
	return s.listPersonSweepOperationRunsFrom(ctx, s.db, query, fetchLimit)
}

func (s *Store) listPersonSweepOperationRunsFrom(
	ctx context.Context, queryer contextRowsQuerier, query operations.Query, fetchLimit int,
) ([]operations.Run, error) {
	if err := query.Validate(); err != nil {
		return nil, fmt.Errorf("list operation runs: %w", err)
	}
	if fetchLimit < 1 || fetchLimit > query.Limit+1 {
		return nil, errors.New("person sweep operation fetch limit must be between one and query limit plus one")
	}

	conditions := make([]string, 0, 4)
	args := make([]any, 0, 6)
	if len(query.States) > 0 {
		conditions = append(conditions, personSweepOperationStateCondition(query.States))
	}
	if query.StartedFrom != nil {
		conditions = append(conditions, `r.started_at >= ?`)
		args = append(args, s.dialect.TimestampParam(*query.StartedFrom))
	}
	if query.StartedBefore != nil {
		conditions = append(conditions, `r.started_at < ?`)
		args = append(args, s.dialect.TimestampParam(*query.StartedBefore))
	}
	if query.Position != nil {
		condition, positionArgs, err := s.personSweepOperationPositionCondition(*query.Position)
		if err != nil {
			return nil, fmt.Errorf("list operation runs: %w", err)
		}
		conditions = append(conditions, condition)
		args = append(args, positionArgs...)
	}

	textCollation := s.bytewiseTextCollation()
	statement := `SELECT ` + personSweepOperationRunColumns(textCollation) + ` FROM person_sweep_runs r`
	if len(conditions) > 0 {
		statement += ` WHERE ` + strings.Join(conditions, ` AND `)
	}
	statement += ` ORDER BY r.started_at DESC, r.id` + textCollation + ` DESC LIMIT ?`
	args = append(args, fetchLimit)

	rows, err := queryer.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("list person sweep operation runs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	runs := make([]operations.Run, 0, fetchLimit)
	for rows.Next() {
		run, scanErr := scanPersonSweepOperationRun(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan person sweep operation run: %w", scanErr)
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate person sweep operation runs: %w", err)
	}
	return runs, nil
}

func (s *Store) getPersonSweepOperationRun(
	ctx context.Context, id operations.StableID,
) (operations.Run, error) {
	if err := id.Validate(); err != nil {
		return operations.Run{}, fmt.Errorf("get operation run: %w", err)
	}
	if id.Kind() != operations.KindPersonSweep {
		return operations.Run{}, fmt.Errorf("get operation run: %w", ErrOperationRunNotFound)
	}
	runID, ok := id.Text()
	if !ok {
		return operations.Run{}, fmt.Errorf("get operation run: %w", ErrOperationRunNotFound)
	}
	run, err := scanPersonSweepOperationRun(s.db.QueryRowContext(ctx,
		`SELECT `+personSweepOperationRunColumns(s.bytewiseTextCollation())+
			` FROM person_sweep_runs r WHERE r.id = ?`, runID))
	if errors.Is(err, sql.ErrNoRows) {
		return operations.Run{}, fmt.Errorf("get operation run: %w", ErrOperationRunNotFound)
	}
	if err != nil {
		return operations.Run{}, fmt.Errorf("get person sweep operation run: %w", err)
	}
	return run, nil
}

func (s *Store) personSweepOperationLaneStatus(
	ctx context.Context,
) (operations.LaneHistoryStatus, error) {
	status := operations.LaneHistoryStatus{
		Kind:                operations.KindPersonSweep,
		Lane:                operations.LanePersonFacts,
		HistoryAvailability: operations.HistoryAvailable,
	}
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		var err error
		columns := personSweepOperationRunColumns(s.bytewiseTextCollation())
		status.Active, err = personSweepOperationStatusRun(
			ctx, tx, columns, s.bytewiseTextCollation(), `r.status = 'running'`)
		if err != nil {
			return fmt.Errorf("read active person sweep operation run: %w", err)
		}
		if s.operationHistoryStatusAfterActiveHook != nil {
			s.operationHistoryStatusAfterActiveHook(string(operations.KindPersonSweep))
		}
		status.Latest, err = personSweepOperationStatusRun(
			ctx, tx, columns, s.bytewiseTextCollation(), `TRUE`)
		if err != nil {
			return fmt.Errorf("read latest person sweep operation run: %w", err)
		}
		status.LatestSuccessful, err = personSweepOperationStatusRun(
			ctx, tx, columns, s.bytewiseTextCollation(), `r.status = 'succeeded'`)
		if err != nil {
			return fmt.Errorf("read latest successful person sweep operation run: %w", err)
		}
		return nil
	})
	if err != nil {
		return operations.LaneHistoryStatus{}, fmt.Errorf("read person sweep operation status: %w", err)
	}
	if err := status.Validate(); err != nil {
		return operations.LaneHistoryStatus{}, fmt.Errorf("validate person sweep operation status: %w", err)
	}
	return status, nil
}

func personSweepOperationStateCondition(states []operations.State) string {
	conditions := make([]string, 0, len(states))
	for _, state := range states {
		switch state {
		case operations.StateRunning:
			conditions = append(conditions, `r.status = 'running'`)
		case operations.StateSucceeded:
			conditions = append(conditions, `r.status = 'succeeded'`)
		case operations.StatePartial:
			conditions = append(conditions, `r.status = 'partial'`)
		case operations.StateFailed:
			conditions = append(conditions, `r.status = 'failed'`)
		case operations.StateQueued, operations.StateCancelled:
			// Person sweep has no durable queued or cancelled run state.
		}
	}
	if len(conditions) == 0 {
		return `FALSE`
	}
	return `(` + strings.Join(conditions, ` OR `) + `)`
}

func (s *Store) personSweepOperationPositionCondition(
	position operations.Position,
) (string, []any, error) {
	timestamp := s.dialect.TimestampParam(position.StartedAt)
	if s.sqliteOperationPositionFinerThan(position.StartedAt, time.Millisecond) {
		return `r.started_at <= ?`, []any{timestamp}, nil
	}
	switch cmp.Compare(operations.KindPersonSweep, position.ID.Kind()) {
	case -1:
		return `r.started_at < ?`, []any{timestamp}, nil
	case 1:
		return `r.started_at <= ?`, []any{timestamp}, nil
	default:
		positionID, ok := position.ID.Text()
		if !ok {
			return "", nil, errors.New("person sweep operation position requires a text sweep run ID")
		}
		return `(r.started_at < ? OR (r.started_at = ? AND r.id` +
				s.bytewiseTextCollation() + ` < ?))`,
			[]any{timestamp, timestamp, positionID}, nil
	}
}

func personSweepOperationStatusQuery(columns, textCollation, condition string) string {
	return `SELECT ` + columns + ` FROM person_sweep_runs r WHERE ` + condition +
		` ORDER BY r.started_at DESC, r.id` + textCollation + ` DESC LIMIT 1`
}

func personSweepOperationStatusRun(
	ctx context.Context, queryer *loggedTx, columns, textCollation, condition string,
) (*operations.Run, error) {
	run, err := scanPersonSweepOperationRun(queryer.QueryRowContext(ctx,
		personSweepOperationStatusQuery(columns, textCollation, condition)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil //nolint:nilnil // No matching person sweep run is a valid empty status.
	}
	if err != nil {
		return nil, err
	}
	return &run, nil
}

func (s *Store) bytewiseTextCollation() string {
	if s.dialect.DriverName() == postgresDriverName {
		return ` COLLATE "C"`
	}
	return ` COLLATE BINARY`
}

func scanPersonSweepOperationRun(sc scanner) (operations.Run, error) {
	var (
		runID          string
		durableTrigger string
		durableState   string
		attempted      int64
		succeeded      int64
		failed         int64
		projected      int64
		startedAt      requiredTimestamp
		completedAt    strictOperationNullableTimestamp
		failureClass   string
	)
	if err := sc.Scan(
		&runID, &durableTrigger, &durableState,
		&attempted, &succeeded, &failed, &projected,
		&startedAt, &completedAt, &failureClass,
	); err != nil {
		return operations.Run{}, err
	}

	id, err := operations.NewTextID(operations.KindPersonSweep, runID)
	if err != nil {
		return operations.Run{}, fmt.Errorf("person sweep operation stable ID: %w", err)
	}
	trigger := operations.Trigger(durableTrigger)
	if err := trigger.Validate(); err != nil {
		return operations.Run{}, fmt.Errorf("person sweep operation trigger: %w", err)
	}
	state := operations.State(durableState)
	if err := state.Validate(); err != nil {
		return operations.Run{}, fmt.Errorf("person sweep operation state: %w", err)
	}
	run := operations.Run{
		ID:        id,
		Lane:      operations.LanePersonFacts,
		State:     state,
		Trigger:   &trigger,
		StartedAt: startedAt.Time.UTC(),
		Counters: []operations.PublicCounter{
			{Name: operations.CounterAttempted, Unit: operations.CounterUnitPeople, Value: attempted},
			{Name: operations.CounterSucceeded, Unit: operations.CounterUnitPeople, Value: succeeded},
			{Name: operations.CounterFailed, Unit: operations.CounterUnitPeople, Value: failed},
			{Name: operations.CounterProjectedWrites, Unit: operations.CounterUnitWrites, Value: projected},
		},
	}
	if completedAt.Valid {
		finishedAt := completedAt.Time.UTC()
		run.FinishedAt = &finishedAt
	}
	if state == operations.StatePartial || state == operations.StateFailed {
		publicError := operations.ProjectPersonSweepFailure(peoplesweep.FailureClass(failureClass))
		run.Error = &publicError
	}
	if err := run.Validate(); err != nil {
		return operations.Run{}, fmt.Errorf("validate person sweep operation run: %w", err)
	}
	return run, nil
}

// listCardDAVOperationRuns projects CardDAV sync rows using only the durable
// columns allowed by the normalized operations contract.
func (s *Store) listCardDAVOperationRuns(
	ctx context.Context, query operations.Query, fetchLimit int,
) ([]operations.Run, error) {
	return s.listCardDAVOperationRunsFrom(ctx, s.db, query, fetchLimit)
}

func (s *Store) listCardDAVOperationRunsFrom(
	ctx context.Context, queryer contextRowsQuerier, query operations.Query, fetchLimit int,
) ([]operations.Run, error) {
	if err := query.Validate(); err != nil {
		return nil, fmt.Errorf("list operation runs: %w", err)
	}
	if fetchLimit < 1 || fetchLimit > query.Limit+1 {
		return nil, errors.New("CardDAV operation fetch limit must be between one and query limit plus one")
	}

	conditions := make([]string, 0, 4)
	args := make([]any, 0, 6)
	if len(query.States) > 0 {
		conditions = append(conditions, cardDAVOperationStateCondition(query.States))
	}
	if query.StartedFrom != nil {
		conditions = append(conditions, `started_at >= ?`)
		args = append(args, s.operationTimestampParam(*query.StartedFrom))
	}
	if query.StartedBefore != nil {
		conditions = append(conditions, `started_at < ?`)
		args = append(args, s.operationTimestampParam(*query.StartedBefore))
	}
	if query.Position != nil {
		condition, positionArgs, err := s.cardDAVOperationPositionCondition(*query.Position)
		if err != nil {
			return nil, fmt.Errorf("list operation runs: %w", err)
		}
		conditions = append(conditions, condition)
		args = append(args, positionArgs...)
	}

	statement := `SELECT ` + cardDAVOperationRunColumns + ` FROM carddav_sync_runs`
	if len(conditions) > 0 {
		statement += ` WHERE ` + strings.Join(conditions, ` AND `)
	}
	statement += ` ORDER BY started_at DESC, id DESC LIMIT ?`
	args = append(args, fetchLimit)

	rows, err := queryer.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("list CardDAV operation runs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	runs := make([]operations.Run, 0, fetchLimit)
	for rows.Next() {
		run, scanErr := scanCardDAVOperationRun(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan CardDAV operation run: %w", scanErr)
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate CardDAV operation runs: %w", err)
	}
	return runs, nil
}

func (s *Store) getCardDAVOperationRun(
	ctx context.Context, id operations.StableID,
) (operations.Run, error) {
	if err := id.Validate(); err != nil {
		return operations.Run{}, fmt.Errorf("get operation run: %w", err)
	}
	if id.Kind() != operations.KindCardDAVSync {
		return operations.Run{}, fmt.Errorf("get operation run: %w", ErrOperationRunNotFound)
	}
	runID, ok := id.Int64()
	if !ok {
		return operations.Run{}, fmt.Errorf("get operation run: %w", ErrOperationRunNotFound)
	}

	run, err := scanCardDAVOperationRun(s.db.QueryRowContext(ctx,
		`SELECT `+cardDAVOperationRunColumns+` FROM carddav_sync_runs WHERE id = ?`, runID))
	if errors.Is(err, sql.ErrNoRows) {
		return operations.Run{}, fmt.Errorf("get operation run: %w", ErrOperationRunNotFound)
	}
	if err != nil {
		return operations.Run{}, fmt.Errorf("get CardDAV operation run: %w", err)
	}
	return run, nil
}

func (s *Store) cardDAVOperationLaneStatus(
	ctx context.Context,
) (operations.LaneHistoryStatus, error) {
	status := operations.LaneHistoryStatus{
		Kind:                operations.KindCardDAVSync,
		Lane:                operations.LaneContacts,
		HistoryAvailability: operations.HistoryAvailable,
	}
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		var err error
		status.Active, err = cardDAVOperationStatusRun(ctx, tx, `state = 'running'`)
		if err != nil {
			return fmt.Errorf("read active CardDAV operation run: %w", err)
		}
		if s.operationHistoryStatusAfterActiveHook != nil {
			s.operationHistoryStatusAfterActiveHook(string(operations.KindCardDAVSync))
		}
		status.Latest, err = cardDAVOperationStatusRun(ctx, tx, `TRUE`)
		if err != nil {
			return fmt.Errorf("read latest CardDAV operation run: %w", err)
		}
		status.LatestSuccessful, err = cardDAVOperationStatusRun(ctx, tx, `state = 'succeeded'`)
		if err != nil {
			return fmt.Errorf("read latest successful CardDAV operation run: %w", err)
		}
		return nil
	})
	if err != nil {
		return operations.LaneHistoryStatus{}, fmt.Errorf("read CardDAV operation status: %w", err)
	}
	if err := status.Validate(); err != nil {
		return operations.LaneHistoryStatus{}, fmt.Errorf("validate CardDAV operation status: %w", err)
	}
	return status, nil
}

func cardDAVOperationStateCondition(states []operations.State) string {
	conditions := make([]string, 0, len(states))
	for _, state := range states {
		switch state {
		case operations.StateRunning:
			conditions = append(conditions, `state = 'running'`)
		case operations.StateSucceeded:
			conditions = append(conditions, `state = 'succeeded'`)
		case operations.StatePartial:
			conditions = append(conditions, `state = 'partial'`)
		case operations.StateFailed:
			conditions = append(conditions, `state = 'failed'`)
		case operations.StateCancelled:
			conditions = append(conditions, `state = 'cancelled'`)
		case operations.StateQueued:
			// CardDAV has no durable queued rows.
		}
	}
	if len(conditions) == 0 {
		return `FALSE`
	}
	return `(` + strings.Join(conditions, ` OR `) + `)`
}

func (s *Store) cardDAVOperationPositionCondition(
	position operations.Position,
) (string, []any, error) {
	timestamp := s.operationTimestampParam(position.StartedAt)
	// SQLite stores CardDAV run times at whole-second precision. When a cursor
	// falls later within that second, every row at the stored second is older
	// than the cursor regardless of kind or ID. Apply kind/ID tie-breaking only
	// when the cursor itself is exactly representable by the CardDAV ledger.
	if s.sqliteOperationPositionFinerThan(position.StartedAt, time.Second) {
		return `started_at <= ?`, []any{timestamp}, nil
	}
	switch cmp.Compare(operations.KindCardDAVSync, position.ID.Kind()) {
	case -1:
		return `started_at < ?`, []any{timestamp}, nil
	case 1:
		return `started_at <= ?`, []any{timestamp}, nil
	default:
		positionID, ok := position.ID.Int64()
		if !ok {
			return "", nil, errors.New("CardDAV operation position requires a numeric CardDAV run ID")
		}
		return `(started_at < ? OR (started_at = ? AND id < ?))`,
			[]any{timestamp, timestamp, positionID}, nil
	}
}

func (s *Store) sqliteOperationPositionFinerThan(
	value time.Time, precision time.Duration,
) bool {
	return !s.IsPostgreSQL() && value.Nanosecond()%int(precision) != 0
}

func cardDAVOperationStatusQuery(condition string) string {
	return `SELECT ` + cardDAVOperationRunColumns + ` FROM carddav_sync_runs WHERE ` + condition +
		` ORDER BY started_at DESC, id DESC LIMIT 1`
}

func cardDAVOperationStatusRun(
	ctx context.Context, queryer *loggedTx, condition string,
) (*operations.Run, error) {
	run, err := scanCardDAVOperationRun(queryer.QueryRowContext(ctx, cardDAVOperationStatusQuery(condition)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil //nolint:nilnil // No matching CardDAV run is a valid empty status.
	}
	if err != nil {
		return nil, err
	}
	return &run, nil
}

func scanCardDAVOperationRun(sc scanner) (operations.Run, error) {
	var (
		runID          int64
		durableTrigger string
		durableState   string
		startedAt      requiredTimestamp
		finishedAt     strictOperationNullableTimestamp
		books          int64
		created        int64
		updated        int64
		removed        int64
		errorCode      string
	)
	if err := sc.Scan(
		&runID, &durableTrigger, &durableState, &startedAt, &finishedAt,
		&books, &created, &updated, &removed, &errorCode,
	); err != nil {
		return operations.Run{}, err
	}

	id, err := operations.NewInt64ID(operations.KindCardDAVSync, runID)
	if err != nil {
		return operations.Run{}, fmt.Errorf("CardDAV operation stable ID: %w", err)
	}
	trigger := operations.Trigger(durableTrigger)
	if err := trigger.Validate(); err != nil {
		return operations.Run{}, err
	}
	state := operations.State(durableState)
	if err := state.Validate(); err != nil {
		return operations.Run{}, err
	}
	run := operations.Run{
		ID:        id,
		Lane:      operations.LaneContacts,
		State:     state,
		Trigger:   &trigger,
		StartedAt: startedAt.Time.UTC(),
		Counters: []operations.PublicCounter{
			{Name: operations.CounterBooks, Unit: operations.CounterUnitBooks, Value: books},
			{Name: operations.CounterCreated, Unit: operations.CounterUnitContacts, Value: created},
			{Name: operations.CounterUpdated, Unit: operations.CounterUnitContacts, Value: updated},
			{Name: operations.CounterRemoved, Unit: operations.CounterUnitContacts, Value: removed},
		},
	}
	if finishedAt.Valid {
		finished := finishedAt.Time.UTC()
		run.FinishedAt = &finished
	}
	if state == operations.StatePartial || state == operations.StateFailed || state == operations.StateCancelled {
		run.Error = operations.ProjectCardDAVFailure(errorCode)
	}
	if err := run.Validate(); err != nil {
		return operations.Run{}, fmt.Errorf("validate CardDAV operation run: %w", err)
	}
	return run, nil
}

// Kinds returns the closed set of operation histories backed by durable
// ledgers in this store. The slice is ordered and detached from package state.
func (s *Store) Kinds() []operations.Kind {
	return slices.Clone(durableOperationKinds)
}

// ListRuns reads every selected durable ledger inside one repeatable-read
// snapshot, then applies the normalized cross-kind comparator. It returns at
// most limit+1 rows so the HTTP layer can detect continuation without another
// count query.
func (s *Store) ListRuns(
	ctx context.Context, query operations.Query,
) (operations.HistorySnapshot, error) {
	return s.listOperationHistorySnapshot(ctx, query)
}

func selectedDurableOperationKinds(requested []operations.Kind) ([]operations.Kind, error) {
	if len(requested) == 0 {
		return slices.Clone(durableOperationKinds), nil
	}
	for _, kind := range requested {
		if !slices.Contains(durableOperationKinds, kind) {
			return nil, fmt.Errorf("operation kind %q: %w", kind, ErrOperationHistoryUnavailable)
		}
	}
	return slices.Clone(requested), nil
}

// GetRun dispatches exclusively through the stable ID's validated kind/type
// pair; equal numeric values in different ledgers cannot cross-select.
func (s *Store) GetRun(ctx context.Context, id operations.StableID) (operations.Run, error) {
	if err := id.Validate(); err != nil {
		return operations.Run{}, fmt.Errorf("get operation run: %w", err)
	}
	switch id.Kind() {
	case operations.KindCardDAVSync:
		return s.getCardDAVOperationRun(ctx, id)
	case operations.KindDocumentEmbedding, operations.KindDocumentExtraction,
		operations.KindMessageEmbedding, operations.KindPersonEmbedding,
		operations.KindVisualEmbedding:
		return s.getInvocationOperationRun(ctx, id)
	case operations.KindPersonEnrichment:
		return s.getPersonEnrichmentOperationRun(ctx, id)
	case operations.KindPersonSweep:
		return s.getPersonSweepOperationRun(ctx, id)
	case operations.KindSourceSync:
		return s.getSourceOperationRun(ctx, id)
	default:
		return operations.Run{}, fmt.Errorf("get operation run for %q: %w",
			id.Kind(), ErrOperationHistoryUnavailable)
	}
}

// LaneStatus reads only the requested durable ledger. Unavailable kinds are
// rejected explicitly; the API layer owns their stable unavailable metadata.
func (s *Store) LaneStatus(
	ctx context.Context, kind operations.Kind,
) (operations.LaneHistoryStatus, error) {
	if err := kind.Validate(); err != nil {
		return operations.LaneHistoryStatus{}, fmt.Errorf("read operation lane status: %w", err)
	}
	switch kind {
	case operations.KindCardDAVSync:
		return s.cardDAVOperationLaneStatus(ctx)
	case operations.KindDocumentEmbedding, operations.KindDocumentExtraction,
		operations.KindMessageEmbedding, operations.KindPersonEmbedding,
		operations.KindPersonEnrichment, operations.KindVisualEmbedding:
		return s.genericOperationLaneStatus(ctx, kind)
	case operations.KindPersonSweep:
		return s.personSweepOperationLaneStatus(ctx)
	case operations.KindSourceSync:
		return s.sourceOperationLaneStatus(ctx)
	default:
		return operations.LaneHistoryStatus{}, fmt.Errorf("read operation lane status for %q: %w",
			kind, ErrOperationHistoryUnavailable)
	}
}
