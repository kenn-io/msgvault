package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"go.kenn.io/msgvault/internal/persontiers"
)

const personTierFenceKey = "people_tiers_apply_fence"
const personTierClockKey = "people_tiers_run_clock"

type PersonTierRun struct {
	Actor             string             `json:"-"`
	RunID             string             `json:"run_id"`
	Kind              string             `json:"kind"`
	Mode              string             `json:"mode"`
	StartedAt         time.Time          `json:"started_at"`
	AsOf              time.Time          `json:"as_of"`
	Config            persontiers.Config `json:"config_json"`
	ConfigFingerprint string             `json:"config_fingerprint"`
	Timezone          string             `json:"projector_timezone"`
	MaxDirect         int                `json:"max_direct_counterparts"`
	Tier              int                `json:"tier"`
	ChangesOnly       bool               `json:"changes_only"`
	Cursor            string             `json:"cursor"`
}

func personTierRunKey(kind string) string {
	if kind == "manual" {
		return "people_tiers_manual_run"
	}
	return "people_tiers_run"
}

// PersonTierRunContext reads a frozen run without replacing a superseded run.
// Schedulers can finish that run before waiting for the next trigger.
func (s *Store) PersonTierRunContext(ctx context.Context, kind string) (*PersonTierRun, error) {
	if kind != "manual" && kind != "scheduled" {
		return nil, ErrInvalidPersonTierRequest
	}
	return s.readPersonTierRun(ctx, kind)
}

//nolint:nilnil // No persisted run is represented by a nil run and nil error.
func (s *Store) readPersonTierRun(ctx context.Context, kind string) (*PersonTierRun, error) {
	var value string
	err := s.db.QueryRowContext(ctx, s.Rebind(`SELECT value FROM archive_metadata WHERE key=?`), personTierRunKey(kind)).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	run := PersonTierRun{Config: persontiers.DefaultConfig()}
	if err = json.Unmarshal([]byte(value), &run); err != nil {
		return nil, fmt.Errorf("decode people tier run: %w", err)
	}
	if err = validatePersonTierRun(run); err != nil {
		return nil, err
	}
	return &run, nil
}
func validatePersonTierRun(run PersonTierRun) error {
	if run.RunID == "" || run.StartedAt.IsZero() || run.AsOf.IsZero() || (run.Kind != "manual" && run.Kind != "scheduled") || (run.Mode != "apply" && run.Mode != "record_only") || run.ConfigFingerprint != run.Config.Fingerprint() {
		return fmt.Errorf("%w: invalid frozen run", ErrInvalidPersonTierRequest)
	}
	cfg := run.Config
	defaults := persontiers.DefaultConfig()
	cfg.Schedule = defaults.Schedule
	cfg.PreviewRetentionDays = defaults.PreviewRetentionDays
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidPersonTierRequest, err)
	}
	_, err := time.LoadLocation(run.Timezone)
	if err != nil {
		return fmt.Errorf("%w: invalid frozen timezone", ErrInvalidPersonTierRequest)
	}
	if run.MaxDirect <= 0 {
		return ErrInvalidPersonTierRequest
	}
	if run.Tier < 0 || run.Tier > 3 {
		return ErrInvalidPersonTierRequest
	}
	return nil
}
func putPersonTierMetadataTx(ctx context.Context, tx *loggedTx, key string, value any) error {
	b, err := json.Marshal(value, json.Deterministic(true))
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO archive_metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, string(b))
	return err
}
func readPersonTierFenceTx(ctx context.Context, tx *loggedTx) (time.Time, error) {
	return readPersonTierTimestampTx(ctx, tx, personTierFenceKey)
}

func readPersonTierTimestampTx(ctx context.Context, tx *loggedTx, key string) (time.Time, error) {
	var value string
	err := tx.QueryRowContext(ctx, `SELECT value FROM archive_metadata WHERE key=?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	at, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return at, fmt.Errorf("decode tier timestamp %s: %w", key, err)
	}
	return at, nil
}

// allocatePersonTierTimestampTx remains monotonic across completed runs and
// wall-clock adjustments. Callers hold the authority mutation gate.
func (s *Store) allocatePersonTierTimestampTx(ctx context.Context, tx *loggedTx, now time.Time) (time.Time, error) {
	now = now.UTC().Truncate(time.Microsecond)
	for _, key := range []string{personTierFenceKey, personTierClockKey} {
		prior, err := readPersonTierTimestampTx(ctx, tx, key)
		if err != nil {
			return now, err
		}
		if !now.After(prior) {
			now = prior.Add(time.Microsecond)
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO archive_metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, personTierClockKey, now.Format(time.RFC3339Nano))
	return now, err
}

func (s *Store) advancePersonTierFenceTx(ctx context.Context, tx *loggedTx, now time.Time) (time.Time, error) {
	now, err := s.allocatePersonTierTimestampTx(ctx, tx, now)
	if err != nil {
		return now, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO archive_metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, personTierFenceKey, now.Format(time.RFC3339Nano))
	return now, err
}
func (s *Store) StartPersonTierRunContext(ctx context.Context, req PersonTierPageRequest, kind, mode string) (*PersonTierRun, error) {
	if req.AsOf.IsZero() {
		req.AsOf = s.personEnrichmentTime()
	}
	req, err := normalizePersonTierRequest(req)
	if err != nil {
		return nil, err
	}
	run := &PersonTierRun{RunID: uuid.NewString(), Kind: kind, Mode: mode, StartedAt: s.personEnrichmentTime().UTC().Truncate(time.Microsecond), AsOf: req.AsOf, Config: req.Config, ConfigFingerprint: req.Config.Fingerprint(), Timezone: req.Timezone, MaxDirect: req.MaxDirect, Tier: req.Tier, ChangesOnly: req.ChangesOnly}
	if err = validatePersonTierRun(*run); err != nil {
		return nil, err
	}
	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockPersonEnrichmentAuthorityMutationTx(ctx, tx); err != nil {
			return err
		}
		var err error
		if kind == "manual" && mode == "apply" {
			run.StartedAt, err = s.advancePersonTierFenceTx(ctx, tx, run.StartedAt)
		} else {
			run.StartedAt, err = s.allocatePersonTierTimestampTx(ctx, tx, run.StartedAt)
		}
		if err != nil {
			return err
		}
		return putPersonTierMetadataTx(ctx, tx, personTierRunKey(kind), run)
	})
	return run, err
}

// GetOrStartPersonTierRunContext resumes the semantic snapshot even if live
// configuration changed. Operational mode changes take effect on the next run.
func (s *Store) GetOrStartPersonTierRunContext(ctx context.Context, req PersonTierPageRequest, kind, mode string) (*PersonTierRun, error) {
	run, err := s.readPersonTierRun(ctx, kind)
	if err != nil {
		return nil, err
	}
	if run != nil {
		err = s.withTxContext(ctx, func(tx *loggedTx) error { return s.checkPersonTierRunTx(ctx, tx, *run) })
		if err == nil {
			return run, nil
		}
		if !errors.Is(err, ErrPersonTierRunSuperseded) {
			return nil, err
		}
		if err = s.clearPersonTierRunContext(ctx, *run); err != nil {
			return nil, err
		}
	}
	return s.StartPersonTierRunContext(ctx, req, kind, mode)
}
func (s *Store) checkPersonTierRunTx(ctx context.Context, tx *loggedTx, run PersonTierRun) error {
	if err := validatePersonTierRun(run); err != nil {
		return err
	}
	// Callers that also mutate authority take its gate first. Holding the
	// projector's identity lock through the transaction makes this check and
	// each decision write atomic with projection transitions and replay.
	if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
		return err
	}
	if err := s.checkPersonTierProjectionContext(ctx, tx, run.Timezone, run.MaxDirect); err != nil {
		if errors.Is(err, errPersonTierProjectionMismatch) {
			return ErrPersonTierRunSuperseded
		}
		return err
	}
	fence, err := readPersonTierFenceTx(ctx, tx)
	if err != nil {
		return err
	}
	if run.StartedAt.Before(fence) {
		return ErrPersonTierRunSuperseded
	}
	var value string
	if err = tx.QueryRowContext(ctx, `SELECT value FROM archive_metadata WHERE key=?`, personTierRunKey(run.Kind)).Scan(&value); errors.Is(err, sql.ErrNoRows) {
		return ErrPersonTierRunSuperseded
	} else if err != nil {
		return err
	}
	current := PersonTierRun{Config: persontiers.DefaultConfig()}
	if err = json.Unmarshal([]byte(value), &current); err != nil {
		return err
	}
	if err := validatePersonTierRun(current); err != nil {
		return err
	}
	if current.RunID != run.RunID || current.Kind != run.Kind || current.Mode != run.Mode ||
		!current.StartedAt.Equal(run.StartedAt) || !current.AsOf.Equal(run.AsOf) ||
		current.ConfigFingerprint != run.ConfigFingerprint || current.Timezone != run.Timezone || current.MaxDirect != run.MaxDirect ||
		current.Tier != run.Tier || current.ChangesOnly != run.ChangesOnly {
		return ErrPersonTierRunSuperseded
	}
	return nil
}
func (s *Store) clearPersonTierRunContext(ctx context.Context, run PersonTierRun) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockPersonEnrichmentAuthorityMutationTx(ctx, tx); err != nil {
			return err
		}
		var value string
		err := tx.QueryRowContext(ctx, `SELECT value FROM archive_metadata WHERE key=?`, personTierRunKey(run.Kind)).Scan(&value)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		var current PersonTierRun
		if err = json.Unmarshal([]byte(value), &current); err != nil {
			return err
		}
		if current.RunID != run.RunID {
			return nil
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM archive_metadata WHERE key=?`, personTierRunKey(run.Kind))
		return err
	})
}

type PersonTierApplyRequest struct {
	PersonTierPageRequest

	RunID  string `json:"run_id"`
	Actor  string `json:"-"`
	DryRun bool   `json:"dry_run"`
}

func (s *Store) ApplyPersonTiersPageContext(ctx context.Context, request PersonTierApplyRequest) (*PersonTierPage, error) {
	if request.DryRun {
		return s.PlanPersonTiersContext(ctx, request.PersonTierPageRequest)
	}
	if err := validatePersonTierActor(request.Actor); err != nil {
		return nil, err
	}
	limit := request.Limit
	if limit == 0 {
		limit = 500
	}
	if limit < 1 || limit > 500 {
		return nil, ErrInvalidPersonTierRequest
	}
	var run *PersonTierRun
	var err error
	if request.RunID == "" {
		if request.Cursor != "" {
			return nil, fmt.Errorf("%w: run_id required for continuation", ErrInvalidPersonTierRequest)
		}
		run, err = s.StartPersonTierRunContext(ctx, request.PersonTierPageRequest, "manual", "apply")
	} else {
		run, err = s.readPersonTierRun(ctx, "manual")
		if err == nil && (run == nil || run.RunID != request.RunID) {
			return nil, ErrPersonTierRunSuperseded
		}
		if err == nil && request.Cursor != run.Cursor {
			return nil, fmt.Errorf("%w: cursor does not match committed progress", ErrPersonTierRunSuperseded)
		}
	}
	if err != nil {
		return nil, err
	}
	return s.ProcessPersonTierRunPageContext(ctx, *run, limit, request.Actor)
}
func (s *Store) ProcessPersonTierRunPageContext(ctx context.Context, run PersonTierRun, limit int, actor string) (*PersonTierPage, error) {
	return s.ProcessPersonTierRunPageWithCheckpointContext(ctx, run, limit, actor, nil)
}

// ProcessPersonTierRunPageWithCheckpointContext reports each committed person
// checkpoint after its decision and run cursor have committed. Scheduled jobs
// use the callback to let the scheduler queue a continuation after a budgeted
// partial pass; manual callers keep their page-level response semantics.
func (s *Store) ProcessPersonTierRunPageWithCheckpointContext(ctx context.Context, run PersonTierRun, limit int, actor string, onCheckpoint func()) (*PersonTierPage, error) {
	if limit < 1 || limit > 500 {
		return nil, ErrInvalidPersonTierRequest
	}
	if err := validatePersonTierActor(actor); err != nil {
		return nil, err
	}
	err := s.withTxContext(ctx, func(tx *loggedTx) error { return s.checkPersonTierRunTx(ctx, tx, run) })
	if err != nil {
		return nil, s.personTierRunPageErrorContext(ctx, run, err)
	}
	page, err := s.PlanPersonTiersContext(ctx, PersonTierPageRequest{Config: run.Config, Timezone: run.Timezone, MaxDirect: run.MaxDirect, AsOf: run.AsOf, Limit: limit, Cursor: run.Cursor, Tier: run.Tier, ChangesOnly: run.ChangesOnly})
	if err != nil {
		return nil, s.personTierRunPageErrorContext(ctx, run, err)
	}
	run.Actor = actor
	page.RunID = run.RunID
	page.Counts = map[string]int{}
	for i, input := range page.People {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		checkpointCursor := ""
		if run.Kind == "scheduled" {
			checkpointCursor = encodePersonTierCursor(personTierCursor{
				AfterID: input.PersonID, AsOf: run.AsOf, Fingerprint: run.ConfigFingerprint,
				RunID: run.RunID, Timezone: run.Timezone, MaxDirect: run.MaxDirect,
			})
		}
		var applied PersonTierInput
		applied, err = s.applyPersonTierDecisionWithCheckpointContext(ctx, run, input, checkpointCursor)
		if errors.Is(err, ErrPersonTierRunSuperseded) {
			page.Counts["superseded"]++
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			page.Counts["failed"]++
			slog.WarnContext(ctx, "people tier evaluation failed", "person_id", input.PersonID, "error", err)
			if recordErr := s.recordPersonTierFailureWithCheckpointContext(ctx, run, input, actor, checkpointCursor); recordErr != nil {
				return nil, fmt.Errorf("record people tier failure: %w", recordErr)
			}
			if checkpointCursor != "" && onCheckpoint != nil {
				onCheckpoint()
			}
			continue
		}
		page.People[i] = applied
		countPersonTierDecision(page.Counts, applied.Decision)
		if checkpointCursor != "" && onCheckpoint != nil {
			onCheckpoint()
		}
	}
	page.Counts["evaluated"] = len(page.People)
	run.Cursor = page.NextCursor
	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockPersonEnrichmentAuthorityMutationTx(ctx, tx); err != nil {
			return err
		}
		if err := s.checkPersonTierRunTx(ctx, tx, run); err != nil {
			return err
		}
		if run.Cursor == "" {
			_, err := tx.ExecContext(ctx, `DELETE FROM archive_metadata WHERE key=?`, personTierRunKey(run.Kind))
			return err
		}
		return putPersonTierMetadataTx(ctx, tx, personTierRunKey(run.Kind), run)
	})
	return page, s.personTierRunPageErrorContext(ctx, run, err)
}

func (s *Store) personTierRunPageErrorContext(ctx context.Context, run PersonTierRun, err error) error {
	if errors.Is(err, errPersonTierProjectionMismatch) {
		err = ErrPersonTierRunSuperseded
	}
	if errors.Is(err, ErrPersonTierRunSuperseded) && run.Kind == "scheduled" {
		if cleanupErr := s.clearPersonTierRunContext(ctx, run); cleanupErr != nil {
			return fmt.Errorf("clear superseded people tier run: %w", cleanupErr)
		}
	}
	return err
}
