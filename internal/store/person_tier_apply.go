package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.kenn.io/msgvault/internal/personenrichment"
	"go.kenn.io/msgvault/internal/persontiers"
)

// ApplyPersonTierDecisionContext re-evaluates authority and current signals
// under the shared mutation gate. The run freezes its policy and as-of date;
// current activity and manual grants are read when each person is applied.
func (s *Store) ApplyPersonTierDecisionContext(ctx context.Context, run PersonTierRun, input PersonTierInput) error {
	_, err := s.applyPersonTierDecisionContext(ctx, run, input)
	return err
}

func (s *Store) applyPersonTierDecisionContext(ctx context.Context, run PersonTierRun, input PersonTierInput) (PersonTierInput, error) {
	return s.applyPersonTierDecisionWithCheckpointContext(ctx, run, input, "")
}

func (s *Store) applyPersonTierDecisionWithCheckpointContext(ctx context.Context, run PersonTierRun, input PersonTierInput, checkpointCursor string) (PersonTierInput, error) {
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockPersonEnrichmentAuthorityMutationTx(ctx, tx); err != nil {
			return err
		}
		if err := s.checkPersonTierRunTx(ctx, tx, run); err != nil {
			return err
		}
		if err := s.lockProfileIdentityKeyTxContext(ctx, tx, "person-fact-generation", input.PersonID); err != nil {
			return err
		}
		if _, err := lockPersonEnrichmentPersonTx(ctx, tx, s.dialect, input.PersonID); errors.Is(err, sql.ErrNoRows) {
			return ErrPersonNotFound
		} else if err != nil {
			return err
		}
		var started nullableTimestamp
		var priorRunID string
		err := tx.QueryRowContext(ctx, `SELECT run_started_at,run_id FROM person_tier_state WHERE person_id=?`, input.PersonID).Scan(&started, &priorRunID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if started.Valid && started.Time.After(run.StartedAt) {
			return ErrPersonTierRunSuperseded
		}
		if run.Mode == "apply" && priorRunID == run.RunID {
			return s.persistPersonTierRunCheckpointTx(ctx, tx, run, checkpointCursor)
		}
		loc, _ := time.LoadLocation(run.Timezone)
		asOf := run.AsOf.In(loc)
		if err = s.lockActivityProjectionQueueFreshnessTx(ctx, tx); err != nil {
			return err
		}
		input.Readiness, err = s.personTierReadinessTx(ctx, tx)
		if err != nil {
			return err
		}
		// Refresh only untouched planner output; direct callers can supply their own signals.
		if input.plannedSignals != nil && personTierSignalsEqual(input.Signals, *input.plannedSignals) {
			input.Signals, err = s.personTierSignalsTx(ctx, tx, input.PersonID, asOf, run.Config, run.MaxDirect)
			if err != nil {
				return err
			}
		}
		if err = s.loadPersonTierInputTx(ctx, tx, &input, loc); err != nil {
			return err
		}
		d := persontiers.Evaluate(input.Signals, run.Config, input.Prior, input.Manual, asOf, input.Readiness.Ready)
		input.Decision = d
		applyPersonTierEligibilityTransition(&input)
		actor := run.Actor
		if actor == "" {
			actor = "people-tiers"
		}
		now := s.personEnrichmentTime().UTC()
		if run.Mode == "record_only" {
			if err = s.recordPersonTierObservationTx(ctx, tx, run, input, actor, now); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `DELETE FROM archive_metadata WHERE key=?`, personTierFailureKey(input.PersonID)); err != nil {
				return err
			}
			return s.persistPersonTierRunCheckpointTx(ctx, tx, run, checkpointCursor)
		}
		wasEligible, err := s.personEnrichmentScheduledEligibleTx(ctx, tx, input.PersonID)
		if err != nil {
			return err
		}
		since, err := time.ParseInLocation(time.DateOnly, d.TierSince, loc)
		if err != nil {
			return fmt.Errorf("parse people tier date: %w", err)
		}
		lanes, err := json.Marshal(d.AppliedLanes)
		if err != nil {
			return err
		}
		enrichmentLaneEligible := slices.Contains(d.AppliedLanes, "tracking") && slices.Contains(d.AppliedLanes, "enrichment")
		_, err = tx.ExecContext(ctx, `INSERT INTO person_tier_state
   (person_id,tier,tier_since,candidate_tier,candidate_since,lanes_json,enrichment_eligible,rule,rule_version,config_fingerprint,as_of_local_date,run_id,run_started_at,applied_at)
   VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(person_id) DO UPDATE SET
   tier=excluded.tier,tier_since=excluded.tier_since,candidate_tier=excluded.candidate_tier,candidate_since=excluded.candidate_since,
   lanes_json=excluded.lanes_json,enrichment_eligible=excluded.enrichment_eligible,rule=excluded.rule,rule_version=excluded.rule_version,
   config_fingerprint=excluded.config_fingerprint,as_of_local_date=excluded.as_of_local_date,run_id=excluded.run_id,run_started_at=excluded.run_started_at,applied_at=excluded.applied_at`,
			input.PersonID, d.Tier, since, nullableTier(d.CandidateTier), nullableTierDate(d.CandidateSince), string(lanes), enrichmentLaneEligible, d.Rule, persontiers.RuleVersion, run.ConfigFingerprint, asOf.Format(time.DateOnly), run.RunID, run.StartedAt, now)
		if err != nil {
			return fmt.Errorf("persist people tier authority: %w", err)
		}
		opts := personTierGrantOptions{Provenance: PersonTierProvenanceAuto, Actor: "people-tiers", At: now}
		// Remove briefs before tracking; create tracking before briefs.
		if d.Lanes["briefs"] == "removed" {
			if _, err = s.setPersonBriefEnrollmentWithOptionsTx(ctx, tx, input.PersonID, false, false, opts); err != nil {
				return err
			}
		}
		if action := d.Lanes["tracking"]; action == "created" || action == "removed" {
			if _, err = s.setPersonTrackingWithOptionsTx(ctx, tx, input.PersonID, action == "created", opts); err != nil {
				return err
			}
		}
		if d.Lanes["briefs"] == "created" {
			if _, err = s.setPersonBriefEnrollmentWithOptionsTx(ctx, tx, input.PersonID, true, false, opts); err != nil {
				return err
			}
		}
		eligible, err := s.personEnrichmentScheduledEligibleTx(ctx, tx, input.PersonID)
		if err != nil {
			return err
		}
		if !eligible {
			if err = s.cancelScheduledPersonEnrichmentTx(ctx, tx, input.PersonID); err != nil {
				return err
			}
		}
		if !wasEligible && eligible {
			if err = s.publishPersonEnrichmentTx(ctx, tx, input.PersonID, personenrichment.TriggerTracked, "tier:"+run.RunID, now); err != nil {
				return err
			}
		}
		reason := personTierEventReason(d)
		if wasEligible != eligible && d.Reason == "" && d.DeferredReason == "" {
			reason = "config_change"
		}
		if reason != "" {
			if err = s.insertPersonTierEventTx(ctx, tx, run, input, d, reason, "apply", actor, now); err != nil {
				return err
			}
		}
		if err = s.recordPersonTierObservationTx(ctx, tx, run, input, actor, now); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM archive_metadata WHERE key=?`, personTierFailureKey(input.PersonID)); err != nil {
			return err
		}
		return s.persistPersonTierRunCheckpointTx(ctx, tx, run, checkpointCursor)
	})
	return input, err
}

func clonePersonTierSignals(signals persontiers.Signals) *persontiers.Signals {
	cloned := signals
	cloned.Emails = slices.Clone(signals.Emails)
	if signals.LastDirectAt != nil {
		last := *signals.LastDirectAt
		cloned.LastDirectAt = &last
	}
	return &cloned
}

func personTierSignalsEqual(left, right persontiers.Signals) bool {
	if left.OutboundShort != right.OutboundShort || left.OutboundLong != right.OutboundLong ||
		left.InboundShort != right.InboundShort || left.InboundLong != right.InboundLong ||
		left.ActiveDaysShort != right.ActiveDaysShort || left.ActiveDaysLong != right.ActiveDaysLong ||
		left.MeetingDaysLong != right.MeetingDaysLong || left.ChannelsLong != right.ChannelsLong ||
		left.ExcludedListEvents != right.ExcludedListEvents ||
		left.LargeAudienceInboundEvents != right.LargeAudienceInboundEvents ||
		left.CoPresenceEvents != right.CoPresenceEvents || left.ObservedEvents != right.ObservedEvents ||
		left.Owner != right.Owner || left.HasPhoneOrChat != right.HasPhoneOrChat ||
		!slices.Equal(left.Emails, right.Emails) {
		return false
	}
	if left.LastDirectAt == nil || right.LastDirectAt == nil {
		return left.LastDirectAt == nil && right.LastDirectAt == nil
	}
	return left.LastDirectAt.Equal(*right.LastDirectAt)
}

func (s *Store) personTierReadinessTx(ctx context.Context, tx *loggedTx) (PersonTierReadiness, error) {
	result := PersonTierReadiness{Reasons: []string{}}
	var pending bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
 SELECT 1 FROM activity_projection_queue WHERE revision>processed_revision
)`).Scan(&pending); err != nil {
		return result, fmt.Errorf("read pending activity projections for tier apply: %w", err)
	}
	if pending {
		result.Reasons = append(result.Reasons, "pending_activity")
	}
	timezone, err := s.activityTimezoneTransitionQueryContext(ctx, tx)
	if err != nil {
		return result, err
	}
	if timezone.Active {
		result.Reasons = append(result.Reasons, "timezone_transition")
	}
	direct, err := s.activityDirectLimitTransitionQueryContext(ctx, tx)
	if err != nil {
		return result, err
	}
	if direct.Active {
		result.Reasons = append(result.Reasons, "direct_limit_transition")
	}
	identity, err := s.currentIdentityRevisionTxContext(ctx, tx)
	if err != nil {
		return result, fmt.Errorf("read tier identity revision: %w", err)
	}
	account, err := readAccountIdentityRevision(tx)
	if err != nil {
		return result, err
	}
	reconciled, err := s.activityEpochReconciledTx(ctx, tx, ContactRevisions{
		IdentityRevision: identity, AccountIdentityRevision: account,
	})
	if err != nil {
		return result, err
	}
	if !reconciled {
		result.Reasons = append(result.Reasons, "identity_revision_changed")
	}
	var watermarkValue sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT value FROM archive_metadata WHERE key=?`, activityWatermarkKey).Scan(&watermarkValue)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return result, fmt.Errorf("read activity watermark for tier apply: %w", err)
	}
	watermark := int64(0)
	if watermarkValue.Valid && watermarkValue.String != "" {
		watermark, err = strconv.ParseInt(watermarkValue.String, 10, 64)
		if err != nil || watermark < 0 {
			return result, fmt.Errorf("invalid activity watermark %q", watermarkValue.String)
		}
	}
	var liveMax int64
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM messages WHERE deleted_at IS NULL AND deleted_from_source_at IS NULL`).Scan(&liveMax); err != nil {
		return result, fmt.Errorf("read live messages for tier apply readiness: %w", err)
	}
	if watermark < liveMax {
		result.Reasons = append(result.Reasons, "watermark_behind")
	}
	result.Ready = len(result.Reasons) == 0
	return result, nil
}

func (s *Store) personTierSignalsTx(
	ctx context.Context,
	tx *loggedTx,
	personID int64,
	asOf time.Time,
	cfg persontiers.Config,
	maxDirect int,
) (persontiers.Signals, error) {
	var signal persontiers.Signals
	var resultPersonID int64
	from := asOf.AddDate(0, 0, -(cfg.LongWindowDays - 1)).Format(time.DateOnly)
	short := asOf.AddDate(0, 0, -(cfg.ShortWindowDays - 1)).Format(time.DateOnly)
	through := asOf.Format(time.DateOnly)
	excludeList := 0
	if cfg.ExcludeListMessages {
		excludeList = 1
	}
	err := tx.QueryRowContext(ctx, personTierSignalQuery("?"), personID, from, through, maxDirect, excludeList, short).Scan(
		&resultPersonID, &signal.OutboundShort, &signal.OutboundLong, &signal.InboundShort, &signal.InboundLong,
		&signal.ActiveDaysShort, &signal.ActiveDaysLong, &signal.MeetingDaysLong, &signal.ChannelsLong,
		&signal.ExcludedListEvents, &signal.LargeAudienceInboundEvents, &signal.CoPresenceEvents, &signal.ObservedEvents,
	)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return signal, fmt.Errorf("read current person tier signals: %w", err)
	}
	if err == nil && resultPersonID != personID {
		return signal, fmt.Errorf("read current person tier signals: got person %d, want %d", resultPersonID, personID)
	}
	var last nullableTimestamp
	err = tx.QueryRowContext(ctx, `SELECT MAX(ae.occurred_at)
 FROM activity_event_persons aep JOIN activity_events ae ON ae.message_id=aep.message_id
 JOIN messages m ON m.id=ae.message_id
 WHERE aep.person_id=? AND aep.local_date<=? AND aep.evidence='direct'
 AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL`, personID, through).Scan(&last)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return signal, fmt.Errorf("read current person tier direct recency: %w", err)
	}
	signal.LastDirectAt = nullableTimePointer(last)
	rows, err := tx.QueryContext(ctx, `SELECT COALESCE(p.email_address,''),COALESCE(p.phone_number,''),
 COALESCE(pi.identifier_type,''),COALESCE(pi.identifier_value,''),`+personTierOwnerMatch("p.id", "")+`
 FROM person_participants pp JOIN participants p ON p.id=pp.participant_id
 LEFT JOIN participant_identifiers pi ON pi.participant_id=p.id WHERE pp.person_id=?
 ORDER BY pp.person_id,p.id,pi.identifier_type,pi.identifier_value`, personID)
	if err != nil {
		return signal, fmt.Errorf("read current person tier bindings: %w", err)
	}
	for rows.Next() {
		var email, phone, kind, value string
		var owner bool
		if err = rows.Scan(&email, &phone, &kind, &value, &owner); err != nil {
			_ = rows.Close()
			return signal, fmt.Errorf("scan current person tier bindings: %w", err)
		}
		signal.Owner = signal.Owner || owner
		signal.HasPhoneOrChat = signal.HasPhoneOrChat || strings.TrimSpace(phone) != "" || (kind != "" && kind != "email" && strings.TrimSpace(value) != "")
		emails := []string{email}
		if kind == "email" {
			emails = append(emails, value)
		}
		for _, address := range emails {
			address = strings.ToLower(strings.TrimSpace(address))
			if address != "" && !slices.Contains(signal.Emails, address) {
				signal.Emails = append(signal.Emails, address)
			}
		}
		slices.Sort(signal.Emails)
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return signal, fmt.Errorf("iterate current person tier bindings: %w", err)
	}
	if err = rows.Close(); err != nil {
		return signal, fmt.Errorf("close current person tier bindings: %w", err)
	}
	return signal, nil
}

func (s *Store) persistPersonTierRunCheckpointTx(ctx context.Context, tx *loggedTx, run PersonTierRun, cursor string) error {
	if cursor == "" || run.Kind != "scheduled" {
		return nil
	}
	checkpoint := run
	checkpoint.Cursor = cursor
	return putPersonTierMetadataTx(ctx, tx, personTierRunKey(run.Kind), checkpoint)
}

func personTierEventReason(d persontiers.Decision) string {
	if d.DeferredReason != "" {
		return "deferred"
	}
	if d.Reason != "" {
		if d.Reason == "config_change" {
			return d.Reason
		}
		if d.Rule == "minimum_tier" {
			return "minimum_tier"
		}
		return d.Reason
	}
	for _, a := range d.Lanes {
		if a == "created" || a == "removed" {
			return "config_change"
		}
	}
	for _, a := range d.Lanes {
		if a == "blocked_manual" || a == "blocked_opt_out" {
			return "manual_blocked"
		}
	}
	return ""
}
func (s *Store) recordPersonTierObservationTx(ctx context.Context, tx *loggedTx, run PersonTierRun, input PersonTierInput, actor string, now time.Time) error {
	loc, _ := time.LoadLocation(run.Timezone)
	asOf := run.AsOf.In(loc)
	var prior persontiers.Prior
	var candidate sql.NullInt64
	var date sql.NullString
	var previousDate string
	err := tx.QueryRowContext(ctx, `SELECT shadow_tier,shadow_since,shadow_candidate_tier,shadow_candidate_since,rule_version,config_fingerprint,as_of_local_date FROM person_tier_observations WHERE person_id=?`, input.PersonID).Scan(&prior.Tier, &prior.TierSince, &candidate, &date, &prior.RuleVersion, &prior.ConfigFingerprint, &previousDate)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	prior.CandidateTier = int(candidate.Int64)
	prior.CandidateSince = date.String
	// Shadows carry their own hysteresis and never borrow applied authority.
	d := persontiers.Evaluate(input.Signals, run.Config, prior, input.Manual, asOf, input.Readiness.Ready)
	signals, err := json.Marshal(struct {
		persontiers.Signals

		Readiness PersonTierReadiness `json:"readiness"`
	}{input.Signals, input.Readiness}, json.Deterministic(true))
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO person_tier_observations
  (person_id,computed_tier,rule,signals_json,shadow_tier,shadow_since,shadow_candidate_tier,shadow_candidate_since,rule_version,config_fingerprint,as_of_local_date,activity_ready,evaluated_at)
  VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(person_id) DO UPDATE SET
  computed_tier=excluded.computed_tier,rule=excluded.rule,signals_json=excluded.signals_json,shadow_tier=excluded.shadow_tier,shadow_since=excluded.shadow_since,
  shadow_candidate_tier=excluded.shadow_candidate_tier,shadow_candidate_since=excluded.shadow_candidate_since,rule_version=excluded.rule_version,
  config_fingerprint=excluded.config_fingerprint,as_of_local_date=excluded.as_of_local_date,activity_ready=excluded.activity_ready,evaluated_at=excluded.evaluated_at`,
		input.PersonID, d.ComputedTier, d.Rule, string(signals), d.Tier, d.TierSince, nullableTier(d.CandidateTier), nullableTierDate(d.CandidateSince), persontiers.RuleVersion, run.ConfigFingerprint, asOf.Format(time.DateOnly), input.Readiness.Ready, now)
	if err != nil {
		return fmt.Errorf("record people tier observation: %w", err)
	}
	priorTier := prior.Tier
	if priorTier == 0 {
		priorTier = 3
	}
	due := d.DemotionDue != "" && previousDate < d.DemotionDue && asOf.Format(time.DateOnly) >= d.DemotionDue
	if run.Mode == "record_only" && (priorTier != d.Tier || due) {
		reason := personTierEventReason(d)
		if reason == "" {
			reason = "config_change"
		}
		input.Prior = prior
		return s.insertPersonTierEventTx(ctx, tx, run, input, d, reason, "preview", actor, now)
	}
	return nil
}

type PersonTierResetResult struct {
	Cleared  int                       `json:"cleared"`
	Deferred []PersonTierResetDeferral `json:"deferred"`
}
type PersonTierResetDeferral struct {
	PersonID int64  `json:"person_id"`
	RunID    string `json:"run_id"`
}

func (s *Store) ResetPersonTiersContext(ctx context.Context, actor string) (*PersonTierResetResult, error) {
	if err := validatePersonTierActor(actor); err != nil {
		return nil, err
	}
	out := &PersonTierResetResult{Deferred: []PersonTierResetDeferral{}}
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockPersonEnrichmentAuthorityMutationTx(ctx, tx); err != nil {
			return err
		}
		now, err := s.advancePersonTierFenceTx(ctx, tx, s.personEnrichmentTime())
		if err != nil {
			return err
		}
		// Retain a scheduled run until its pending follow-up observes the fence.
		// Removing it here would let that follow-up start a fresh automatic run.
		if _, err = tx.ExecContext(ctx, `DELETE FROM archive_metadata WHERE key='people_tiers_manual_run'`); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT id FROM persons WHERE EXISTS(SELECT 1 FROM person_tier_state WHERE person_id=persons.id) OR EXISTS(SELECT 1 FROM person_tracking WHERE person_id=persons.id AND provenance='auto') OR EXISTS(SELECT 1 FROM person_brief_enrollments WHERE person_id=persons.id AND provenance='auto') ORDER BY id`)
		if err != nil {
			return err
		}
		ids := []int64{}
		for rows.Next() {
			var id int64
			if err = rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		if err = rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err = rows.Close(); err != nil {
			return err
		}
		for _, id := range ids {
			var input PersonTierInput
			input.PersonID = id
			if err = s.loadPersonTierInputTx(ctx, tx, &input, time.UTC); err != nil {
				return err
			}
			if input.Manual.TrackingProvenance == PersonTierProvenanceAuto && input.Manual.OpenManualRunID != "" {
				out.Deferred = append(out.Deferred, PersonTierResetDeferral{id, input.Manual.OpenManualRunID})
				continue
			}
			opts := personTierGrantOptions{Provenance: PersonTierProvenanceAuto, Actor: actor, At: now}
			if input.Manual.BriefsProvenance == PersonTierProvenanceAuto {
				if _, err = s.setPersonBriefEnrollmentWithOptionsTx(ctx, tx, id, false, false, opts); err != nil {
					return err
				}
			}
			if input.Manual.TrackingProvenance == PersonTierProvenanceAuto {
				if _, err = s.setPersonTrackingWithOptionsTx(ctx, tx, id, false, opts); err != nil {
					return err
				}
			}
			if _, err = tx.ExecContext(ctx, `DELETE FROM person_tier_state WHERE person_id=?`, id); err != nil {
				return err
			}
			eligible, err := s.personEnrichmentScheduledEligibleTx(ctx, tx, id)
			if err != nil {
				return err
			}
			if !eligible {
				if err = s.cancelScheduledPersonEnrichmentTx(ctx, tx, id); err != nil {
					return err
				}
			}
			d := persontiers.Decision{
				Tier:  3,
				Rule:  "none",
				Lanes: map[string]string{"tracking": "unchanged", "briefs": "unchanged", "enrichment": "unchanged"},
			}
			for _, lane := range []string{"tracking", "briefs"} {
				provenance := input.Manual.TrackingProvenance
				if lane == "briefs" {
					provenance = input.Manual.BriefsProvenance
				}
				if provenance == "auto" {
					d.Lanes[lane] = "removed"
				}
			}
			run := PersonTierRun{RunID: "reset:" + uuid.NewString()}
			if err = s.insertPersonTierEventTx(ctx, tx, run, input, d, "reset", "apply", actor, now); err != nil {
				return err
			}
			out.Cleared++
		}
		return nil
	})
	return out, err
}

// PrunePersonTierPreviewsContext only removes record-only audit history.
func (s *Store) PrunePersonTierPreviewsContext(ctx context.Context, retentionDays int) error {
	if retentionDays < 1 {
		return ErrInvalidPersonTierRequest
	}
	_, err := s.db.ExecContext(ctx, s.Rebind(`DELETE FROM person_tier_events WHERE mode='preview' AND created_at<?`), s.personEnrichmentTime().AddDate(0, 0, -retentionDays))
	return err
}
