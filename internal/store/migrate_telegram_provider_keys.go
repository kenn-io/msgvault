package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"go.kenn.io/msgvault/internal/beeperidentity"
)

// migrationTelegramSharedProviderKeys rewrites the account-scoped Telegram
// provider identity keys a Beeper bridge wrote before Telegram native IDs
// moved to one shared service-scoped key, so Telegram export observations
// and Beeper observations of the same person stop being two identities. The
// scope columns of the rewritten rows are cleared with the key, because the
// shared key's namespace is the whole service and an account scope beside
// it would keep splitting one identity into per-account candidate edges.
const migrationTelegramSharedProviderKeys = "telegram_shared_provider_keys_v1"

// telegramKeyPrefix narrows the candidate rows before Go-side validation
// ("telegram" is 8 bytes, so the length prefix is fixed). Only the trailing
// percent is a wildcard; sharedTelegramKeyRewrite does the exact filtering.
const telegramKeyPrefix = "provider:8:telegram:%"

// rewriteTelegramProviderKeys rewrites the account-scoped Telegram provider
// keys (numeric native IDs only) to the shared service-scoped form in
// participant_contact_observations, identity_match_candidates, and
// identity_match_evidence, rekeys the observation conflicts whose collisions
// stay valid between distinct Telegram users onto the shared namespace,
// retires the observation conflicts the converged keys leave unsupported,
// then refreshes the stored evidence fingerprints of every surviving
// candidate whose fingerprint inputs changed so prior accepted and rejected
// decisions stay valid instead of going stale. Only proofs that matched the
// pre-migration evidence are renewed: one that was already stale keeps
// requiring the fresh review that made it stale.
func (s *Store) rewriteTelegramProviderKeys(ctx context.Context, tx *loggedTx) error {
	// Plan every rewrite and fingerprint every touched candidate before
	// the first mutation: validity is judged against the pre-migration
	// evidence, which cannot be recomputed after the rewrites land.
	observations, err := planTelegramObservationRewrites(ctx, tx)
	if err != nil {
		return err
	}
	candidateUpdates, err := planTelegramCandidateKeyRewrites(ctx, tx)
	if err != nil {
		return err
	}
	evidence, err := planTelegramEvidenceDetailRewrites(ctx, tx, observations.rewrites)
	if err != nil {
		return err
	}
	conflictRekeys, err := planTelegramObservationConflictRekeys(ctx, tx)
	if err != nil {
		return err
	}
	endpointCandidates, err := telegramCandidatesTouchingParticipants(ctx, tx, observations.participants)
	if err != nil {
		return err
	}
	touched := make(map[int64]struct{},
		len(candidateUpdates)+len(evidence.candidates)+len(endpointCandidates))
	for _, update := range candidateUpdates {
		touched[update.id] = struct{}{}
	}
	for _, id := range evidence.candidates {
		touched[id] = struct{}{}
	}
	for _, id := range endpointCandidates {
		touched[id] = struct{}{}
	}
	preFingerprints, err := telegramPreMigrationFingerprintsTx(ctx, tx, touched)
	if err != nil {
		return err
	}

	if err := applyTelegramObservationRewrites(ctx, tx, observations); err != nil {
		return err
	}
	survivors, err := s.applyTelegramCandidateKeyRewrites(ctx, tx, candidateUpdates)
	if err != nil {
		return err
	}
	for id := range survivors {
		touched[id] = struct{}{}
	}
	if err := applyTelegramEvidenceDetailRewrites(ctx, tx, evidence); err != nil {
		return err
	}
	// The rewrite also moves the observations under every username conflict
	// they support. Two distinct Telegram users claiming one username
	// collide just as validly in the shared namespace, so those conflicts —
	// and the decisions a conflicting observation promoted — are rekeyed
	// onto the scope-less edge instead of discarded. Rows that converge on
	// one shared edge — a second account scope holding the same collision,
	// or any row the shared namespace already carries — reconcile through
	// the canonical candidate collapse, and collisions the scope clears
	// expose for the first time are rebuilt through the same
	// observation-recording path that would have recorded them, so the
	// migration's post-state equals what the canonical matcher would have
	// built. What survives the rekey covers only conflicts whose support
	// truly vanished: the sides converged on one identity, or no current
	// observation pair still backs the conflict, and a resync cannot clear
	// those later, because an unchanged observation records early.
	// Reconcile before the fingerprint refresh, so a demoted decision is
	// renewed against the rewritten evidence and a deleted conflict leaves
	// no proof to renew.
	conflictSurvivors, err := s.rekeyTelegramObservationConflicts(ctx, tx, conflictRekeys)
	if err != nil {
		return err
	}
	for id := range conflictSurvivors {
		touched[id] = struct{}{}
	}
	materialized, err := s.materializeTelegramSharedObservationConflicts(ctx, tx, observations)
	if err != nil {
		return err
	}
	for id := range materialized {
		touched[id] = struct{}{}
	}
	if err := s.deleteUnsupportedObservationIdentityConflictsContext(ctx, tx); err != nil {
		return err
	}
	// A collapse or the conflict reconciliation may have retired some of
	// the collected IDs; only rows that still exist carry a fingerprint.
	existing, err := telegramExistingCandidates(ctx, tx, touched)
	if err != nil {
		return err
	}
	return refreshIdentityMatchFingerprintsTx(ctx, tx, existing, preFingerprints)
}

// telegramKeyRewrite maps one old account-scoped key to its shared form.
type telegramKeyRewrite struct {
	oldKey string
	newKey string
}

// telegramObservationUpdate is one contact-observation row's planned change.
type telegramObservationUpdate struct {
	id          int64
	participant int64
	rewrite     telegramKeyRewrite
	hasRewrite  bool
	normalized  bool
}

// telegramObservationPlan is the read-only plan of one archive's
// observation rewrites: the row updates to apply, the old-to-new key
// rewrites they perform (reused for evidence), and the participants whose
// observations change, because those participants' candidates have
// fingerprint inputs even when the candidate itself carries no Telegram
// key.
type telegramObservationPlan struct {
	updates      []telegramObservationUpdate
	rewrites     []telegramKeyRewrite
	participants map[int64]struct{}
}

// planTelegramObservationRewrites lists the provider-key rewrites and
// scope clears contact observations need, without changing anything: the
// rewriteTelegramProviderKeys coordinator needs the plan read-only so
// pre-migration fingerprints are captured before the first mutation.
func planTelegramObservationRewrites(
	ctx context.Context, tx *loggedTx,
) (*telegramObservationPlan, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, participant_id, address_kind, provider_user_id, normalized_value
		FROM participant_contact_observations
		WHERE provider_user_id LIKE ?
		ORDER BY id
	`, telegramKeyPrefix)
	if err != nil {
		return nil, fmt.Errorf("telegram keys: list observations: %w", err)
	}
	plan := &telegramObservationPlan{participants: map[int64]struct{}{}}
	byOldKey := make(map[string]telegramKeyRewrite)
	for rows.Next() {
		var (
			id, participant                   int64
			addressKind, provider, normalized string
		)
		if err := rows.Scan(&id, &participant, &addressKind, &provider, &normalized); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("telegram keys: scan observation: %w", err)
		}
		rewrite, ok := sharedTelegramKeyRewrite(provider)
		if !ok {
			// Already the shared form: a pre-fix bridge may still have
			// stamped an account scope beside it.
			if !telegramSharedKeyShape(provider) {
				continue
			}
			plan.updates = append(plan.updates,
				telegramObservationUpdate{id: id, participant: participant})
			plan.participants[participant] = struct{}{}
			continue
		}
		plan.updates = append(plan.updates, telegramObservationUpdate{
			id:          id,
			participant: participant,
			rewrite:     rewrite,
			hasRewrite:  true,
			normalized: addressKind == string(ContactAddressProviderIdentity) &&
				normalized == provider,
		})
		byOldKey[rewrite.oldKey] = rewrite
		plan.participants[participant] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("telegram keys: iterate observations: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("telegram keys: close observations: %w", err)
	}
	plan.rewrites = make([]telegramKeyRewrite, 0, len(byOldKey))
	for _, rewrite := range byOldKey {
		plan.rewrites = append(plan.rewrites, rewrite)
	}
	return plan, nil
}

// applyTelegramObservationRewrites rewrites provider keys on contact
// observations and clears the scope columns of every row whose key is — or
// becomes — the shared service-scoped form, so migrated rows match what the
// fixed importer writes.
func applyTelegramObservationRewrites(
	ctx context.Context, tx *loggedTx, plan *telegramObservationPlan,
) error {
	for _, update := range plan.updates {
		var err error
		switch {
		case update.hasRewrite && update.normalized:
			_, err = tx.ExecContext(ctx, `
				UPDATE participant_contact_observations
				SET provider_user_id = ?, normalized_value = ?,
				    scope_kind = NULL, scope_value = NULL
				WHERE id = ?
			`, update.rewrite.newKey, update.rewrite.newKey, update.id)
		case update.hasRewrite:
			_, err = tx.ExecContext(ctx, `
				UPDATE participant_contact_observations
				SET provider_user_id = ?, scope_kind = NULL, scope_value = NULL
				WHERE id = ?
			`, update.rewrite.newKey, update.id)
		default:
			_, err = tx.ExecContext(ctx, `
				UPDATE participant_contact_observations
				SET scope_kind = NULL, scope_value = NULL
				WHERE id = ?
			`, update.id)
		}
		if err != nil {
			return fmt.Errorf("telegram keys: rewrite observation %d: %w", update.id, err)
		}
	}
	return nil
}

// sharedTelegramKeyRewrite validates one key's shape and returns its shared
// replacement. Only numeric Telegram native IDs under the "account" scope —
// the form providerUserIDScoped produced for Beeper bridges — rewrite;
// non-numeric IDs and every other shape keep their account scoping.
func sharedTelegramKeyRewrite(key string) (telegramKeyRewrite, bool) {
	service, scopeKind, _, native, ok := beeperidentity.DecodeProviderKey(key)
	if !ok || service != "telegram" || scopeKind != "account" || native == "" {
		return telegramKeyRewrite{}, false
	}
	if !beeperidentity.NumericTelegramID(native) {
		return telegramKeyRewrite{}, false
	}
	return telegramKeyRewrite{
		oldKey: key,
		newKey: beeperidentity.TelegramStableProviderKey(native),
	}, true
}

// telegramSharedKeyShape reports whether key already is the shared
// service-scoped Telegram form the rewrites converge on. The shape check and
// the numeric-native-ID rule live beside the provider key encoding in
// internal/beeperidentity, so the migration, the bridge, and the Telegram
// export importer all accept exactly the same keys.
func telegramSharedKeyShape(key string) bool {
	return beeperidentity.TelegramSharedProviderKey(key)
}

// telegramCandidateKeyUpdate moves one stable-provider candidate onto the
// shared key's scope-less edge.
type telegramCandidateKeyUpdate struct {
	id     int64
	newKey string
}

// planTelegramCandidateKeyRewrites lists the stable-provider candidates
// whose normalized value is an account-scoped Telegram key to rewrite and
// those already carrying the shared key whose scope columns need clearing.
// Each key is validated against the key encoding itself, so a candidate
// never depends on an observation surviving. Planning is read-only, so the
// coordinator can capture pre-migration fingerprints first.
func planTelegramCandidateKeyRewrites(
	ctx context.Context, tx *loggedTx,
) ([]telegramCandidateKeyUpdate, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, normalized_value FROM identity_match_candidates
		WHERE basis = 'stable_provider_id' AND normalized_value LIKE ?
		ORDER BY id
	`, telegramKeyPrefix)
	if err != nil {
		return nil, fmt.Errorf("telegram keys: list candidates: %w", err)
	}
	var updates []telegramCandidateKeyUpdate
	for rows.Next() {
		var id int64
		var value *string
		if err := rows.Scan(&id, &value); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("telegram keys: scan candidate: %w", err)
		}
		if value == nil {
			continue
		}
		if rewrite, ok := sharedTelegramKeyRewrite(*value); ok {
			updates = append(updates, telegramCandidateKeyUpdate{id: id, newKey: rewrite.newKey})
			continue
		}
		if telegramSharedKeyShape(*value) {
			updates = append(updates, telegramCandidateKeyUpdate{id: id, newKey: *value})
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("telegram keys: iterate candidates: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("telegram keys: close candidates: %w", err)
	}
	return updates, nil
}

// applyTelegramCandidateKeyRewrites applies the planned candidate key
// rewrites. A rewrite that would land on an occupied edge reconciles with
// the canonical candidate collapse: the surviving row keeps the group's
// decisions, evidence, source support, and link ownership, and the retired
// row leaves a redirect. It returns the set of surviving candidate IDs.
func (s *Store) applyTelegramCandidateKeyRewrites(
	ctx context.Context, tx *loggedTx, updates []telegramCandidateKeyUpdate,
) (map[int64]struct{}, error) {
	survivors := make(map[int64]struct{}, len(updates))
	for _, update := range updates {
		var exists bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM identity_match_candidates WHERE id = ?)`, update.id,
		).Scan(&exists); err != nil {
			return nil, fmt.Errorf("telegram keys: check candidate %d: %w", update.id, err)
		}
		if !exists {
			// An earlier collapse already retired this row into its survivor.
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE identity_match_candidates
			SET normalized_value = ?, scope_kind = NULL, scope_value = NULL
			WHERE id = ?
		`, update.newKey, update.id); err != nil {
			return nil, fmt.Errorf("telegram keys: rewrite candidate %d: %w", update.id, err)
		}
		candidate, err := s.loadIdentityMatchCandidateMergeRowTx(ctx, tx, update.id)
		if err != nil {
			return nil, err
		}
		collisions, err := s.loadIdentityMatchCandidateMergeCollisionsTx(ctx, tx, candidate)
		if err != nil {
			return nil, err
		}
		if len(collisions) == 0 {
			survivors[candidate.ID] = struct{}{}
			continue
		}
		group := append([]identityMatchCandidateMergeRow{candidate}, collisions...)
		winner := group[0].ID
		for _, row := range group[1:] {
			if row.ID < winner {
				winner = row.ID
			}
		}
		appliedAccepted, err := s.telegramCandidateAppliedAcceptedTx(ctx, tx, group)
		if err != nil {
			return nil, err
		}
		if err := s.collapseIdentityMatchCandidateMergeGroupTx(
			ctx, tx, group, appliedAccepted, 0,
			"telegram key migration reconciled opposing identity decisions",
		); err != nil {
			return nil, err
		}
		survivors[winner] = struct{}{}
	}
	return survivors, nil
}

// loadIdentityMatchCandidateMergeRowTx loads one candidate in the merge-row
// shape the collapse machinery reads.
func (s *Store) loadIdentityMatchCandidateMergeRowTx(
	ctx context.Context, tx *loggedTx, candidateID int64,
) (identityMatchCandidateMergeRow, error) {
	rows, err := tx.QueryContext(ctx,
		identityMatchCandidateMergeSelect+` WHERE id = ?`, candidateID)
	if err != nil {
		return identityMatchCandidateMergeRow{},
			fmt.Errorf("telegram keys: load candidate %d: %w", candidateID, err)
	}
	candidates, err := scanIdentityMatchCandidateMergeRows(rows)
	if err != nil {
		return identityMatchCandidateMergeRow{},
			fmt.Errorf("telegram keys: scan candidate %d: %w", candidateID, err)
	}
	if len(candidates) != 1 {
		return identityMatchCandidateMergeRow{},
			fmt.Errorf("telegram keys: candidate %d not found", candidateID)
	}
	return candidates[0], nil
}

// telegramCandidateAppliedAcceptedTx reports whether an accepted member of
// the group is applied through the accepted-link graph: its endpoints are
// connected — directly or through a chain of participant links — so a
// collapse keeps that decision applied instead of re-marking it pending or
// replacing it with a conflict. Direct edges alone would misread an
// acceptance the archive applied through an indirect chain.
func (s *Store) telegramCandidateAppliedAcceptedTx(
	ctx context.Context, tx *loggedTx, group []identityMatchCandidateMergeRow,
) (bool, error) {
	edges, err := s.loadLinkEdgesTxContext(ctx, tx)
	if err != nil {
		return false, fmt.Errorf("telegram keys: load participant links: %w", err)
	}
	linkAdjacency := buildAdjacency(edges)
	for _, candidate := range group {
		if acceptedMergeCandidateIsLinked(candidate, linkAdjacency) {
			return true, nil
		}
	}
	return false, nil
}

// telegramEvidencePlan is the read-only plan of the evidence-detail
// rewrites: the detail renames to apply and the candidates whose evidence
// carries an affected key — the old form about to be renamed or the shared
// form already in place.
type telegramEvidencePlan struct {
	rewrites   []telegramKeyRewrite
	candidates []int64
}

// planTelegramEvidenceDetailRewrites lists the stable-provider evidence
// details to rewrite and the candidates whose evidence carries an affected
// key. Each detail is validated against the key encoding itself, so
// evidence rewriting never depends on an observation surviving. Planning is
// read-only: the candidate set is collected before the rename, so it equals
// the post-rewrite set the old inline collection produced.
func planTelegramEvidenceDetailRewrites(
	ctx context.Context, tx *loggedTx, observationRewrites []telegramKeyRewrite,
) (*telegramEvidencePlan, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT DISTINCT detail FROM identity_match_evidence WHERE detail LIKE ?
	`, telegramKeyPrefix)
	if err != nil {
		return nil, fmt.Errorf("telegram keys: list evidence details: %w", err)
	}
	var details []string
	for rows.Next() {
		var detail *string
		if err := rows.Scan(&detail); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("telegram keys: scan evidence detail: %w", err)
		}
		if detail == nil {
			continue
		}
		if _, ok := sharedTelegramKeyRewrite(*detail); ok {
			details = append(details, *detail)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("telegram keys: iterate evidence details: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("telegram keys: close evidence details: %w", err)
	}

	plan := &telegramEvidencePlan{}
	seen := make(map[string]struct{}, len(details))
	// A candidate's fingerprint inputs change when its evidence carries a
	// key that is renamed or already equals any rewrite target — the
	// observation rewrites converge on the same shared keys.
	affected := make(map[string]struct{}, len(details)+len(observationRewrites))
	for _, detail := range details {
		rewrite, _ := sharedTelegramKeyRewrite(detail)
		if _, ok := seen[rewrite.oldKey]; !ok {
			seen[rewrite.oldKey] = struct{}{}
			plan.rewrites = append(plan.rewrites, rewrite)
		}
		affected[rewrite.oldKey] = struct{}{}
		affected[rewrite.newKey] = struct{}{}
	}
	for _, rewrite := range observationRewrites {
		affected[rewrite.newKey] = struct{}{}
	}
	keys := make([]string, 0, len(affected))
	for key := range affected {
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return plan, nil
	}
	// Chunked for the same reason as the endpoint lookup: the affected-key
	// set scales with archive cardinality, not with one export.
	if err := queryInChunksContext(ctx, tx, keys, nil,
		`SELECT DISTINCT candidate_id FROM identity_match_evidence
		 WHERE detail IN (%s)`,
		func(rows *loggedRows) error {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return fmt.Errorf("telegram keys: scan evidence candidate: %w", err)
			}
			plan.candidates = append(plan.candidates, id)
			return nil
		}); err != nil {
		return nil, fmt.Errorf("telegram keys: list evidence candidates: %w", err)
	}
	return plan, nil
}

// applyTelegramEvidenceDetailRewrites renames the planned stable-provider
// evidence details onto their shared keys.
func applyTelegramEvidenceDetailRewrites(
	ctx context.Context, tx *loggedTx, plan *telegramEvidencePlan,
) error {
	for _, rewrite := range plan.rewrites {
		if _, err := tx.ExecContext(ctx, `
			UPDATE identity_match_evidence SET detail = ? WHERE detail = ?
		`, rewrite.newKey, rewrite.oldKey); err != nil {
			return fmt.Errorf("telegram keys: rewrite evidence: %w", err)
		}
	}
	return nil
}

// planTelegramObservationConflictRekeys lists the observation conflicts whose
// supporting pairs the rewrite breaks: participant conflicts still backed by
// a current observation pair under the conflict's own scope, with a
// Telegram-keyed observation joining the pair. Clearing that observation's
// scope retires the pair against the stored scope, but the collision may
// persist in the shared namespace between two distinct Telegram users, and
// a resync of the unchanged observations records early — so those conflicts
// must be rebuilt here rather than dropped. Planning is read-only, before
// the first mutation, so support is judged against pre-rewrite observations
// and a conflict already unsupported before the migration keeps the
// retirement the general reconciliation owes it.
func planTelegramObservationConflictRekeys(
	ctx context.Context, tx *loggedTx,
) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT c.id
		FROM identity_match_candidates c
		WHERE c.left_kind = 'participant'
		  AND c.right_kind = 'participant'
		  AND c.state = 'conflict'
		  AND c.observation_conflict_origin IN ('generated', 'promoted')
		  AND c.normalized_value IS NOT NULL
		  AND c.basis IN ('email', 'phone', 'service_scope_username')
		  AND (c.scope_kind IS NOT NULL OR c.scope_value IS NOT NULL)
		  AND EXISTS (
			SELECT 1 FROM participant_contact_observations pre_left
			WHERE pre_left.participant_id = c.left_id
			  AND `+identityCandidateObservationMatchSQL("pre_left")+`
			  AND EXISTS (
				SELECT 1 FROM participant_contact_observations pre_right
				WHERE pre_right.participant_id = c.right_id
				  AND `+identityCandidateObservationMatchSQL("pre_right")+`
				  AND `+identityCandidateObservationPairSQL("pre_left", "pre_right")+`
				  AND (pre_left.provider_user_id LIKE ? OR pre_right.provider_user_id LIKE ?)
			  )
		)
		ORDER BY c.id
	`, telegramKeyPrefix, telegramKeyPrefix)
	if err != nil {
		return nil, fmt.Errorf("telegram keys: list rekeyable conflicts: %w", err)
	}
	conflicts := make([]int64, 0)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("telegram keys: scan rekeyable conflict: %w", err)
		}
		conflicts = append(conflicts, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("telegram keys: iterate rekeyable conflicts: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("telegram keys: close rekeyable conflicts: %w", err)
	}
	return conflicts, nil
}

// rekeyTelegramObservationConflicts moves the planned conflicts onto the
// shared namespace their supporting observations were rewritten into: scope
// columns cleared to match, every decision column — state, conflict origin,
// pre-conflict state, decision author — kept as it was. A conflict moves
// only while a supporting pair with distinct provider users still exists
// between the same participants under the shared scope, so a collision whose
// sides converged on one Telegram identity is left for the general
// reconciliation to retire. A conflict that lands on an occupied edge — a
// second account scope holding the same collision, or any row the shared
// namespace already carries, whatever its state — reconciles through the
// canonical candidate collapse rather than a statement-level existence
// check, which cannot see the duplicate it forks when two scoped conflicts
// rekey in one statement. The row already holding the scope-less edge is
// the collapse's preferred survivor, so the edge keeps its identity while
// the group's decisions, evidence, and source support merge under one
// policy. It returns the set of surviving candidate IDs.
func (s *Store) rekeyTelegramObservationConflicts(
	ctx context.Context, tx *loggedTx, conflictIDs []int64,
) (map[int64]struct{}, error) {
	survivors := make(map[int64]struct{}, len(conflictIDs))
	for _, conflictID := range conflictIDs {
		var exists bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM identity_match_candidates WHERE id = ?)`,
			conflictID,
		).Scan(&exists); err != nil {
			return nil, fmt.Errorf("telegram keys: check conflict %d: %w", conflictID, err)
		}
		if !exists {
			// An earlier collapse already retired this row into its survivor.
			continue
		}
		candidate, err := s.loadIdentityMatchCandidateMergeRowTx(ctx, tx, conflictID)
		if err != nil {
			return nil, err
		}
		if !candidate.NormalizedValue.Valid {
			continue
		}
		supported, err := s.telegramConflictSupportedOnSharedEdgeTx(ctx, tx, candidate)
		if err != nil {
			return nil, err
		}
		if !supported {
			// The collision did not survive the rewrite: the general
			// reconciliation retires the row.
			continue
		}
		occupantID, err := s.telegramSharedEdgeOccupantTx(ctx, tx, candidate)
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE identity_match_candidates
			SET scope_kind = NULL, scope_value = NULL, updated_at = `+s.dialect.Now()+`
			WHERE id = ?
		`, conflictID); err != nil {
			return nil, fmt.Errorf("telegram keys: rekey observation conflict %d: %w",
				conflictID, err)
		}
		candidate, err = s.loadIdentityMatchCandidateMergeRowTx(ctx, tx, conflictID)
		if err != nil {
			return nil, err
		}
		collisions, err := s.loadIdentityMatchCandidateMergeCollisionsTx(ctx, tx, candidate)
		if err != nil {
			return nil, err
		}
		if len(collisions) == 0 {
			survivors[candidate.ID] = struct{}{}
			continue
		}
		group := append([]identityMatchCandidateMergeRow{candidate}, collisions...)
		winner := occupantID
		if winner == 0 {
			winner = group[0].ID
			for _, row := range group[1:] {
				if row.ID < winner {
					winner = row.ID
				}
			}
		}
		appliedAccepted, err := s.telegramCandidateAppliedAcceptedTx(ctx, tx, group)
		if err != nil {
			return nil, err
		}
		if err := s.collapseIdentityMatchCandidateMergeGroupTx(
			ctx, tx, group, appliedAccepted, winner,
			"telegram key migration reconciled converging username conflicts",
		); err != nil {
			return nil, err
		}
		survivors[winner] = struct{}{}
	}
	return survivors, nil
}

// telegramConflictSupportedOnSharedEdgeTx reports whether a current
// observation pair with distinct provider users still backs the conflict in
// the shared namespace its supporting observations were rewritten into. The
// candidate is bound as a derived row, so the shared-namespace match SQL
// reads exactly the conditions the statement-level rekey used.
func (s *Store) telegramConflictSupportedOnSharedEdgeTx(
	ctx context.Context, tx *loggedTx, candidate identityMatchCandidateMergeRow,
) (bool, error) {
	var service any
	if candidate.ServiceID.Valid {
		service = candidate.ServiceID.Int64
	}
	var supported bool
	err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM (
				SELECT CAST(? AS BIGINT) AS left_id, CAST(? AS BIGINT) AS right_id,
					CAST(? AS TEXT) AS basis, CAST(? AS BIGINT) AS service_id,
					CAST(? AS TEXT) AS normalized_value
			) c
			WHERE EXISTS (
				SELECT 1 FROM participant_contact_observations shared_left
				WHERE shared_left.participant_id = c.left_id
				  AND `+identityCandidateSharedObservationMatchSQL("shared_left")+`
				  AND EXISTS (
					SELECT 1 FROM participant_contact_observations shared_right
					WHERE shared_right.participant_id = c.right_id
					  AND `+identityCandidateSharedObservationMatchSQL("shared_right")+`
					  AND `+identityCandidateObservationPairSQL("shared_left", "shared_right")+`
				  )
			)
		)`,
		candidate.LeftID, candidate.RightID, string(candidate.Basis),
		service, candidate.NormalizedValue.String,
	).Scan(&supported)
	if err != nil {
		return false, fmt.Errorf(
			"telegram keys: check conflict %d shared support: %w", candidate.ID, err)
	}
	return supported, nil
}

// telegramSharedEdgeOccupantTx returns the row that already holds the
// scope-less edge the conflict would rekey onto, whatever its state — a
// conflict, a decided row, or a pending suggestion. The occupant is the
// collapse's preferred survivor: it keeps the edge's identity while the
// group's decisions and support merge under the canonical policy.
func (s *Store) telegramSharedEdgeOccupantTx(
	ctx context.Context, tx *loggedTx, candidate identityMatchCandidateMergeRow,
) (int64, error) {
	var service any
	if candidate.ServiceID.Valid {
		service = candidate.ServiceID.Int64
	}
	var occupantID int64
	err := tx.QueryRowContext(ctx, `
		SELECT occupant.id
		FROM identity_match_candidates occupant
		WHERE occupant.id != ?
		  AND occupant.left_kind = ?
		  AND occupant.left_id = ?
		  AND occupant.right_kind = ?
		  AND occupant.right_id = ?
		  AND occupant.basis = ?
		  AND (occupant.service_id = ? OR
			(occupant.service_id IS NULL AND CAST(? AS BIGINT) IS NULL))
		  AND occupant.scope_kind IS NULL
		  AND occupant.scope_value IS NULL
		  AND occupant.normalized_value = ?
		ORDER BY occupant.id
		LIMIT 1`,
		candidate.ID, candidate.LeftKind, candidate.LeftID,
		candidate.RightKind, candidate.RightID, string(candidate.Basis),
		service, service, candidate.NormalizedValue.String,
	).Scan(&occupantID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf(
			"telegram keys: find occupant for conflict %d: %w", candidate.ID, err)
	}
	return occupantID, nil
}

// materializeTelegramSharedObservationConflicts rebuilds, through the
// canonical observation-recording path, the conflicts the scope clears
// expose: two distinct Telegram users whose account scopes kept them apart
// collide in the shared namespace for the first time, and a pending
// cross-scope suggestion for that username must be promoted instead of
// left blocking the shared edge. The migration's post-state must equal
// what the canonical matcher would have built, so every collision between
// a rewritten observation and another current scope-less observation goes
// through upsertIdentityMatchCandidateTx with the observation-conflict
// flag the recording path uses: a pending suggestion is promoted with the
// canonical promotion columns, an already decided or conflicting row is
// left exactly as the recording path would leave it, and a genuine new
// collision becomes a generated conflict. It returns the candidate IDs the
// pass touched. Provider-identity rows are skipped: a shared stable ID is
// the same person, and those candidates went through the key-rewrite
// collapse.
func (s *Store) materializeTelegramSharedObservationConflicts(
	ctx context.Context, tx *loggedTx, plan *telegramObservationPlan,
) (map[int64]struct{}, error) {
	touched := make(map[int64]struct{})
	if len(plan.updates) == 0 {
		return touched, nil
	}
	observationIDs := make([]int64, 0, len(plan.updates))
	for _, update := range plan.updates {
		observationIDs = append(observationIDs, update.id)
	}
	type collisionKey struct {
		left, right int64
		basis       IdentityMatchBasis
		service     sql.NullInt64
		normalized  string
	}
	seen := make(map[collisionKey]struct{})
	if err := queryInChunksContext(ctx, tx, observationIDs, nil, `
		SELECT rewritten.participant_id, rewritten.address_kind,
			rewritten.service_id, rewritten.normalized_value, rewritten.source_id,
			other.participant_id
		FROM participant_contact_observations rewritten
		JOIN participant_contact_observations other
			ON other.participant_id != rewritten.participant_id
			AND other.address_kind = rewritten.address_kind
			AND (other.service_id = rewritten.service_id
				OR (other.service_id IS NULL AND rewritten.service_id IS NULL))
			AND other.scope_kind IS NULL
			AND other.scope_value IS NULL
			AND other.normalized_value = rewritten.normalized_value
			AND other.active_until IS NULL
			AND other.superseded_at IS NULL
			AND (rewritten.provider_user_id IS NULL
				OR other.provider_user_id IS NULL
				OR rewritten.provider_user_id != other.provider_user_id)
		WHERE rewritten.id IN (%s)
			AND rewritten.address_kind != 'provider_identity'
			AND rewritten.scope_kind IS NULL
			AND rewritten.scope_value IS NULL
			AND rewritten.active_until IS NULL
			AND rewritten.superseded_at IS NULL
		ORDER BY rewritten.participant_id, other.participant_id, rewritten.id`,
		func(rows *loggedRows) error {
			var (
				participant, otherParticipant int64
				addressKind                   string
				serviceID                     sql.NullInt64
				normalized                    string
				sourceID                      sql.NullInt64
			)
			if err := rows.Scan(&participant, &addressKind, &serviceID,
				&normalized, &sourceID, &otherParticipant); err != nil {
				return fmt.Errorf("telegram keys: scan shared collision: %w", err)
			}
			leftKind, leftID, rightKind, rightID, err := canonicalMatchEndpoints(
				IdentityMatchParticipant, participant,
				IdentityMatchParticipant, otherParticipant,
			)
			if err != nil {
				return fmt.Errorf("telegram keys: order shared collision endpoints: %w", err)
			}
			basis := identityMatchBasisForAddressKind(ContactAddressKind(addressKind))
			key := collisionKey{
				left: leftID, right: rightID, basis: basis,
				service: serviceID, normalized: normalized,
			}
			if _, done := seen[key]; done {
				return nil
			}
			seen[key] = struct{}{}
			var service any
			if serviceID.Valid {
				service = serviceID.Int64
			}
			var source *int64
			if sourceID.Valid {
				id := sourceID.Int64
				source = &id
			}
			value := normalized
			candidate, _, err := s.upsertIdentityMatchCandidateTx(
				ctx, tx,
				IdentityMatchCandidateInput{
					LeftKind: leftKind, LeftID: leftID,
					RightKind: rightKind, RightID: rightID,
					Basis:           basis,
					NormalizedValue: &value,
					State:           IdentityMatchStateConflict,
					Source:          ProvenanceArchiveObservation,
					SourceID:        source,
				},
				leftKind, leftID, rightKind, rightID, service, true,
			)
			if err != nil {
				return fmt.Errorf(
					"telegram keys: materialize shared observation conflict: %w", err)
			}
			touched[candidate.ID] = struct{}{}
			return nil
		}); err != nil {
		return nil, fmt.Errorf("telegram keys: list shared observation collisions: %w", err)
	}
	return touched, nil
}

// identityCandidateSharedObservationMatchSQL matches one observation against
// a candidate exactly as identityCandidateObservationMatchSQL does, except
// the candidate is judged in the shared namespace the Telegram rewrites
// converge on: the observation must carry no scope, because the rekey moves
// the conflict onto the scope-less edge its rewritten observations live in.
func identityCandidateSharedObservationMatchSQL(observationAlias string) string {
	return `(c.basis = 'email' AND ` + observationAlias + `.address_kind = 'email'
		OR c.basis = 'phone' AND ` + observationAlias + `.address_kind = 'phone'
		OR c.basis = 'service_scope_username' AND ` + observationAlias + `.address_kind NOT IN ('email', 'phone'))
		AND (` + observationAlias + `.service_id = c.service_id OR
			(` + observationAlias + `.service_id IS NULL AND c.service_id IS NULL))
		AND ` + observationAlias + `.scope_kind IS NULL
		AND ` + observationAlias + `.scope_value IS NULL
		AND ` + observationAlias + `.normalized_value = c.normalized_value
		AND ` + observationAlias + `.active_until IS NULL
		AND ` + observationAlias + `.superseded_at IS NULL`
}

// telegramCandidatesTouchingParticipants lists candidates with a
// participant endpoint in the given set. Their evidence fingerprints embed
// the endpoint participants' observations, so a rewritten observation
// changes them even when the candidate carries no Telegram key itself. Each
// side's lookup runs chunked, because archive cardinality — not the export
// — decides the set's size and an unchunked IN list would pass a backend's
// bind-variable limit inside InitSchema.
func telegramCandidatesTouchingParticipants(
	ctx context.Context, tx *loggedTx, participants map[int64]struct{},
) ([]int64, error) {
	ids := make([]int64, 0, len(participants))
	for id := range participants {
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	slices.Sort(ids)
	seen := make(map[int64]struct{})
	for _, side := range []struct{ kindColumn, idColumn string }{
		{"left_kind", "left_id"},
		{"right_kind", "right_id"},
	} {
		query := `SELECT id FROM identity_match_candidates
			WHERE ` + side.kindColumn + ` = 'participant' AND ` + side.idColumn + ` IN (%s)`
		if err := queryInChunksContext(ctx, tx, ids, nil, query, func(rows *loggedRows) error {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return fmt.Errorf("telegram keys: scan endpoint candidate: %w", err)
			}
			seen[id] = struct{}{}
			return nil
		}); err != nil {
			return nil, fmt.Errorf("telegram keys: list endpoint candidates: %w", err)
		}
	}
	candidates := make([]int64, 0, len(seen))
	for id := range seen {
		candidates = append(candidates, id)
	}
	slices.Sort(candidates)
	return candidates, nil
}

// telegramExistingCandidates filters candidate IDs down to rows that still
// exist, dropping any a collapse retired.
func telegramExistingCandidates(
	ctx context.Context, tx *loggedTx, candidateIDs map[int64]struct{},
) (map[int64]struct{}, error) {
	existing := make(map[int64]struct{}, len(candidateIDs))
	for id := range candidateIDs {
		var found bool
		err := tx.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM identity_match_candidates WHERE id = ?)`, id,
		).Scan(&found)
		if err != nil {
			return nil, fmt.Errorf("telegram keys: check candidate %d: %w", id, err)
		}
		if found {
			existing[id] = struct{}{}
		}
	}
	return existing, nil
}

// telegramPreMigrationFingerprintsTx records each candidate's fingerprint
// against the pre-migration evidence, before any rewrite lands, so the
// later refresh can renew only proofs that were valid when the migration
// started.
func telegramPreMigrationFingerprintsTx(
	ctx context.Context, tx *loggedTx, candidateIDs map[int64]struct{},
) (map[int64]string, error) {
	fingerprints := make(map[int64]string, len(candidateIDs))
	for id := range candidateIDs {
		candidate, err := getIdentityMatchCandidateTx(ctx, tx, id)
		if err != nil {
			return nil, fmt.Errorf("telegram keys: load candidate %d: %w", id, err)
		}
		fingerprint, err := identityMatchFingerprintContext(ctx, tx, candidate, false)
		if err != nil {
			return nil, fmt.Errorf("telegram keys: fingerprint candidate %d: %w", id, err)
		}
		fingerprints[id] = fingerprint
	}
	return fingerprints, nil
}

// refreshIdentityMatchFingerprintsTx recomputes the evidence fingerprints
// stored beside user decisions for the given candidates, so a decision
// recorded against the pre-rewrite evidence keeps matching its candidate.
// Only a stored fingerprint that matched the candidate's pre-migration
// evidence is refreshed: a proof that was already stale stays stale,
// because the user never approved the evidence the rewrite produced.
func refreshIdentityMatchFingerprintsTx(
	ctx context.Context, tx *loggedTx, candidateIDs map[int64]struct{},
	preFingerprints map[int64]string,
) error {
	for id := range candidateIDs {
		pre, ok := preFingerprints[id]
		if !ok {
			// No pre-migration fingerprint was captured, so validity
			// cannot be proven; the stored proof is left untouched.
			continue
		}
		candidate, err := getIdentityMatchCandidateTx(ctx, tx, id)
		if err != nil {
			return fmt.Errorf("telegram keys: load candidate %d: %w", id, err)
		}
		fingerprint, err := identityMatchFingerprintContext(ctx, tx, candidate, false)
		if err != nil {
			return fmt.Errorf("telegram keys: fingerprint candidate %d: %w", id, err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE identity_match_review_decisions SET evidence_fingerprint = ?
			WHERE candidate_id = ? AND evidence_fingerprint = ?
		`, fingerprint, id, pre); err != nil {
			return fmt.Errorf("telegram keys: refresh decision fingerprint %d: %w", id, err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE identity_match_review_receipts SET evidence_fingerprint = ?
			WHERE candidate_id = ? AND evidence_fingerprint = ?
		`, fingerprint, id, pre); err != nil {
			return fmt.Errorf("telegram keys: refresh receipt fingerprint %d: %w", id, err)
		}
	}
	return nil
}
