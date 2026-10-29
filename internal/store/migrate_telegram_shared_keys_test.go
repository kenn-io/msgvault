package store_test

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/beeperidentity"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

const telegramSharedKeysMigration = "telegram_shared_provider_keys_v1"

// TestTelegramSharedKeyMigrationCollapsesDuplicateEdges recreates a pre-fix
// archive whose account-scoped Telegram keys converge on one shared
// service-scoped key. Rows are seeded with raw SQL because the store API
// refuses a scope on telegram observations (its catalog policy is "none") —
// the pre-fix shapes predate that refusal. Runs on SQLite and PostgreSQL.
func TestTelegramSharedKeyMigrationCollapsesDuplicateEdges(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)

	export, err := st.EnsureParticipantByIdentifier("telegram", "482111", "Export Alice")
	require.NoError(err)
	shared := beeperidentity.ProviderKey("telegram", "service", "", "482111")

	bridge := func(account, identifier string) (int64, string) {
		oldKey := beeperidentity.ProviderKey("telegram", "account", account, "482111")
		pid, err := st.EnsureParticipantByIdentifier("beeper", identifier, "Bridge Alice "+account)
		require.NoError(err)
		source, err := st.GetOrCreateSource("beeper", account)
		require.NoError(err)
		_, err = st.DB().Exec(st.Rebind(`
			INSERT INTO participant_contact_observations (
				participant_id, source_id, address_kind, scope_kind, scope_value,
				provider_user_id, original_value, normalized_value,
				normalization, source
			) VALUES (?, ?, 'provider_identity', 'account', ?, ?, ?, ?, 'none', 'archive_observation')
		`), pid, source.ID, account, oldKey, oldKey, oldKey)
		require.NoError(err)
		return pid, oldKey
	}
	bridgeSeven, oldSeven := bridge("acct-7", "beeper:6:acct-7:9:bridge-7")
	bridgeEight, oldEight := bridge("acct-8", "beeper:6:acct-8:9:bridge-8")

	// The export-side observation already carries the shared key with no
	// scope — what the fixed importer writes.
	telegramSource, err := st.GetOrCreateSource("telegram", "user482111")
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`
		INSERT INTO participant_contact_observations (
			participant_id, source_id, address_kind, provider_user_id,
			original_value, normalized_value, normalization, source
		) VALUES (?, ?, 'provider_identity', ?, ?, ?, 'none', 'archive_observation')
	`), export, telegramSource.ID, shared, shared, shared)
	require.NoError(err)

	// seededCandidate writes one pre-fix candidate row with an explicit
	// scope, a state, and a decision author.
	seededCandidate := func(left int64, key, scopeValue, state, decidedBy string) int64 {
		_, err := st.DB().Exec(st.Rebind(`
			INSERT INTO identity_match_candidates (
				left_kind, left_id, right_kind, right_id, basis,
				scope_kind, scope_value, normalized_value, state,
				decided_by, decided_at, source
			) VALUES ('participant', ?, 'participant', ?, 'stable_provider_id',
				'account', ?, ?, ?, ?, CURRENT_TIMESTAMP, 'archive_observation')
		`), left, export, scopeValue, key, state, decidedBy)
		require.NoError(err)
		var id int64
		require.NoError(st.DB().QueryRow(st.Rebind(
			`SELECT id FROM identity_match_candidates WHERE left_id = ? AND normalized_value = ?`),
			left, key).Scan(&id))
		return id
	}
	evidenceFor := func(candidateID int64, detail string) {
		_, err := st.DB().Exec(st.Rebind(`
			INSERT INTO identity_match_evidence (candidate_id, evidence_kind, detail, source)
			VALUES (?, 'stable_provider_id', ?, 'archive_observation')
		`), candidateID, detail)
		require.NoError(err)
	}
	// decisionFor records a review decision against the candidate's genuine
	// pre-migration evidence fingerprint and returns that fingerprint.
	decisionFor := func(candidateID int64) string {
		fingerprint, err := store.IdentityMatchDecisionFingerprintForTest(context.Background(), st, candidateID)
		require.NoError(err, "fingerprint the pre-migration evidence")
		_, err = st.DB().Exec(st.Rebind(`
			INSERT INTO identity_match_review_decisions (candidate_id, evidence_fingerprint)
			 VALUES (?, ?)`), candidateID, fingerprint)
		require.NoError(err)
		return fingerprint
	}

	// Pair one: a candidate-state row beside a user-rejected row. Both
	// converge on the shared edge; the collapse must keep the rejection.
	pendingID := seededCandidate(bridgeSeven, oldSeven, "acct-7", "candidate", "")
	rejectedID := seededCandidate(bridgeSeven, shared, "acct-7", "rejected", "user")
	evidenceFor(rejectedID, oldSeven)
	preFingerprint := decisionFor(pendingID)

	// Pair two: an accepted-but-unapplied row beside a rejected row. The
	// collapse must record the conflict, not resurrect either decision.
	seededCandidate(bridgeEight, oldEight, "acct-8", "accepted", "user")
	rejectedTwoID := seededCandidate(bridgeEight, shared, "acct-8", "rejected", "user")
	evidenceFor(rejectedTwoID, shared)

	// Pair three: the same collision with a reviewed acceptance still
	// waiting to apply — the collapse must force a fresh review instead of
	// linking changed evidence.
	bridgeNine, oldNine := bridge("acct-9", "beeper:6:acct-9:9:bridge-9")
	reviewedID := seededCandidate(bridgeNine, oldNine, "acct-9", "accepted", "user")
	rejectedThreeID := seededCandidate(bridgeNine, shared, "acct-9", "rejected", "user")
	evidenceFor(rejectedThreeID, shared)
	decisionFor(reviewedID)

	_, err = st.DB().Exec(st.Rebind(
		`DELETE FROM applied_migrations WHERE name = ?`), telegramSharedKeysMigration)
	require.NoError(err)
	require.NoError(st.InitSchema(), "upgrade telegram provider keys")

	// Every observation converges on the shared key with no scope.
	var oldObservations int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM participant_contact_observations
		WHERE provider_user_id IN (?, ?)
	`), oldSeven, oldEight).Scan(&oldObservations))
	assert.Zero(oldObservations, "both accounts' observations converge on one shared key")
	var scoped int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT (SELECT COUNT(*) FROM participant_contact_observations
		        WHERE provider_user_id = ? AND (scope_kind IS NOT NULL OR scope_value IS NOT NULL))
		     + (SELECT COUNT(*) FROM identity_match_candidates
		        WHERE normalized_value = ? AND (scope_kind IS NOT NULL OR scope_value IS NOT NULL))
	`), shared, shared).Scan(&scoped))
	assert.Zero(scoped, "no shared-key row keeps an account scope")

	// No stale rows and no duplicate scope-less edges remain.
	var staleCandidates int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM identity_match_candidates WHERE normalized_value IN (?, ?)
	`), oldSeven, oldEight).Scan(&staleCandidates))
	assert.Zero(staleCandidates, "colliding rows collapse instead of staying stale")
	var duplicateEdges int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM (
			SELECT left_id, right_id
			FROM identity_match_candidates
			WHERE basis = 'stable_provider_id' AND normalized_value = ?
			  AND scope_kind IS NULL AND scope_value IS NULL
			GROUP BY left_id, right_id
			HAVING COUNT(*) > 1
		)
	`), shared).Scan(&duplicateEdges))
	assert.Zero(duplicateEdges, "one shared edge per participant pair")

	// Pair one kept the rejection: the lowest-id row survives as rejected
	// with the user decision, and the loser left a redirect and its
	// evidence behind on the survivor.
	var state, decidedBy string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT state, COALESCE(decided_by, '') FROM identity_match_candidates
		WHERE left_id = ? AND right_id = ?`), bridgeSeven, export).Scan(&state, &decidedBy))
	assert.Equal("rejected", state, "pending + rejected stays rejected")
	assert.Equal("user", decidedBy, "the user decision survives the collapse")
	var redirects int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM identity_match_candidate_redirects r
		JOIN identity_match_candidates c ON c.id = r.surviving_candidate_id
		WHERE r.retired_candidate_id <> c.id
		  AND (c.left_id = ? AND c.right_id = ?)`), bridgeSeven, export).Scan(&redirects))
	assert.Equal(1, redirects, "the retired row redirects to the survivor")
	var evidenceOnSurvivor int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM identity_match_evidence e
		JOIN identity_match_candidates c ON c.id = e.candidate_id
		WHERE c.left_id = ? AND c.right_id = ?`), bridgeSeven, export).Scan(&evidenceOnSurvivor))
	assert.Equal(1, evidenceOnSurvivor, "the loser's evidence moved to the survivor")

	// Pair two collapsed to a conflict.
	var conflictState, notes string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT state, COALESCE(notes, '') FROM identity_match_candidates
		WHERE left_id = ? AND right_id = ?`), bridgeEight, export).Scan(&conflictState, &notes))
	assert.Equal("conflict", conflictState, "accepted + rejected collapses to a conflict")
	assert.Contains(notes, "reconcil", "the conflict names what happened")

	// Pair three: the reviewed acceptance is demoted to a conflict that
	// asks for a fresh review rather than linking changed evidence.
	var reviewedState, reviewedNotes string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT state, COALESCE(notes, '') FROM identity_match_candidates
		WHERE left_id = ? AND right_id = ?`), bridgeNine, export).Scan(&reviewedState, &reviewedNotes))
	assert.Equal("conflict", reviewedState, "a pending reviewed acceptance cannot survive collapsed evidence")
	assert.Contains(reviewedNotes, "review again", "the guard names what must happen next")

	// The survivor's stored decision fingerprint was recomputed instead of
	// comparing against the pre-rewrite evidence.
	var fingerprint string
	var survivorID int64
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT d.evidence_fingerprint, d.candidate_id FROM identity_match_review_decisions d
		JOIN identity_match_candidates c ON c.id = d.candidate_id
		WHERE c.left_id = ? AND c.right_id = ?`), bridgeSeven, export).Scan(&fingerprint, &survivorID))
	postFingerprint, err := store.IdentityMatchDecisionFingerprintForTest(context.Background(), st, survivorID)
	require.NoError(err)
	assert.Equal(postFingerprint, fingerprint, "fingerprint matches the rewritten evidence")
	assert.NotEqual(preFingerprint, fingerprint, "the pre-rewrite fingerprint no longer matches")
}

// TestTelegramSharedKeyMigrationReconcilesOrphanedConflicts pins the cleanup
// the rewrite owes its orphans: converging the rewritten keys leaves both the
// generated username conflicts those observations supported — and decisions a
// conflicting observation promoted — without a supporting pair, and a resync
// cannot clear them later because an unchanged observation records early. The
// migration reconciles them itself, before the fingerprint refresh, so the
// restored user decision is renewed against the rewritten evidence while a
// proof that was stale before the migration stays stale. Runs on SQLite and
// PostgreSQL.
func TestTelegramSharedKeyMigrationReconcilesOrphanedConflicts(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)

	// One Beeper account bridged the same Telegram username for one Telegram
	// user under two key forms: the account-scoped key (first and second)
	// and the shared key a later bridge version wrote beside a leftover
	// scope (middle). Pre-fix, the two forms looked like two users claiming
	// one username, so the rewrite converging them retires the conflicts.
	source, err := st.GetOrCreateSource("beeper", "acct-7")
	require.NoError(err)
	seedUsername := func(identifier, providerKey string) int64 {
		pid, err := st.EnsureParticipantByIdentifier("beeper", identifier, "Bridge "+identifier)
		require.NoError(err)
		_, err = st.DB().Exec(st.Rebind(`
			INSERT INTO participant_contact_observations (
				participant_id, source_id, address_kind, scope_kind, scope_value,
				provider_user_id, original_value, normalized_value,
				normalization, source
			) VALUES (?, ?, 'username', 'account', 'acct-7', ?,
				'shared-handle', 'shared-handle', 'none', 'archive_observation')
		`), pid, source.ID, providerKey)
		require.NoError(err)
		return pid
	}
	firstKey := beeperidentity.ProviderKey("telegram", "account", "acct-7", "482111")
	shared := beeperidentity.ProviderKey("telegram", "service", "", "482111")
	first := seedUsername("beeper:6:acct-7:9:conflict-first", firstKey)
	middle := seedUsername("beeper:6:acct-7:9:conflict-middle", shared)
	second := seedUsername("beeper:6:acct-7:9:conflict-second", firstKey)

	// conflictCandidate seeds one username conflict the observations
	// generated on a fresh edge.
	conflictCandidate := func(left, right int64, origin, preConflictState, decidedBy any) int64 {
		_, err := st.DB().Exec(st.Rebind(`
			INSERT INTO identity_match_candidates (
				left_kind, left_id, right_kind, right_id, basis,
				scope_kind, scope_value, normalized_value, state,
				observation_conflict_origin, pre_conflict_state,
				decided_by, decided_at, source
			) VALUES ('participant', ?, 'participant', ?, 'service_scope_username',
				'account', 'acct-7', 'shared-handle', 'conflict', ?, ?, ?,
				CURRENT_TIMESTAMP, 'archive_observation')
		`), left, right, origin, preConflictState, decidedBy)
		require.NoError(err)
		var id int64
		require.NoError(st.DB().QueryRow(st.Rebind(`
			SELECT id FROM identity_match_candidates
			WHERE left_id = ? AND right_id = ?
			  AND observation_conflict_origin IS NOT NULL`), left, right).Scan(&id))
		return id
	}
	// The first-middle conflict was generated when the middle binding
	// arrived claiming the same username; the middle-second row is the
	// user's rejection, promoted to a conflict when the same Telegram
	// user's account-scoped key claimed that username again. Both sides of
	// each pair converge on one shared key, so both conflicts lose their
	// supporting pair for good.
	generatedID := conflictCandidate(first, middle, "generated", nil, nil)
	promotedID := conflictCandidate(middle, second, "promoted", "rejected", "user")
	promotedFingerprint, err := store.IdentityMatchDecisionFingerprintForTest(
		context.Background(), st, promotedID)
	require.NoError(err, "fingerprint the promoted decision's pre-migration evidence")
	_, err = st.DB().Exec(st.Rebind(`
		INSERT INTO identity_match_review_decisions (candidate_id, evidence_fingerprint)
		VALUES (?, ?)`), promotedID, promotedFingerprint)
	require.NoError(err)

	// A user-rejected row whose stored proof was already stale before the
	// migration keeps requiring the fresh review that made it stale.
	_, err = st.DB().Exec(st.Rebind(`
		INSERT INTO identity_match_candidates (
			left_kind, left_id, right_kind, right_id, basis,
			scope_kind, scope_value, normalized_value, state,
			decided_by, decided_at, source
		) VALUES ('participant', ?, 'participant', ?, 'service_scope_username',
			'account', 'acct-7', 'shared-handle', 'rejected',
			'user', CURRENT_TIMESTAMP, 'archive_observation')
	`), first, second)
	require.NoError(err)
	var rejectedID int64
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT id FROM identity_match_candidates
		WHERE left_id = ? AND right_id = ? AND state = 'rejected'`),
		first, second).Scan(&rejectedID))
	_, err = st.DB().Exec(st.Rebind(`
		INSERT INTO identity_match_review_decisions (candidate_id, evidence_fingerprint)
		VALUES (?, 'stale-before-migration')`), rejectedID)
	require.NoError(err)

	_, err = st.DB().Exec(st.Rebind(
		`DELETE FROM applied_migrations WHERE name = ?`), telegramSharedKeysMigration)
	require.NoError(err)
	require.NoError(st.InitSchema(), "upgrade telegram provider keys")

	// The rewritten username observations converge on scope-less keys.
	var staleUsernameObservations int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM participant_contact_observations
		WHERE address_kind = 'username'
		  AND (scope_kind IS NOT NULL OR scope_value IS NOT NULL
		       OR provider_user_id = ?)
	`), firstKey).Scan(&staleUsernameObservations))
	assert.Zero(staleUsernameObservations,
		"the rewritten username observations converge on scope-less shared keys")

	// The generated conflict is gone: the pair that supported it no longer
	// exists, and a resync of unchanged observations cannot retire it.
	var generatedConflicts int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM identity_match_candidates
		WHERE id = ? AND state = 'conflict'`), generatedID).Scan(&generatedConflicts))
	assert.Zero(generatedConflicts,
		"the migration deletes the generated conflict its rewrite orphaned")

	// The promoted conflict returned to the user's rejection.
	var state, decidedBy, origin string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT state, COALESCE(decided_by, ''), COALESCE(observation_conflict_origin, '')
		FROM identity_match_candidates WHERE id = ?`), promotedID).
		Scan(&state, &decidedBy, &origin))
	assert.Equal("rejected", state, "the promoted conflict demotes to the user's decision")
	assert.Equal("user", decidedBy, "the explicit decision author survives the demotion")
	assert.Empty(origin, "demotion clears the conflict origin")

	// The restored decision's proof was renewed against the rewritten
	// evidence; the proof that was stale before the migration stays stale.
	var renewed, stale string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT evidence_fingerprint FROM identity_match_review_decisions
		WHERE candidate_id = ?`), promotedID).Scan(&renewed))
	postFingerprint, err := store.IdentityMatchDecisionFingerprintForTest(
		context.Background(), st, promotedID)
	require.NoError(err)
	assert.Equal(postFingerprint, renewed,
		"the demoted decision's fingerprint matches the rewritten evidence")
	assert.NotEqual(promotedFingerprint, renewed,
		"the pre-rewrite fingerprint no longer matches")
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT evidence_fingerprint FROM identity_match_review_decisions
		WHERE candidate_id = ?`), rejectedID).Scan(&stale))
	assert.Equal("stale-before-migration", stale,
		"a proof already stale before the migration is not renewed")
}

// TestTelegramSharedKeyMigrationRekeysValidUsernameConflicts pins the
// conflicts the rewrite owes a rebuild, not a retirement: two distinct
// numeric Telegram users claiming one username in one account scope collide
// just as validly in the shared namespace, so their conflicts survive the
// migration rekeyed onto the shared keys — a generated conflict intact, a
// decision a conflicting observation promoted still promoted with its proof
// renewed — instead of losing the user decision signal. A conflict whose
// scope-less edge a post-fix conflict already occupies is retired rather
// than rekeyed, so one collision never forks into two reviewable rows. Rows
// are seeded with raw SQL for the same pre-fix-shape reason as above. Runs
// on SQLite and PostgreSQL.
func TestTelegramSharedKeyMigrationRekeysValidUsernameConflicts(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)

	source, err := st.GetOrCreateSource("beeper", "acct-7")
	require.NoError(err)
	bridge := func(identifier string) int64 {
		pid, err := st.EnsureParticipantByIdentifier("beeper", identifier, "Bridge "+identifier)
		require.NoError(err)
		return pid
	}
	seedUsername := func(pid int64, normalized, providerKey string, scopeKind, scopeValue any) {
		_, err := st.DB().Exec(st.Rebind(`
			INSERT INTO participant_contact_observations (
				participant_id, source_id, address_kind, scope_kind, scope_value,
				provider_user_id, original_value, normalized_value,
				normalization, source
			) VALUES (?, ?, 'username', ?, ?, ?, ?, ?, 'none', 'archive_observation')
		`), pid, source.ID, scopeKind, scopeValue, providerKey, normalized, normalized)
		require.NoError(err)
	}
	conflictCandidate := func(left, right int64, normalized string, scopeKind, scopeValue,
		origin, preConflictState, decidedBy any,
	) int64 {
		_, err := st.DB().Exec(st.Rebind(`
			INSERT INTO identity_match_candidates (
				left_kind, left_id, right_kind, right_id, basis,
				scope_kind, scope_value, normalized_value, state,
				observation_conflict_origin, pre_conflict_state,
				decided_by, decided_at, source
			) VALUES ('participant', ?, 'participant', ?, 'service_scope_username',
				?, ?, ?, 'conflict', ?, ?, ?,
				CURRENT_TIMESTAMP, 'archive_observation')
		`), left, right, scopeKind, scopeValue, normalized,
			origin, preConflictState, decidedBy)
		require.NoError(err)
		var id int64
		require.NoError(st.DB().QueryRow(st.Rebind(`
			SELECT id FROM identity_match_candidates
			WHERE left_id = ? AND right_id = ? AND normalized_value = ?
			  AND (scope_kind = ? OR (scope_kind IS NULL AND CAST(? AS TEXT) IS NULL))
			  AND (scope_value = ? OR (scope_value IS NULL AND CAST(? AS TEXT) IS NULL))
			  AND observation_conflict_origin IS NOT NULL`),
			left, right, normalized, scopeKind, scopeKind, scopeValue, scopeValue).Scan(&id))
		return id
	}

	// Two distinct Telegram users claimed one username in one account scope.
	firstKey := beeperidentity.ProviderKey("telegram", "account", "acct-7", "482111")
	otherKey := beeperidentity.ProviderKey("telegram", "account", "acct-7", "999777")
	first := bridge("beeper:6:acct-7:9:rekey-first")
	seedUsername(first, "split-handle", firstKey, "account", "acct-7")
	second := bridge("beeper:6:acct-7:9:rekey-second")
	seedUsername(second, "split-handle", otherKey, "account", "acct-7")
	third := bridge("beeper:6:acct-7:9:rekey-third")
	seedUsername(third, "split-handle", otherKey, "account", "acct-7")
	generatedID := conflictCandidate(first, second, "split-handle",
		"account", "acct-7", "generated", nil, nil)
	promotedID := conflictCandidate(first, third, "split-handle",
		"account", "acct-7", "promoted", "rejected", "user")
	promotedFingerprint, err := store.IdentityMatchDecisionFingerprintForTest(
		context.Background(), st, promotedID)
	require.NoError(err, "fingerprint the promoted decision's pre-migration evidence")
	_, err = st.DB().Exec(st.Rebind(`
		INSERT INTO identity_match_review_decisions (candidate_id, evidence_fingerprint)
		VALUES (?, ?)`), promotedID, promotedFingerprint)
	require.NoError(err)

	// The occupied edge: a post-fix conflict already holds the scope-less
	// pair — backed by scope-less observations — while the pre-fix
	// account-scoped conflict describes the same collision.
	fourth := bridge("beeper:6:acct-7:9:rekey-fourth")
	seedUsername(fourth, "occupied-handle", firstKey, "account", "acct-7")
	seedUsername(fourth, "occupied-handle",
		beeperidentity.ProviderKey("telegram", "service", "", "482111"), nil, nil)
	fifth := bridge("beeper:6:acct-7:9:rekey-fifth")
	seedUsername(fifth, "occupied-handle", otherKey, "account", "acct-7")
	seedUsername(fifth, "occupied-handle",
		beeperidentity.ProviderKey("telegram", "service", "", "999777"), nil, nil)
	scopedID := conflictCandidate(fourth, fifth, "occupied-handle",
		"account", "acct-7", "generated", nil, nil)
	occupantID := conflictCandidate(fourth, fifth, "occupied-handle",
		nil, nil, "generated", nil, nil)

	_, err = st.DB().Exec(st.Rebind(
		`DELETE FROM applied_migrations WHERE name = ?`), telegramSharedKeysMigration)
	require.NoError(err)
	require.NoError(st.InitSchema(), "upgrade telegram provider keys")

	// The generated conflict between two distinct Telegram users survives
	// on the shared keys: same state, same origin, scope-less edge.
	var state, origin, scopeKind, scopeValue string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT state, COALESCE(observation_conflict_origin, ''),
		       COALESCE(scope_kind, ''), COALESCE(scope_value, '')
		FROM identity_match_candidates WHERE id = ?`), generatedID).
		Scan(&state, &origin, &scopeKind, &scopeValue))
	assert.Equal("conflict", state,
		"the distinct-user conflict stays a conflict on the shared keys")
	assert.Equal("generated", origin,
		"the surviving conflict keeps its origin")
	assert.Empty(scopeKind, "the surviving conflict is rekeyed onto the scope-less edge")
	assert.Empty(scopeValue, "the surviving conflict is rekeyed onto the scope-less edge")

	// The promoted conflict keeps its promotion — the user decision it
	// suspended — and its proof is renewed against the rewritten evidence.
	var preConflictState, decidedBy string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT state, COALESCE(observation_conflict_origin, ''),
		       COALESCE(pre_conflict_state, ''), COALESCE(decided_by, ''),
		       COALESCE(scope_kind, ''), COALESCE(scope_value, '')
		FROM identity_match_candidates WHERE id = ?`), promotedID).
		Scan(&state, &origin, &preConflictState, &decidedBy, &scopeKind, &scopeValue))
	assert.Equal("conflict", state, "the promotion survives the migration")
	assert.Equal("promoted", origin, "the promoted conflict stays promoted")
	assert.Equal("rejected", preConflictState,
		"the suspended user decision survives the migration")
	assert.Equal("user", decidedBy, "the decision author survives the migration")
	assert.Empty(scopeKind, "the promoted conflict is rekeyed onto the scope-less edge")
	assert.Empty(scopeValue, "the promoted conflict is rekeyed onto the scope-less edge")
	var renewed string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT evidence_fingerprint FROM identity_match_review_decisions
		WHERE candidate_id = ?`), promotedID).Scan(&renewed))
	postFingerprint, err := store.IdentityMatchDecisionFingerprintForTest(
		context.Background(), st, promotedID)
	require.NoError(err)
	assert.Equal(postFingerprint, renewed,
		"the surviving conflict's proof matches the rewritten evidence")
	assert.NotEqual(promotedFingerprint, renewed,
		"the pre-rewrite fingerprint no longer matches")

	// The occupied edge keeps its post-fix conflict; the pre-fix
	// account-scoped duplicate is retired instead of forking a second row.
	var retired int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM identity_match_candidates WHERE id = ?`), scopedID).
		Scan(&retired))
	assert.Zero(retired,
		"the account-scoped conflict yields to the conflict holding the shared edge")
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT state, COALESCE(scope_kind, ''), COALESCE(scope_value, '')
		FROM identity_match_candidates WHERE id = ?`), occupantID).
		Scan(&state, &scopeKind, &scopeValue))
	assert.Equal("conflict", state, "the occupant conflict survives untouched")
	assert.Empty(scopeKind, "the occupant conflict keeps its scope-less edge")
	assert.Empty(scopeValue, "the occupant conflict keeps its scope-less edge")
}

// insertObservationBatch inserts count observation rows in bounded
// multi-row statements, so a scale fixture never itself trips a backend's
// bind-variable limit. One observation argument set per row keeps each
// statement far below the 32,766-variable SQLite ceiling.
func insertObservationBatch(t *testing.T, st *store.Store, count int, row func(i int) []any) {
	t.Helper()
	const chunk = 250
	for start := 0; start < count; start += chunk {
		end := min(start+chunk, count)
		values := make([]string, 0, end-start)
		args := make([]any, 0, (end-start)*10)
		for i := start; i < end; i++ {
			rowArgs := row(i)
			placeholders := make([]string, len(rowArgs))
			for j, arg := range rowArgs {
				placeholders[j] = "?"
				args = append(args, arg)
			}
			values = append(values, "("+strings.Join(placeholders, ",")+")")
		}
		_, err := st.DB().Exec(st.Rebind(`
			INSERT INTO participant_contact_observations (
				participant_id, address_kind, scope_kind, scope_value,
				provider_user_id, original_value, normalized_value,
				normalization, source
			) VALUES `+strings.Join(values, ",")), args...)
		require.NoError(t, err)
	}
}

// TestTelegramSharedKeyMigrationSurvivesParticipantScale pins the
// bind-variable boundary: an archive whose rewrites touch 16,384
// participants makes the endpoint lookup bind each id twice — 32,768
// arguments, past SQLite's 32,766-variable limit — so the unchunked
// migration failed inside InitSchema and blocked opening the archive. The
// chunked lookups keep every statement under the limit on both dialects.
func TestTelegramSharedKeyMigrationSurvivesParticipantScale(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)

	const participants = 16384
	const chunk = 250
	for start := 0; start < participants; start += chunk {
		end := min(start+chunk, participants)
		values := make([]string, 0, end-start)
		args := make([]any, 0, end-start)
		for i := start; i < end; i++ {
			values = append(values, "(?)")
			args = append(args, fmt.Sprintf("Scale Participant %d", i))
		}
		_, err := st.DB().Exec(st.Rebind(
			`INSERT INTO participants (display_name) VALUES `+strings.Join(values, ",")), args...)
		require.NoError(err)
	}
	ids := make([]int64, 0, participants)
	rows, err := st.DB().Query(`SELECT id FROM participants ORDER BY id`)
	require.NoError(err)
	defer func() { require.NoError(rows.Close()) }()
	for rows.Next() {
		var id int64
		require.NoError(rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(rows.Err())
	require.Len(ids, participants)

	insertObservationBatch(t, st, participants, func(i int) []any {
		key := beeperidentity.ProviderKey("telegram", "account", "acct-7",
			strconv.FormatInt(int64(482000+i), 10))
		return []any{ids[i], "provider_identity", "account", "acct-7", key, key, key, "none", "archive_observation"}
	})

	_, err = st.DB().Exec(st.Rebind(
		`DELETE FROM applied_migrations WHERE name = ?`), telegramSharedKeysMigration)
	require.NoError(err)
	require.NoError(st.InitSchema(), "rewrite 16,384 participants within the bind limits")

	var remaining int
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM participant_contact_observations
		 WHERE provider_user_id LIKE 'provider:8:telegram:7:account:%'`)).Scan(&remaining))
	assert.Zero(remaining, "every account-scoped key converges on the shared form")
}

// TestTelegramSharedKeyMigrationKeepsChainAppliedAcceptance pins how the
// collapse judges an accepted decision: applied-ness follows the link
// graph's connectivity, not direct edges. An acceptance whose endpoints are
// joined through a two-hop link chain is already applied, so the collapse
// keeps it accepted instead of replacing it with a conflict.
func TestTelegramSharedKeyMigrationKeepsChainAppliedAcceptance(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)

	bridge, err := st.EnsureParticipantByIdentifier("beeper", "beeper:6:acct-7:9:bridge-7", "Bridge Alice")
	require.NoError(err)
	mid, err := st.EnsureParticipantByIdentifier("telegram", "999001", "Chain Mid")
	require.NoError(err)
	export, err := st.EnsureParticipantByIdentifier("telegram", "482111", "Export Alice")
	require.NoError(err)

	oldKey := beeperidentity.ProviderKey("telegram", "account", "acct-7", "482111")
	shared := beeperidentity.ProviderKey("telegram", "service", "", "482111")
	seededCandidate := func(key, scopeValue, state string) int64 {
		_, err := st.DB().Exec(st.Rebind(`
			INSERT INTO identity_match_candidates (
				left_kind, left_id, right_kind, right_id, basis,
				scope_kind, scope_value, normalized_value, state,
				decided_by, decided_at, source
			) VALUES ('participant', ?, 'participant', ?, 'stable_provider_id',
				'account', ?, ?, ?, 'user', CURRENT_TIMESTAMP, 'archive_observation')
		`), bridge, export, scopeValue, key, state)
		require.NoError(err)
		var id int64
		require.NoError(st.DB().QueryRow(st.Rebind(
			`SELECT id FROM identity_match_candidates WHERE normalized_value = ?`), key).Scan(&id))
		return id
	}
	acceptedID := seededCandidate(oldKey, "acct-7", "accepted")
	rejectedID := seededCandidate(shared, "acct-7", "rejected")

	// The acceptance is applied through a two-hop chain: the archive links
	// the bridge participant to the export participant through a middle
	// participant, without a direct edge.
	_, err = st.LinkParticipants(bridge, mid)
	require.NoError(err)
	_, err = st.LinkParticipants(mid, export)
	require.NoError(err)

	_, err = st.DB().Exec(st.Rebind(
		`DELETE FROM applied_migrations WHERE name = ?`), telegramSharedKeysMigration)
	require.NoError(err)
	require.NoError(st.InitSchema(), "upgrade telegram provider keys")

	var state string
	var pending bool
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT state, application_pending FROM identity_match_candidates WHERE id = ?`),
		min(acceptedID, rejectedID)).Scan(&state, &pending))
	assert.Equal("accepted", state, "a chain-applied acceptance survives the collapse as applied")
	assert.False(pending, "the applied decision is not re-marked pending")
	var survivors int
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM identity_match_candidates WHERE id IN (?, ?)`),
		acceptedID, rejectedID).Scan(&survivors))
	assert.Equal(1, survivors, "the colliding edge collapses to one row")
}
