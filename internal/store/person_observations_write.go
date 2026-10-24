package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/personobs"
)

type SuppressionCandidate struct {
	Origin                      personobs.Origin
	PostRejectionSupport, Floor int
}
type SuppressionVerdict string

const (
	SuppressionAllow      SuppressionVerdict = "allow"
	SuppressionSuppressed SuppressionVerdict = "suppressed"
	SuppressionLifted     SuppressionVerdict = "lifted"
)

func (s *Store) lockPersonMutationTx(ctx context.Context, tx *loggedTx, personID int64) error {
	return s.lockProfileIdentityKeyTxContext(ctx, tx, "person-observations", personID)
}
func (s *Store) nextObservationSequenceTx(ctx context.Context, tx *loggedTx, personID int64) (int64, error) {
	var sequence int64
	err := tx.QueryRowContext(ctx, `INSERT INTO person_observation_clocks(person_id,last_sequence) VALUES(?,1)
  ON CONFLICT(person_id) DO UPDATE SET last_sequence=person_observation_clocks.last_sequence+1 RETURNING last_sequence`, personID).Scan(&sequence)
	if err != nil {
		return 0, fmt.Errorf("advance person observation clock: %w", err)
	}
	return sequence, nil
}
func (s *Store) loadObservationTx(ctx context.Context, q contextRowsQuerier, id int64) (personobs.Observation, error) {
	rows, err := s.queryObservationRows(ctx, q, `id=?`, `id`, id)
	if err != nil {
		return personobs.Observation{}, err
	}
	if len(rows) == 0 {
		return personobs.Observation{}, sql.ErrNoRows
	}
	return rows[0], nil
}
func observationProtected(o personobs.Observation) bool {
	return o.Pinned || o.OwnerState == personobs.OwnerStateConfirmed
}
func observationNowAfter(previous time.Time) time.Time {
	now := time.Now().UTC().Truncate(time.Microsecond)
	if !now.After(previous) {
		return previous.Add(time.Microsecond)
	}
	return now
}

// observationEventGenerationKey scopes provenance to the operation that triggered
// an event, including recursive support updates. Owner operations have no value.
type observationEventGenerationKey struct{}

// appendObservationEventTx advances the optimistic revision with the event.
// Explicit timestamps give SQLite the same precision as PostgreSQL.
func (s *Store) appendObservationEventTx(ctx context.Context, tx *loggedTx, personID, id int64, event, actor, detail string, related *int64) error {
	var previous requiredTimestamp
	if err := tx.QueryRowContext(ctx, `SELECT updated_at FROM person_observations WHERE id=? AND person_id=?`, id, personID).Scan(&previous); err != nil {
		return err
	}
	now := observationNowAfter(previous.Time)
	if _, err := tx.ExecContext(ctx, `UPDATE person_observations SET updated_at=? WHERE id=?`, now, id); err != nil {
		return err
	}
	if actor == "" {
		actor = "system"
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO person_observation_events(person_id,observation_id,generation_id,event,actor,detail,related_observation_id,created_at)
  SELECT person_id,id,?,?,?,?,?,? FROM person_observations WHERE id=?`, observationEventGeneration(ctx), event, actor, detail, personFactOptionalInt64(related), now, id)
	if err != nil {
		return fmt.Errorf("append observation event: %w", err)
	}
	return nil
}
func observationEventGeneration(ctx context.Context) any {
	generationID, _ := ctx.Value(observationEventGenerationKey{}).(int64)
	return observationOptionalGeneration(generationID)
}

func (s *Store) transitionObservationTx(ctx context.Context, tx *loggedTx, id int64, to personobs.Status, reason personobs.StatusReason, event, actor, detail string) error {
	var personID int64
	if err := tx.QueryRowContext(ctx, `UPDATE person_observations SET status=?,status_reason=? WHERE id=? RETURNING person_id`, to, reason, id).Scan(&personID); err != nil {
		return fmt.Errorf("transition observation: %w", err)
	}
	return s.appendObservationEventTx(ctx, tx, personID, id, event, actor, detail, nil)
}
func (s *Store) deletePersonObservationTx(ctx context.Context, tx *loggedTx, id int64) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM person_observations WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("delete person observation: %w", err)
	}
	return nil
}
func observationOptionalDate(date *personfacts.PartialDateValue) any {
	if date == nil {
		return nil
	}
	if date.Day > 0 {
		return fmt.Sprintf("%04d-%02d-%02d", date.Year, date.Month, date.Day)
	}
	if date.Month > 0 {
		return fmt.Sprintf("%04d-%02d", date.Year, date.Month)
	}
	return fmt.Sprintf("%04d", date.Year)
}
func observationOptionalText(value string) any {
	if value == "" {
		return nil
	}
	return value
}
func observationOptionalGeneration(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

func (s *Store) ApplyPersonObservationBatch(ctx context.Context, b PreparedObservationBatch, generationID int64, actor string) (personobs.BatchResult, error) {
	result, err := retryContendedWrite(ctx, s, "apply person observations", func() (*personobs.BatchResult, error) {
		var result personobs.BatchResult
		err := s.withTxContext(ctx, func(tx *loggedTx) error {
			var err error
			result, err = s.applyPersonObservationBatchTx(ctx, tx, b, generationID, actor)
			return err
		})
		return &result, err
	})
	if err != nil {
		return personobs.BatchResult{}, err
	}
	return *result, nil
}
func (s *Store) applyPersonObservationBatchTx(ctx context.Context, tx *loggedTx, b PreparedObservationBatch, generationID int64, actor string) (personobs.BatchResult, error) {
	ctx = context.WithValue(ctx, observationEventGenerationKey{}, generationID)
	result := personobs.BatchResult{Items: make([]personobs.BatchItemResult, 0, len(b.Items))}
	if err := verifyPreparedObservationBatch(b); err != nil {
		return result, err
	}
	if (generationID == 0) != (b.Generation == nil) {
		return result, errors.New("observation generation ID does not match prepared provenance")
	}
	if generationID != 0 {
		var personID int64
		var fingerprint string
		if err := tx.QueryRowContext(ctx, `SELECT person_id,program_fingerprint FROM person_fact_generations WHERE id=?`, generationID).Scan(&personID, &fingerprint); err != nil {
			return result, fmt.Errorf("read observation generation: %w", err)
		}
		if personID != b.PersonID {
			return result, errors.New("observation generation belongs to another person")
		}
		if fingerprint != b.ProgramFingerprint {
			return result, errors.New("observation generation belongs to another program")
		}
	}
	if err := s.lockPersonMutationTx(ctx, tx, b.PersonID); err != nil {
		return result, err
	}
	for index, item := range b.Items {
		out := personobs.BatchItemResult{Index: index, Failure: item.Failure}
		if item.Failure != nil {
			out.Outcome = "dropped-invalid"
			result.Items = append(result.Items, out)
			continue
		}
		premises := make(map[int64]personobs.Observation, len(item.Proposal.Premises))
		for _, edge := range item.Proposal.Premises {
			premise, err := s.loadObservationTx(ctx, tx, edge.ID)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					out.Failure = &personobs.ValidationError{Field: "premises", Reason: "premise-missing"}
					break
				}
				return result, err
			}
			if premise.PersonID == b.PersonID {
				_, changed, err := s.refreshObservationCitationExclusionsTx(ctx, tx, premise.ID)
				if err != nil {
					return result, err
				}
				if changed {
					if err := s.notifyObservationSupportChangedTx(ctx, tx, b.PersonID, []int64{premise.ID}, "citation-exclusions-changed"); err != nil {
						return result, err
					}
					premise, err = s.loadObservationTx(ctx, tx, edge.ID)
					if err != nil {
						return result, err
					}
				}
			}
			premises[edge.ID] = premise
		}
		if out.Failure == nil {
			out.Failure = personobs.ValidateProposal(item.Proposal, premises)
		}
		if out.Failure != nil {
			out.Outcome = "dropped-invalid"
			result.Items = append(result.Items, out)
			continue
		}
		item.Categories = append([]personobs.Category(nil), item.Categories...)
		for _, edge := range item.Proposal.Premises {
			premise := premises[edge.ID]
			item.Categories = append(item.Categories, premise.Categories...)
			item.Proposal.Sensitive = item.Proposal.Sensitive || premise.Sensitive
		}
		slices.Sort(item.Categories)
		item.Categories = slices.Compact(item.Categories)
		item.Proposal.Sensitive = item.Proposal.Sensitive || len(item.Categories) > 0
		var existing personobs.Observation
		if item.Proposal.ReinforcesID != nil {
			var err error
			existing, err = s.loadObservationTx(ctx, tx, *item.Proposal.ReinforcesID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return result, err
			}
			if err != nil || existing.PersonID != b.PersonID || existing.Level != item.Proposal.Level || existing.SubjectRef != item.Proposal.SubjectRef || existing.Status == personobs.StatusRejected {
				out.Outcome = "dropped-invalid"
				out.Failure = &personobs.ValidationError{Field: "reinforces_id", Reason: "reinforcement-target"}
				result.Items = append(result.Items, out)
				continue
			}
			// A target superseded since preparation falls back to the current
			// live key, or a new row if that key is no longer live.
			if existing.Status == personobs.StatusSuperseded {
				existing = personobs.Observation{}
			}
		}
		if existing.ID == 0 {
			matches, err := s.queryObservationRows(ctx, tx, `person_id=? AND content_key=? AND status IN ('active','unsupported')`, `id LIMIT 1`, b.PersonID, item.ContentKey)
			if err != nil {
				return result, err
			}
			if len(matches) > 0 {
				existing = matches[0]
			}
		}
		existingCitationsChanged := false
		if existing.ID != 0 {
			_, changed, err := s.refreshObservationCitationExclusionsTx(ctx, tx, existing.ID)
			if err != nil {
				return result, err
			}
			existingCitationsChanged = changed
		}
		candidate, err := s.observationSuppressionCandidateTx(ctx, tx, b.PersonID, item, premises)
		if err != nil {
			return result, err
		}
		verdict, err := s.DecideSuppressionTx(ctx, tx, b.PersonID, item.ContentKey, candidate)
		if err != nil {
			return result, err
		}
		if verdict == SuppressionSuppressed {
			if existingCitationsChanged {
				if err := s.notifyObservationSupportChangedTx(ctx, tx, b.PersonID, []int64{existing.ID}, "citation-exclusions-changed"); err != nil {
					return result, err
				}
			}
			out.Outcome = "suppressed"
			result.Items = append(result.Items, out)
			continue
		}
		id := existing.ID
		ownerAssertion := id != 0 && item.Proposal.Origin == personobs.OriginOwner && existing.Origin != personobs.OriginOwner
		if ownerAssertion {
			if err := s.promoteObservationToOwnerTx(ctx, tx, b.PersonID, id, item.Proposal, item.Stability); err != nil {
				return result, err
			}
		}
		if id == 0 {
			sequence, err := s.nextObservationSequenceTx(ctx, tx, b.PersonID)
			if err != nil {
				return result, err
			}
			source := personobs.StabilitySourceProgram
			if item.Proposal.Origin == personobs.OriginOwner {
				source = personobs.StabilitySourceOwner
			}
			now := observationNowAfter(time.Time{})
			err = tx.QueryRowContext(ctx, `INSERT INTO person_observations
    (person_id,sequence,generation_id,level,origin,kind,content,content_key,normalization_version,
     subject_ref,directness,sensitive,confidence_bp,valid_from,valid_until,occurs_on,program_fingerprint,
     status,stability,stability_source,target_kind,target_key,created_at,updated_at)
    VALUES(?,?,?,?,?,?,?,?,'obs-norm-v1',?,?,?,?,?,?,?,?,'active',?,?,?,?,?,?) RETURNING id`,
				b.PersonID, sequence, observationOptionalGeneration(generationID), item.Proposal.Level, item.Proposal.Origin, item.Proposal.Kind, item.Proposal.Content, item.ContentKey,
				item.Proposal.SubjectRef, personobs.DirectnessFor(item.Proposal.Level, item.Proposal.Origin, item.Proposal.Attribution), item.Proposal.Sensitive, item.Proposal.ConfidenceBP,
				item.Proposal.ValidFrom, item.Proposal.ValidUntil, observationOptionalDate(item.Proposal.OccursOn), b.ProgramFingerprint, item.Stability, source,
				observationOptionalText(item.Proposal.TargetKind), observationOptionalText(item.Proposal.TargetKey), now, now).Scan(&id)
			if err != nil {
				return result, fmt.Errorf("insert person observation: %w", err)
			}
			out.Outcome = "created"
			if err := s.appendObservationEventTx(ctx, tx, b.PersonID, id, "created", actor, "", nil); err != nil {
				return result, err
			}
		} else {
			out.Outcome = "reinforced"
		}
		if err := s.writeObservationCitationsTx(ctx, tx, b.PersonID, id, generationID, item); err != nil {
			return result, err
		}
		if err := s.unionObservationCategoriesTx(ctx, tx, b.PersonID, id, item, actor, out.Outcome == "created"); err != nil {
			return result, err
		}
		for _, edge := range item.Proposal.Premises {
			if err := s.appendObservationPremiseTx(ctx, tx, id, edge, generationID, actor); err != nil {
				return result, err
			}
		}
		if out.Outcome == "reinforced" {
			quotes := make([]string, 0, len(item.Proposal.Support))
			for _, support := range item.Proposal.Support {
				quotes = append(quotes, support.Quote)
			}
			detail, err := json.Marshal(struct {
				Quotes         []string `json:"quotes"`
				OwnerAssertion bool     `json:"owner_assertion,omitempty"`
			}{quotes, ownerAssertion})
			if err != nil {
				return result, err
			}
			if err := s.appendObservationEventTx(ctx, tx, b.PersonID, id, "reinforced", actor, string(detail), nil); err != nil {
				return result, err
			}
		}
		if verdict == SuppressionLifted {
			if err := s.ensureObservationSuppressionLiftEventTx(ctx, tx, b.PersonID, item.ContentKey, id); err != nil {
				return result, err
			}
		}
		if err := s.notifyObservationSupportChangedTx(ctx, tx, b.PersonID, []int64{id}, "written"); err != nil {
			return result, err
		}
		out.ObservationID = id
		canSupersede := true
		if item.Proposal.Origin != personobs.OriginOwner {
			replacement, err := s.loadObservationTx(ctx, tx, id)
			if err != nil {
				return result, err
			}
			canSupersede = personobs.PremiseSupported(replacement)
		}
		if canSupersede {
			for _, targetID := range item.Proposal.SupersedesIDs {
				if targetID == id {
					continue
				}
				target, err := s.loadObservationTx(ctx, tx, targetID)
				if err != nil {
					return result, err
				}
				if target.PersonID != b.PersonID {
					return result, errors.New("supersession target belongs to another person")
				}
				if target.Status != personobs.StatusActive && target.Status != personobs.StatusUnsupported {
					continue
				}
				out.AffectedIDs = append(out.AffectedIDs, targetID)
				if observationProtected(target) {
					if err := s.appendObservationEventTx(ctx, tx, b.PersonID, targetID, "supersede-blocked", actor, "", &id); err != nil {
						return result, err
					}
					if err := s.appendObservationEventTx(ctx, tx, b.PersonID, id, "supersede-blocked", actor, "", &targetID); err != nil {
						return result, err
					}
					out.Outcome = "supersede-blocked"
					continue
				}
				var end *time.Time
				end = item.Proposal.ValidFrom
				if end == nil {
					end, err = s.observationLatestSupportedEvidenceTimeTx(ctx, tx, b.PersonID, id)
					if err != nil {
						return result, err
					}
				}
				if target.ValidUntil != nil && (end == nil || target.ValidUntil.Before(*end)) {
					end = target.ValidUntil
				}
				if end != nil && target.ValidFrom != nil && end.Before(*target.ValidFrom) {
					return result, errors.New("supersession ends before target validity")
				}
				if _, err := tx.ExecContext(ctx, `UPDATE person_observations SET superseded_by_id=?,valid_until=COALESCE(?,valid_until) WHERE id=?`, id, end, targetID); err != nil {
					return result, err
				}
				if err := s.transitionObservationTx(ctx, tx, targetID, personobs.StatusSuperseded, personobs.StatusReasonKnowledgeUpdate, "superseded", actor, "knowledge-update"); err != nil {
					return result, err
				}
				if err := s.notifyObservationSupportChangedTx(ctx, tx, b.PersonID, []int64{targetID}, "superseded"); err != nil {
					return result, err
				}
				if out.Outcome != "supersede-blocked" {
					out.Outcome = "superseded-target"
				}
			}
		}
		result.Items = append(result.Items, out)
	}
	return result, nil
}

func (s *Store) writeObservationCitationsTx(ctx context.Context, tx *loggedTx, personID, id, generationID int64, item PreparedObservation) error {
	if len(item.Citations) != len(item.Proposal.Support) {
		return errors.New("prepared observation citations do not match support")
	}
	for i, input := range item.Proposal.Support {
		key, err := personfacts.EvidenceKey(input.Evidence)
		if err != nil {
			return err
		}
		evidence, err := s.insertPersonFactEvidenceTx(ctx, tx, personID, key, input.Evidence)
		if err != nil {
			return err
		}
		c := item.Citations[i]
		c.Exclusion, err = s.observationCitationExclusionTx(ctx, tx, c.Leaf.MessageID, c.Exclusion)
		if err != nil {
			return fmt.Errorf("refresh observation citation exclusion: %w", err)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO person_observation_evidence
   (observation_id,evidence_id,generation_id,quote_start,quote_end,quote_sha256,exclusion_reason,message_id,conversation_id,conversation_kind,authored_by_person,author_is_owner,event_time)
	   VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT (observation_id,evidence_id) DO UPDATE SET exclusion_reason=excluded.exclusion_reason`, id, evidence.ID, observationOptionalGeneration(generationID), c.QuoteStart, c.QuoteEnd, c.QuoteSHA256, c.Exclusion, c.Leaf.MessageID, c.Leaf.ConversationID, c.Leaf.ConversationKind, c.Leaf.AuthoredByPerson, c.Leaf.AuthorIsOwner, c.Leaf.EventTime)
		if err != nil {
			return fmt.Errorf("write observation citation: %w", err)
		}
	}
	return s.recomputeObservationCitationSummaryTx(ctx, tx, id)
}

func (s *Store) refreshObservationCitationExclusionsTx(ctx context.Context, tx *loggedTx, observationID int64) ([]personobs.Citation, bool, error) {
	citations, err := s.observationCitations(ctx, tx, observationID)
	if err != nil {
		return nil, false, err
	}
	changed := false
	for i := range citations {
		citation := citations[i]
		exclusion, err := s.observationCitationExclusionTx(ctx, tx, citation.Leaf.MessageID, citation.Exclusion)
		if err != nil {
			return nil, false, fmt.Errorf("refresh observation citation exclusion: %w", err)
		}
		citations[i].Exclusion = exclusion
		if exclusion == citation.Exclusion {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE person_observation_evidence SET exclusion_reason=? WHERE observation_id=? AND evidence_id=?`, exclusion, observationID, citation.EvidenceID); err != nil {
			return nil, false, fmt.Errorf("update observation citation exclusion: %w", err)
		}
		changed = true
	}
	if !changed {
		return citations, false, nil
	}
	if err := s.recomputeObservationCitationSummaryTx(ctx, tx, observationID); err != nil {
		return nil, false, err
	}
	return citations, true, nil
}

func (s *Store) recomputeObservationCitationSummaryTx(ctx context.Context, tx *loggedTx, id int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE person_observations SET
  mention_count=(SELECT COUNT(*) FROM (SELECT message_id FROM person_observation_evidence WHERE observation_id=? AND exclusion_reason='' GROUP BY message_id) mentions),
  first_seen_at=(SELECT MIN(event_time) FROM person_observation_evidence WHERE observation_id=? AND exclusion_reason=''),
  last_seen_at=(SELECT MAX(event_time) FROM person_observation_evidence WHERE observation_id=? AND exclusion_reason='') WHERE id=?`, id, id, id, id)
	return err
}

func (s *Store) observationCitationExclusionTx(ctx context.Context, tx *loggedTx, messageID int64, prepared personobs.ExclusionReason) (personobs.ExclusionReason, error) {
	if prepared != personobs.ExclusionReasonNone && prepared != personobs.ExclusionReasonList && prepared != personobs.ExclusionReasonDraft {
		return prepared, nil
	}
	var listID string
	var draft bool
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(list_id,''),draft_authored FROM messages WHERE id=?`, messageID).Scan(&listID, &draft)
	if errors.Is(err, sql.ErrNoRows) {
		// Keep the prepared projection if its source message has since been deleted.
		return prepared, nil
	}
	if err != nil {
		return "", fmt.Errorf("read observation source exclusions: %w", err)
	}
	if draft {
		return personobs.ExclusionReasonDraft, nil
	}
	if strings.TrimSpace(listID) != "" {
		return personobs.ExclusionReasonList, nil
	}
	return personobs.ExclusionReasonNone, nil
}

func (s *Store) unionObservationCategoriesTx(ctx context.Context, tx *loggedTx, personID, id int64, item PreparedObservation, actor string, created bool) error {
	changed := false
	lexicon := personobs.DetectCategories(item.Proposal.Content)
	for _, category := range item.Categories {
		source := "premise"
		if slices.Contains(lexicon, category) {
			source = "lexicon"
		}
		if slices.Contains(item.Proposal.ModelCategories, category) {
			source = "model"
		}
		if item.Proposal.Origin == personobs.OriginOwner {
			source = "owner"
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO person_observation_categories(observation_id,category,source) VALUES(?,?,?) ON CONFLICT DO NOTHING`, id, category, source)
		if err != nil {
			return err
		}
		added, err := result.RowsAffected()
		if err != nil {
			return err
		}
		changed = changed || added > 0
	}
	sensitiveChanged := false
	if item.Proposal.Sensitive {
		result, err := tx.ExecContext(ctx, `UPDATE person_observations SET sensitive=TRUE WHERE id=? AND sensitive=FALSE`, id)
		if err != nil {
			return err
		}
		updated, err := result.RowsAffected()
		if err != nil {
			return err
		}
		sensitiveChanged = updated > 0
	}
	if !changed && !sensitiveChanged {
		return nil
	}
	// Creation already captures the initial set; every later change gets an event.
	if !created {
		return s.appendObservationEventTx(ctx, tx, personID, id, "sensitivity-changed", actor, "", nil)
	}
	return nil
}

func (s *Store) promoteObservationToOwnerTx(ctx context.Context, tx *loggedTx, personID, id int64, proposal personobs.ProposedObservation, stability personobs.Stability) error {
	_, err := tx.ExecContext(ctx, `UPDATE person_observations SET
  generation_id=NULL,origin=?,kind=?,content=?,content_key=?,directness=?,
  sensitive=CASE WHEN ? THEN TRUE ELSE sensitive END,confidence_bp=?,valid_from=?,valid_until=?,occurs_on=?,
  status='active',status_reason='',support_flag='',stability=?,stability_source=?,
  target_kind=?,target_key=?,program_fingerprint='',updated_at=? WHERE id=? AND person_id=?`,
		personobs.OriginOwner, proposal.Kind, proposal.Content, personobs.ContentKey(proposal.Level, proposal.SubjectRef, proposal.Content),
		personobs.DirectnessFor(proposal.Level, proposal.Origin, proposal.Attribution), proposal.Sensitive, proposal.ConfidenceBP,
		proposal.ValidFrom, proposal.ValidUntil, observationOptionalDate(proposal.OccursOn), stability, personobs.StabilitySourceOwner,
		observationOptionalText(proposal.TargetKind), observationOptionalText(proposal.TargetKey), time.Now().UTC().Truncate(time.Microsecond), id, personID)
	if err != nil {
		return fmt.Errorf("promote person observation to owner assertion: %w", err)
	}
	return nil
}
func (s *Store) appendObservationPremiseTx(ctx context.Context, tx *loggedTx, derivedID int64, p personobs.PremiseInput, generationID int64, actor string) error {
	derived, err := s.loadObservationTx(ctx, tx, derivedID)
	if err != nil {
		return err
	}
	premise, err := s.loadObservationTx(ctx, tx, p.ID)
	if err != nil {
		return err
	}
	if err := personobs.ValidateAppend(derived, premise, p.Role); err != nil {
		return err
	}
	var existing personobs.PremiseRole
	err = tx.QueryRowContext(ctx, `SELECT role FROM person_observation_premises WHERE derived_id=? AND premise_id=?`, derivedID, p.ID).Scan(&existing)
	if err == nil {
		if existing != p.Role {
			return errors.New("premise-role-immutable")
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO person_observation_premises(derived_id,premise_id,role,ordinal,added_generation_id)
  VALUES(?,?,?,(SELECT COALESCE(MAX(ordinal),-1)+1 FROM person_observation_premises WHERE derived_id=?),?)`, derivedID, p.ID, p.Role, derivedID, observationOptionalGeneration(generationID))
	if err != nil {
		return fmt.Errorf("append observation premise: %w", err)
	}
	if err := s.appendObservationEventTx(ctx, tx, derived.PersonID, derivedID, "premises-appended", actor, "", &p.ID); err != nil {
		return err
	}
	return s.notifyObservationSupportChangedTx(ctx, tx, derived.PersonID, []int64{derivedID}, "premises-appended")
}

func (s *Store) observationSuppressionCandidateTx(ctx context.Context, tx *loggedTx, personID int64, item PreparedObservation, premises map[int64]personobs.Observation) (SuppressionCandidate, error) {
	candidate := SuppressionCandidate{Origin: item.Proposal.Origin, Floor: 1}
	if item.Proposal.Level == personobs.LevelInductive || item.Proposal.Level == personobs.LevelContradiction {
		candidate.Floor = 2
	}
	var rejected requiredTimestamp
	err := tx.QueryRowContext(ctx, `SELECT rejected_at FROM person_observation_suppressions WHERE person_id=? AND content_key=? AND cleared_at IS NULL`, personID, item.ContentKey).Scan(&rejected)
	if errors.Is(err, sql.ErrNoRows) {
		return candidate, nil
	}
	if err != nil {
		return candidate, err
	}
	if len(item.Citations) != len(item.Proposal.Support) {
		return candidate, errors.New("prepared observation citations do not match support")
	}
	type eligibleCitation struct {
		citation    personobs.Citation
		evidenceKey string
	}
	eligible := make([]eligibleCitation, 0, len(item.Citations))
	evidenceKeys := make([]string, 0, len(item.Citations))
	seenEvidenceKeys := make(map[string]struct{}, len(item.Citations))
	for i, c := range item.Citations {
		exclusion, err := s.observationCitationExclusionTx(ctx, tx, c.Leaf.MessageID, c.Exclusion)
		if err != nil {
			return candidate, fmt.Errorf("refresh observation proposal exclusion: %w", err)
		}
		if exclusion != personobs.ExclusionReasonNone || !c.Leaf.AuthoredByPerson || !c.Leaf.EventTime.After(rejected.Time) {
			continue
		}
		evidenceKey, err := personfacts.EvidenceKey(item.Proposal.Support[i].Evidence)
		if err != nil {
			return candidate, fmt.Errorf("resolve observation proposal evidence: %w", err)
		}
		eligible = append(eligible, eligibleCitation{citation: c, evidenceKey: evidenceKey})
		if _, seen := seenEvidenceKeys[evidenceKey]; !seen {
			seenEvidenceKeys[evidenceKey] = struct{}{}
			evidenceKeys = append(evidenceKeys, evidenceKey)
		}
	}
	existingIDs, err := s.personFactEvidenceIDsByKeyTx(ctx, tx, personID, evidenceKeys)
	if err != nil {
		return candidate, err
	}
	evidenceIDs := make([]int64, 0, len(existingIDs))
	for _, evidenceKey := range evidenceKeys {
		if evidenceID, exists := existingIDs[evidenceKey]; exists {
			evidenceIDs = append(evidenceIDs, evidenceID)
		}
	}
	latestSupport, err := s.latestEvidenceSupportedTx(ctx, tx, evidenceIDs)
	if err != nil {
		return candidate, err
	}
	messages := map[int64]bool{}
	for _, item := range eligible {
		if evidenceID, exists := existingIDs[item.evidenceKey]; exists && !latestSupport[evidenceID] {
			continue
		}
		messages[item.citation.Leaf.MessageID] = true
	}
	candidate.PostRejectionSupport = len(messages)
	if item.Proposal.Level != personobs.LevelExplicit {
		for _, edge := range item.Proposal.Premises {
			if edge.Role != personobs.PremiseRoleSupport && (item.Proposal.Level != personobs.LevelContradiction || edge.Role != personobs.PremiseRoleCounter) {
				continue
			}
			p := premises[edge.ID]
			if !personobs.PremiseSupported(p) {
				continue
			}
			fresh, err := s.observationHasFreshSupportedCitationTx(ctx, tx, personID, p.ID, rejected.Time)
			if err != nil {
				return candidate, err
			}
			if fresh {
				candidate.PostRejectionSupport++
			}
		}
	}
	return candidate, nil
}

func (s *Store) personFactEvidenceIDsByKeyTx(ctx context.Context, tx *loggedTx, personID int64, evidenceKeys []string) (map[string]int64, error) {
	ids := make(map[string]int64, len(evidenceKeys))
	keys := slices.Clone(evidenceKeys)
	slices.Sort(keys)
	keys = slices.Compact(keys)
	for start := 0; start < len(keys); start += 500 {
		chunk := keys[start:min(start+500, len(keys))]
		args := make([]any, 1, len(chunk)+1)
		args[0] = personID
		for _, key := range chunk {
			args = append(args, key)
		}
		rows, err := tx.QueryContext(ctx, `SELECT id,evidence_key FROM person_fact_evidence WHERE person_id=? AND evidence_key IN (`+placeholders(len(chunk))+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("resolve existing person fact evidence: %w", err)
		}
		for rows.Next() {
			var id int64
			var key string
			if err := rows.Scan(&id, &key); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan existing person fact evidence: %w", err)
			}
			ids[key] = id
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("iterate existing person fact evidence: %w", err)
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("close existing person fact evidence: %w", err)
		}
	}
	return ids, nil
}

func (s *Store) observationHasFreshSupportedCitationTx(ctx context.Context, tx *loggedTx, personID, observationID int64, after time.Time) (bool, error) {
	latest, err := s.observationLatestSupportedEvidenceTimeTx(ctx, tx, personID, observationID)
	if err != nil {
		return false, err
	}
	return latest != nil && latest.After(after), nil
}

func (s *Store) observationLatestSupportedEvidenceTimeTx(ctx context.Context, tx *loggedTx, personID, observationID int64) (*time.Time, error) {
	pending := []int64{observationID}
	seenObservations := make(map[int64]struct{})
	citationsByEvidenceID := make(map[int64]personobs.Citation)
	for len(pending) > 0 {
		currentID := pending[0]
		pending = pending[1:]
		if _, seen := seenObservations[currentID]; seen {
			continue
		}
		seenObservations[currentID] = struct{}{}

		citations, changed, err := s.refreshObservationCitationExclusionsTx(ctx, tx, currentID)
		if err != nil {
			return nil, err
		}
		if changed {
			if err := s.notifyObservationSupportChangedTx(ctx, tx, personID, []int64{currentID}, "citation-exclusions-changed"); err != nil {
				return nil, err
			}
		}
		observation, err := s.loadObservationTx(ctx, tx, currentID)
		if errors.Is(err, sql.ErrNoRows) && currentID != observationID {
			continue
		}
		if err != nil {
			return nil, err
		}
		if observation.PersonID != personID || !personobs.PremiseSupported(observation) {
			continue
		}
		for _, citation := range citations {
			if citation.Exclusion != personobs.ExclusionReasonNone || !citation.Leaf.AuthoredByPerson {
				continue
			}
			current, exists := citationsByEvidenceID[citation.EvidenceID]
			if !exists || citation.Leaf.EventTime.After(current.Leaf.EventTime) {
				citationsByEvidenceID[citation.EvidenceID] = citation
			}
		}

		rolePredicate := `e.role=?`
		args := []any{currentID, personID, personobs.PremiseRoleSupport}
		if observation.Level == personobs.LevelContradiction {
			rolePredicate = `e.role IN (?,?)`
			args = append(args, personobs.PremiseRoleCounter)
		}
		rows, err := tx.QueryContext(ctx, `SELECT e.premise_id FROM person_observation_premises e
  JOIN person_observations derived ON derived.id=e.derived_id
  JOIN person_observations premise ON premise.id=e.premise_id AND premise.person_id=derived.person_id
  WHERE e.derived_id=? AND derived.person_id=? AND `+rolePredicate, args...)
		if err != nil {
			return nil, fmt.Errorf("read supported observation premises: %w", err)
		}
		for rows.Next() {
			var premiseID int64
			if err := rows.Scan(&premiseID); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan supported observation premise: %w", err)
			}
			pending = append(pending, premiseID)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("iterate supported observation premises: %w", err)
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("close supported observation premises: %w", err)
		}
	}

	evidenceIDs := make([]int64, 0, len(citationsByEvidenceID))
	for evidenceID := range citationsByEvidenceID {
		evidenceIDs = append(evidenceIDs, evidenceID)
	}
	slices.Sort(evidenceIDs)
	supported, err := s.latestEvidenceSupportedTx(ctx, tx, evidenceIDs)
	if err != nil {
		return nil, err
	}
	var latest *time.Time
	for evidenceID, citation := range citationsByEvidenceID {
		if !supported[evidenceID] || (latest != nil && !citation.Leaf.EventTime.After(*latest)) {
			continue
		}
		value := citation.Leaf.EventTime
		latest = &value
	}
	return latest, nil
}
func (s *Store) DecideSuppressionTx(ctx context.Context, tx *loggedTx, personID int64, contentKey string, c SuppressionCandidate) (SuppressionVerdict, error) {
	if c.Origin == personobs.OriginOwner {
		return SuppressionAllow, nil
	}
	var id int64
	var reason string
	err := tx.QueryRowContext(ctx, `SELECT id,reason FROM person_observation_suppressions WHERE person_id=? AND content_key=? AND cleared_at IS NULL`, personID, contentKey).Scan(&id, &reason)
	if errors.Is(err, sql.ErrNoRows) {
		return SuppressionAllow, nil
	}
	if err != nil {
		return "", err
	}
	if reason != "outdated" || c.PostRejectionSupport < max(1, c.Floor) {
		return SuppressionSuppressed, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE person_observation_suppressions SET cleared_at=? WHERE id=?`, time.Now().UTC(), id); err != nil {
		return "", err
	}
	var observationID int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM person_observations WHERE person_id=? AND content_key=? ORDER BY id DESC LIMIT 1`, personID, contentKey).Scan(&observationID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if observationID != 0 {
		if err := s.ensureObservationSuppressionLiftEventTx(ctx, tx, personID, contentKey, observationID); err != nil {
			return "", err
		}
	}
	return SuppressionLifted, nil
}
func (s *Store) ensureObservationSuppressionLiftEventTx(ctx context.Context, tx *loggedTx, personID int64, key string, observationID int64) error {
	var suppressionID int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM person_observation_suppressions WHERE person_id=? AND content_key=? AND cleared_at IS NOT NULL ORDER BY id DESC LIMIT 1`, personID, key).Scan(&suppressionID); err != nil {
		return err
	}
	detail := fmt.Sprintf("outdated-new-support:suppression:%d", suppressionID)
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM person_observation_events WHERE person_id=? AND event='suppression-lifted' AND detail=?`, personID, detail).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return nil
	}
	return s.appendObservationEventTx(ctx, tx, personID, observationID, "suppression-lifted", "system", detail, nil)
}

// Support propagation replaces this hook in the next store layer.
func (s *Store) notifyObservationSupportChangedTx(_ context.Context, _ *loggedTx, _ int64, _ []int64, _ string) error {
	return nil
}
