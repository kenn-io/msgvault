package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.kenn.io/msgvault/internal/personenrichment"
)

const (
	PersonTierProvenanceManual = "manual"
	PersonTierProvenanceAuto   = "auto"
	PersonTierLaneTracking     = "tracking"
	PersonTierLaneBriefs       = "briefs"
	PersonTierLaneEnrichment   = "enrichment"
)

var (
	ErrPersonTierManualGrant   = errors.New("person tier lane is granted manually")
	ErrPersonTierManualRunOpen = errors.New("person has an open manual enrichment target")
)

// PersonTierOptOut is a durable decision that restrains tier automation.
type PersonTierOptOut struct {
	ID        int64     `json:"id"`
	PersonID  int64     `json:"person_id"`
	Lane      string    `json:"lane"`
	Actor     string    `json:"actor"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
}

// personTierGrantOptions keeps manual intent separate from automatic grants.
// At is optional and lets apply use its frozen evaluation time.
type personTierGrantOptions struct {
	Provenance   string
	Actor        string
	RecordOptOut bool
	At           time.Time
}

func (s *Store) normalizePersonTierGrantOptions(opts personTierGrantOptions) (personTierGrantOptions, error) {
	if opts.Provenance == "" {
		opts.Provenance = PersonTierProvenanceManual
	}
	if opts.Provenance != PersonTierProvenanceManual && opts.Provenance != PersonTierProvenanceAuto {
		return opts, fmt.Errorf("person tier grant: invalid provenance %q", opts.Provenance)
	}
	if opts.Provenance == PersonTierProvenanceAuto && opts.RecordOptOut {
		return opts, errors.New("person tier grant: automatic transitions cannot record opt-outs")
	}
	opts.Actor = strings.TrimSpace(opts.Actor)
	if opts.At.IsZero() {
		opts.At = s.personEnrichmentTime()
	} else {
		opts.At = opts.At.UTC()
	}
	return opts, nil
}

func validPersonTierLane(lane string) bool {
	return lane == PersonTierLaneTracking || lane == PersonTierLaneBriefs || lane == PersonTierLaneEnrichment
}

func (s *Store) personTierOptedOutTx(ctx context.Context, tx *loggedTx, personID int64, lane string) (bool, error) {
	var optedOut bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM person_tier_opt_outs WHERE person_id=? AND lane=?)`, personID, lane).Scan(&optedOut)
	if err != nil {
		return false, fmt.Errorf("read person %d tier %s opt-out: %w", personID, lane, err)
	}
	return optedOut, nil
}

func (s *Store) recordPersonTierOptOutTx(ctx context.Context, tx *loggedTx, personID int64, lane, actor, reason string, at time.Time) error {
	if !validPersonTierLane(lane) {
		return fmt.Errorf("person tier opt-out: invalid lane %q", lane)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO person_tier_opt_outs(person_id,lane,actor,reason,created_at)
 VALUES(?,?,?,?,?) ON CONFLICT(person_id,lane) DO NOTHING`, personID, lane, actor, reason, at)
	if err != nil {
		return fmt.Errorf("record person %d tier %s opt-out: %w", personID, lane, err)
	}
	return nil
}

func (s *Store) clearPersonTierOptOutTx(ctx context.Context, tx *loggedTx, personID int64, lane string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM person_tier_opt_outs WHERE person_id=? AND lane=?`, personID, lane); err != nil {
		return fmt.Errorf("clear person %d tier %s opt-out: %w", personID, lane, err)
	}
	return nil
}

// SetPersonTierOptOutContext changes one automation preference and reconciles
// existing automatic grants in the same authority transaction.
func (s *Store) SetPersonTierOptOutContext(ctx context.Context, personID int64, lane string, optedOut bool, actor, reason string) error {
	if !validPersonTierLane(lane) {
		return fmt.Errorf("person tier opt-out: invalid lane %q", lane)
	}
	actor, reason = strings.TrimSpace(actor), strings.TrimSpace(reason)
	if optedOut && actor == "" {
		return errors.New("person tier opt-out: actor is required")
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockPersonEnrichmentAuthorityMutationTx(ctx, tx); err != nil {
			return err
		}
		if err := s.lockProfileIdentityKeyTxContext(ctx, tx, "person-fact-generation", personID); err != nil {
			return err
		}
		if _, err := lockPersonEnrichmentPersonTx(ctx, tx, s.dialect, personID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("person tier opt-out %d: %w", personID, ErrPersonNotFound)
			}
			return err
		}
		before, err := s.personEnrichmentScheduledEligibleTx(ctx, tx, personID)
		if err != nil {
			return err
		}
		if !optedOut {
			if err := s.clearPersonTierOptOutTx(ctx, tx, personID, lane); err != nil {
				return err
			}
			after, err := s.personEnrichmentScheduledEligibleTx(ctx, tx, personID)
			if err != nil {
				return err
			}
			if lane == PersonTierLaneEnrichment && !before && after {
				return s.publishPersonEnrichmentTx(ctx, tx, personID, personenrichment.TriggerTracked, "opt-in:"+uuid.NewString(), s.personEnrichmentTime())
			}
			return nil
		}
		switch lane {
		case PersonTierLaneTracking:
			tracking, err := s.getPersonTrackingTx(ctx, tx, personID)
			if err != nil {
				return err
			}
			if tracking.Provenance == PersonTierProvenanceManual {
				return fmt.Errorf("%w: person %d is tracked manually; run `person untrack` first", ErrPersonTierManualGrant, personID)
			}
			if err := s.refusePersonTierTrackingRemovalWithManualRunTx(ctx, tx, personID); err != nil {
				return err
			}
		case PersonTierLaneBriefs:
			brief, err := s.getPersonBriefEnrollmentTx(ctx, tx, personID)
			if err != nil {
				return err
			}
			if brief.Provenance == PersonTierProvenanceManual {
				return fmt.Errorf("%w: person %d is enrolled manually; run `person brief unenroll` first", ErrPersonTierManualGrant, personID)
			}
		}
		if err := s.recordPersonTierOptOutTx(ctx, tx, personID, lane, actor, reason, s.personEnrichmentTime()); err != nil {
			return err
		}
		return s.reconcilePersonTierOptOutsTx(ctx, tx, personID)
	})
}

func (s *Store) refusePersonTierTrackingRemovalWithManualRunTx(ctx context.Context, tx *loggedTx, personID int64) error {
	runID, err := s.openPersonManualEnrichmentRunTx(ctx, tx, personID)
	if err != nil {
		return err
	}
	if runID != "" {
		return fmt.Errorf("%w: person %d, run %s", ErrPersonTierManualRunOpen, personID, runID)
	}
	return nil
}

// reconcilePersonTierOptOutsTx also handles merge unions: manual grants remain
// valid while the unioned opt-out continues to restrain automation.
func (s *Store) reconcilePersonTierOptOutsTx(ctx context.Context, tx *loggedTx, personID int64) error {
	trackingOptOut, err := s.personTierOptedOutTx(ctx, tx, personID, PersonTierLaneTracking)
	if err != nil {
		return err
	}
	if trackingOptOut {
		if _, err := s.setPersonTrackingWithOptionsTx(ctx, tx, personID, false, personTierGrantOptions{Provenance: PersonTierProvenanceAuto}); err != nil {
			return err
		}
	}
	briefsOptOut, err := s.personTierOptedOutTx(ctx, tx, personID, PersonTierLaneBriefs)
	if err != nil {
		return err
	}
	if briefsOptOut || trackingOptOut {
		if _, err := s.setPersonBriefEnrollmentWithOptionsTx(ctx, tx, personID, false, false, personTierGrantOptions{Provenance: PersonTierProvenanceAuto}); err != nil {
			return err
		}
	}
	enrichmentOptOut, err := s.personTierOptedOutTx(ctx, tx, personID, PersonTierLaneEnrichment)
	if err != nil {
		return err
	}
	if enrichmentOptOut {
		return s.cancelScheduledPersonEnrichmentTx(ctx, tx, personID)
	}
	return nil
}

func (s *Store) ListPersonTierOptOutsContext(ctx context.Context, personID int64) ([]PersonTierOptOut, error) {
	// Match the other per-person readers: unknown people are errors, not empty lists.
	if _, err := s.GetPersonTrackingContext(ctx, personID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, s.Rebind(`SELECT id,person_id,lane,actor,reason,created_at FROM person_tier_opt_outs WHERE person_id=? ORDER BY lane`), personID)
	if err != nil {
		return nil, fmt.Errorf("list person %d tier opt-outs: %w", personID, err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]PersonTierOptOut, 0)
	for rows.Next() {
		var item PersonTierOptOut
		var at nullableTimestamp
		if err := rows.Scan(&item.ID, &item.PersonID, &item.Lane, &item.Actor, &item.Reason, &at); err != nil {
			return nil, fmt.Errorf("scan person tier opt-out: %w", err)
		}
		item.CreatedAt = at.Time
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate person tier opt-outs: %w", err)
	}
	return result, nil
}
