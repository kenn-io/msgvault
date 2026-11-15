package store

import (
	"context"
	"fmt"
	"maps"
	"slices"
)

// CardDAVParticipantBindingOptions selects a snapshot preview or an ownership-only undo.
type CardDAVParticipantBindingOptions struct {
	DryRun bool `json:"dry_run"`
	Undo   bool `json:"undo"`
}

// CardDAVParticipantBindingConflict exposes only archive handles, never addresses.
type CardDAVParticipantBindingConflict struct {
	ParticipantID int64   `json:"participant_id"`
	PersonIDs     []int64 `json:"person_ids"`
	Reason        string  `json:"reason" enum:"shared_address,existing_person"`
}

// CardDAVParticipantBindingResult reports changes and conflicts for a captured state.
type CardDAVParticipantBindingResult struct {
	CardsScanned        int                                 `json:"cards_scanned"`
	ParticipantsBound   int                                 `json:"participants_bound"`
	ParticipantsUnbound int                                 `json:"participants_unbound"`
	Conflicts           int                                 `json:"conflicts"`
	ConflictDetails     []CardDAVParticipantBindingConflict `json:"conflict_details"`
}

type cardDAVParticipantBinding struct {
	personID, participantID int64
	owned                   bool
}
type cardDAVParticipantBindingPlan struct {
	result      CardDAVParticipantBindingResult
	add, remove []cardDAVParticipantBinding
}

// ReconcileCardDAVParticipantBindingsContext uses retained CardDAV evidence;
// it never contacts a provider. Preview is write-free and describes its read
// snapshot. Apply always recomputes under the identity mutation lock.
func (s *Store) ReconcileCardDAVParticipantBindingsContext(ctx context.Context, options CardDAVParticipantBindingOptions) (*CardDAVParticipantBindingResult, error) {
	var result *CardDAVParticipantBindingResult
	run := s.withTxContext
	if options.DryRun {
		run = s.withReadSnapshotContext
	}
	err := run(ctx, func(tx *loggedTx) error {
		if !options.DryRun {
			if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
				return err
			}
		}
		plan, err := s.planCardDAVParticipantBindingsTx(ctx, tx, options.Undo, false)
		if err != nil {
			return err
		}
		if !options.DryRun {
			if err := s.applyCardDAVParticipantBindingsTx(ctx, tx, plan); err != nil {
				return err
			}
		}
		result = &plan.result
		return nil
	})
	return result, err
}

// Callers hold the identity mutation lock, before any person/resource locks.
func (s *Store) reconcileCardDAVParticipantBindingsTx(ctx context.Context, tx *loggedTx, cleanupOnly bool) error {
	plan, err := s.planCardDAVParticipantBindingsTx(ctx, tx, false, cleanupOnly)
	if err != nil {
		return err
	}
	return s.applyCardDAVParticipantBindingsTx(ctx, tx, plan)
}

func (s *Store) cleanupCardDAVParticipantBindingsTx(ctx context.Context, tx *loggedTx) error {
	var owned bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM person_participants WHERE carddav_owned = TRUE)`).Scan(&owned); err != nil {
		return err
	}
	if !owned {
		return nil
	}
	return s.reconcileCardDAVParticipantBindingsTx(ctx, tx, true)
}

func (s *Store) planCardDAVParticipantBindingsTx(ctx context.Context, tx *loggedTx, undo, cleanupOnly bool) (*cardDAVParticipantBindingPlan, error) {
	plan := &cardDAVParticipantBindingPlan{result: CardDAVParticipantBindingResult{ConflictDetails: []CardDAVParticipantBindingConflict{}}}
	bindings := map[int64]cardDAVParticipantBinding{}
	rows, err := tx.QueryContext(ctx, `SELECT person_id, participant_id, carddav_owned FROM person_participants ORDER BY participant_id`)
	if err != nil {
		return nil, fmt.Errorf("read CardDAV membership ownership: %w", err)
	}
	for rows.Next() {
		var binding cardDAVParticipantBinding
		if err := rows.Scan(&binding.personID, &binding.participantID, &binding.owned); err != nil {
			_ = rows.Close()
			return nil, err
		}
		bindings[binding.participantID] = binding
		if undo && binding.owned {
			plan.remove = append(plan.remove, binding)
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	if undo {
		plan.result.ParticipantsUnbound = len(plan.remove)
		return plan, nil
	}
	claims, conflicted, common, uncertain, scanned, err := s.cardDAVParticipantClaimsTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	plan.result.CardsScanned = scanned
	matched, disputed, retained, shadowed, participantKinds, err := cardDAVParticipantMatchesTx(ctx, tx, claims, conflicted, common)
	if err != nil {
		return nil, err
	}
	seeds := cardDAVPersonSet{}
	for id := range matched {
		seeds[id] = struct{}{}
	}
	for id, binding := range bindings {
		if binding.owned {
			seeds[id] = struct{}{}
		}
	}
	visited := cardDAVPersonSet{}
	for _, seed := range slices.Sorted(maps.Keys(seeds)) {
		if _, ok := visited[seed]; ok {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		edges, err := s.loadComponentEdgesTxContext(ctx, tx, seed, seed)
		if err != nil {
			return nil, err
		}
		members := sortedComponentMembers(seed, edges)
		wanted := cardDAVPersonSet{}
		uncertainOwners := cardDAVPersonSet{}
		hasSupportedClaim := false
		for _, id := range members {
			visited[id] = struct{}{}
			for personID := range matched[id] {
				hasSupportedClaim = true
				wanted[personID] = struct{}{}
			}
			// An unreadable address may still support an existing automatic member.
			if binding, ok := bindings[id]; ok && binding.owned {
				if _, kept := retained[id][binding.personID]; kept {
					hasSupportedClaim = true
					wanted[binding.personID] = struct{}{}
				}
				kinds := uncertain[binding.personID]
				if kinds[""] || kinds[ContactAddressEmail] && participantKinds[id][ContactAddressEmail] || kinds[ContactAddressPhone] && participantKinds[id][ContactAddressPhone] {
					wanted[binding.personID] = struct{}{}
					uncertainOwners[binding.personID] = struct{}{}
				}
			}
		}
		// Disputed evidence cannot add a binding, but it can block an existing
		// supported claim anywhere in the linked participant component.
		if hasSupportedClaim {
			for _, id := range members {
				for personID := range disputed[id] {
					wanted[personID] = struct{}{}
				}
			}
		}
		localOwners := cardDAVManualOwners(bindings, members)
		owners := maps.Clone(localOwners)
		for _, id := range members {
			for _, holder := range shadowed[id] {
				edges, err := s.loadComponentEdgesTxContext(ctx, tx, holder, holder)
				if err != nil {
					return nil, err
				}
				for owner := range cardDAVManualOwners(bindings, sortedComponentMembers(holder, edges)) {
					owners[owner] = struct{}{}
				}
			}
		}
		personID, reason := cardDAVBindingTarget(wanted, owners)
		if reason != "" {
			people := cardDAVPersonSet{}
			for id := range wanted {
				people[id] = struct{}{}
			}
			for id := range owners {
				people[id] = struct{}{}
			}
			plan.result.ConflictDetails = append(plan.result.ConflictDetails, CardDAVParticipantBindingConflict{members[0], slices.Sorted(maps.Keys(people)), reason})
		}
		for _, id := range members {
			binding, exists := bindings[id]
			_, incomplete := uncertainOwners[binding.personID]
			if _, manuallyOwned := localOwners[binding.personID]; exists && binding.owned && !manuallyOwned && !incomplete && (reason != "" || personID == 0 || binding.personID != personID) {
				plan.remove = append(plan.remove, binding)
			}
			if (!exists || binding.owned && binding.personID != personID) && personID != 0 && reason == "" && !cleanupOnly {
				plan.add = append(plan.add, cardDAVParticipantBinding{personID: personID, participantID: id, owned: true})
			}
		}
	}
	plan.result.ParticipantsBound, plan.result.ParticipantsUnbound = len(plan.add), len(plan.remove)
	plan.result.Conflicts = len(plan.result.ConflictDetails)
	return plan, nil
}

// cardDAVBindingTarget names the one person a component may bind to, or why it may not.
func cardDAVBindingTarget(wanted, owners cardDAVPersonSet) (int64, string) {
	if len(wanted) > 1 {
		return 0, "shared_address"
	}
	for personID := range wanted {
		for owner := range owners {
			if owner != personID {
				return personID, "existing_person"
			}
		}
		return personID, ""
	}
	return 0, ""
}

// cardDAVManualOwners collects the people manually holding component members.
func cardDAVManualOwners(bindings map[int64]cardDAVParticipantBinding, members []int64) cardDAVPersonSet {
	owners := cardDAVPersonSet{}
	for _, id := range members {
		if binding, ok := bindings[id]; ok && !binding.owned {
			owners[binding.personID] = struct{}{}
		}
	}
	return owners
}

func (s *Store) applyCardDAVParticipantBindingsTx(ctx context.Context, tx *loggedTx, plan *cardDAVParticipantBindingPlan) error {
	changed := cardDAVPersonSet{}
	for _, binding := range plan.remove {
		if _, err := tx.ExecContext(ctx, `DELETE FROM person_participants WHERE person_id = ? AND participant_id = ? AND carddav_owned = TRUE`, binding.personID, binding.participantID); err != nil {
			return fmt.Errorf("release CardDAV participant binding: %w", err)
		}
		changed[binding.personID] = struct{}{}
	}
	for _, binding := range plan.add {
		if _, err := s.bindPersonParticipantsOwnedTx(ctx, tx, binding.personID, []int64{binding.participantID}, true); err != nil {
			return err
		}
		changed[binding.personID] = struct{}{}
	}
	if len(changed) == 0 {
		return nil
	}
	for _, personID := range slices.Sorted(maps.Keys(changed)) {
		if err := s.advanceCardDAVAutomaticBindingBaselineTx(ctx, tx, personID); err != nil {
			return err
		}

		if err := s.markContactStateDirtyTx(ctx, tx, personID); err != nil {
			return err
		}
		if err := s.bumpPersonRevisionsTx(ctx, tx, personID); err != nil {
			return err
		}
		if err := s.invalidatePersonEnrichmentIdentitiesAfterRevisionTx(ctx, tx, personID); err != nil {
			return err
		}
	}
	if _, err := s.bumpIdentityRevisionContext(ctx, tx); err != nil {
		return err
	}
	return s.coalescePersonSweepPeopleTx(ctx, tx, slices.Sorted(maps.Keys(changed)))
}

// Confirmation is a provenance-only change: membership and its revisions stay
// unchanged. A later card edit cannot remove an explicitly confirmed binding.
func confirmCardDAVParticipantBindingsTx(ctx context.Context, tx *loggedTx, personID int64, members []int64) error {
	placeholders, args := sortedIDPlaceholders(members)
	if placeholders == "" {
		return nil
	}
	args = append([]any{personID}, args...)
	_, err := tx.ExecContext(ctx, `UPDATE person_participants SET carddav_owned = FALSE
  WHERE person_id = ? AND carddav_owned = TRUE AND participant_id IN (`+placeholders+`)`, args...)
	return err
}

func cardDAVBindingOwnershipTx(ctx context.Context, tx *loggedTx, personID int64, members []int64) (bool, error) {
	placeholders, args := sortedIDPlaceholders(members)
	if placeholders == "" {
		return false, nil
	}
	args = append([]any{personID}, args...)
	var count, manual int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(CASE WHEN carddav_owned THEN 0 ELSE 1 END),0)
 FROM person_participants WHERE person_id = ? AND participant_id IN (`+placeholders+`)`, args...).Scan(&count, &manual)
	return count > 0 && manual == 0, err
}

// Carry only baselines proven current before automatic identity bookkeeping;
// never hide an earlier user edit behind a newer automatic person revision.
func (s *Store) advanceCardDAVAutomaticBindingBaselineTx(ctx context.Context, tx *loggedTx, personID int64) error {
	if _, err := tx.ExecContext(ctx, `UPDATE carddav_resources SET person_revision_at_bind = person_revision_at_bind + 1
   WHERE person_id = ? AND person_revision_at_bind = (SELECT revision FROM persons WHERE id = ?)`, personID, personID); err != nil {
		return fmt.Errorf("advance automatic CardDAV binding baseline: %w", err)
	}
	return nil
}
