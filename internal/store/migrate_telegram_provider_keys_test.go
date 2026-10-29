package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/beeperidentity"
)

// seedTelegramKeyRows recreates a pre-migration archive: a Beeper bridge
// observed Telegram user 482111 under an account-scoped key, a
// stable-provider candidate was accepted, and a review decision recorded a
// fingerprint against that evidence. It returns the observing participant,
// the candidate id, the old and shared keys, and the seeded pre-migration
// fingerprint.
func seedTelegramKeyRows(t *testing.T, st *Store) (left, candidateID int64, oldKey, newKey, preFingerprint string) {
	t.Helper()
	oldKey = beeperidentity.ProviderKey("telegram", "account", "acct-7", "482111")
	newKey = beeperidentity.ProviderKey("telegram", "service", "", "482111")

	source, err := st.GetOrCreateSource("beeper", "acct-7")
	require.NoError(t, err)
	left, err = st.EnsureParticipantByIdentifier("beeper", "beeper:6:acct-7:9:bridge-al", "Bridge Alice")
	require.NoError(t, err)
	right, err := st.EnsureParticipantByIdentifier("telegram", "482111", "Export Alice")
	require.NoError(t, err)

	providerKey := oldKey
	slug := "telegram"
	observedAt := time.Now().UTC()
	_, err = st.RecordContactObservationContext(context.Background(), left, ParticipantContactObservationInput{
		SourceID:       &source.ID,
		AddressKind:    ContactAddressProviderIdentity,
		ServiceSlug:    &slug,
		ProviderUserID: &providerKey,
		OriginalValue:  oldKey,
		ObservedAt:     &observedAt,
		Envelope:       ValueEnvelopeInput{Source: ProvenanceArchiveObservation, SourceRef: &slug},
	})
	require.NoError(t, err)

	value := oldKey
	candidate, _, err := st.UpsertIdentityMatchCandidateContext(context.Background(), IdentityMatchCandidateInput{
		LeftKind:        IdentityMatchParticipant,
		LeftID:          left,
		RightKind:       IdentityMatchParticipant,
		RightID:         right,
		Basis:           IdentityMatchStableProviderID,
		ServiceSlug:     &slug,
		NormalizedValue: &value,
		State:           IdentityMatchStateCandidate,
		Source:          ProvenanceArchiveObservation,
		SourceID:        &source.ID,
	})
	require.NoError(t, err)
	_, _, err = st.AcceptIdentityMatchCandidateContext(context.Background(), candidate.ID, "system", nil)
	require.NoError(t, err, "system accepts a stable-provider candidate")
	_, err = st.AddIdentityMatchEvidenceContext(context.Background(), candidate.ID, IdentityMatchEvidenceInput{
		EvidenceKind: "stable_provider_id",
		Detail:       &value,
		Source:       ProvenanceArchiveObservation,
		SourceID:     &source.ID,
	})
	require.NoError(t, err)
	preFingerprint, err = IdentityMatchDecisionFingerprintForTest(context.Background(), st, candidate.ID)
	require.NoError(t, err, "fingerprint the pre-migration evidence")
	_, err = st.DB().Exec(
		`INSERT INTO identity_match_review_decisions (candidate_id, evidence_fingerprint) VALUES (?, ?)`,
		candidate.ID, preFingerprint,
	)
	require.NoError(t, err)
	return left, candidate.ID, oldKey, newKey, preFingerprint
}

func TestInitSchemaRewritesTelegramProviderKeys(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, err := Open(filepath.Join(t.TempDir(), "telegram-keys.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema())

	left, candidateID, oldKey, newKey, preFingerprint := seedTelegramKeyRows(t, st)

	// A non-numeric Telegram native ID and another network's account-scoped
	// key must survive untouched.
	staleTelegram := beeperidentity.ProviderKey("telegram", "account", "acct-7", "not-a-num")
	staleDiscord := beeperidentity.ProviderKey("discord", "account", "acct-7", "482111")
	for _, key := range []string{staleTelegram, staleDiscord} {
		_, err = st.DB().Exec(`
			INSERT INTO participant_contact_observations (
				participant_id, address_kind, original_value, normalized_value,
				provider_user_id, normalization, source
			) VALUES (?, 'provider_identity', ?, ?, ?, 'none', 'archive_observation')
		`, left, key, key, key)
		require.NoError(err)
	}
	// The stale-shape observations join the evidence before the decision is
	// recorded, so the proof is valid against the complete pre-migration
	// state and only the key rewrite can stale it.
	preFingerprint, err = IdentityMatchDecisionFingerprintForTest(context.Background(), st, candidateID)
	require.NoError(err)
	_, err = st.DB().Exec(
		`UPDATE identity_match_review_decisions SET evidence_fingerprint = ? WHERE candidate_id = ?`,
		preFingerprint, candidateID,
	)
	require.NoError(err)

	_, err = st.DB().Exec(`DELETE FROM applied_migrations WHERE name = ?`, migrationTelegramSharedProviderKeys)
	require.NoError(err)
	require.NoError(st.InitSchema(), "upgrade telegram provider keys")

	// The bridged observation is rewritten in both key columns and lands on
	// the scope-less shared edge.
	var provider, normalized string
	var obsScope int
	require.NoError(st.DB().QueryRow(`
		SELECT provider_user_id, normalized_value,
		       (scope_kind IS NOT NULL) + (scope_value IS NOT NULL)
		FROM participant_contact_observations
		WHERE participant_id = ? AND address_kind = 'provider_identity' AND source_id IS NOT NULL
	`, left).Scan(&provider, &normalized, &obsScope))
	assert.Equal(newKey, provider, "provider_user_id rewritten to the shared key")
	assert.Equal(newKey, normalized, "provider_identity normalized_value rewritten")
	assert.Zero(obsScope, "shared key carries no account scope")

	// Stale shapes untouched.
	var stale int
	require.NoError(st.DB().QueryRow(`
		SELECT COUNT(*) FROM participant_contact_observations WHERE provider_user_id IN (?, ?)
	`, staleTelegram, staleDiscord).Scan(&stale))
	assert.Equal(2, stale, "non-numeric and non-telegram keys keep account scoping")

	// Candidate and evidence rewritten; the accepted decision survives.
	var state string
	require.NoError(st.DB().QueryRow(
		`SELECT state FROM identity_match_candidates WHERE id = ?`, candidateID,
	).Scan(&state))
	assert.Equal("accepted", state, "accepted decision preserved")
	var candidateValue, evidenceDetail string
	var candidateScope int
	require.NoError(st.DB().QueryRow(`
		SELECT normalized_value, (scope_kind IS NOT NULL) + (scope_value IS NOT NULL)
		FROM identity_match_candidates WHERE id = ?`, candidateID,
	).Scan(&candidateValue, &candidateScope))
	assert.Equal(newKey, candidateValue, "candidate normalized_value rewritten")
	assert.Zero(candidateScope, "candidate converges on the scope-less edge")
	require.NoError(st.DB().QueryRow(
		`SELECT detail FROM identity_match_evidence WHERE candidate_id = ? LIMIT 1`, candidateID,
	).Scan(&evidenceDetail))
	assert.Equal(newKey, evidenceDetail, "evidence detail rewritten")

	// The stored fingerprint was refreshed off the rewritten evidence, so
	// the recorded decision no longer compares against a stale snapshot.
	var fingerprint string
	require.NoError(st.DB().QueryRow(
		`SELECT evidence_fingerprint FROM identity_match_review_decisions WHERE candidate_id = ?`,
		candidateID,
	).Scan(&fingerprint))
	postFingerprint, err := IdentityMatchDecisionFingerprintForTest(context.Background(), st, candidateID)
	require.NoError(err)
	assert.Equal(postFingerprint, fingerprint, "fingerprint matches the rewritten evidence")
	assert.NotEqual(preFingerprint, fingerprint, "the pre-rewrite fingerprint no longer matches")

	// No old keys remain anywhere the matcher looks.
	var remaining int
	require.NoError(st.DB().QueryRow(`
		SELECT (SELECT COUNT(*) FROM participant_contact_observations WHERE provider_user_id = ?)
		     + (SELECT COUNT(*) FROM identity_match_candidates WHERE normalized_value = ?)
		     + (SELECT COUNT(*) FROM identity_match_evidence WHERE detail = ?)
	`, oldKey, oldKey, oldKey).Scan(&remaining))
	assert.Zero(remaining, "no account-scoped telegram key remains")

	// The ledger gate keeps the migration from running again.
	_, err = st.DB().Exec(`DELETE FROM participant_contact_observations WHERE provider_user_id = ?`, newKey)
	require.NoError(err)
	require.NoError(st.InitSchema())
	var after int
	require.NoError(st.DB().QueryRow(
		`SELECT COUNT(*) FROM participant_contact_observations WHERE provider_user_id = ?`,
		newKey,
	).Scan(&after))
	assert.Zero(after, "ledger gate prevents a re-run")
}

// TestInitSchemaDoesNotRenewStaleTelegramDecisionProofs pins the refresh
// gate: the migration renews only fingerprints that matched the
// pre-migration evidence. A proof that was already stale before the
// migration — recorded against older evidence — stays stale instead of
// becoming valid for rewritten evidence the user never reviewed.
func TestInitSchemaDoesNotRenewStaleTelegramDecisionProofs(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, err := Open(filepath.Join(t.TempDir(), "telegram-keys-stale.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema())

	_, candidateID, _, _, _ := seedTelegramKeyRows(t, st)
	// The recorded proof went stale before the migration: the evidence
	// changed after the decision, without any migration involvement.
	const staleDecision = "recorded-against-older-evidence"
	const staleReceipt = "receipt-against-older-evidence"
	_, err = st.DB().Exec(
		`UPDATE identity_match_review_decisions SET evidence_fingerprint = ? WHERE candidate_id = ?`,
		staleDecision, candidateID)
	require.NoError(err)
	_, err = st.DB().Exec(`INSERT INTO identity_match_review_receipts
		(candidate_id, decision, review_token, evidence_fingerprint)
		VALUES (?, 'accepted', 'stale-token', ?)`, candidateID, staleReceipt)
	require.NoError(err)

	_, err = st.DB().Exec(`DELETE FROM applied_migrations WHERE name = ?`, migrationTelegramSharedProviderKeys)
	require.NoError(err)
	require.NoError(st.InitSchema(), "upgrade telegram provider keys")

	var decision, receipt string
	require.NoError(st.DB().QueryRow(`
		SELECT (SELECT evidence_fingerprint FROM identity_match_review_decisions WHERE candidate_id = ?),
		       (SELECT evidence_fingerprint FROM identity_match_review_receipts WHERE candidate_id = ?)`,
		candidateID, candidateID).Scan(&decision, &receipt))
	assert.Equal(staleDecision, decision, "a stale decision proof is not renewed")
	assert.Equal(staleReceipt, receipt, "a stale receipt proof is not renewed")
	postFingerprint, err := IdentityMatchDecisionFingerprintForTest(context.Background(), st, candidateID)
	require.NoError(err)
	assert.NotEqual(postFingerprint, decision, "the stale proof still does not match the rewritten evidence")
	assert.NotEqual(postFingerprint, receipt)
}
