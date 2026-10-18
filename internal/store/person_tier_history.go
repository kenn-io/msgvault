package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strconv"
	"time"

	"go.kenn.io/msgvault/internal/persontiers"
)

type PersonTierEvent struct {
	ID                 int64               `json:"id"`
	PersonID           int64               `json:"person_id"`
	MergedFromPersonID *int64              `json:"merged_from_person_id"`
	FromTier           *int                `json:"from_tier"`
	ToTier             int                 `json:"to_tier"`
	Reason             string              `json:"reason"`
	Mode               string              `json:"mode"`
	Lanes              map[string]string   `json:"lanes"`
	Signals            persontiers.Signals `json:"signals"`
	Readiness          PersonTierReadiness `json:"readiness"`
	Rule               string              `json:"rule"`
	RuleVersion        int                 `json:"rule_version"`
	Actor              string              `json:"actor"`
	RunID              string              `json:"run_id"`
	CreatedAt          time.Time           `json:"created_at"`
}

func (s *Store) insertPersonTierEventTx(ctx context.Context, tx *loggedTx, run PersonTierRun, input PersonTierInput, d persontiers.Decision, reason, mode, actor string, now time.Time) error {
	lanes, err := json.Marshal(d.Lanes, json.Deterministic(true))
	if err != nil {
		return err
	}
	signals, err := json.Marshal(struct {
		persontiers.Signals

		Readiness PersonTierReadiness `json:"readiness"`
	}{input.Signals, input.Readiness}, json.Deterministic(true))
	if err != nil {
		return err
	}
	// The authority mutation gate serializes this replay check with insertion.
	var exists bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM person_tier_events WHERE person_id=? AND run_id=? AND mode=? AND reason=?)`, input.PersonID, run.RunID, mode, reason).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	from := input.Prior.Tier
	if from == 0 {
		from = 3
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO person_tier_events(person_id,from_tier,to_tier,reason,mode,lanes_json,signals_json,rule,rule_version,actor,run_id,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, input.PersonID, from, d.Tier, reason, mode, string(lanes), string(signals), d.Rule, persontiers.RuleVersion, actor, run.RunID, now)
	return err
}
func (s *Store) PersonTierHistoryContext(ctx context.Context, personID int64, limit, offset int) ([]PersonTierEvent, error) {
	if limit == 0 {
		limit = 20
	}
	if personID < 0 || limit < 1 || limit > 200 || offset < 0 {
		return nil, ErrInvalidPersonTierRequest
	}
	query := `SELECT id,person_id,merged_from_person_id,from_tier,to_tier,reason,mode,lanes_json,signals_json,rule,rule_version,actor,run_id,created_at FROM person_tier_events`
	args := []any{}
	if personID > 0 {
		query += ` WHERE person_id=?`
		args = append(args, personID)
	}
	query += ` ORDER BY id DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)
	rows, err := s.db.QueryContext(ctx, s.Rebind(query), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	events := []PersonTierEvent{}
	for rows.Next() {
		var e PersonTierEvent
		var from, merged sql.NullInt64
		var lanes, signals string
		var at nullableTimestamp
		if err = rows.Scan(&e.ID, &e.PersonID, &merged, &from, &e.ToTier, &e.Reason, &e.Mode, &lanes, &signals, &e.Rule, &e.RuleVersion, &e.Actor, &e.RunID, &at); err != nil {
			return nil, err
		}
		if merged.Valid {
			v := merged.Int64
			e.MergedFromPersonID = &v
		}
		if from.Valid {
			v := int(from.Int64)
			e.FromTier = &v
		}
		e.CreatedAt = at.Time
		if err = json.Unmarshal([]byte(lanes), &e.Lanes); err != nil {
			return nil, err
		}
		var detail struct {
			persontiers.Signals

			Readiness PersonTierReadiness `json:"readiness"`
		}
		if err = json.Unmarshal([]byte(signals), &detail); err != nil {
			return nil, err
		}
		e.Signals = detail.Signals
		e.Readiness = detail.Readiness
		events = append(events, e)
	}
	return events, rows.Err()
}
func personTierFailureKey(id int64) string {
	return "people_tiers_failure:" + strconv.FormatInt(id, 10)
}
func (s *Store) recordPersonTierFailure(ctx context.Context, run PersonTierRun, input PersonTierInput, actor string) error {
	return s.recordPersonTierFailureWithCheckpointContext(ctx, run, input, actor, "")
}

func (s *Store) recordPersonTierFailureWithCheckpointContext(ctx context.Context, run PersonTierRun, input PersonTierInput, actor, checkpointCursor string) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockPersonEnrichmentAuthorityMutationTx(ctx, tx); err != nil {
			return err
		}
		if err := s.checkPersonTierRunTx(ctx, tx, run); err != nil {
			return err
		}
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM persons WHERE id=?)`, input.PersonID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return s.persistPersonTierRunCheckpointTx(ctx, tx, run, checkpointCursor)
		}
		var value string
		err := tx.QueryRowContext(ctx, `SELECT value FROM archive_metadata WHERE key=?`, personTierFailureKey(input.PersonID)).Scan(&value)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var failure struct {
			RunID string `json:"run_id"`
			Count int    `json:"count"`
		}
		if value != "" {
			if err = json.Unmarshal([]byte(value), &failure); err != nil {
				return fmt.Errorf("read people tier failure streak: %w", err)
			}
		}
		if failure.RunID == run.RunID {
			return s.persistPersonTierRunCheckpointTx(ctx, tx, run, checkpointCursor)
		}
		failure.RunID = run.RunID
		failure.Count++
		if err = putPersonTierMetadataTx(ctx, tx, personTierFailureKey(input.PersonID), failure); err != nil {
			return err
		}
		if failure.Count >= 3 {
			d := input.Decision
			if d.Tier == 0 {
				d.Tier = 3
			}
			if err := s.insertPersonTierEventTx(ctx, tx, run, input, d, "apply_failed", runEventMode(run), actor, s.personEnrichmentTime()); err != nil {
				return err
			}
		}
		return s.persistPersonTierRunCheckpointTx(ctx, tx, run, checkpointCursor)
	})
}
func runEventMode(run PersonTierRun) string {
	if run.Mode == "record_only" {
		return "preview"
	}
	return "apply"
}
