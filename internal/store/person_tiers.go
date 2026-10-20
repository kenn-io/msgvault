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

// ErrInvalidPersonTierRequest marks a tier request with a bad tier filter,
// timezone, config, or person ID.
var ErrInvalidPersonTierRequest = errors.New("invalid people tier request")

// PersonTierOptions chooses the rules and the date the counts are read at.
// The timezone and direct-contact limit come from the projection, since the
// stored rows were built under them. ActivityTimezone and ActivityMaxDirect are
// the configured [activity] values and only decide whether the result is
// provisional.
type PersonTierOptions struct {
	Config            persontiers.Config
	ActivityTimezone  string
	ActivityMaxDirect int
	// AsOf defaults to now. Only its calendar date in the projection timezone
	// matters.
	AsOf time.Time
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
	// LastContactDate is a calendar date in the projection timezone.
	LastContactDate *string `json:"last_contact_date"`
}

// PersonTierPreview lists every saved person in tier 1 or 2, tier 1 first and
// the most recent contact first within a tier.
type PersonTierPreview struct {
	People     []PersonTierRow      `json:"people"`
	AsOf       string               `json:"as_of"`
	Readiness  PersonTierReadiness  `json:"readiness"`
	Projection PersonTierProjection `json:"projection"`
	// Provisional means the configured [activity] settings differ from the
	// projection's, so the counts follow the stored settings until it rebuilds.
	Provisional bool `json:"provisional"`
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
	Provisional     bool                         `json:"provisional"`
	Signals         persontiers.Signals          `json:"signals"`
	Thresholds      []persontiers.ThresholdCheck `json:"thresholds"`
}

// PreviewPersonTiersContext reads every saved person's counts and returns those
// in tier 1 or 2, or only in the given tier when tier is 1 or 2. It writes
// nothing.
func (s *Store) PreviewPersonTiersContext(ctx context.Context, options PersonTierOptions, tier int) (*PersonTierPreview, error) {
	if tier < 0 || tier > 2 {
		return nil, fmt.Errorf("%w: tier must be 1 or 2", ErrInvalidPersonTierRequest)
	}
	plan, err := s.planPersonTiersContext(ctx, options)
	if err != nil {
		return nil, err
	}
	readiness, err := s.PersonTierReadinessContext(ctx)
	if err != nil {
		return nil, err
	}
	preview := &PersonTierPreview{
		People: []PersonTierRow{}, AsOf: plan.asOf.Format(time.DateOnly), Readiness: readiness,
		Projection: plan.projection, Provisional: plan.provisional,
	}
	for afterID := int64(0); ; {
		ids, names, err := s.personTierPageContext(ctx, afterID)
		if err != nil {
			return nil, err
		}
		if len(ids) == 0 {
			break
		}
		afterID = ids[len(ids)-1]
		signals, err := s.PersonTierSignalsContext(ctx, ids, plan.asOf, options.Config, plan.projection.MaxDirect)
		if err != nil {
			return nil, err
		}
		lastContacts, err := s.personTierLastContactsContext(ctx, ids, plan.location)
		if err != nil {
			return nil, err
		}
		tracked, err := s.trackedPersonIDsContext(ctx, ids)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			d := persontiers.Evaluate(signals[id], options.Config)
			if d.Tier == 3 || (tier != 0 && d.Tier != tier) {
				continue
			}
			preview.People = append(preview.People, PersonTierRow{
				PersonID: id, DisplayName: names[id], Tier: d.Tier, Rule: d.Rule,
				Tracked: tracked[id], LastContactDate: lastContacts[id],
			})
		}
	}
	slices.SortStableFunc(preview.People, comparePersonTierRows)
	return preview, nil
}

func comparePersonTierRows(a, b PersonTierRow) int {
	if a.Tier != b.Tier {
		return a.Tier - b.Tier
	}
	switch {
	case a.LastContactDate == nil && b.LastContactDate != nil:
		return 1
	case a.LastContactDate != nil && b.LastContactDate == nil:
		return -1
	case a.LastContactDate != nil && b.LastContactDate != nil:
		return strings.Compare(*b.LastContactDate, *a.LastContactDate)
	}
	return 0
}

// personTierPageContext returns the next saved people after afterID, in ID
// order, with their display names.
func (s *Store) personTierPageContext(ctx context.Context, afterID int64) ([]int64, map[int64]*string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id,display_name FROM persons WHERE id>? ORDER BY id LIMIT ?`, afterID, personTierSignalPageSize)
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
	if personID <= 0 {
		return nil, fmt.Errorf("%w: person ID must be positive", ErrInvalidPersonTierRequest)
	}
	plan, err := s.planPersonTiersContext(ctx, options)
	if err != nil {
		return nil, err
	}
	explanation := &PersonTierExplanation{
		PersonID: personID, AsOf: plan.asOf.Format(time.DateOnly),
		Projection: plan.projection, Provisional: plan.provisional,
	}
	var name sql.NullString
	err = s.db.QueryRowContext(ctx, `SELECT display_name FROM persons WHERE id=?`, personID).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("explain person %d: %w", personID, ErrPersonNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("read person for tier explanation: %w", err)
	}
	if name.Valid {
		explanation.DisplayName = &name.String
	}
	if explanation.Readiness, err = s.PersonTierReadinessContext(ctx); err != nil {
		return nil, err
	}
	signals, err := s.PersonTierSignalsContext(ctx, []int64{personID}, plan.asOf, options.Config, plan.projection.MaxDirect)
	if err != nil {
		return nil, err
	}
	lastContacts, err := s.personTierLastContactsContext(ctx, []int64{personID}, plan.location)
	if err != nil {
		return nil, err
	}
	tracked, err := s.trackedPersonIDsContext(ctx, []int64{personID})
	if err != nil {
		return nil, err
	}
	d := persontiers.Evaluate(signals[personID], options.Config)
	explanation.Tier, explanation.Rule, explanation.Exclusion = d.Tier, d.Rule, d.Exclusion
	explanation.Thresholds = d.Thresholds
	explanation.Signals = signals[personID]
	explanation.Tracked = tracked[personID]
	explanation.LastContactDate = lastContacts[personID]
	return explanation, nil
}

// personTierPlan is what a request needs once the config is validated: the
// as-of date and the settings the stored counts were built under.
type personTierPlan struct {
	asOf        time.Time
	location    *time.Location
	projection  PersonTierProjection
	provisional bool
}

// planPersonTiersContext validates the config once and reads the projection
// timezone and direct limit. A missing timezone means nothing is projected
// yet; readiness reports that, and counts use UTC and the default limit.
func (s *Store) planPersonTiersContext(ctx context.Context, options PersonTierOptions) (personTierPlan, error) {
	if err := options.Config.Validate(); err != nil {
		return personTierPlan{}, fmt.Errorf("%w: %w", ErrInvalidPersonTierRequest, err)
	}
	zone, err := s.ActivityTimezoneTransitionContext(ctx)
	if err != nil {
		return personTierPlan{}, err
	}
	limit, err := s.ActivityDirectLimitTransitionContext(ctx)
	if err != nil {
		return personTierPlan{}, err
	}
	plan := personTierPlan{projection: PersonTierProjection{Timezone: zone.Target, MaxDirect: limit.Target}}
	if plan.projection.Timezone == "" {
		plan.projection.Timezone = "UTC"
	}
	if plan.projection.MaxDirect <= 0 {
		plan.projection.MaxDirect = DefaultMaxDirectActivityCounterparts
	}
	if plan.location, err = time.LoadLocation(plan.projection.Timezone); err != nil {
		return personTierPlan{}, fmt.Errorf("load projection timezone: %w", err)
	}
	plan.provisional = (zone.Target != "" && options.ActivityTimezone != "" && options.ActivityTimezone != zone.Target) ||
		(limit.Target > 0 && options.ActivityMaxDirect > 0 && options.ActivityMaxDirect != limit.Target)
	at := options.AsOf
	if at.IsZero() {
		at = time.Now()
	}
	local := at.In(plan.location)
	plan.asOf = time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, plan.location)
	return plan, nil
}

// personTierLastContactsContext returns each person's last contact as a date in
// the projection timezone, the same calendar the windows use.
func (s *Store) personTierLastContactsContext(ctx context.Context, ids []int64, loc *time.Location) (map[int64]*string, error) {
	result := make(map[int64]*string, len(ids))
	rows, err := s.db.QueryContext(ctx,
		`SELECT person_id,last_contact_at FROM person_contact_state WHERE person_id IN (`+
			int64Placeholders(len(ids))+`)`, personMergeSnapshotIDArgs(ids)...)
	if err != nil {
		return nil, fmt.Errorf("read person tier contact state: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		var last nullableTimestamp
		if err = rows.Scan(&id, &last); err != nil {
			return nil, fmt.Errorf("scan person tier contact state: %w", err)
		}
		if at := nullableTimePointer(last); at != nil {
			date := at.In(loc).Format(time.DateOnly)
			result[id] = &date
		}
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate person tier contact state: %w", err)
	}
	return result, nil
}

func (s *Store) trackedPersonIDsContext(ctx context.Context, ids []int64) (map[int64]bool, error) {
	result := make(map[int64]bool, len(ids))
	rows, err := s.db.QueryContext(ctx,
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
