package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/persontiers"
)

var ErrPersonTierRunSuperseded = errors.New("people tier run has been superseded")
var ErrInvalidPersonTierRequest = errors.New("invalid people tier request")

var errPersonTierProjectionMismatch = errors.New("people tier projection settings do not match")

// Projected dates and direct evidence must use the requested policy. Legacy
// archives without a recorded policy remain readable until projection records it.
func (s *Store) checkPersonTierProjectionContext(ctx context.Context, queryer contextRowQuerier, timezone string, maxDirect int) error {
	zone, err := s.activityTimezoneTransitionQueryContext(ctx, queryer)
	if err != nil {
		return err
	}
	if zone.Target != "" && zone.Target != timezone {
		return fmt.Errorf("%w: projected timezone %q differs from requested %q", errPersonTierProjectionMismatch, zone.Target, timezone)
	}
	direct, err := s.activityDirectLimitTransitionQueryContext(ctx, queryer)
	if err != nil {
		return err
	}
	if direct.Target != 0 && direct.Target != maxDirect {
		return fmt.Errorf("%w: projected direct-counterpart limit %d differs from requested %d", errPersonTierProjectionMismatch, direct.Target, maxDirect)
	}
	return nil
}

func (s *Store) validatePersonTierProjectionRequestContext(ctx context.Context, req PersonTierPageRequest) error {
	err := s.checkPersonTierProjectionContext(ctx, s.db, req.Timezone, req.MaxDirect)
	if errors.Is(err, errPersonTierProjectionMismatch) {
		return fmt.Errorf("%w: %w; wait for matching activity projection", ErrInvalidPersonTierRequest, err)
	}
	return err
}

type PersonTierPageRequest struct {
	Config      persontiers.Config `json:"-"`
	Timezone    string             `json:"-"`
	MaxDirect   int                `json:"-"`
	AsOf        time.Time          `json:"-"`
	Limit       int                `json:"limit"`
	Cursor      string             `json:"cursor"`
	Tier        int                `json:"tier"`
	ChangesOnly bool               `json:"changes_only"`
}

type PersonTierInput struct {
	PersonID  int64                   `json:"person_id"`
	Name      string                  `json:"name"`
	Signals   persontiers.Signals     `json:"signals"`
	Prior     persontiers.Prior       `json:"prior"`
	Manual    persontiers.ManualState `json:"manual"`
	Readiness PersonTierReadiness     `json:"readiness"`
	Decision  persontiers.Decision    `json:"decision"`
	Tracking  *PersonTracking         `json:"tracking"`
	Briefs    *PersonBriefEnrollment  `json:"briefs"`
	OptOuts   []PersonTierOptOut      `json:"opt_outs"`
}

type PersonTierPage struct {
	People     []PersonTierInput   `json:"people"`
	NextCursor string              `json:"next_cursor"`
	RunID      string              `json:"run_id"`
	AsOf       time.Time           `json:"as_of"`
	Readiness  PersonTierReadiness `json:"readiness"`
	Counts     map[string]int      `json:"counts"`
}

type personTierCursor struct {
	AfterID     int64     `json:"after_id"`
	AsOf        time.Time `json:"as_of"`
	Fingerprint string    `json:"fingerprint"`
	RunID       string    `json:"run_id"`
	Timezone    string    `json:"timezone"`
	MaxDirect   int       `json:"max_direct"`
}

func encodePersonTierCursor(c personTierCursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}
func decodePersonTierCursor(value string) (personTierCursor, error) {
	var c personTierCursor
	if value == "" {
		return c, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(value)
	if err == nil {
		err = json.Unmarshal(b, &c)
	}
	if err != nil || c.AfterID < 1 || c.AsOf.IsZero() {
		return c, fmt.Errorf("%w: malformed cursor", ErrInvalidPersonTierRequest)
	}
	return c, nil
}
func normalizePersonTierRequest(req PersonTierPageRequest) (PersonTierPageRequest, error) {
	if err := req.Config.Validate(); err != nil {
		return req, fmt.Errorf("%w: %w", ErrInvalidPersonTierRequest, err)
	}
	if req.Limit == 0 {
		req.Limit = 200
	}
	if req.Limit < 1 || req.Limit > 500 || req.Tier < 0 || req.Tier > 3 {
		return req, fmt.Errorf("%w: limit must be 1..500 and tier 1..3", ErrInvalidPersonTierRequest)
	}
	if req.Timezone == "" {
		req.Timezone = "UTC"
	}
	loc, err := time.LoadLocation(req.Timezone)
	if err != nil {
		return req, fmt.Errorf("%w: invalid timezone", ErrInvalidPersonTierRequest)
	}
	if req.AsOf.IsZero() {
		req.AsOf = time.Now()
	}
	req.AsOf = req.AsOf.In(loc)
	req.AsOf = time.Date(req.AsOf.Year(), req.AsOf.Month(), req.AsOf.Day(), 0, 0, 0, 0, loc)
	if req.MaxDirect <= 0 {
		req.MaxDirect = DefaultMaxDirectActivityCounterparts
	}
	return req, nil
}

// PlanPersonTiersContext is read-only. Page predicates apply after evaluation;
// the cursor always advances through the scanned population, including filtered rows.
func (s *Store) PlanPersonTiersContext(ctx context.Context, req PersonTierPageRequest) (*PersonTierPage, error) {
	if req.AsOf.IsZero() {
		req.AsOf = s.personEnrichmentTime()
	}
	req, err := normalizePersonTierRequest(req)
	if err != nil {
		return nil, err
	}
	cursor, err := decodePersonTierCursor(req.Cursor)
	if err != nil {
		return nil, err
	}
	if req.Cursor != "" {
		if cursor.Fingerprint != req.Config.Fingerprint() || cursor.Timezone != req.Timezone || cursor.MaxDirect != req.MaxDirect {
			return nil, fmt.Errorf("%w: configuration changed; restart preview", ErrInvalidPersonTierRequest)
		}
		loc, _ := time.LoadLocation(req.Timezone)
		req.AsOf = cursor.AsOf.In(loc)
	}
	if err := s.validatePersonTierProjectionRequestContext(ctx, req); err != nil {
		return nil, err
	}
	readiness, err := s.PersonTierReadinessContext(ctx)
	if err != nil {
		return nil, err
	}
	page := &PersonTierPage{People: []PersonTierInput{}, AsOf: req.AsOf, Readiness: readiness, Counts: map[string]int{}}
	rows, err := s.db.QueryContext(ctx, s.Rebind(`SELECT id,COALESCE(display_name,'') FROM persons WHERE id>? ORDER BY id LIMIT ?`), cursor.AfterID, req.Limit+1)
	if err != nil {
		return nil, fmt.Errorf("page people for tiers: %w", err)
	}
	type person struct {
		id   int64
		name string
	}
	people := []person{}
	for rows.Next() {
		var p person
		if err = rows.Scan(&p.id, &p.name); err != nil {
			_ = rows.Close()
			return nil, err
		}
		people = append(people, p)
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	more := len(people) > req.Limit
	if more {
		people = people[:req.Limit]
	}
	ids := make([]int64, len(people))
	for i, p := range people {
		ids[i] = p.id
	}
	signals, err := s.PersonTierSignalsContext(ctx, ids, req.AsOf, req.Config, req.MaxDirect)
	if err != nil {
		return nil, err
	}
	for _, p := range people {
		input := PersonTierInput{PersonID: p.id, Name: p.name, Signals: signals[p.id], Readiness: readiness}
		err = s.withTxContext(ctx, func(tx *loggedTx) error { return s.loadPersonTierInputTx(ctx, tx, &input, req.AsOf.Location()) })
		if err != nil {
			return nil, err
		}
		input.Decision = persontiers.Evaluate(input.Signals, req.Config, input.Prior, input.Manual, req.AsOf, readiness.Ready)
		countPersonTierDecision(page.Counts, input.Decision)
		if req.Tier != 0 && input.Decision.ComputedTier != req.Tier {
			continue
		}
		if req.ChangesOnly && !personTierDecisionChanges(input.Decision) {
			continue
		}
		page.People = append(page.People, input)
	}
	if more {
		page.NextCursor = encodePersonTierCursor(personTierCursor{AfterID: people[len(people)-1].id, AsOf: req.AsOf, Fingerprint: req.Config.Fingerprint(), RunID: cursor.RunID, Timezone: req.Timezone, MaxDirect: req.MaxDirect})
	}
	if err := s.validatePersonTierProjectionRequestContext(ctx, req); err != nil {
		return nil, err
	}
	return page, nil
}
func personTierDecisionChanges(d persontiers.Decision) bool {
	if d.Reason != "" || d.DeferredReason != "" {
		return true
	}
	for _, action := range d.Lanes {
		if action == "created" || action == "removed" {
			return true
		}
	}
	return false
}
func countPersonTierDecision(counts map[string]int, d persontiers.Decision) {
	counts["evaluated"]++
	counts["tier_"+strconv.Itoa(d.ComputedTier)]++
	for _, action := range d.Lanes {
		if slices.Contains([]string{"created", "removed", "blocked_manual", "blocked_opt_out"}, action) {
			counts[action]++
		}
	}
	if d.DeferredReason != "" {
		counts["deferred"]++
	}
}
func (s *Store) loadPersonTierInputTx(ctx context.Context, tx *loggedTx, input *PersonTierInput, loc *time.Location) error {
	var err error
	input.Tracking, err = s.getPersonTrackingTx(ctx, tx, input.PersonID)
	if err != nil {
		return err
	}
	input.Briefs, err = s.getPersonBriefEnrollmentTx(ctx, tx, input.PersonID)
	if err != nil {
		return err
	}
	input.Manual = persontiers.ManualState{TrackingProvenance: input.Tracking.Provenance, BriefsProvenance: input.Briefs.Provenance, OptOuts: []string{}}
	input.OptOuts = []PersonTierOptOut{}
	rows, err := tx.QueryContext(ctx, `SELECT id,lane,actor,reason,created_at FROM person_tier_opt_outs WHERE person_id=? ORDER BY lane`, input.PersonID)
	if err != nil {
		return err
	}
	for rows.Next() {
		o := PersonTierOptOut{PersonID: input.PersonID}
		var at nullableTimestamp
		if err = rows.Scan(&o.ID, &o.Lane, &o.Actor, &o.Reason, &at); err != nil {
			_ = rows.Close()
			return err
		}
		o.CreatedAt = at.Time
		input.OptOuts = append(input.OptOuts, o)
		input.Manual.OptOuts = append(input.Manual.OptOuts, o.Lane)
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	input.Manual.OpenManualRunID, err = s.openPersonManualEnrichmentRunTx(ctx, tx, input.PersonID)
	if err != nil {
		return err
	}
	var since nullableTimestamp
	var candidate sql.NullInt64
	var candidateSince sql.NullString
	input.Prior = persontiers.Prior{}
	err = tx.QueryRowContext(ctx, `SELECT tier,tier_since,candidate_tier,candidate_since,rule_version,config_fingerprint FROM person_tier_state WHERE person_id=?`, input.PersonID).Scan(&input.Prior.Tier, &since, &candidate, &candidateSince, &input.Prior.RuleVersion, &input.Prior.ConfigFingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	input.Prior.TierSince = since.Time.In(loc).Format(time.DateOnly)
	input.Prior.CandidateTier = int(candidate.Int64)
	input.Prior.CandidateSince = candidateSince.String
	return nil
}

func (s *Store) ExplainPersonTierContext(ctx context.Context, personID int64, req PersonTierPageRequest) (*PersonTierExplanation, error) {
	if personID <= 0 {
		return nil, ErrInvalidPersonTierRequest
	}
	if req.AsOf.IsZero() {
		req.AsOf = s.personEnrichmentTime()
	}
	req, err := normalizePersonTierRequest(req)
	if err != nil {
		return nil, err
	}
	if err := s.validatePersonTierProjectionRequestContext(ctx, req); err != nil {
		return nil, err
	}
	signals, err := s.PersonTierSignalsContext(ctx, []int64{personID}, req.AsOf, req.Config, req.MaxDirect)
	if err != nil {
		return nil, err
	}
	ready, err := s.PersonTierReadinessContext(ctx)
	if err != nil {
		return nil, err
	}
	input := PersonTierInput{PersonID: personID, Signals: signals[personID], Readiness: ready}
	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(display_name,'') FROM persons WHERE id=?`, personID).Scan(&input.Name); errors.Is(err, sql.ErrNoRows) {
			return ErrPersonNotFound
		} else if err != nil {
			return err
		}
		return s.loadPersonTierInputTx(ctx, tx, &input, req.AsOf.Location())
	})
	if err != nil {
		return nil, err
	}
	input.Decision = persontiers.Evaluate(input.Signals, req.Config, input.Prior, input.Manual, req.AsOf, ready.Ready)
	out := &PersonTierExplanation{PersonTierInput: input, AsOf: req.AsOf}
	if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM person_participants WHERE person_id=?`, personID).Scan(&out.LinkedParticipants); err != nil {
		return nil, err
	}
	if out.LinkedParticipants == 0 {
		out.SignalNote = "no linked participants"
	}
	if err = s.db.QueryRowContext(ctx, `SELECT EXISTS(
  SELECT 1 FROM person_enrichment_attempts a
  JOIN person_enrichment_attempt_identifiers identifier ON identifier.attempt_id=a.id
  WHERE a.person_id=? AND a.person_revision=(SELECT revision FROM persons WHERE id=?)
   AND EXISTS(SELECT 1 FROM person_enrichment_suppressions suppression
    WHERE suppression.provider_namespace=identifier.provider_namespace AND suppression.identifier_class=identifier.identifier_class
     AND suppression.normalization_version=identifier.normalization_version AND suppression.key_id=identifier.key_id AND suppression.digest=identifier.digest))`, personID, personID).Scan(&out.KnownSuppression); err != nil {
		return nil, err
	}
	out.SuppressionNote = "Known checks cover previously attempted identifiers at the current person revision; providers recheck suppression at dispatch."

	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		var err error
		out.EnrichmentEligible, err = s.personEnrichmentScheduledEligibleTx(ctx, tx, personID)
		return err
	})
	if err != nil {
		return nil, err
	}
	if err = s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM `+activeConsentsSQL(ConsentPersonEnrichment)+` c)`).Scan(&out.EnrichmentConsent); err != nil {
		return nil, err
	}
	out.RecentEvents, err = s.PersonTierHistoryContext(ctx, personID, 5, 0)
	if err == nil {
		err = s.validatePersonTierProjectionRequestContext(ctx, req)
	}
	return out, err
}

type PersonTierExplanation struct {
	LinkedParticipants int    `json:"linked_participants"`
	SignalNote         string `json:"signal_note"`
	KnownSuppression   bool   `json:"known_suppression"`
	SuppressionNote    string `json:"suppression_note"`
	PersonTierInput
	AsOf               time.Time         `json:"as_of"`
	EnrichmentEligible bool              `json:"enrichment_eligible"`
	EnrichmentConsent  bool              `json:"enrichment_consent"`
	RecentEvents       []PersonTierEvent `json:"recent_events"`
}

func nullableTier(value int) any {
	if value == 0 {
		return nil
	}
	return value
}
func nullableTierDate(value string) any {
	if value == "" {
		return nil
	}
	return value
}
func validatePersonTierActor(actor string) error {
	if strings.TrimSpace(actor) == "" {
		return fmt.Errorf("%w: actor required", ErrInvalidPersonTierRequest)
	}
	return nil
}
