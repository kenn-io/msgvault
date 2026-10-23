package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/personobs"
)

var ErrObservationConflict = errors.New("person observation revision conflict")

type ObservationSuppression struct {
	ID, PersonID              int64
	ContentKey, Reason, Actor string
	RejectedAt, CreatedAt     time.Time
}

func validObservationContentKey(key string) bool {
	if len(key) != 64 || key != strings.ToLower(key) {
		return false
	}
	_, err := hex.DecodeString(key)
	return err == nil
}
func (s *Store) ListObservationSuppressions(ctx context.Context, personID int64) ([]ObservationSuppression, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,person_id,content_key,reason,actor,rejected_at,created_at FROM person_observation_suppressions WHERE person_id=? AND cleared_at IS NULL ORDER BY rejected_at DESC,id DESC`, personID)
	if err != nil {
		return nil, fmt.Errorf("list observation suppressions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := []ObservationSuppression{}
	for rows.Next() {
		var row ObservationSuppression
		var rejected, created requiredTimestamp
		if err := rows.Scan(&row.ID, &row.PersonID, &row.ContentKey, &row.Reason, &row.Actor, &rejected, &created); err != nil {
			return nil, err
		}
		row.RejectedAt = rejected.Time
		row.CreatedAt = created.Time
		result = append(result, row)
	}
	return result, rows.Err()
}
func (s *Store) AddOwnerObservation(ctx context.Context, in personobs.OwnerInput) (personobs.Observation, error) {
	in.ValidFrom = personFactPortableTimePointer(in.ValidFrom)
	in.ValidUntil = personFactPortableTimePointer(in.ValidUntil)
	if in.Kind == "" {
		in.Kind = personobs.KindState
	}
	if strings.TrimSpace(in.Actor) == "" {
		in.Actor = "owner"
	}
	proposal := personobs.ProposedObservation{PersonID: in.PersonID, Level: personobs.LevelExplicit, Origin: personobs.OriginOwner, Kind: in.Kind, Content: in.Content, SubjectRef: in.SubjectRef, ConfidenceBP: 1000, ModelCategories: in.Categories, Stability: in.Stability, ValidFrom: in.ValidFrom, ValidUntil: in.ValidUntil, OccursOn: in.OccursOn}
	prepared, err := s.PreparePersonObservationBatch(ctx, personobs.BatchInput{PersonID: in.PersonID, Proposals: []personobs.ProposedObservation{proposal}, Actor: in.Actor})
	if err != nil {
		return personobs.Observation{}, err
	}
	if prepared.Items[0].Failure != nil {
		return personobs.Observation{}, prepared.Items[0].Failure
	}
	result, err := retryContendedWrite(ctx, s, "add owner observation", func() (*personobs.Observation, error) {
		var result personobs.Observation
		err := s.withTxContext(ctx, func(tx *loggedTx) error {
			if err := s.lockPersonMutationTx(ctx, tx, in.PersonID); err != nil {
				return err
			}
			cleared, err := tx.ExecContext(ctx, `UPDATE person_observation_suppressions SET cleared_at=? WHERE person_id=? AND content_key=? AND cleared_at IS NULL`, time.Now().UTC(), in.PersonID, prepared.Items[0].ContentKey)
			if err != nil {
				return err
			}
			lifted, err := cleared.RowsAffected()
			if err != nil {
				return err
			}
			batch, err := s.applyPersonObservationBatchTx(ctx, tx, prepared, 0, in.Actor)
			if err != nil {
				return err
			}
			if batch.Items[0].Failure != nil {
				return batch.Items[0].Failure
			}
			id := batch.Items[0].ObservationID
			// An owner repeating a live machine statement takes responsibility for it.
			if _, err := tx.ExecContext(ctx, `UPDATE person_observations SET origin='owner',owner_state='confirmed',generation_id=NULL,program_fingerprint='',directness='direct-other',confidence_bp=1000,mention_count=0,first_seen_at=NULL,last_seen_at=NULL,stability_source='owner',kind=?,stability=?,valid_from=?,valid_until=?,occurs_on=?,support_flag='',status='active',status_reason='' WHERE id=?`, prepared.Items[0].Proposal.Kind, prepared.Items[0].Stability, prepared.Items[0].Proposal.ValidFrom, prepared.Items[0].Proposal.ValidUntil, observationOptionalDate(prepared.Items[0].Proposal.OccursOn), id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM person_observation_evidence WHERE observation_id=?`, id); err != nil {
				return err
			}
			if lifted > 0 {
				if err := s.appendObservationEventTx(ctx, tx, in.PersonID, id, "suppression-lifted", in.Actor, "owner-readded", nil); err != nil {
					return err
				}
			}
			if err := s.notifyObservationSupportChangedTx(ctx, tx, in.PersonID, []int64{id}, "owner-added"); err != nil {
				return err
			}
			result, err = s.loadObservationTx(ctx, tx, id)
			return err
		})
		return &result, err
	})
	if err != nil {
		return personobs.Observation{}, err
	}
	return *result, nil
}
func (s *Store) DecidePersonObservation(ctx context.Context, d personobs.Decision) (personobs.Observation, error) {
	if !slices.Contains([]personobs.DecisionAction{personobs.DecisionActionConfirm, personobs.DecisionActionUnsure, personobs.DecisionActionClear, personobs.DecisionActionReject, personobs.DecisionActionPin, personobs.DecisionActionUnpin, personobs.DecisionActionEnd, personobs.DecisionActionForget}, d.Action) {
		return personobs.Observation{}, &personobs.ValidationError{Field: "action", Reason: "action"}
	}
	if d.Reason != "" && !slices.Contains([]string{"wrong", "not_useful", "outdated", "owner-rejected"}, d.Reason) {
		return personobs.Observation{}, &personobs.ValidationError{Field: "reason", Reason: "reason"}
	}
	if strings.TrimSpace(d.Actor) == "" {
		d.Actor = "owner"
	}
	// Resolve the person without opening a transaction, then recheck it under its
	// lock. A merge may have moved the row in between these two reads.
	row, err := s.GetPersonObservation(ctx, d.ObservationID)
	if err != nil {
		return personobs.Observation{}, err
	}
	result, err := retryContendedWrite(ctx, s, "decide person observation", func() (*personobs.Observation, error) {
		var result personobs.Observation
		err := s.withTxContext(ctx, func(tx *loggedTx) error {
			if err := s.lockPersonMutationTx(ctx, tx, row.PersonID); err != nil {
				return err
			}
			current, err := s.loadObservationTx(ctx, tx, d.ObservationID)
			if err != nil {
				return err
			}
			if current.PersonID != row.PersonID || !current.UpdatedAt.Equal(d.ExpectedUpdatedAt) {
				return ErrObservationConflict
			}
			event := string(d.Action)
			notify := false
			switch d.Action {
			case personobs.DecisionActionConfirm, personobs.DecisionActionUnsure, personobs.DecisionActionClear:
				notify = true
				state := personobs.OwnerStateNone
				event = "cleared"
				if d.Action == personobs.DecisionActionConfirm {
					state = personobs.OwnerStateConfirmed
					event = "confirmed"
				}
				if d.Action == personobs.DecisionActionUnsure {
					state = personobs.OwnerStateUnsure
					event = "unsure"
				}
				if _, err := tx.ExecContext(ctx, `UPDATE person_observations SET owner_state=? WHERE id=?`, state, current.ID); err != nil {
					return err
				}
			case personobs.DecisionActionPin, personobs.DecisionActionUnpin:
				pinned := d.Action == personobs.DecisionActionPin
				event = "unpinned"
				if pinned {
					event = "pinned"
				}
				notify = !pinned
				if _, err := tx.ExecContext(ctx, `UPDATE person_observations SET pinned=? WHERE id=?`, pinned, current.ID); err != nil {
					return err
				}
			case personobs.DecisionActionReject, personobs.DecisionActionForget:
				reason := d.Reason
				if reason == "" {
					reason = "owner-rejected"
				}
				if d.Action == personobs.DecisionActionForget {
					reason = "owner-forgotten"
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO person_observation_suppressions(person_id,content_key,reason,actor,rejected_at) VALUES(?,?,?,?,?) ON CONFLICT (person_id,content_key) WHERE cleared_at IS NULL DO UPDATE SET reason=excluded.reason,actor=excluded.actor,rejected_at=excluded.rejected_at`, current.PersonID, current.ContentKey, reason, d.Actor, time.Now().UTC()); err != nil {
					return err
				}
				if d.Action == personobs.DecisionActionForget {
					dependents, err := personMergeRowIDsTx(ctx, tx, `SELECT derived_id FROM person_observation_premises WHERE premise_id=? ORDER BY derived_id`, current.ID)
					if err != nil {
						return err
					}
					for _, id := range dependents {
						var role personobs.PremiseRole
						if err := tx.QueryRowContext(ctx, `SELECT role FROM person_observation_premises WHERE derived_id=? AND premise_id=?`, id, current.ID).Scan(&role); err != nil {
							return err
						}
						detail := "counter-premise-forgotten"
						if role == personobs.PremiseRoleSupport {
							detail = "support-premise-forgotten"
						}
						if _, err := tx.ExecContext(ctx, `UPDATE person_observations SET premise_changed_at=? WHERE id=?`, time.Now().UTC(), id); err != nil {
							return err
						}
						if err := s.appendObservationEventTx(ctx, tx, current.PersonID, id, "premise-changed", d.Actor, detail, &current.ID); err != nil {
							return err
						}
					}
					if err := s.deletePersonObservationTx(ctx, tx, current.ID); err != nil {
						return err
					}
					if err := s.notifyObservationSupportChangedTx(ctx, tx, current.PersonID, dependents, "forgotten"); err != nil {
						return err
					}
					result = current
					return nil
				}
				if err := s.transitionObservationTx(ctx, tx, current.ID, personobs.StatusRejected, personobs.StatusReasonOwnerRejected, "rejected", d.Actor, reason); err != nil {
					return err
				}
				notify = true
				event = ""
			case personobs.DecisionActionEnd:
				end := personFactPortableTimePointer(d.EndAt)
				if end == nil {
					now := time.Now().UTC()
					end = &now
				}
				if current.ValidFrom != nil && end.Before(*current.ValidFrom) {
					return &personobs.ValidationError{Field: "end_at", Reason: "validity-order"}
				}
				if _, err := tx.ExecContext(ctx, `UPDATE person_observations SET valid_until=? WHERE id=?`, end, current.ID); err != nil {
					return err
				}
				if err := s.transitionObservationTx(ctx, tx, current.ID, personobs.StatusSuperseded, personobs.StatusReasonOwnerSuperseded, "ended", d.Actor, ""); err != nil {
					return err
				}
				notify = true
				event = ""
			}
			if event != "" {
				if err := s.appendObservationEventTx(ctx, tx, current.PersonID, current.ID, event, d.Actor, "", nil); err != nil {
					return err
				}
			}
			if notify {
				if err := s.notifyObservationSupportChangedTx(ctx, tx, current.PersonID, []int64{current.ID}, "owner-decision"); err != nil {
					return err
				}
			}
			result, err = s.loadObservationTx(ctx, tx, current.ID)
			return err
		})
		return &result, err
	})
	if err != nil {
		return personobs.Observation{}, err
	}
	return *result, nil
}
func (s *Store) ClearObservationSuppression(ctx context.Context, personID int64, key, actor string) error {
	if !validObservationContentKey(key) {
		return &personobs.ValidationError{Field: "content_key", Reason: "content-key"}
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockPersonMutationTx(ctx, tx, personID); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE person_observation_suppressions SET cleared_at=? WHERE person_id=? AND content_key=? AND cleared_at IS NULL`, time.Now().UTC(), personID, key)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil || changed == 0 {
			return err
		}
		var id int64
		err = tx.QueryRowContext(ctx, `SELECT id FROM person_observations WHERE person_id=? AND content_key=? ORDER BY id DESC LIMIT 1`, personID, key).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		return s.appendObservationEventTx(ctx, tx, personID, id, "suppression-lifted", actor, "owner-cleared", nil)
	})
}
