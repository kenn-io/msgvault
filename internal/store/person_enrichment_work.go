package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/personenrichment"
)

var (
	ErrStaleLease                = errors.New("person enrichment lease is stale")
	ErrActiveAttemptConflict     = errors.New("person enrichment work already has a different active attempt")
	ErrRequestBudgetExceeded     = personenrichment.ErrRequestBudgetExceeded
	ErrCostBudgetExceeded        = personenrichment.ErrCostBudgetExceeded
	ErrAccountingDisabled        = personenrichment.ErrAccountingDisabled
	ErrProviderCostBoundExceeded = errors.New("person enrichment provider cost exceeded guaranteed maximum")
	ErrRequestHashConflict       = errors.New("person enrichment request hash belongs to different work")
)

var _ personenrichment.WorkStore = (*Store)(nil)

type PersonEnrichmentWorkInput struct {
	PersonID           int64
	ProfileFingerprint string
	Trigger            personenrichment.Trigger
	DueAt              time.Time
}

type PersonEnrichmentWork struct {
	PersonID           int64      `json:"person_id"`
	ProfileFingerprint string     `json:"profile_fingerprint"`
	TriggerMask        int64      `json:"trigger_mask"`
	TriggerGeneration  string     `json:"trigger_generation"`
	DueAt              time.Time  `json:"due_at"`
	LeaseOwner         *string    `json:"lease_owner,omitempty"`
	LeaseFence         int64      `json:"lease_fence"`
	LeaseUntil         *time.Time `json:"lease_until,omitempty"`
	RunID              *int64     `json:"run_id,omitempty"`
	ActiveAttemptID    *int64     `json:"active_attempt_id,omitempty"`
	HasFreshTrigger    bool       `json:"has_fresh_trigger"`
}

type PersonEnrichmentWorkFilter struct {
	PersonID           int64
	ProfileFingerprint string
	DueBefore          *time.Time
	Limit              int
}

type PersonEnrichmentAttempt struct {
	ID                    int64      `json:"id"`
	RunID                 int64      `json:"run_id"`
	PersonID              int64      `json:"person_id"`
	ProfileFingerprint    string     `json:"profile_fingerprint"`
	TriggerKind           string     `json:"trigger_kind"`
	TriggerGeneration     string     `json:"trigger_generation"`
	PersonRevision        int64      `json:"person_revision"`
	PayloadHash           string     `json:"payload_hash"`
	RequestHash           string     `json:"request_hash"`
	State                 string     `json:"state"`
	ProviderRequestID     *string    `json:"provider_request_id,omitempty"`
	ProviderJobID         *string    `json:"provider_job_id,omitempty"`
	AdapterVersion        *string    `json:"adapter_version,omitempty"`
	SchemaVersion         *string    `json:"schema_version,omitempty"`
	GeneratedSchema       bool       `json:"generated_schema"`
	GeneratedSchemaHash   *string    `json:"generated_schema_hash,omitempty"`
	ProgramFingerprint    *string    `json:"program_fingerprint,omitempty"`
	FactGenerationKey     *string    `json:"fact_generation_key,omitempty"`
	LeaseOwner            *string    `json:"lease_owner,omitempty"`
	LeaseFence            int64      `json:"lease_fence"`
	LeaseUntil            *time.Time `json:"lease_until,omitempty"`
	NextActionAt          *time.Time `json:"next_action_at,omitempty"`
	AttemptCount          int64      `json:"attempt_count"`
	HardCostCapEnforced   bool       `json:"hard_cost_cap_enforced"`
	ReservedCostUSDMicros int64      `json:"reserved_cost_usd_micros"`
	ActualCostUSDMicros   *int64     `json:"actual_cost_usd_micros,omitempty"`
	FailureClass          *string    `json:"failure_class,omitempty"`
	CreatedAt             time.Time  `json:"created_at"`
	ProviderStartedAt     *time.Time `json:"provider_started_at,omitempty"`
	CompletedAt           *time.Time `json:"completed_at,omitempty"`
}

type PersonEnrichmentAttemptFilter struct {
	PersonID           int64
	ProfileFingerprint string
	State              string
	RunID              int64
	BeforeID           int64
	Limit              int
}

type PersonEnrichmentRunCounters struct {
	RequestsStarted       int64
	CostReservedUSDMicros int64
	CostChargedUSDMicros  int64
}

type personEnrichmentAttemptCompletion struct {
	State             string
	ActualCost        personenrichment.Cost
	ActualCostMissing bool
	CostReconciled    bool
	FactGenerationKey string
	CompletedAt       time.Time
	RefreshAt         *time.Time
	RefreshGeneration string
}

type personEnrichmentBudgetPolicy struct {
	MaxRequestsPerRun               int64 `json:"max_requests_per_run"`
	MaxRequestsPerDay               int64 `json:"max_requests_per_day"`
	MaxCostUSDMicrosPerPersonPerDay int64 `json:"max_cost_usd_micros_per_person_per_day"`
	MaxCostUSDMicrosPerRun          int64 `json:"max_cost_usd_micros_per_run"`
	MaxCostUSDMicrosPerDay          int64 `json:"max_cost_usd_micros_per_day"`
}

// personEnrichmentWorkConflictClause keeps the generation attached to the
// trigger kind selected by triggerFromMaskAndGeneration. Each publisher writes
// one trigger bit, so a generation replaces the stored value only when that
// incoming kind has at least the stored kind's priority.
const personEnrichmentWorkConflictClause = `
	ON CONFLICT (person_id, profile_fingerprint) DO UPDATE SET
		trigger_mask = CASE
			WHEN person_enrichment_work.lease_owner IS NOT NULL
			 AND NOT person_enrichment_work.has_fresh_trigger
			 AND (person_enrichment_work.trigger_mask <> excluded.trigger_mask
			      OR person_enrichment_work.trigger_generation <> excluded.trigger_generation)
			THEN excluded.trigger_mask
			ELSE person_enrichment_work.trigger_mask | excluded.trigger_mask
		END,
		trigger_generation = CASE
			WHEN person_enrichment_work.lease_owner IS NOT NULL
			 AND NOT person_enrichment_work.has_fresh_trigger
			 AND (person_enrichment_work.trigger_mask <> excluded.trigger_mask
			      OR person_enrichment_work.trigger_generation <> excluded.trigger_generation)
			THEN excluded.trigger_generation
			WHEN excluded.trigger_mask = 16 THEN excluded.trigger_generation
			WHEN excluded.trigger_mask = 2
			 AND (person_enrichment_work.trigger_mask & 16) = 0 THEN excluded.trigger_generation
			WHEN excluded.trigger_mask = 4
			 AND (person_enrichment_work.trigger_mask & 18) = 0 THEN excluded.trigger_generation
			WHEN excluded.trigger_mask = 8
			 AND (person_enrichment_work.trigger_mask & 22) = 0 THEN excluded.trigger_generation
			WHEN excluded.trigger_mask = 1
			 AND (person_enrichment_work.trigger_mask & 30) = 0 THEN excluded.trigger_generation
			ELSE person_enrichment_work.trigger_generation
		END,
		has_fresh_trigger = CASE
			WHEN (person_enrichment_work.lease_owner IS NOT NULL
			      OR person_enrichment_work.active_attempt_id IS NOT NULL)
			 AND (person_enrichment_work.has_fresh_trigger
			      OR person_enrichment_work.trigger_mask <> excluded.trigger_mask
			      OR person_enrichment_work.trigger_generation <> excluded.trigger_generation)
			THEN TRUE
			ELSE person_enrichment_work.has_fresh_trigger
		END,
		due_at = CASE WHEN excluded.due_at < person_enrichment_work.due_at
		              THEN excluded.due_at ELSE person_enrichment_work.due_at END
	WHERE NOT EXISTS (
		SELECT 1 FROM person_enrichment_attempts consumed
		WHERE consumed.id = person_enrichment_work.active_attempt_id
		  AND consumed.trigger_generation = excluded.trigger_generation
		  AND consumed.trigger_kind = CASE excluded.trigger_mask
			WHEN 1 THEN 'tracked'
			WHEN 2 THEN 'identity'
			WHEN 4 THEN 'claim_expiry'
			WHEN 8 THEN 'refresh'
			WHEN 16 THEN 'manual'
		  END
	)`

func (s *Store) personEnrichmentTime() time.Time {
	if s.personEnrichmentClock != nil {
		return s.personEnrichmentClock().UTC()
	}
	return time.Now().UTC()
}

func (s *Store) PutPersonEnrichmentWorkContext(
	ctx context.Context, input PersonEnrichmentWorkInput,
) error {
	mask, err := personEnrichmentTriggerMask(input.Trigger.Kind)
	if err != nil {
		return err
	}
	if input.PersonID <= 0 || !validLowerSHA256(input.ProfileFingerprint) ||
		strings.TrimSpace(input.Trigger.Generation) == "" || input.DueAt.IsZero() {
		return errors.New("person enrichment work input is invalid")
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO person_enrichment_work
			(person_id, profile_fingerprint, trigger_mask, trigger_generation, due_at)
		VALUES (?, ?, ?, ?, ?)`+personEnrichmentWorkConflictClause,
		input.PersonID, input.ProfileFingerprint, mask,
		strings.TrimSpace(input.Trigger.Generation), input.DueAt.UTC())
	if err != nil {
		return fmt.Errorf("put person enrichment work: %w", err)
	}
	return nil
}

// CancelPersonEnrichmentWorkOutsideProfilesContext terminalizes active
// attempts and removes work whose immutable profile is no longer configured.
func (s *Store) CancelPersonEnrichmentWorkOutsideProfilesContext(
	ctx context.Context, activeFingerprints []string,
) error {
	seen := make(map[string]struct{}, len(activeFingerprints))
	arguments := make([]any, 0, len(activeFingerprints))
	for _, fingerprint := range activeFingerprints {
		if !validLowerSHA256(fingerprint) {
			return errors.New("active person enrichment profile fingerprint is invalid")
		}
		if _, duplicate := seen[fingerprint]; duplicate {
			continue
		}
		seen[fingerprint] = struct{}{}
		arguments = append(arguments, fingerprint)
	}
	query := `SELECT person_id, profile_fingerprint, active_attempt_id
		FROM person_enrichment_work`
	if len(arguments) > 0 {
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(arguments)), ",")
		query += ` WHERE profile_fingerprint NOT IN (` + placeholders + `)`
	}
	type staleWork struct {
		personID      int64
		fingerprint   string
		activeAttempt sql.NullInt64
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		rows, err := tx.QueryContext(ctx, query+s.dialect.SelectForUpdate(), arguments...)
		if err != nil {
			return fmt.Errorf("list unavailable person enrichment work: %w", err)
		}
		stale := make([]staleWork, 0)
		for rows.Next() {
			var item staleWork
			if err := rows.Scan(&item.personID, &item.fingerprint, &item.activeAttempt); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan unavailable person enrichment work: %w", err)
			}
			stale = append(stale, item)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("iterate unavailable person enrichment work: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close unavailable person enrichment work: %w", err)
		}

		completedAt := s.personEnrichmentTime()
		for _, item := range stale {
			if item.activeAttempt.Valid {
				if _, err := reconcilePersonEnrichmentCostTx(ctx, tx, s.dialect,
					item.activeAttempt.Int64, personenrichment.Cost{}, true, completedAt); err != nil {
					return err
				}
				result, err := tx.ExecContext(ctx, `UPDATE person_enrichment_attempts
					SET state = 'terminal', failure_class = ?, completed_at = ?, next_action_at = NULL,
					    lease_owner = NULL, lease_until = NULL
					WHERE id = ? AND state IN ('queued','starting','pending','retry_wait','uncertain_start')`,
					personenrichment.FailurePolicy, completedAt, item.activeAttempt.Int64)
				if err != nil {
					return fmt.Errorf("terminalize unavailable person enrichment attempt: %w", err)
				}
				if err := requireOneLeaseRow(result); err != nil {
					return err
				}
			}
			result, err := tx.ExecContext(ctx, `DELETE FROM person_enrichment_work
				WHERE person_id = ? AND profile_fingerprint = ?`, item.personID, item.fingerprint)
			if err != nil {
				return fmt.Errorf("delete unavailable person enrichment work: %w", err)
			}
			if err := requireOneLeaseRow(result); err != nil {
				return err
			}
		}
		return nil
	})
}

func personEnrichmentTriggerMask(kind personenrichment.TriggerKind) (int64, error) {
	switch kind {
	case personenrichment.TriggerTracked:
		return 1, nil
	case personenrichment.TriggerIdentity:
		return 2, nil
	case personenrichment.TriggerExpiry:
		return 4, nil
	case personenrichment.TriggerRefresh:
		return 8, nil
	case personenrichment.TriggerManual:
		return 16, nil
	default:
		return 0, fmt.Errorf("invalid person enrichment trigger kind %q", kind)
	}
}

func (s *Store) ClaimWork(
	ctx context.Context, options personenrichment.ClaimOptions,
) (*personenrichment.WorkLease, error) {
	options.Owner = strings.TrimSpace(options.Owner)
	options.ProviderName = strings.TrimSpace(options.ProviderName)
	if options.RunID <= 0 || options.Owner == "" || options.ProviderName == "" ||
		options.Now.IsZero() || options.LeaseDuration <= 0 {
		return nil, errors.New("person enrichment claim options are invalid")
	}
	options.Now = options.Now.UTC()
	leaseUntil := options.Now.Add(options.LeaseDuration)
	var lease *personenrichment.WorkLease
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		var runState string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM person_enrichment_runs WHERE id = ?`+
			s.dialect.SelectForUpdate(), options.RunID).Scan(&runState); err != nil {
			return fmt.Errorf("lock person enrichment run for claim: %w", err)
		}
		if s.personEnrichmentRunBarrier != nil {
			s.personEnrichmentRunBarrier("claim_run_locked")
		}
		if runState != "running" {
			return errors.New("person enrichment claim requires a running run")
		}
		lock := ""
		if s.IsPostgreSQL() {
			lock = " FOR UPDATE OF w SKIP LOCKED"
		}
		query := `WITH candidate AS (
			SELECT w.person_id, w.profile_fingerprint
			FROM person_enrichment_work w
			JOIN person_enrichment_profiles p ON p.fingerprint = w.profile_fingerprint
			WHERE p.provider_name = ?
			  AND (w.run_id = ? OR w.run_id IS NULL)
			  AND w.due_at <= ?
			  AND (w.lease_owner IS NULL OR w.lease_until <= ?)
			ORDER BY CASE WHEN w.run_id = ? THEN 0 ELSE 1 END, w.due_at, w.person_id
			LIMIT 1` + lock + `
		)
		UPDATE person_enrichment_work
		SET run_id = COALESCE(run_id, ?), lease_owner = ?,
		    lease_fence = lease_fence + 1, lease_until = ?
		WHERE EXISTS (
			SELECT 1 FROM candidate c
			WHERE c.person_id = person_enrichment_work.person_id
			  AND c.profile_fingerprint = person_enrichment_work.profile_fingerprint)
		RETURNING person_id, profile_fingerprint, trigger_mask, trigger_generation,
		          run_id, active_attempt_id, lease_fence, lease_until`
		var (
			personID, triggerMask, runID, fence int64
			fingerprint, generation             string
			activeID                            sql.NullInt64
			until                               nullableTimestamp
		)
		err := tx.QueryRowContext(ctx, query,
			options.ProviderName, options.RunID, options.Now, options.Now, options.RunID,
			options.RunID, options.Owner, leaseUntil).Scan(
			&personID, &fingerprint, &triggerMask, &generation, &runID, &activeID, &fence, &until)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("claim person enrichment work: %w", err)
		}
		if !until.Valid {
			return errors.New("claimed person enrichment work has invalid lease_until")
		}
		trigger, err := triggerFromMaskAndGeneration(triggerMask, generation)
		if err != nil {
			return err
		}
		token := personenrichment.LeaseToken{
			RunID: runID, WorkPersonID: personID, ProfileFingerprint: fingerprint,
			Owner: options.Owner, Fence: fence,
		}
		var active *personenrichment.DurableAttempt
		if activeID.Valid {
			token.AttemptID = activeID.Int64
			if _, err := tx.ExecContext(ctx, `
				UPDATE person_enrichment_attempts
				SET state = CASE
				        WHEN state = 'starting' THEN 'uncertain_start'
				        WHEN state = 'retry_wait' AND provider_job_id IS NOT NULL THEN 'pending'
				        ELSE state
				    END,
				    next_action_at = CASE
				        WHEN state = 'retry_wait' AND provider_job_id IS NOT NULL THEN NULL
				        ELSE next_action_at
				    END,
				    lease_owner = ?, lease_fence = ?, lease_until = ?
				WHERE id = ? AND run_id = ?`, options.Owner, fence, leaseUntil,
				activeID.Int64, runID); err != nil {
				return fmt.Errorf("reclaim person enrichment active attempt: %w", err)
			}
			active, err = loadDurableAttemptTx(ctx, tx, token)
			if err != nil {
				return err
			}
		}
		lease = &personenrichment.WorkLease{
			Token: token, ProviderName: options.ProviderName, ProfileFingerprint: fingerprint,
			PersonID: personID, RunID: runID, ActiveAttempt: active, Trigger: trigger,
			LeaseUntil: until.Time,
		}
		return nil
	})
	return lease, err
}

func triggerFromMaskAndGeneration(mask int64, generation string) (personenrichment.Trigger, error) {
	// A coalesced row carries several trigger bits but one latest generation.
	// Prefer the most specific latest cause for request hashing.
	var kind personenrichment.TriggerKind
	switch {
	case mask&16 != 0:
		kind = personenrichment.TriggerManual
	case mask&2 != 0:
		kind = personenrichment.TriggerIdentity
	case mask&4 != 0:
		kind = personenrichment.TriggerExpiry
	case mask&8 != 0:
		kind = personenrichment.TriggerRefresh
	case mask&1 != 0:
		kind = personenrichment.TriggerTracked
	default:
		return personenrichment.Trigger{}, errors.New("person enrichment work has invalid trigger mask")
	}
	return personenrichment.Trigger{Kind: kind, Generation: generation}, nil
}

func (s *Store) RenewLease(
	ctx context.Context, token personenrichment.LeaseToken, until time.Time,
) error {
	if until.IsZero() || !until.After(s.personEnrichmentTime()) {
		return errors.New("person enrichment lease renewal must be in the future")
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := updateEnrichmentWorkLeaseTx(ctx, tx, token, `lease_until = ?`, until.UTC()); err != nil {
			return err
		}
		if token.AttemptID > 0 {
			result, err := tx.ExecContext(ctx, `UPDATE person_enrichment_attempts
				SET lease_until = ? WHERE id = ? AND run_id = ? AND lease_owner = ? AND lease_fence = ?`,
				until.UTC(), token.AttemptID, token.RunID, token.Owner, token.Fence)
			if err != nil {
				return fmt.Errorf("renew person enrichment attempt lease: %w", err)
			}
			if err := requireOneLeaseRow(result); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) ReleaseWork(
	ctx context.Context, token personenrichment.LeaseToken, release personenrichment.WorkRelease,
) error {
	if release.Outcome != "policy" && release.Outcome != "suppressed" && release.Outcome != "defer" &&
		release.Outcome != "retry" && release.Outcome != "complete" {
		return fmt.Errorf("invalid person enrichment work release outcome %q", release.Outcome)
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := verifyEnrichmentLeaseTx(ctx, tx, s.dialect, token); err != nil {
			return err
		}
		switch release.Outcome {
		case "defer":
			if token.AttemptID != 0 || release.NextActionAt == nil ||
				!release.NextActionAt.After(s.personEnrichmentTime()) {
				return errors.New("person enrichment defer release requires no attempt and a future next action")
			}
			if release.Failure != nil {
				if err := validateSafeFailure(*release.Failure); err != nil {
					return err
				}
			}
			return updateEnrichmentWorkLeaseTx(ctx, tx, token,
				`due_at = ?, run_id = NULL, lease_owner = NULL, lease_until = NULL`,
				release.NextActionAt.UTC())
		case "retry":
			if release.NextActionAt == nil || !release.NextActionAt.After(s.personEnrichmentTime()) {
				return errors.New("person enrichment retry release requires a future next action")
			}
			if token.AttemptID != 0 {
				if release.Failure == nil {
					return errors.New("active person enrichment retry release requires a safe failure")
				}
				if err := validateSafeFailure(*release.Failure); err != nil {
					return err
				}
				result, err := tx.ExecContext(ctx, `UPDATE person_enrichment_attempts
					SET state = 'retry_wait', next_action_at = ?, failure_class = ?,
					    lease_owner = NULL, lease_until = NULL
					WHERE id = ? AND run_id = ? AND lease_owner = ? AND lease_fence = ?`,
					release.NextActionAt.UTC(), release.Failure.Class, token.AttemptID,
					token.RunID, token.Owner, token.Fence)
				if err != nil {
					return fmt.Errorf("record active person enrichment retry release: %w", err)
				}
				if err := requireOneLeaseRow(result); err != nil {
					return err
				}
			}
			return updateEnrichmentWorkLeaseTx(ctx, tx, token,
				`due_at = ?, lease_owner = NULL, lease_until = NULL`, release.NextActionAt.UTC())
		case "complete":
			if token.AttemptID != 0 {
				return errors.New("active person enrichment attempts complete only through the claim sink")
			}
		case "policy", "suppressed":
			hasHashes := validLowerSHA256(release.PayloadHash) && validLowerSHA256(release.RequestHash)
			hasNoHashes := release.PayloadHash == "" && release.RequestHash == ""
			if token.AttemptID != 0 || release.PersonRevision < 0 || (!hasHashes && !hasNoHashes) {
				return errors.New("terminal work release requires revision and request hashes before an attempt")
			}
			state := "terminal"
			failureClass := string(personenrichment.FailurePolicy)
			if release.Outcome == "suppressed" {
				state = "suppressed"
				failureClass = string(personenrichment.FailureSuppressed)
			}
			var triggerMask int64
			var triggerGeneration string
			if err := tx.QueryRowContext(ctx, `SELECT trigger_mask, trigger_generation
				FROM person_enrichment_work WHERE person_id = ? AND profile_fingerprint = ?`,
				token.WorkPersonID, token.ProfileFingerprint).Scan(
				&triggerMask, &triggerGeneration); err != nil {
				return fmt.Errorf("load terminal work trigger: %w", err)
			}
			trigger, err := triggerFromMaskAndGeneration(triggerMask, triggerGeneration)
			if err != nil {
				return err
			}
			if hasHashes {
				_, err = tx.ExecContext(ctx, `
					INSERT INTO person_enrichment_attempts
						(run_id, person_id, profile_fingerprint, trigger_kind, trigger_generation,
						 person_revision, payload_hash, request_hash, state, lease_owner,
						 lease_fence, hard_cost_cap_enforced, reserved_cost_usd_micros,
						 failure_class, completed_at)
					VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?)
					ON CONFLICT (request_hash) DO NOTHING`, token.RunID, token.WorkPersonID,
					token.ProfileFingerprint, trigger.Kind, trigger.Generation, release.PersonRevision,
					release.PayloadHash, release.RequestHash, state, token.Owner, token.Fence,
					false, failureClass, s.personEnrichmentTime())
				if err != nil {
					return fmt.Errorf("record person enrichment terminal release: %w", err)
				}
			}
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM person_enrichment_work
			WHERE person_id = ? AND profile_fingerprint = ? AND run_id = ?
			  AND lease_owner = ? AND lease_fence = ?
			  AND ((? = 0 AND active_attempt_id IS NULL) OR active_attempt_id = ?)`,
			token.WorkPersonID, token.ProfileFingerprint, token.RunID, token.Owner,
			token.Fence, token.AttemptID, token.AttemptID)
		if err != nil {
			return fmt.Errorf("release person enrichment work: %w", err)
		}
		return requireOneLeaseRow(result)
	})
}

func (s *Store) BeginAttempt(
	ctx context.Context, token personenrichment.LeaseToken, start personenrichment.AttemptStart,
) (*personenrichment.DurableAttempt, bool, error) {
	if err := validateAttemptStart(token, start); err != nil {
		return nil, false, err
	}
	var attempt *personenrichment.DurableAttempt
	var created bool
	var stalePublication bool
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if s.personEnrichmentTxBarrier != nil {
			s.personEnrichmentTxBarrier("begin_before_person_lock")
		}
		currentRevision, err := lockPersonEnrichmentPersonTx(ctx, tx, s.dialect, start.PersonID)
		if err != nil {
			return err
		}
		if s.personEnrichmentTxBarrier != nil {
			s.personEnrichmentTxBarrier("begin_person_locked")
		}
		work, err := lockEnrichmentWorkStateTx(ctx, tx, s.dialect, token)
		if err != nil {
			return err
		}
		if work.ActiveID.Valid {
			activeToken := token
			activeToken.AttemptID = work.ActiveID.Int64
			existing, err := loadDurableAttemptTx(ctx, tx, activeToken)
			if err != nil {
				return err
			}
			if existing.RunID != start.RunID || existing.Token.WorkPersonID != start.PersonID ||
				existing.Token.ProfileFingerprint != start.ProfileFingerprint {
				return ErrActiveAttemptConflict
			}
			if work.HasFreshTrigger || currentRevision != start.PersonRevision ||
				currentRevision != existing.PersonRevision {
				if !work.HasFreshTrigger {
					if err := s.putPersonEnrichmentWorkTx(ctx, tx, EnrichmentTriggerInput{
						PersonID: start.PersonID, ProfileFingerprint: start.ProfileFingerprint,
						Kind:       personenrichment.TriggerIdentity,
						Generation: "revision:" + strconv.FormatInt(currentRevision, 10),
						DueAt:      s.personEnrichmentTime(),
					}); err != nil {
						return err
					}
				}
				if _, err := reconcilePersonEnrichmentCostTx(
					ctx, tx, s.dialect, existing.ID,
					personenrichment.Cost{Currency: "USD"}, false, s.personEnrichmentTime(),
				); err != nil {
					return err
				}
				result, err := tx.ExecContext(ctx, `UPDATE person_enrichment_attempts
					SET state = 'terminal', completed_at = ?, failure_class = ?,
					    next_action_at = NULL, lease_owner = NULL, lease_until = NULL
					WHERE id = ? AND run_id = ? AND lease_owner = ? AND lease_fence = ?`,
					s.personEnrichmentTime(), personenrichment.FailurePolicy, existing.ID,
					token.RunID, token.Owner, token.Fence)
				if err != nil {
					return fmt.Errorf("terminalize stale person enrichment attempt: %w", err)
				}
				if err := requireOneLeaseRow(result); err != nil {
					return err
				}
				activeToken.AttemptID = existing.ID
				if err := settleTerminalEnrichmentWorkTx(ctx, tx, activeToken, nil); err != nil {
					return err
				}
				stalePublication = true
				return nil
			}
			var requestHash string
			if err := tx.QueryRowContext(ctx, `SELECT request_hash FROM person_enrichment_attempts WHERE id = ?`,
				existing.ID).Scan(&requestHash); err != nil {
				return fmt.Errorf("read active person enrichment request hash: %w", err)
			}
			if requestHash != start.RequestHash {
				return ErrActiveAttemptConflict
			}
			if err := s.recheckPersonEnrichmentSuppressionsTx(
				ctx, tx, start.CheckedIdentifiers); err != nil {
				return err
			}
			if err := bindPersonEnrichmentAttemptIdentifiersTx(
				ctx, tx, existing.ID, start.CheckedIdentifiers); err != nil {
				return err
			}
			if existing.State == "retry_wait" && existing.JobID == "" && existing.RequestID == "" {
				result, updateErr := tx.ExecContext(ctx, `UPDATE person_enrichment_attempts
					SET state = 'starting', next_action_at = NULL, failure_class = NULL
					WHERE id = ? AND run_id = ? AND lease_owner = ? AND lease_fence = ?
					  AND state = 'retry_wait' AND provider_request_id IS NULL AND provider_job_id IS NULL`,
					existing.ID, token.RunID, token.Owner, token.Fence)
				if updateErr != nil {
					return fmt.Errorf("restart retryable person enrichment start: %w", updateErr)
				}
				if err := requireOneLeaseRow(result); err != nil {
					return err
				}
				existing.State = "starting"
			}
			attempt = existing
			return nil
		}
		if work.HasFreshTrigger || currentRevision != start.PersonRevision {
			result, err := tx.ExecContext(ctx, `UPDATE person_enrichment_work
				SET run_id = NULL, lease_owner = NULL, lease_until = NULL,
				    active_attempt_id = NULL, has_fresh_trigger = FALSE
				WHERE person_id = ? AND profile_fingerprint = ? AND run_id = ?
				  AND lease_owner = ? AND lease_fence = ? AND active_attempt_id IS NULL`,
				token.WorkPersonID, token.ProfileFingerprint, token.RunID, token.Owner, token.Fence)
			if err != nil {
				return fmt.Errorf("detach stale person enrichment publication: %w", err)
			}
			if err := requireOneLeaseRow(result); err != nil {
				return err
			}
			stalePublication = true
			return nil
		}

		var existingID, existingRunID, existingPersonID int64
		var existingFingerprint string
		err = tx.QueryRowContext(ctx, `SELECT id, run_id, person_id, profile_fingerprint
			FROM person_enrichment_attempts WHERE request_hash = ?`, start.RequestHash).Scan(
			&existingID, &existingRunID, &existingPersonID, &existingFingerprint)
		if err == nil {
			if existingRunID != start.RunID || existingPersonID != start.PersonID ||
				existingFingerprint != start.ProfileFingerprint {
				return ErrRequestHashConflict
			}
			existingToken := token
			existingToken.AttemptID = existingID
			attempt, err = loadDurableAttemptTx(ctx, tx, existingToken)
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check person enrichment request replay: %w", err)
		}
		if err := s.recheckPersonEnrichmentSuppressionsTx(
			ctx, tx, start.CheckedIdentifiers); err != nil {
			return err
		}
		if s.personEnrichmentTxBarrier != nil {
			s.personEnrichmentTxBarrier("begin_suppressions_rechecked")
		}

		policy, err := loadPersonEnrichmentBudgetPolicyTx(ctx, tx, start.ProfileFingerprint)
		if err != nil {
			return err
		}
		var startsDisabled bool
		if err := tx.QueryRowContext(ctx, `
			SELECT COALESCE((SELECT starts_disabled FROM person_enrichment_profile_accounting
			                 WHERE profile_fingerprint = ?), FALSE)`,
			start.ProfileFingerprint).Scan(&startsDisabled); err != nil {
			return fmt.Errorf("check person enrichment accounting state: %w", err)
		}
		if startsDisabled {
			return ErrAccountingDisabled
		}
		hardCostConfigured := policy.MaxCostUSDMicrosPerPersonPerDay > 0 ||
			policy.MaxCostUSDMicrosPerRun > 0 || policy.MaxCostUSDMicrosPerDay > 0
		if hardCostConfigured != start.HardCostCap {
			return errors.New("person enrichment hard-cost enforcement does not match the immutable profile")
		}

		if err := s.reservePersonEnrichmentBudgetTx(ctx, tx, policy, start); err != nil {
			return err
		}
		bound := start.GuaranteedMaxCost.AmountMicros
		startMask, err := personEnrichmentTriggerMask(start.Trigger.Kind)
		if err != nil {
			return err
		}

		var id int64
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO person_enrichment_attempts
				(run_id, person_id, profile_fingerprint, trigger_kind, trigger_generation,
				 person_revision, payload_hash, request_hash, state, lease_owner,
				 lease_fence, lease_until, attempt_count, hard_cost_cap_enforced,
				 reserved_cost_usd_micros, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'starting', ?, ?,
			        (SELECT lease_until FROM person_enrichment_work
			         WHERE person_id = ? AND profile_fingerprint = ?), 1, ?, ?, ?)
			RETURNING id`, start.RunID, start.PersonID, start.ProfileFingerprint,
			start.Trigger.Kind, start.Trigger.Generation, start.PersonRevision,
			start.PayloadHash, start.RequestHash, token.Owner, token.Fence,
			start.PersonID, start.ProfileFingerprint, start.HardCostCap, bound,
			s.personEnrichmentTime()).Scan(&id); err != nil {
			return fmt.Errorf("insert person enrichment attempt: %w", err)
		}
		if err := bindPersonEnrichmentAttemptIdentifiersTx(
			ctx, tx, id, start.CheckedIdentifiers); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE person_enrichment_work
			SET active_attempt_id = ?, trigger_mask = ?, trigger_generation = ?,
			    has_fresh_trigger = FALSE
			WHERE person_id = ? AND profile_fingerprint = ?
			  AND run_id = ? AND lease_owner = ? AND lease_fence = ? AND active_attempt_id IS NULL`,
			id, startMask,
			strings.TrimSpace(start.Trigger.Generation), token.WorkPersonID,
			token.ProfileFingerprint, token.RunID, token.Owner, token.Fence)
		if err != nil {
			return fmt.Errorf("bind person enrichment active attempt: %w", err)
		}
		if err := requireOneLeaseRow(result); err != nil {
			return err
		}
		created = true
		attemptToken := token
		attemptToken.AttemptID = id
		attempt, err = loadDurableAttemptTx(ctx, tx, attemptToken)
		return err
	})
	if err == nil && stalePublication {
		return nil, false, ErrStaleLease
	}
	return attempt, created, err
}

func validateAttemptStart(token personenrichment.LeaseToken, start personenrichment.AttemptStart) error {
	if start.RunID != token.RunID || start.PersonID != token.WorkPersonID ||
		start.ProfileFingerprint != token.ProfileFingerprint ||
		start.PersonRevision < 0 || !validLowerSHA256(start.PayloadHash) ||
		!validLowerSHA256(start.RequestHash) || strings.TrimSpace(start.Trigger.Generation) == "" {
		return errors.New("person enrichment attempt start does not match its lease")
	}
	if _, err := personEnrichmentTriggerMask(start.Trigger.Kind); err != nil {
		return err
	}
	if start.HardCostCap {
		if err := start.GuaranteedMaxCost.ValidateGuaranteed(); err != nil {
			return fmt.Errorf("validate guaranteed enrichment cost: %w", err)
		}
	} else if start.GuaranteedMaxCost != (personenrichment.Cost{}) {
		return errors.New("request-only enrichment attempt requires a zero guaranteed maximum cost")
	}
	for i, digest := range start.CheckedIdentifiers {
		if err := validatePersonEnrichmentSuppressionLookup(digest); err != nil {
			return fmt.Errorf("validate checked person enrichment identifier %d: %w", i, err)
		}
	}
	return nil
}

func bindPersonEnrichmentAttemptIdentifiersTx(
	ctx context.Context, tx *loggedTx, attemptID int64,
	digests []personenrichment.SuppressionDigest,
) error {
	for i, digest := range digests {
		if _, err := tx.ExecContext(ctx, `INSERT INTO person_enrichment_attempt_identifiers
			(attempt_id, provider_namespace, identifier_class, normalization_version, key_id, digest)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (attempt_id, provider_namespace, identifier_class,
			             normalization_version, key_id, digest) DO NOTHING`,
			attemptID, digest.ProviderNamespace, digest.IdentifierClass,
			digest.NormalizationVersion, digest.KeyID, digest.Digest); err != nil {
			return fmt.Errorf("record checked person enrichment identifier %d: %w", i, err)
		}
	}
	return nil
}

func (s *Store) reservePersonEnrichmentBudgetTx(
	ctx context.Context, tx *loggedTx, policy personEnrichmentBudgetPolicy,
	start personenrichment.AttemptStart,
) error {
	utcDay := s.personEnrichmentTime().Format("2006-01-02")
	if err := ensurePersonEnrichmentBudgetCountersTx(ctx, tx, start, utcDay); err != nil {
		return err
	}
	if s.personEnrichmentBudgetBarrier != nil && s.IsPostgreSQL() {
		s.personEnrichmentBudgetBarrier()
	}

	runCounter, personCounter, dayCounter, err := lockPersonEnrichmentCountersTx(
		ctx, tx, s.dialect, start.RunID, start.PersonID, start.ProfileFingerprint, utcDay)
	if err != nil {
		return err
	}
	if runCounter.RequestsStarted >= policy.MaxRequestsPerRun ||
		dayCounter.RequestsStarted >= policy.MaxRequestsPerDay {
		return ErrRequestBudgetExceeded
	}
	bound := start.GuaranteedMaxCost.AmountMicros
	if start.HardCostCap &&
		((policy.MaxCostUSDMicrosPerPersonPerDay > 0 &&
			personCounter.CostChargedUSDMicros+personCounter.CostReservedUSDMicros+bound > policy.MaxCostUSDMicrosPerPersonPerDay) ||
			(policy.MaxCostUSDMicrosPerRun > 0 &&
				runCounter.CostChargedUSDMicros+runCounter.CostReservedUSDMicros+bound > policy.MaxCostUSDMicrosPerRun) ||
			(policy.MaxCostUSDMicrosPerDay > 0 &&
				dayCounter.CostChargedUSDMicros+dayCounter.CostReservedUSDMicros+bound > policy.MaxCostUSDMicrosPerDay)) {
		return ErrCostBudgetExceeded
	}
	if _, err := tx.ExecContext(ctx, `UPDATE person_enrichment_run_counters
		SET requests_started = requests_started + 1,
		    cost_reserved_usd_micros = cost_reserved_usd_micros + ? WHERE run_id = ?`,
		bound, start.RunID); err != nil {
		return fmt.Errorf("reserve run enrichment budget: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE person_enrichment_person_day_counters
		SET requests_started = requests_started + 1,
		    cost_reserved_usd_micros = cost_reserved_usd_micros + ?
		WHERE person_id = ? AND profile_fingerprint = ? AND utc_day = ?`,
		bound, start.PersonID, start.ProfileFingerprint, utcDay); err != nil {
		return fmt.Errorf("reserve person-day enrichment budget: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE person_enrichment_day_counters
		SET requests_started = requests_started + 1,
		    cost_reserved_usd_micros = cost_reserved_usd_micros + ?
		WHERE profile_fingerprint = ? AND utc_day = ?`,
		bound, start.ProfileFingerprint, utcDay); err != nil {
		return fmt.Errorf("reserve day enrichment budget: %w", err)
	}
	return nil
}

func ensurePersonEnrichmentBudgetCountersTx(
	ctx context.Context, tx *loggedTx, start personenrichment.AttemptStart, utcDay string,
) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO person_enrichment_run_counters (run_id)
		VALUES (?) ON CONFLICT (run_id) DO NOTHING`, start.RunID); err != nil {
		return fmt.Errorf("initialize run budget counter: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO person_enrichment_person_day_counters
		(person_id, profile_fingerprint, utc_day) VALUES (?, ?, ?)
		ON CONFLICT (person_id, profile_fingerprint, utc_day) DO NOTHING`,
		start.PersonID, start.ProfileFingerprint, utcDay); err != nil {
		return fmt.Errorf("initialize person-day budget counter: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO person_enrichment_day_counters
		(profile_fingerprint, utc_day) VALUES (?, ?)
		ON CONFLICT (profile_fingerprint, utc_day) DO NOTHING`,
		start.ProfileFingerprint, utcDay); err != nil {
		return fmt.Errorf("initialize day budget counter: %w", err)
	}
	return nil
}

func (s *Store) RecordProviderStarted(
	ctx context.Context, token personenrichment.LeaseToken, started personenrichment.Attempt,
) error {
	if err := started.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(started.AdapterVersion) == "" || strings.TrimSpace(started.SchemaVersion) == "" ||
		!validLowerSHA256(started.ProgramFingerprint) ||
		(started.GeneratedSchema != validLowerSHA256(started.GeneratedSchemaHash)) {
		return errors.New("provider start requires immutable adapter, schema, and program metadata")
	}
	if started.State == personenrichment.AttemptPending && started.StartedAt.IsZero() {
		return errors.New("pending provider start requires exact provider start time")
	}
	var targetsJSON any
	if started.GeneratedSchema && started.JobID != "" {
		encoded, _, err := personenrichment.EncodeDurableAttemptTargets(started.Targets)
		if err != nil {
			return fmt.Errorf("encode provider start targets: %w", err)
		}
		targetsJSON = encoded
	} else if len(started.Targets) != 0 {
		return errors.New("only generated asynchronous provider starts may carry durable targets")
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := verifyEnrichmentLeaseTx(ctx, tx, s.dialect, token); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE person_enrichment_attempts
			SET state = 'pending', provider_request_id = ?, provider_job_id = ?,
			    adapter_version = ?, schema_version = ?, generated_schema = ?,
			    generated_schema_hash = ?, targets_json = ?, program_fingerprint = ?,
			    provider_started_at = ?
			WHERE id = ? AND run_id = ? AND lease_owner = ? AND lease_fence = ?
			  AND state = 'starting' AND dispatch_authorized_at IS NOT NULL`, nullableOpaqueID(started.RequestID), nullableOpaqueID(started.JobID),
			strings.TrimSpace(started.AdapterVersion), strings.TrimSpace(started.SchemaVersion),
			started.GeneratedSchema, nullableTrimmed(started.GeneratedSchemaHash),
			targetsJSON, started.ProgramFingerprint, nullableProviderStartedAt(started),
			token.AttemptID, token.RunID, token.Owner, token.Fence)
		if err != nil {
			return fmt.Errorf("record person enrichment provider start: %w", err)
		}
		return requireOneLeaseRow(result)
	})
}

func (s *Store) AuthorizeAttemptDispatch(
	ctx context.Context, token personenrichment.LeaseToken,
) error {
	if token.AttemptID <= 0 {
		return errors.New("person enrichment dispatch authorization requires an active attempt")
	}
	staleRevision := false
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockPersonEnrichmentAuthorityMutationTx(ctx, tx); err != nil {
			return err
		}
		currentRevision, err := lockPersonEnrichmentPersonTx(
			ctx, tx, s.dialect, token.WorkPersonID)
		if err != nil {
			return err
		}
		if err := verifyEnrichmentLeaseTx(ctx, tx, s.dialect, token); err != nil {
			return err
		}
		var active bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM person_tracking tracked
			JOIN person_enrichment_consents consent
			  ON consent.profile_fingerprint = ? AND consent.revoked_at IS NULL
			WHERE tracked.person_id = ?)`, token.ProfileFingerprint,
			token.WorkPersonID).Scan(&active); err != nil {
			return fmt.Errorf("check person enrichment dispatch authority: %w", err)
		}
		if !active {
			return personenrichment.ErrConsentRequired
		}
		var attemptRevision int64
		if err := tx.QueryRowContext(ctx, `SELECT person_revision
			FROM person_enrichment_attempts
			WHERE id = ? AND run_id = ? AND person_id = ? AND profile_fingerprint = ?
			  AND lease_owner = ? AND lease_fence = ? AND state = 'starting'`,
			token.AttemptID, token.RunID, token.WorkPersonID, token.ProfileFingerprint,
			token.Owner, token.Fence).Scan(&attemptRevision); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrStaleLease
			}
			return fmt.Errorf("load person enrichment dispatch revision: %w", err)
		}
		if attemptRevision != currentRevision {
			now := s.personEnrichmentTime()
			if err := s.putPersonEnrichmentWorkTx(ctx, tx, EnrichmentTriggerInput{
				PersonID: token.WorkPersonID, ProfileFingerprint: token.ProfileFingerprint,
				Kind:       personenrichment.TriggerIdentity,
				Generation: "revision:" + strconv.FormatInt(currentRevision, 10),
				DueAt:      now,
			}); err != nil {
				return err
			}
			if _, err := reconcilePersonEnrichmentCostTx(ctx, tx, s.dialect,
				token.AttemptID, personenrichment.Cost{Currency: "USD"}, false, now); err != nil {
				return err
			}
			result, err := tx.ExecContext(ctx, `UPDATE person_enrichment_attempts
				SET state = 'terminal', completed_at = ?, failure_class = ?,
				    next_action_at = NULL, lease_owner = NULL, lease_until = NULL
				WHERE id = ? AND run_id = ? AND lease_owner = ? AND lease_fence = ?
				  AND state = 'starting'`, now, personenrichment.FailurePolicy,
				token.AttemptID, token.RunID, token.Owner, token.Fence)
			if err != nil {
				return fmt.Errorf("terminalize stale person enrichment dispatch: %w", err)
			}
			if err := requireOneLeaseRow(result); err != nil {
				return err
			}
			if err := settleTerminalEnrichmentWorkTx(ctx, tx, token, nil); err != nil {
				return err
			}
			staleRevision = true
			return nil
		}
		digests, err := s.loadPersonEnrichmentAttemptIdentifiersTx(ctx, tx, token.AttemptID)
		if err != nil {
			return err
		}
		if err := s.recheckPersonEnrichmentSuppressionsTx(ctx, tx, digests); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE person_enrichment_attempts
			SET dispatch_authorized_at = ?
			WHERE id = ? AND run_id = ? AND person_id = ? AND profile_fingerprint = ?
			  AND lease_owner = ? AND lease_fence = ? AND state = 'starting'`, s.personEnrichmentTime(), token.AttemptID,
			token.RunID, token.WorkPersonID, token.ProfileFingerprint, token.Owner, token.Fence)
		if err != nil {
			return fmt.Errorf("authorize person enrichment dispatch: %w", err)
		}
		return requireOneLeaseRow(result)
	})
	if err == nil && staleRevision {
		return ErrStaleLease
	}
	return err
}

func (s *Store) SchedulePoll(
	ctx context.Context, token personenrichment.LeaseToken, pending personenrichment.Result,
) error {
	if err := pending.Validate(); err != nil {
		return err
	}
	if pending.State != personenrichment.ResultPending || pending.PollAfter <= 0 {
		return errors.New("person enrichment poll schedule requires a pending result and positive delay")
	}
	next := s.personEnrichmentTime().Add(pending.PollAfter)
	return s.schedulePersonEnrichmentAction(ctx, token, "pending", nil, &pending, next)
}

func (s *Store) ScheduleRetry(
	ctx context.Context, token personenrichment.LeaseToken, retry personenrichment.RetryUpdate,
) error {
	if err := validateSafeFailure(retry.Failure); err != nil {
		return err
	}
	if !retry.NextActionAt.After(s.personEnrichmentTime()) {
		return errors.New("person enrichment retry must be scheduled in the future")
	}
	return s.schedulePersonEnrichmentAction(ctx, token, "retry_wait", &retry.Failure, nil, retry.NextActionAt.UTC())
}

func (s *Store) schedulePersonEnrichmentAction(
	ctx context.Context, token personenrichment.LeaseToken, state string,
	failure *personenrichment.SafeFailure, pending *personenrichment.Result, next time.Time,
) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := verifyEnrichmentLeaseTx(ctx, tx, s.dialect, token); err != nil {
			return err
		}
		if pending != nil {
			var requestID, jobID, adapter, schema, generatedHash sql.NullString
			var generated bool
			if err := tx.QueryRowContext(ctx, `SELECT provider_request_id, provider_job_id,
				adapter_version, schema_version, generated_schema, generated_schema_hash
				FROM person_enrichment_attempts WHERE id = ? AND run_id = ?`+s.dialect.SelectForUpdate(),
				token.AttemptID, token.RunID).Scan(&requestID, &jobID, &adapter, &schema,
				&generated, &generatedHash); err != nil {
				return fmt.Errorf("load person enrichment poll binding: %w", err)
			}
			if pending.RequestID != requestID.String || pending.JobID != jobID.String ||
				pending.AdapterVersion != adapter.String || pending.SchemaVersion != schema.String ||
				pending.GeneratedSchema != generated || pending.GeneratedSchemaHash != generatedHash.String {
				return errors.New("person enrichment poll result does not match the bound provider job")
			}
		}
		var failureClass any
		incrementAttempt := int64(0)
		if failure != nil {
			failureClass = string(failure.Class)
			incrementAttempt = 1
		}
		result, err := tx.ExecContext(ctx, `UPDATE person_enrichment_attempts
			SET state = ?, next_action_at = ?, failure_class = ?,
			    attempt_count = attempt_count + ?,
			    lease_owner = NULL, lease_until = NULL
			WHERE id = ? AND run_id = ? AND lease_owner = ? AND lease_fence = ?`,
			state, next, failureClass, incrementAttempt,
			token.AttemptID, token.RunID, token.Owner, token.Fence)
		if err != nil {
			return fmt.Errorf("schedule person enrichment action: %w", err)
		}
		if err := requireOneLeaseRow(result); err != nil {
			return err
		}
		return updateEnrichmentWorkLeaseTx(ctx, tx, token,
			`due_at = ?, lease_owner = NULL, lease_until = NULL`, next)
	})
}

func (s *Store) MarkUncertainStart(
	ctx context.Context, token personenrichment.LeaseToken, failure personenrichment.SafeFailure,
) error {
	if err := validateSafeFailure(failure); err != nil {
		return err
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := verifyEnrichmentLeaseTx(ctx, tx, s.dialect, token); err != nil {
			return err
		}
		// An ambiguous start is terminal for scheduling but keeps its distinct
		// audit state. Charge the reserved maximum because the provider may have
		// accepted the request, then detach the work without replaying it.
		if _, err := reconcilePersonEnrichmentCostTx(ctx, tx, s.dialect, token.AttemptID,
			personenrichment.Cost{}, true, s.personEnrichmentTime()); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE person_enrichment_attempts
			SET state = 'uncertain_start', failure_class = ?, completed_at = ?,
			    next_action_at = NULL, lease_owner = NULL, lease_until = NULL
			WHERE id = ? AND run_id = ? AND lease_owner = ? AND lease_fence = ?`,
			personenrichment.FailureUncertainStart, s.personEnrichmentTime(),
			token.AttemptID, token.RunID, token.Owner, token.Fence)
		if err != nil {
			return fmt.Errorf("mark uncertain person enrichment start: %w", err)
		}
		if err := requireOneLeaseRow(result); err != nil {
			return err
		}
		return settleTerminalEnrichmentWorkTx(ctx, tx, token, nil)
	})
}

func (s *Store) MarkTerminal(
	ctx context.Context, token personenrichment.LeaseToken, failure personenrichment.SafeFailure,
) error {
	if err := validateSafeFailure(failure); err != nil {
		return err
	}
	state := "terminal"
	switch failure.Class {
	case personenrichment.FailureIdentityRejected:
		state = "identity_rejected"
	case personenrichment.FailureSuppressed:
		state = "suppressed"
	case personenrichment.FailurePolicy, personenrichment.FailureRateLimited,
		personenrichment.FailureTransient, personenrichment.FailureInvalidOutput,
		personenrichment.FailureTerminal, personenrichment.FailureUncertainStart:
		// The default terminal state is correct for every other safe failure.
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := verifyEnrichmentLeaseTx(ctx, tx, s.dialect, token); err != nil {
			return err
		}
		if _, err := reconcilePersonEnrichmentCostTx(ctx, tx, s.dialect, token.AttemptID,
			personenrichment.Cost{}, true, s.personEnrichmentTime()); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE person_enrichment_attempts
			SET state = ?, failure_class = ?, completed_at = ?, next_action_at = NULL,
			    lease_owner = NULL, lease_until = NULL
			WHERE id = ? AND run_id = ? AND lease_owner = ? AND lease_fence = ?`,
			state, failure.Class, s.personEnrichmentTime(), token.AttemptID,
			token.RunID, token.Owner, token.Fence)
		if err != nil {
			return fmt.Errorf("mark terminal person enrichment attempt: %w", err)
		}
		if err := requireOneLeaseRow(result); err != nil {
			return err
		}
		return settleTerminalEnrichmentWorkTx(ctx, tx, token, nil)
	})
}

func (s *Store) completePersonEnrichmentAttemptContext(
	ctx context.Context, token personenrichment.LeaseToken, completion personEnrichmentAttemptCompletion,
) error {
	var costViolation bool
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		var err error
		costViolation, err = s.completePersonEnrichmentAttemptTx(ctx, tx, token, completion)
		return err
	})
	if err != nil {
		return err
	}
	if costViolation {
		return ErrProviderCostBoundExceeded
	}
	return nil
}

func (s *Store) completePersonEnrichmentAttemptTx(
	ctx context.Context, tx *loggedTx, token personenrichment.LeaseToken,
	completion personEnrichmentAttemptCompletion,
) (bool, error) {
	if completion.State != "succeeded" {
		return false, errors.New("successful person enrichment completion requires succeeded state")
	}
	if !completion.ActualCostMissing {
		if err := completion.ActualCost.Validate(); err != nil {
			return false, err
		}
	}
	if completion.FactGenerationKey != "" && !validPersonFactGenerationKey(completion.FactGenerationKey) {
		return false, errors.New("successful person enrichment completion has invalid fact generation key")
	}
	completionTime := completion.CompletedAt.UTC()
	if completion.CompletedAt.IsZero() {
		completionTime = s.personEnrichmentTime()
	}
	if completion.RefreshAt != nil {
		if !completion.RefreshAt.After(completionTime) || strings.TrimSpace(completion.RefreshGeneration) == "" {
			return false, errors.New("person enrichment refresh completion is invalid")
		}
	}
	if err := verifyEnrichmentLeaseTx(ctx, tx, s.dialect, token); err != nil {
		return false, err
	}
	costViolation := false
	if !completion.CostReconciled {
		var err error
		costViolation, err = reconcilePersonEnrichmentCostTx(ctx, tx, s.dialect, token.AttemptID,
			completion.ActualCost, completion.ActualCostMissing, completionTime)
		if err != nil {
			return false, err
		}
	}
	state := "succeeded"
	var failureClass any
	if costViolation {
		state = "terminal"
		failureClass = string(personenrichment.FailureTerminal)
	}
	result, err := tx.ExecContext(ctx, `UPDATE person_enrichment_attempts
		SET state = ?, failure_class = ?, completed_at = ?, fact_generation_key = ?, next_action_at = NULL,
		    lease_owner = NULL, lease_until = NULL
		WHERE id = ? AND run_id = ? AND lease_owner = ? AND lease_fence = ?`,
		state, failureClass, completionTime, nullableTrimmed(completion.FactGenerationKey),
		token.AttemptID, token.RunID,
		token.Owner, token.Fence)
	if err != nil {
		return false, fmt.Errorf("complete person enrichment attempt: %w", err)
	}
	if err := requireOneLeaseRow(result); err != nil {
		return false, err
	}
	if completion.RefreshAt == nil || costViolation {
		return costViolation, settleTerminalEnrichmentWorkTx(ctx, tx, token, nil)
	}
	refresh := EnrichmentTriggerInput{
		PersonID: token.WorkPersonID, ProfileFingerprint: token.ProfileFingerprint,
		Kind: personenrichment.TriggerRefresh, Generation: completion.RefreshGeneration,
		DueAt: completion.RefreshAt.UTC(),
	}
	return false, settleTerminalEnrichmentWorkTx(ctx, tx, token, &refresh)
}

func validPersonFactGenerationKey(value string) bool {
	digest, ok := strings.CutPrefix(value, "sha256:")
	return ok && validLowerSHA256(digest)
}

func (s *Store) GetPersonEnrichmentAttemptContext(
	ctx context.Context, id int64,
) (*PersonEnrichmentAttempt, error) {
	if id <= 0 {
		return nil, errors.New("person enrichment attempt ID must be positive")
	}
	attempt, err := scanPersonEnrichmentAttempt(s.db.QueryRowContext(ctx,
		personEnrichmentAttemptSelect+` WHERE id = ?`, id))
	if err != nil {
		return nil, fmt.Errorf("get person enrichment attempt: %w", err)
	}
	return attempt, nil
}

func (s *Store) ListPersonEnrichmentAttemptsContext(
	ctx context.Context, filter PersonEnrichmentAttemptFilter,
) ([]PersonEnrichmentAttempt, error) {
	if filter.Limit < 1 || filter.Limit > 200 || filter.PersonID < 0 || filter.RunID < 0 || filter.BeforeID < 0 ||
		(filter.ProfileFingerprint != "" && !validLowerSHA256(filter.ProfileFingerprint)) ||
		(filter.State != "" && !validPersonEnrichmentAttemptState(filter.State)) {
		return nil, errors.New("person enrichment attempt filter is invalid")
	}
	query := personEnrichmentAttemptSelect + ` WHERE (? = 0 OR person_id = ?)
		AND (? = '' OR profile_fingerprint = ?) AND (? = '' OR state = ?)
		AND (? = 0 OR run_id = ?) AND (? = 0 OR id < ?)
		ORDER BY id DESC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, query,
		filter.PersonID, filter.PersonID, filter.ProfileFingerprint, filter.ProfileFingerprint,
		filter.State, filter.State, filter.RunID, filter.RunID, filter.BeforeID, filter.BeforeID,
		filter.Limit)
	if err != nil {
		return nil, fmt.Errorf("list person enrichment attempts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]PersonEnrichmentAttempt, 0)
	for rows.Next() {
		attempt, err := scanPersonEnrichmentAttempt(rows)
		if err != nil {
			return nil, fmt.Errorf("scan person enrichment attempt: %w", err)
		}
		result = append(result, *attempt)
	}
	return result, rows.Err()
}

func validPersonEnrichmentAttemptState(state string) bool {
	switch state {
	case "queued", "starting", "pending", "retry_wait", "succeeded", "terminal",
		"suppressed", "identity_rejected", "uncertain_start":
		return true
	default:
		return false
	}
}

func (s *Store) ListPersonEnrichmentWorkContext(
	ctx context.Context, filter PersonEnrichmentWorkFilter,
) ([]PersonEnrichmentWork, error) {
	if filter.Limit < 1 || filter.Limit > 200 || filter.PersonID < 0 ||
		(filter.ProfileFingerprint != "" && !validLowerSHA256(filter.ProfileFingerprint)) {
		return nil, errors.New("person enrichment work filter is invalid")
	}
	query := `
		SELECT person_id, profile_fingerprint, trigger_mask, trigger_generation, due_at,
		       lease_owner, lease_fence, lease_until, run_id, active_attempt_id,
		       has_fresh_trigger
		FROM person_enrichment_work
		WHERE (? = 0 OR person_id = ?) AND (? = '' OR profile_fingerprint = ?)`
	args := []any{filter.PersonID, filter.PersonID, filter.ProfileFingerprint, filter.ProfileFingerprint}
	if filter.DueBefore != nil {
		query += ` AND due_at <= ?`
		args = append(args, filter.DueBefore.UTC())
	}
	query += ` ORDER BY due_at, person_id, profile_fingerprint LIMIT ?`
	args = append(args, filter.Limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list person enrichment work: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]PersonEnrichmentWork, 0)
	for rows.Next() {
		var work PersonEnrichmentWork
		var dueAt, leaseUntil nullableTimestamp
		var owner sql.NullString
		var runID, attemptID sql.NullInt64
		if err := rows.Scan(&work.PersonID, &work.ProfileFingerprint, &work.TriggerMask,
			&work.TriggerGeneration, &dueAt, &owner, &work.LeaseFence, &leaseUntil,
			&runID, &attemptID, &work.HasFreshTrigger); err != nil {
			return nil, fmt.Errorf("scan person enrichment work: %w", err)
		}
		if !dueAt.Valid {
			return nil, errors.New("person enrichment work has invalid due_at")
		}
		work.DueAt = dueAt.Time
		work.LeaseUntil = optionalTimestamp(leaseUntil)
		if owner.Valid {
			value := owner.String
			work.LeaseOwner = &value
		}
		if runID.Valid {
			value := runID.Int64
			work.RunID = &value
		}
		if attemptID.Valid {
			value := attemptID.Int64
			work.ActiveAttemptID = &value
		}
		result = append(result, work)
	}
	return result, rows.Err()
}

func (s *Store) GetPersonEnrichmentRunCountersContext(
	ctx context.Context, runID int64,
) (PersonEnrichmentRunCounters, error) {
	var counters PersonEnrichmentRunCounters
	err := s.db.QueryRowContext(ctx, `SELECT requests_started, cost_reserved_usd_micros,
		cost_charged_usd_micros FROM person_enrichment_run_counters WHERE run_id = ?`,
		runID).Scan(&counters.RequestsStarted, &counters.CostReservedUSDMicros,
		&counters.CostChargedUSDMicros)
	return counters, err
}

const personEnrichmentAttemptSelect = `SELECT id, run_id, person_id, profile_fingerprint,
	trigger_kind, trigger_generation, person_revision, payload_hash, request_hash, state,
	provider_request_id, provider_job_id, adapter_version, schema_version, generated_schema,
	generated_schema_hash, program_fingerprint, fact_generation_key, lease_owner, lease_fence, lease_until,
	next_action_at, attempt_count, hard_cost_cap_enforced, reserved_cost_usd_micros,
	actual_cost_usd_micros, failure_class, created_at, provider_started_at, completed_at
	FROM person_enrichment_attempts`

func scanPersonEnrichmentAttempt(row scanner) (*PersonEnrichmentAttempt, error) {
	var attempt PersonEnrichmentAttempt
	var requestID, jobID, adapter, schema, generatedHash, program, generationKey, owner, failure sql.NullString
	var leaseUntil, nextAction, createdAt, providerStartedAt, completedAt nullableTimestamp
	var actual sql.NullInt64
	if err := row.Scan(&attempt.ID, &attempt.RunID, &attempt.PersonID, &attempt.ProfileFingerprint,
		&attempt.TriggerKind, &attempt.TriggerGeneration, &attempt.PersonRevision,
		&attempt.PayloadHash, &attempt.RequestHash, &attempt.State, &requestID, &jobID,
		&adapter, &schema, &attempt.GeneratedSchema, &generatedHash, &program, &generationKey, &owner,
		&attempt.LeaseFence, &leaseUntil, &nextAction, &attempt.AttemptCount,
		&attempt.HardCostCapEnforced, &attempt.ReservedCostUSDMicros, &actual,
		&failure, &createdAt, &providerStartedAt, &completedAt); err != nil {
		return nil, err
	}
	if !createdAt.Valid {
		return nil, errors.New("person enrichment attempt has invalid created_at")
	}
	attempt.CreatedAt = createdAt.Time
	attempt.ProviderStartedAt = optionalTimestamp(providerStartedAt)
	attempt.CompletedAt = optionalTimestamp(completedAt)
	attempt.LeaseUntil = optionalTimestamp(leaseUntil)
	attempt.NextActionAt = optionalTimestamp(nextAction)
	attempt.ProviderRequestID = nullStringPtr(requestID)
	attempt.ProviderJobID = nullStringPtr(jobID)
	attempt.AdapterVersion = nullStringPtr(adapter)
	attempt.SchemaVersion = nullStringPtr(schema)
	attempt.GeneratedSchemaHash = nullStringPtr(generatedHash)
	attempt.ProgramFingerprint = nullStringPtr(program)
	attempt.FactGenerationKey = nullStringPtr(generationKey)
	attempt.LeaseOwner = nullStringPtr(owner)
	attempt.FailureClass = nullStringPtr(failure)
	if actual.Valid {
		value := actual.Int64
		attempt.ActualCostUSDMicros = &value
	}
	return &attempt, nil
}

func loadDurableAttemptTx(
	ctx context.Context, tx *loggedTx, token personenrichment.LeaseToken,
) (*personenrichment.DurableAttempt, error) {
	var attempt personenrichment.DurableAttempt
	var requestID, jobID, adapter, schema, generatedHash, targetsJSON, program sql.NullString
	var next, providerStarted, created nullableTimestamp
	var personID int64
	err := tx.QueryRowContext(ctx, `
		SELECT id, run_id, person_id, state, payload_hash, request_hash, person_revision,
		       trigger_kind, trigger_generation, provider_request_id, provider_job_id,
		       adapter_version, schema_version, generated_schema, generated_schema_hash,
		       targets_json, program_fingerprint, next_action_at, provider_started_at,
		       created_at, attempt_count,
		       hard_cost_cap_enforced, reserved_cost_usd_micros
		FROM person_enrichment_attempts WHERE id = ? AND run_id = ?`,
		token.AttemptID, token.RunID).Scan(&attempt.ID, &attempt.RunID, &personID,
		&attempt.State, &attempt.PayloadHash, &attempt.RequestHash, &attempt.PersonRevision,
		&attempt.Trigger.Kind, &attempt.Trigger.Generation,
		&requestID, &jobID, &adapter, &schema, &attempt.GeneratedSchema,
		&generatedHash, &targetsJSON, &program, &next, &providerStarted, &created, &attempt.AttemptCount,
		&attempt.HardCostCap, &attempt.ReservedCostMicros)
	if err != nil {
		return nil, fmt.Errorf("load bound person enrichment attempt: %w", err)
	}
	attempt.Token = token
	attempt.Token.WorkPersonID = personID
	attempt.RequestID = requestID.String
	attempt.JobID = jobID.String
	attempt.AdapterVersion = adapter.String
	attempt.SchemaVersion = schema.String
	attempt.GeneratedSchemaHash = generatedHash.String
	attempt.ProgramFingerprint = program.String
	if attempt.GeneratedSchema && jobID.Valid {
		if !targetsJSON.Valid {
			return nil, errors.New("bound generated-schema attempt has no durable targets")
		}
		attempt.Targets, err = personenrichment.DecodeDurableAttemptTargets(targetsJSON.String)
		if err != nil {
			return nil, fmt.Errorf("load bound person enrichment targets: %w", err)
		}
	} else if targetsJSON.Valid {
		return nil, errors.New("bound non-asynchronous attempt has unexpected durable targets")
	}
	if next.Valid {
		attempt.NextActionAt = next.Time
	}
	if !created.Valid {
		return nil, errors.New("bound person enrichment attempt has invalid created_at")
	}
	if jobID.Valid && !providerStarted.Valid {
		return nil, errors.New("bound asynchronous person enrichment attempt has no provider start time")
	}
	if providerStarted.Valid {
		attempt.StartedAt = providerStarted.Time
	}
	return &attempt, nil
}

func nullableProviderStartedAt(attempt personenrichment.Attempt) any {
	if attempt.StartedAt.IsZero() {
		return nil
	}
	return attempt.StartedAt.UTC()
}

func lockEnrichmentWorkTx(
	ctx context.Context, tx *loggedTx, dialect Dialect, token personenrichment.LeaseToken,
) (sql.NullInt64, error) {
	work, err := lockEnrichmentWorkStateTx(ctx, tx, dialect, token)
	return work.ActiveID, err
}

type lockedPersonEnrichmentWork struct {
	ActiveID        sql.NullInt64
	HasFreshTrigger bool
}

func lockEnrichmentWorkStateTx(
	ctx context.Context, tx *loggedTx, dialect Dialect, token personenrichment.LeaseToken,
) (lockedPersonEnrichmentWork, error) {
	var work lockedPersonEnrichmentWork
	err := tx.QueryRowContext(ctx, `SELECT active_attempt_id, has_fresh_trigger
		FROM person_enrichment_work
		WHERE person_id = ? AND profile_fingerprint = ? AND run_id = ?
		  AND lease_owner = ? AND lease_fence = ?`+dialect.SelectForUpdate(),
		token.WorkPersonID, token.ProfileFingerprint, token.RunID, token.Owner, token.Fence).Scan(
		&work.ActiveID, &work.HasFreshTrigger)
	if errors.Is(err, sql.ErrNoRows) {
		return work, ErrStaleLease
	}
	if err != nil {
		return work, fmt.Errorf("lock person enrichment work: %w", err)
	}
	if token.AttemptID != 0 && (!work.ActiveID.Valid || work.ActiveID.Int64 != token.AttemptID) {
		return work, ErrStaleLease
	}
	return work, nil
}

func lockPersonEnrichmentPersonTx(
	ctx context.Context, tx *loggedTx, dialect Dialect, personID int64,
) (int64, error) {
	lockClause := dialect.SelectForUpdate()
	// SQLite has no SELECT FOR UPDATE. Make the person gate the transaction's
	// first write so its database-wide writer lock provides the same ordering
	// before this transaction reads work or attempts.
	if lockClause == "" {
		result, err := tx.ExecContext(ctx,
			`UPDATE persons SET revision = revision WHERE id = ?`, personID)
		if err != nil {
			return 0, fmt.Errorf("lock person enrichment person: %w", err)
		}
		if rows, err := result.RowsAffected(); err != nil {
			return 0, fmt.Errorf("count locked person enrichment person: %w", err)
		} else if rows != 1 {
			return 0, sql.ErrNoRows
		}
	} else {
		// PostgreSQL enrichment transactions only mutate non-key person fields.
		// A NO KEY UPDATE lock still serializes those transactions with each
		// other while remaining compatible with the KEY SHARE lock PostgreSQL
		// takes for sweep-work foreign-key publication.
		lockClause = " FOR NO KEY UPDATE"
	}
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM persons WHERE id = ?`+
		lockClause, personID).Scan(&revision); err != nil {
		return 0, fmt.Errorf("lock person enrichment person: %w", err)
	}
	return revision, nil
}

func verifyEnrichmentLeaseTx(
	ctx context.Context, tx *loggedTx, dialect Dialect, token personenrichment.LeaseToken,
) error {
	activeID, err := lockEnrichmentWorkTx(ctx, tx, dialect, token)
	if err != nil {
		return err
	}
	if token.AttemptID == 0 && activeID.Valid {
		return ErrStaleLease
	}
	return nil
}

func updateEnrichmentWorkLeaseTx(
	ctx context.Context, tx *loggedTx, token personenrichment.LeaseToken,
	setClause string, args ...any,
) error {
	args = append(args, token.WorkPersonID, token.ProfileFingerprint, token.RunID,
		token.Owner, token.Fence, token.AttemptID, token.AttemptID)
	result, err := tx.ExecContext(ctx, `UPDATE person_enrichment_work SET `+setClause+`
		WHERE person_id = ? AND profile_fingerprint = ? AND run_id = ?
		  AND lease_owner = ? AND lease_fence = ?
		  AND ((? = 0 AND active_attempt_id IS NULL) OR active_attempt_id = ?)`, args...)
	if err != nil {
		return fmt.Errorf("update person enrichment work lease: %w", err)
	}
	return requireOneLeaseRow(result)
}

func requireOneLeaseRow(result sql.Result) error {
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrStaleLease
	}
	return nil
}

func deleteTerminalEnrichmentWorkTx(
	ctx context.Context, tx *loggedTx, token personenrichment.LeaseToken,
) error {
	return settleTerminalEnrichmentWorkTx(ctx, tx, token, nil)
}

func settleTerminalEnrichmentWorkTx(
	ctx context.Context, tx *loggedTx, token personenrichment.LeaseToken,
	refresh *EnrichmentTriggerInput,
) error {
	var kind personenrichment.TriggerKind
	var generation string
	if err := tx.QueryRowContext(ctx, `SELECT trigger_kind, trigger_generation
		FROM person_enrichment_attempts WHERE id = ? AND run_id = ?`,
		token.AttemptID, token.RunID).Scan(&kind, &generation); err != nil {
		return fmt.Errorf("load terminal person enrichment trigger: %w", err)
	}
	mask, err := personEnrichmentTriggerMask(kind)
	if err != nil {
		return err
	}
	if refresh == nil {
		result, err := tx.ExecContext(ctx, `DELETE FROM person_enrichment_work
			WHERE person_id = ? AND profile_fingerprint = ? AND run_id = ?
			  AND active_attempt_id = ? AND lease_owner = ? AND lease_fence = ?
			  AND trigger_mask = ? AND trigger_generation = ? AND NOT has_fresh_trigger`,
			token.WorkPersonID, token.ProfileFingerprint, token.RunID, token.AttemptID,
			token.Owner, token.Fence, mask, generation)
		if err != nil {
			return fmt.Errorf("delete terminal person enrichment work: %w", err)
		}
		if deleted, err := result.RowsAffected(); err != nil {
			return err
		} else if deleted == 1 {
			return nil
		}
	} else {
		refreshMask, err := personEnrichmentTriggerMask(refresh.Kind)
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE person_enrichment_work
			SET trigger_mask = ?, trigger_generation = ?, due_at = ?,
			    run_id = NULL, active_attempt_id = NULL,
			    lease_owner = NULL, lease_until = NULL
			WHERE person_id = ? AND profile_fingerprint = ? AND run_id = ?
			  AND active_attempt_id = ? AND lease_owner = ? AND lease_fence = ?
			  AND trigger_mask = ? AND trigger_generation = ? AND NOT has_fresh_trigger`,
			refreshMask, strings.TrimSpace(refresh.Generation), refresh.DueAt.UTC(),
			token.WorkPersonID, token.ProfileFingerprint, token.RunID, token.AttemptID,
			token.Owner, token.Fence, mask, generation)
		if err != nil {
			return fmt.Errorf("replace completed person enrichment work with refresh: %w", err)
		}
		if replaced, err := result.RowsAffected(); err != nil {
			return err
		} else if replaced == 1 {
			return nil
		}
	}
	var hasFreshTrigger bool
	if err := tx.QueryRowContext(ctx, `SELECT has_fresh_trigger
		FROM person_enrichment_work
		WHERE person_id = ? AND profile_fingerprint = ? AND run_id = ?
		  AND active_attempt_id = ? AND lease_owner = ? AND lease_fence = ?`,
		token.WorkPersonID, token.ProfileFingerprint, token.RunID, token.AttemptID,
		token.Owner, token.Fence).Scan(&hasFreshTrigger); err != nil {
		return fmt.Errorf("load coalesced person enrichment work: %w", err)
	}
	if !hasFreshTrigger {
		return errors.New("person enrichment work changed without a fresh trigger")
	}
	result, err := tx.ExecContext(ctx, `UPDATE person_enrichment_work
		SET run_id = NULL, active_attempt_id = NULL,
		    lease_owner = NULL, lease_until = NULL, has_fresh_trigger = FALSE
		WHERE person_id = ? AND profile_fingerprint = ? AND run_id = ?
		  AND active_attempt_id = ? AND lease_owner = ? AND lease_fence = ?`,
		token.WorkPersonID, token.ProfileFingerprint, token.RunID, token.AttemptID,
		token.Owner, token.Fence)
	if err != nil {
		return fmt.Errorf("retain coalesced person enrichment work: %w", err)
	}
	if err := requireOneLeaseRow(result); err != nil {
		return err
	}
	if refresh != nil {
		if err := validateEnrichmentTriggerInput(*refresh); err != nil {
			return err
		}
		if err := putPersonEnrichmentWorkWithExecer(ctx, tx, *refresh); err != nil {
			return fmt.Errorf("publish person enrichment refresh work: %w", err)
		}
	}
	return nil
}

func validateSafeFailure(failure personenrichment.SafeFailure) error {
	if !validPersonEnrichmentFailureClass(failure.Class) || failure.HTTPStatus < 0 || failure.HTTPStatus > 999 ||
		len(strings.TrimSpace(failure.Message)) > 512 || len(strings.TrimSpace(failure.ProviderRequestID)) > 512 {
		return errors.New("person enrichment safe failure is invalid")
	}
	return nil
}

func validPersonEnrichmentFailureClass(class personenrichment.FailureClass) bool {
	switch class {
	case personenrichment.FailurePolicy, personenrichment.FailureSuppressed,
		personenrichment.FailureRateLimited, personenrichment.FailureTransient,
		personenrichment.FailureInvalidOutput, personenrichment.FailureIdentityRejected,
		personenrichment.FailureTerminal, personenrichment.FailureUncertainStart:
		return true
	default:
		return false
	}
}

func safeFailureMessage(message string) any {
	message = strings.TrimSpace(message)
	if message == "" {
		return nil
	}
	return message
}

func nullableTrimmed(value string) any {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return value
}

func nullableOpaqueID(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func loadPersonEnrichmentBudgetPolicyTx(
	ctx context.Context, tx *loggedTx, fingerprint string,
) (personEnrichmentBudgetPolicy, error) {
	var encoded string
	if err := tx.QueryRowContext(ctx, `SELECT CAST(policy_json AS TEXT)
		FROM person_enrichment_profiles WHERE fingerprint = ?`, fingerprint).Scan(&encoded); err != nil {
		return personEnrichmentBudgetPolicy{}, fmt.Errorf("load person enrichment budget policy: %w", err)
	}
	var policy personEnrichmentBudgetPolicy
	if err := json.Unmarshal([]byte(encoded), &policy); err != nil {
		return policy, fmt.Errorf("decode person enrichment budget policy: %w", err)
	}
	if policy.MaxRequestsPerRun <= 0 || policy.MaxRequestsPerDay <= 0 ||
		policy.MaxCostUSDMicrosPerPersonPerDay < 0 || policy.MaxCostUSDMicrosPerRun < 0 ||
		policy.MaxCostUSDMicrosPerDay < 0 {
		return policy, errors.New("stored person enrichment budget policy is invalid")
	}
	return policy, nil
}

func lockPersonEnrichmentCountersTx(
	ctx context.Context, tx *loggedTx, dialect Dialect, runID, personID int64,
	fingerprint, day string,
) (PersonEnrichmentRunCounters, PersonEnrichmentRunCounters, PersonEnrichmentRunCounters, error) {
	var run, person, daily PersonEnrichmentRunCounters
	lock := dialect.SelectForUpdate()
	if err := tx.QueryRowContext(ctx, `SELECT requests_started, cost_reserved_usd_micros,
		cost_charged_usd_micros FROM person_enrichment_run_counters WHERE run_id = ?`+lock,
		runID).Scan(&run.RequestsStarted, &run.CostReservedUSDMicros, &run.CostChargedUSDMicros); err != nil {
		return run, person, daily, fmt.Errorf("lock enrichment run counter: %w", err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT requests_started, cost_reserved_usd_micros,
		cost_charged_usd_micros FROM person_enrichment_person_day_counters
		WHERE person_id = ? AND profile_fingerprint = ? AND utc_day = ?`+lock,
		personID, fingerprint, day).Scan(&person.RequestsStarted,
		&person.CostReservedUSDMicros, &person.CostChargedUSDMicros); err != nil {
		return run, person, daily, fmt.Errorf("lock enrichment person-day counter: %w", err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT requests_started, cost_reserved_usd_micros,
		cost_charged_usd_micros FROM person_enrichment_day_counters
		WHERE profile_fingerprint = ? AND utc_day = ?`+lock,
		fingerprint, day).Scan(&daily.RequestsStarted,
		&daily.CostReservedUSDMicros, &daily.CostChargedUSDMicros); err != nil {
		return run, person, daily, fmt.Errorf("lock enrichment day counter: %w", err)
	}
	return run, person, daily, nil
}

func reconcilePersonEnrichmentCostTx(
	ctx context.Context, tx *loggedTx, dialect Dialect, attemptID int64,
	actual personenrichment.Cost, missing bool, now time.Time,
) (bool, error) {
	var runID, personID, reserved int64
	var fingerprint string
	var hard bool
	var created nullableTimestamp
	err := tx.QueryRowContext(ctx, `SELECT run_id, person_id, profile_fingerprint,
		created_at, hard_cost_cap_enforced, reserved_cost_usd_micros
		FROM person_enrichment_attempts WHERE id = ?`+dialect.SelectForUpdate(), attemptID).Scan(
		&runID, &personID, &fingerprint, &created, &hard, &reserved)
	if err != nil {
		return false, fmt.Errorf("lock person enrichment attempt cost: %w", err)
	}
	if !created.Valid {
		return false, errors.New("person enrichment attempt has invalid creation day")
	}
	charged := int64(0)
	actualValue := any(nil)
	violation := false
	if hard {
		charged = reserved
		if !missing {
			if err := actual.Validate(); err != nil {
				return false, err
			}
			if actual.Currency == "USD" && !actual.Estimated {
				actualValue = actual.AmountMicros
				if actual.AmountMicros <= reserved {
					charged = actual.AmountMicros
				} else {
					violation = true
					charged = actual.AmountMicros
					_, disableErr := tx.ExecContext(ctx, `INSERT INTO person_enrichment_profile_accounting
						(profile_fingerprint, starts_disabled, safe_error, disabled_at)
						VALUES (?, ?, 'provider actual charge exceeded guaranteed maximum', ?)
						ON CONFLICT (profile_fingerprint) DO UPDATE SET starts_disabled = excluded.starts_disabled,
							safe_error = excluded.safe_error, disabled_at = excluded.disabled_at`,
						fingerprint, true, now)
					if disableErr != nil {
						return false, fmt.Errorf("disable enrichment starts after accounting violation: %w", disableErr)
					}
				}
			}
		}
	} else if !missing && actual.Currency == "USD" && !actual.Estimated {
		charged = actual.AmountMicros
		actualValue = actual.AmountMicros
	}
	day := created.Time.UTC().Format("2006-01-02")
	if _, _, _, err := lockPersonEnrichmentCountersTx(ctx, tx, dialect, runID, personID, fingerprint, day); err != nil {
		return false, err
	}
	updates := []struct {
		query string
		args  []any
	}{
		{`UPDATE person_enrichment_run_counters SET cost_reserved_usd_micros = cost_reserved_usd_micros - ?,
			cost_charged_usd_micros = cost_charged_usd_micros + ? WHERE run_id = ?`, []any{reserved, charged, runID}},
		{`UPDATE person_enrichment_person_day_counters SET cost_reserved_usd_micros = cost_reserved_usd_micros - ?,
			cost_charged_usd_micros = cost_charged_usd_micros + ?
			WHERE person_id = ? AND profile_fingerprint = ? AND utc_day = ?`, []any{reserved, charged, personID, fingerprint, day}},
		{`UPDATE person_enrichment_day_counters SET cost_reserved_usd_micros = cost_reserved_usd_micros - ?,
			cost_charged_usd_micros = cost_charged_usd_micros + ?
			WHERE profile_fingerprint = ? AND utc_day = ?`, []any{reserved, charged, fingerprint, day}},
	}
	for _, update := range updates {
		if _, err := tx.ExecContext(ctx, update.query, update.args...); err != nil {
			return false, fmt.Errorf("reconcile person enrichment cost: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE person_enrichment_attempts
		SET reserved_cost_usd_micros = 0, actual_cost_usd_micros = ? WHERE id = ?`,
		actualValue, attemptID); err != nil {
		return false, fmt.Errorf("record person enrichment actual cost: %w", err)
	}
	return violation, nil
}

// LoadRequestInput is implemented in Task 5 so a restarted worker reconstructs
// requests from current durable person state rather than a cached raw payload.
func (s *Store) LoadRequestInput(
	ctx context.Context, lease personenrichment.WorkLease,
) (personenrichment.RequestInput, error) {
	person, err := s.GetPersonContext(ctx, lease.PersonID)
	if err != nil {
		return personenrichment.RequestInput{}, err
	}
	catalog, err := s.BuildPersonFactCatalogContext(ctx, true)
	if err != nil {
		return personenrichment.RequestInput{}, err
	}
	input := personenrichment.RequestInput{
		PersonID: person.ID, PersonRevision: person.Revision, Catalog: catalog, Trigger: lease.Trigger,
	}
	names, err := s.ListPersonNamesContext(ctx, person.ID, true)
	if err != nil {
		return input, err
	}
	for _, name := range names {
		if value := strings.TrimSpace(name.OriginalValue); value != "" {
			input.Names = append(input.Names, personenrichment.IdentityCandidate{
				StableID: name.Envelope.ID, Value: value,
				Primary:    name.Envelope.Pref != nil && *name.Envelope.Pref == 1,
				ActiveFrom: valueEnvelopeActiveFrom(name.Envelope),
			})
		}
	}
	participantRows, err := s.db.QueryContext(ctx, `
		SELECT p.id, p.display_name, p.email_address, p.phone_number, p.created_at
		FROM person_participants pp
		JOIN participants p ON p.id = pp.participant_id
		WHERE pp.person_id = ? ORDER BY p.id`, person.ID)
	if err != nil {
		return input, fmt.Errorf("load person enrichment participant identities: %w", err)
	}
	for participantRows.Next() {
		var id int64
		var displayName, email, phone sql.NullString
		var created nullableTimestamp
		if err := participantRows.Scan(&id, &displayName, &email, &phone, &created); err != nil {
			_ = participantRows.Close()
			return input, fmt.Errorf("scan person enrichment participant identity: %w", err)
		}
		activeFrom := time.Time{}
		if created.Valid {
			activeFrom = created.Time
		}
		if displayName.Valid && strings.TrimSpace(displayName.String) != "" {
			input.Names = append(input.Names, personenrichment.IdentityCandidate{
				StableID: id, Value: displayName.String, ActiveFrom: activeFrom,
			})
		}
		if email.Valid && strings.TrimSpace(email.String) != "" {
			input.Emails = append(input.Emails, personenrichment.IdentityCandidate{
				StableID: id, Value: email.String, ActiveFrom: activeFrom,
			})
		}
		if phone.Valid && strings.TrimSpace(phone.String) != "" {
			input.Phones = append(input.Phones, personenrichment.IdentityCandidate{
				StableID: id, Value: phone.String, ActiveFrom: activeFrom,
			})
		}
	}
	if err := participantRows.Close(); err != nil {
		return input, fmt.Errorf("close person enrichment participant identities: %w", err)
	}
	if err := participantRows.Err(); err != nil {
		return input, fmt.Errorf("iterate person enrichment participant identities: %w", err)
	}
	points, err := s.ListPersonContactPointsContext(ctx, person.ID, true)
	if err != nil {
		return input, err
	}
	for _, point := range points {
		candidate := personenrichment.IdentityCandidate{
			StableID: point.Envelope.ID, Value: point.NormalizedValue,
			Primary:    point.Envelope.Pref != nil && *point.Envelope.Pref == 1,
			ActiveFrom: valueEnvelopeActiveFrom(point.Envelope),
		}
		switch point.AddressKind {
		case ContactAddressEmail:
			input.Emails = append(input.Emails, candidate)
		case ContactAddressPhone:
			input.Phones = append(input.Phones, candidate)
		case ContactAddressURL:
			input.PublicProfileURLs = append(input.PublicProfileURLs, candidate)
		case ContactAddressUsername, ContactAddressIMPP, ContactAddressSocial,
			ContactAddressCalendar, ContactAddressContactURI, ContactAddressOrgDirectory,
			ContactAddressLanguage, ContactAddressProviderIdentity:
			// These classes are not one of the approved enrichment identity primitives.
		}
	}
	employments, err := s.ListEmploymentsContext(ctx, EmploymentFilter{
		PersonID: person.ID, CurrentOnly: true, Limit: MaxEmploymentPageSize,
	})
	if err != nil {
		return input, err
	}
	for _, employment := range employments {
		organization, err := s.GetOrganizationContext(ctx, employment.OrganizationID)
		if err != nil {
			return input, err
		}
		if organization.RetiredAt != nil || strings.TrimSpace(organization.Name) == "" {
			continue
		}
		input.CurrentCompanies = append(input.CurrentCompanies, personenrichment.IdentityCandidate{
			StableID: employment.ID, Value: organization.Name, Primary: employment.IsPrimary,
			ActiveFrom: employment.CreatedAt.UTC(),
		})
	}
	return input, nil
}

func valueEnvelopeActiveFrom(envelope ValueEnvelope) time.Time {
	if envelope.ActiveFrom != nil {
		return envelope.ActiveFrom.UTC()
	}
	return envelope.CreatedAt.UTC()
}

func (s *Store) LoadProviderPersonIDs(
	ctx context.Context, personID int64, providerNamespace string,
) ([]string, error) {
	if personID <= 0 || strings.TrimSpace(providerNamespace) == "" {
		return nil, errors.New("provider identity lookup is invalid")
	}
	kind, _, ok := strings.Cut(providerNamespace, ":")
	if !ok || !validPersonEnrichmentProviderNamespace(providerNamespace, kind) {
		return nil, errors.New("provider identity namespace is invalid")
	}
	rows, err := s.db.QueryContext(ctx, s.Rebind(`SELECT provider_person_id
		FROM person_enrichment_provider_identities
		WHERE person_id = ? AND provider_namespace = ?
		ORDER BY provider_person_id`), personID, providerNamespace)
	if err != nil {
		return nil, fmt.Errorf("load provider person identities: %w", err)
	}
	defer func() { _ = rows.Close() }()
	identities := make([]string, 0)
	for rows.Next() {
		var identity string
		if err := rows.Scan(&identity); err != nil {
			return nil, fmt.Errorf("scan provider person identity: %w", err)
		}
		identities = append(identities, identity)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate provider person identities: %w", err)
	}
	return identities, nil
}
