package store

import (
	"context"
	"fmt"

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

// rewriteTelegramProviderKeys rewrites account-scoped Telegram provider keys
// (numeric native IDs only) to the shared service-scoped form in
// participant_contact_observations, identity_match_candidates, and
// identity_match_evidence, then refreshes the stored evidence fingerprints
// of every surviving candidate whose fingerprint inputs changed so prior
// accepted and rejected decisions stay valid instead of going stale.
func (s *Store) rewriteTelegramProviderKeys(ctx context.Context, tx *loggedTx) error {
	rewrites, participants, err := rewriteTelegramObservations(ctx, tx)
	if err != nil {
		return err
	}
	candidates, err := s.rewriteTelegramCandidateKeys(ctx, tx)
	if err != nil {
		return err
	}
	evidenceCandidates, err := rewriteTelegramEvidenceDetails(ctx, tx, rewrites)
	if err != nil {
		return err
	}
	for _, id := range evidenceCandidates {
		candidates[id] = struct{}{}
	}
	endpointCandidates, err := telegramCandidatesTouchingParticipants(ctx, tx, participants)
	if err != nil {
		return err
	}
	for _, id := range endpointCandidates {
		candidates[id] = struct{}{}
	}
	// A collapse may have retired some of the collected IDs; only rows
	// that still exist carry a fingerprint.
	survivors, err := telegramExistingCandidates(ctx, tx, candidates)
	if err != nil {
		return err
	}
	return refreshIdentityMatchFingerprintsTx(ctx, tx, survivors)
}

// telegramKeyRewrite maps one old account-scoped key to its shared form.
type telegramKeyRewrite struct {
	oldKey string
	newKey string
}

// rewriteTelegramObservations rewrites provider keys on contact observations
// and clears the scope columns of every row whose key is — or becomes — the
// shared service-scoped form, so migrated rows match what the fixed
// importer writes. It returns the key rewrites (reused for evidence) and
// the set of participants whose observations changed, because those
// participants' candidates have fingerprint inputs even when the candidate
// itself carries no Telegram key.
func rewriteTelegramObservations(
	ctx context.Context, tx *loggedTx,
) ([]telegramKeyRewrite, map[int64]struct{}, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, participant_id, address_kind, provider_user_id, normalized_value
		FROM participant_contact_observations
		WHERE provider_user_id LIKE ?
		ORDER BY id
	`, telegramKeyPrefix)
	if err != nil {
		return nil, nil, fmt.Errorf("telegram keys: list observations: %w", err)
	}
	type observationUpdate struct {
		id          int64
		participant int64
		rewrite     telegramKeyRewrite
		hasRewrite  bool
		normalized  bool
	}
	var updates []observationUpdate
	byOldKey := make(map[string]telegramKeyRewrite)
	participants := make(map[int64]struct{})
	for rows.Next() {
		var (
			id, participant                   int64
			addressKind, provider, normalized string
		)
		if err := rows.Scan(&id, &participant, &addressKind, &provider, &normalized); err != nil {
			_ = rows.Close()
			return nil, nil, fmt.Errorf("telegram keys: scan observation: %w", err)
		}
		rewrite, ok := sharedTelegramKeyRewrite(provider)
		if !ok {
			// Already the shared form: a pre-fix bridge may still have
			// stamped an account scope beside it.
			if !telegramSharedKeyShape(provider) {
				continue
			}
			updates = append(updates, observationUpdate{id: id, participant: participant})
			participants[participant] = struct{}{}
			continue
		}
		updates = append(updates, observationUpdate{
			id:          id,
			participant: participant,
			rewrite:     rewrite,
			hasRewrite:  true,
			normalized: addressKind == string(ContactAddressProviderIdentity) &&
				normalized == provider,
		})
		byOldKey[rewrite.oldKey] = rewrite
		participants[participant] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, nil, fmt.Errorf("telegram keys: iterate observations: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, nil, fmt.Errorf("telegram keys: close observations: %w", err)
	}

	for _, update := range updates {
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
			return nil, nil, fmt.Errorf("telegram keys: rewrite observation %d: %w", update.id, err)
		}
	}

	rewrites := make([]telegramKeyRewrite, 0, len(byOldKey))
	for _, rewrite := range byOldKey {
		rewrites = append(rewrites, rewrite)
	}
	return rewrites, participants, nil
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

// rewriteTelegramCandidateKeys moves every stable-provider candidate whose
// normalized value is an account-scoped Telegram key onto the shared key's
// scope-less edge, and clears the scope columns of candidates already
// carrying the shared key. Each key is validated against the key encoding
// itself, so a candidate never depends on an observation surviving. A
// rewrite that would land on an occupied edge reconciles with the canonical
// candidate collapse: the surviving row keeps the group's decisions,
// evidence, source support, and link ownership, and the retired row leaves
// a redirect. It returns the set of surviving candidate IDs.
func (s *Store) rewriteTelegramCandidateKeys(
	ctx context.Context, tx *loggedTx,
) (map[int64]struct{}, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, normalized_value FROM identity_match_candidates
		WHERE basis = 'stable_provider_id' AND normalized_value LIKE ?
		ORDER BY id
	`, telegramKeyPrefix)
	if err != nil {
		return nil, fmt.Errorf("telegram keys: list candidates: %w", err)
	}
	type candidateUpdate struct {
		id     int64
		newKey string
	}
	var updates []candidateUpdate
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
			updates = append(updates, candidateUpdate{id: id, newKey: rewrite.newKey})
			continue
		}
		if telegramSharedKeyShape(*value) {
			updates = append(updates, candidateUpdate{id: id, newKey: *value})
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("telegram keys: iterate candidates: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("telegram keys: close candidates: %w", err)
	}

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
// the group has a live participant link, so a collapse keeps that decision
// applied instead of re-marking it pending.
func (s *Store) telegramCandidateAppliedAcceptedTx(
	ctx context.Context, tx *loggedTx, group []identityMatchCandidateMergeRow,
) (bool, error) {
	for _, candidate := range group {
		if candidate.State != IdentityMatchStateAccepted ||
			candidate.LeftKind != IdentityMatchParticipant ||
			candidate.RightKind != IdentityMatchParticipant {
			continue
		}
		var linked bool
		err := tx.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM participant_links
				WHERE (participant_a = ? AND participant_b = ?)
				   OR (participant_a = ? AND participant_b = ?)
			)`, candidate.LeftID, candidate.RightID,
			candidate.RightID, candidate.LeftID).Scan(&linked)
		if err != nil {
			return false, fmt.Errorf("telegram keys: check candidate link %d: %w", candidate.ID, err)
		}
		if linked {
			return true, nil
		}
	}
	return false, nil
}

// rewriteTelegramEvidenceDetails rewrites stable-provider evidence details
// and returns the candidates whose evidence rows changed. Each detail is
// validated against the key encoding itself, so evidence rewriting never
// depends on an observation surviving.
func rewriteTelegramEvidenceDetails(
	ctx context.Context, tx *loggedTx, rewrites []telegramKeyRewrite,
) ([]int64, error) {
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

	seenKeys := make(map[string]struct{}, len(rewrites))
	newKeys := make([]string, 0, len(rewrites)+len(details))
	for _, rewrite := range rewrites {
		newKeys = append(newKeys, rewrite.newKey)
	}
	for _, detail := range details {
		rewrite, _ := sharedTelegramKeyRewrite(detail)
		if _, err := tx.ExecContext(ctx, `
			UPDATE identity_match_evidence SET detail = ? WHERE detail = ?
		`, rewrite.newKey, rewrite.oldKey); err != nil {
			return nil, fmt.Errorf("telegram keys: rewrite evidence: %w", err)
		}
		if _, seen := seenKeys[rewrite.newKey]; !seen {
			seenKeys[rewrite.newKey] = struct{}{}
			newKeys = append(newKeys, rewrite.newKey)
		}
	}

	var candidates []int64
	for _, newKey := range newKeys {
		rows, err := tx.QueryContext(ctx, `
			SELECT DISTINCT candidate_id FROM identity_match_evidence WHERE detail = ?
		`, newKey)
		if err != nil {
			return nil, fmt.Errorf("telegram keys: list evidence candidates: %w", err)
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("telegram keys: scan evidence candidate: %w", err)
			}
			candidates = append(candidates, id)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("telegram keys: iterate evidence candidates: %w", err)
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("telegram keys: close evidence candidates: %w", err)
		}
	}
	return candidates, nil
}

// telegramCandidatesTouchingParticipants lists candidates with a
// participant endpoint in the given set. Their evidence fingerprints embed
// the endpoint participants' observations, so a rewritten observation
// changes them even when the candidate carries no Telegram key itself.
func telegramCandidatesTouchingParticipants(
	ctx context.Context, tx *loggedTx, participants map[int64]struct{},
) ([]int64, error) {
	ids := make([]int64, 0, len(participants))
	for id := range participants {
		ids = append(ids, id)
	}
	inClause, args := sortedIDPlaceholders(ids)
	if inClause == "" {
		return nil, nil
	}
	// The IN list appears twice, so the bound arguments do too.
	doubledArgs := append(append([]any{}, args...), args...)
	rows, err := tx.QueryContext(ctx, `
		SELECT id FROM identity_match_candidates
		WHERE (left_kind = 'participant' AND left_id IN (`+inClause+`))
		   OR (right_kind = 'participant' AND right_id IN (`+inClause+`))
	`, doubledArgs...)
	if err != nil {
		return nil, fmt.Errorf("telegram keys: list endpoint candidates: %w", err)
	}
	var candidates []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("telegram keys: scan endpoint candidate: %w", err)
		}
		candidates = append(candidates, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("telegram keys: iterate endpoint candidates: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("telegram keys: close endpoint candidates: %w", err)
	}
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

// refreshIdentityMatchFingerprintsTx recomputes the evidence fingerprints
// stored beside user decisions for the given candidates, so a decision
// recorded against the pre-rewrite evidence keeps matching its candidate.
func refreshIdentityMatchFingerprintsTx(
	ctx context.Context, tx *loggedTx, candidateIDs map[int64]struct{},
) error {
	for id := range candidateIDs {
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
			WHERE candidate_id = ?
		`, fingerprint, id); err != nil {
			return fmt.Errorf("telegram keys: refresh decision fingerprint %d: %w", id, err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE identity_match_review_receipts SET evidence_fingerprint = ?
			WHERE candidate_id = ?
		`, fingerprint, id); err != nil {
			return fmt.Errorf("telegram keys: refresh receipt fingerprint %d: %w", id, err)
		}
	}
	return nil
}
