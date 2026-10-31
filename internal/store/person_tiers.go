package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/persontiers"
)

// ErrPersonTierRebuilding means the projection is replaying under a new
// timezone or direct limit, so its rows match neither setting.
var ErrPersonTierRebuilding = errors.New("the activity projection is rebuilding under new settings; retry once it finishes")

// PersonTierOptions chooses the rules. Counts are read as of today. The timezone and direct-contact limit come from the projection, since the
// stored rows were built under them. ActivityTimezone and ActivityMaxDirect are
// the configured [activity] values and only decide whether readiness reports
// settings_changed.
type PersonTierOptions struct {
	Config            persontiers.Config
	ActivityTimezone  string
	ActivityMaxDirect int
	// asOf replaces today for tests. Only its calendar date in the projection
	// timezone matters.
	asOf time.Time
}

// PersonTierProjection is the timezone and direct-contact limit the counts
// were built under.
type PersonTierProjection struct {
	Timezone  string `json:"timezone"`
	MaxDirect int    `json:"max_direct"`
}

// PersonTierRow is one saved person in tier 1 or 2.
type PersonTierRow struct {
	PersonID    int64   `json:"person_id"`
	DisplayName *string `json:"display_name"`
	Tier        int     `json:"tier"`
	Rule        string  `json:"rule"`
	Tracked     bool    `json:"tracked"`
	// LastContactDate is the latest day that counted toward the tier.
	LastContactDate *string `json:"last_contact_date"`
}

// PersonTierPreview lists every saved person in tier 1 or 2, tier 1 first and
// the most recent contact first within a tier.
type PersonTierPreview struct {
	People     []PersonTierRow      `json:"people"`
	AsOf       string               `json:"as_of"`
	Readiness  PersonTierReadiness  `json:"readiness"`
	Projection PersonTierProjection `json:"projection"`
}

// PersonTierExplanation shows every count behind one person's tier.
type PersonTierExplanation struct {
	PersonID        int64                        `json:"person_id"`
	DisplayName     *string                      `json:"display_name"`
	Tier            int                          `json:"tier"`
	Rule            string                       `json:"rule"`
	Exclusion       string                       `json:"exclusion"`
	Tracked         bool                         `json:"tracked"`
	LastContactDate *string                      `json:"last_contact_date"`
	AsOf            string                       `json:"as_of"`
	Readiness       PersonTierReadiness          `json:"readiness"`
	Projection      PersonTierProjection         `json:"projection"`
	Signals         persontiers.Signals          `json:"signals"`
	Thresholds      []persontiers.ThresholdCheck `json:"thresholds"`
}

// PreviewPersonTiersContext reads every saved person's counts and returns those
// in tier 1 or 2. It writes nothing.
func (s *Store) PreviewPersonTiersContext(ctx context.Context, options PersonTierOptions) (*PersonTierPreview, error) {
	var preview *PersonTierPreview
	// One snapshot keeps the projection settings and every page of rows from the
	// same moment, so a rebuild that starts mid-read cannot mix generations.
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		plan, err := s.planPersonTiers(ctx, tx, options)
		if err != nil {
			return err
		}
		readiness, err := s.personTierReadiness(ctx, tx)
		if err != nil {
			return err
		}
		preview = &PersonTierPreview{
			People: []PersonTierRow{}, AsOf: plan.asOf.Format(time.DateOnly),
			Readiness: plan.checkSettings(readiness, options), Projection: plan.projection,
		}
		for afterID := int64(0); ; {
			ids, names, err := personTierPage(ctx, tx, afterID)
			if err != nil {
				return err
			}
			if len(ids) == 0 {
				return nil
			}
			afterID = ids[len(ids)-1]
			signals, tracked, err := s.personTierPageData(ctx, tx, ids, plan, options)
			if err != nil {
				return err
			}
			for _, id := range ids {
				d := persontiers.Evaluate(signals[id], options.Config)
				if d.Tier == 3 {
					continue
				}
				preview.People = append(preview.People, PersonTierRow{
					PersonID: id, DisplayName: names[id], Tier: d.Tier, Rule: d.Rule,
					Tracked: tracked[id], LastContactDate: stringPtrOrNil(signals[id].LastDate),
				})
			}
		}
	})
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(preview.People, comparePersonTierRows)
	return preview, nil
}

func comparePersonTierRows(a, b PersonTierRow) int {
	if a.Tier != b.Tier {
		return a.Tier - b.Tier
	}
	return strings.Compare(*b.LastContactDate, *a.LastContactDate)
}

// personTierPageContext returns the next saved people after afterID, in ID
// order, with their display names.
func personTierPage(ctx context.Context, tx *loggedTx, afterID int64) ([]int64, map[int64]*string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id,display_name FROM persons WHERE id>? ORDER BY id LIMIT ?`, afterID, personTierPageSize)
	if err != nil {
		return nil, nil, fmt.Errorf("page people for tiers: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	names := map[int64]*string{}
	for rows.Next() {
		var id int64
		var name sql.NullString
		if err = rows.Scan(&id, &name); err != nil {
			return nil, nil, fmt.Errorf("scan person for tiers: %w", err)
		}
		ids = append(ids, id)
		if name.Valid {
			names[id] = &name.String
		}
	}
	if err = rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate people for tiers: %w", err)
	}
	return ids, names, nil
}

// ExplainPersonTierContext returns the counts and threshold checks behind one
// saved person's tier, including people in tier 3.
func (s *Store) ExplainPersonTierContext(ctx context.Context, personID int64, options PersonTierOptions) (*PersonTierExplanation, error) {
	var explanation *PersonTierExplanation
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		plan, err := s.planPersonTiers(ctx, tx, options)
		if err != nil {
			return err
		}
		explanation = &PersonTierExplanation{
			PersonID: personID, AsOf: plan.asOf.Format(time.DateOnly), Projection: plan.projection,
		}
		var name sql.NullString
		err = tx.QueryRowContext(ctx, `SELECT display_name FROM persons WHERE id=?`, personID).Scan(&name)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("explain person %d: %w", personID, ErrPersonNotFound)
		}
		if err != nil {
			return fmt.Errorf("read person for tier explanation: %w", err)
		}
		if name.Valid {
			explanation.DisplayName = &name.String
		}
		readiness, err := s.personTierReadiness(ctx, tx)
		if err != nil {
			return err
		}
		explanation.Readiness = plan.checkSettings(readiness, options)
		signals, tracked, err := s.personTierPageData(ctx, tx, []int64{personID}, plan, options)
		if err != nil {
			return err
		}
		d := persontiers.Evaluate(signals[personID], options.Config)
		explanation.Tier, explanation.Rule, explanation.Exclusion = d.Tier, d.Rule, d.Exclusion
		explanation.Thresholds = d.Thresholds
		explanation.Signals = signals[personID]
		explanation.Tracked = tracked[personID]
		explanation.LastContactDate = stringPtrOrNil(signals[personID].LastDate)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return explanation, nil
}

// personTierPlan is what a request needs: the as-of date and the settings the
// stored rows were built under.
type personTierPlan struct {
	asOf       time.Time
	projection PersonTierProjection
	// stored is false until the first projection writes a timezone and limit.
	stored bool
}

// planPersonTiers validates the rules, then reads the projection timezone and
// direct limit. While either is changing, the stored rows follow neither the
// old nor the new value, so it refuses. With nothing projected yet, counts use
// UTC and the default limit and readiness reports the missing projection.
func (s *Store) planPersonTiers(ctx context.Context, tx *loggedTx, options PersonTierOptions) (personTierPlan, error) {
	if err := options.Config.Validate(); err != nil {
		return personTierPlan{}, err
	}
	zone, err := s.activityTimezoneTransitionQueryContext(ctx, tx)
	if err != nil {
		return personTierPlan{}, err
	}
	limit, err := s.activityDirectLimitTransitionQueryContext(ctx, tx)
	if err != nil {
		return personTierPlan{}, err
	}
	if zone.Active || limit.Active {
		return personTierPlan{}, ErrPersonTierRebuilding
	}
	plan := personTierPlan{
		projection: PersonTierProjection{Timezone: zone.Target, MaxDirect: limit.Target},
		stored:     zone.Target != "" && limit.Target > 0,
	}
	if plan.projection.Timezone == "" {
		plan.projection.Timezone = "UTC"
	}
	if plan.projection.MaxDirect <= 0 {
		plan.projection.MaxDirect = DefaultMaxDirectActivityCounterparts
	}
	location, err := time.LoadLocation(plan.projection.Timezone)
	if err != nil {
		return personTierPlan{}, fmt.Errorf("load projection timezone: %w", err)
	}
	at := options.asOf
	if at.IsZero() {
		at = time.Now()
	}
	local := at.In(location)
	plan.asOf = time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location)
	return plan, nil
}

// checkSettings adds a settings_changed reason when the configured [activity]
// settings differ from the ones the projection was built under, since the
// counts follow the stored ones until the projection is rebuilt.
func (p personTierPlan) checkSettings(r PersonTierReadiness, options PersonTierOptions) PersonTierReadiness {
	if !p.stored || (options.ActivityTimezone == p.projection.Timezone && options.ActivityMaxDirect == p.projection.MaxDirect) {
		return r
	}
	r.Ready = false
	r.Reasons = append(r.Reasons, fmt.Sprintf("settings_changed: stored timezone %s and direct limit %d, configured %s and %d",
		p.projection.Timezone, p.projection.MaxDirect, options.ActivityTimezone, options.ActivityMaxDirect))
	return r
}

func (s *Store) personTierPageData(ctx context.Context, tx *loggedTx, ids []int64, plan personTierPlan, options PersonTierOptions) (map[int64]persontiers.Signals, map[int64]bool, error) {
	signals, err := s.personTierSignals(ctx, tx, ids, plan.asOf, options.Config, plan.projection.MaxDirect)
	if err != nil {
		return nil, nil, err
	}
	tracked, err := trackedPersonIDs(ctx, tx, ids)
	return signals, tracked, err
}

func trackedPersonIDs(ctx context.Context, tx *loggedTx, ids []int64) (map[int64]bool, error) {
	result := make(map[int64]bool, len(ids))
	rows, err := tx.QueryContext(ctx,
		`SELECT person_id FROM person_tracking WHERE person_id IN (`+int64Placeholders(len(ids))+`)`,
		personMergeSnapshotIDArgs(ids)...)
	if err != nil {
		return nil, fmt.Errorf("read person tracking for tiers: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan person tracking for tiers: %w", err)
		}
		result[id] = true
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate person tracking for tiers: %w", err)
	}
	return result, nil
}
