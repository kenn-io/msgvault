package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInitSchemaBackfillsLegacyIdentityMatchSourceSupport(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, err := Open(filepath.Join(t.TempDir(), "legacy-identity-support.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema())

	firstSource, err := st.GetOrCreateSource("beeper", "account-a")
	require.NoError(err)
	_, err = st.GetOrCreateSource("beeper", "account-b")
	require.NoError(err)
	left, err := st.EnsureParticipantByIdentifier(
		"beeper", "@legacy-left:beeper.local", "Test User",
	)
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier(
		"beeper", "@legacy-right:beeper.local", "Test User",
	)
	require.NoError(err)
	candidate, created, err := st.UpsertIdentityMatchCandidateContext(
		context.Background(), IdentityMatchCandidateInput{
			LeftKind: IdentityMatchParticipant, LeftID: left,
			RightKind: IdentityMatchParticipant, RightID: right,
			Basis:           IdentityMatchStableProviderID,
			NormalizedValue: new("legacy-provider-id"),
			State:           IdentityMatchStateCandidate,
			Source:          ProvenanceArchiveObservation,
			SourceID:        &firstSource.ID,
		},
	)
	require.NoError(err)
	require.True(created)
	evidence, err := st.AddIdentityMatchEvidenceContext(
		context.Background(), candidate.ID, IdentityMatchEvidenceInput{
			EvidenceKind: "stable_provider_id",
			Detail:       new("legacy-provider-id"),
			Source:       ProvenanceArchiveObservation,
			SourceID:     &firstSource.ID,
		},
	)
	require.NoError(err)

	// Recreate the pre-migration shape: generated rows exist, the new support
	// tables do not, and the one-shot sentinel has not been written.
	_, err = st.DB().Exec(`DROP TABLE identity_match_evidence_sources`)
	require.NoError(err)
	_, err = st.DB().Exec(`DROP TABLE identity_match_candidate_sources`)
	require.NoError(err)
	_, err = st.DB().Exec(
		`DELETE FROM applied_migrations WHERE name = ?`, migrationIdentityMatchSourceSupport,
	)
	require.NoError(err)
	require.NoError(st.InitSchema(), "upgrade legacy identity support")

	var candidateSupport, evidenceSupport, conservativeCandidate, conservativeEvidence int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*)
		FROM identity_match_candidate_sources WHERE candidate_id = ?`, candidate.ID).
		Scan(&candidateSupport))
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*)
		FROM identity_match_evidence_sources WHERE evidence_id = ?`, evidence.ID).
		Scan(&evidenceSupport))
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*)
		FROM identity_match_candidate_sources
		WHERE candidate_id = ? AND is_conservative = TRUE`, candidate.ID).
		Scan(&conservativeCandidate))
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*)
		FROM identity_match_evidence_sources
		WHERE evidence_id = ? AND is_conservative = TRUE`, evidence.ID).
		Scan(&conservativeEvidence))
	assert.Equal(2, candidateSupport,
		"a legacy candidate must survive removal of any one pre-upgrade source")
	assert.Equal(2, evidenceSupport,
		"legacy evidence must receive the same conservative support marker")
	assert.Equal(2, conservativeCandidate,
		"legacy candidate support must be marked as conservative")
	assert.Equal(2, conservativeEvidence,
		"legacy evidence support must be marked as conservative")

	// A later exact observation upgrades the conservative placeholder instead
	// of leaving the source permanently hidden from safe subset exports.
	require.NoError(st.AttachIdentityMatchCandidateSourceContext(
		context.Background(), candidate.ID, firstSource.ID,
	))
	require.NoError(st.AttachIdentityMatchEvidenceSourceContext(
		context.Background(), evidence.ID, firstSource.ID,
	))
	assert.Equal(1, countConservativeSupport(t, st,
		`identity_match_candidate_sources`, `candidate_id`, candidate.ID),
		"one unrelated legacy candidate source must remain conservative")
	assert.Equal(1, countConservativeSupport(t, st,
		`identity_match_evidence_sources`, `evidence_id`, evidence.ID),
		"one unrelated legacy evidence source must remain conservative")

	applied, err := st.IsMigrationApplied(migrationIdentityMatchSourceSupport)
	require.NoError(err)
	assert.True(applied)

	_, err = st.GetOrCreateSource("beeper", "account-c")
	require.NoError(err)
	require.NoError(st.InitSchema(), "repeat schema initialization")
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*)
		FROM identity_match_candidate_sources WHERE candidate_id = ?`, candidate.ID).
		Scan(&candidateSupport))
	assert.Equal(2, candidateSupport,
		"the ledger must prevent later sources from widening legacy support")
}

func countConservativeSupport(
	t *testing.T, st *Store, table, ownerColumn string, ownerID int64,
) int {
	t.Helper()
	var count int
	require.NoError(t, st.DB().QueryRow(
		`SELECT COUNT(*) FROM `+table+` WHERE `+ownerColumn+` = ? AND is_conservative = TRUE`,
		ownerID,
	).Scan(&count))
	return count
}
