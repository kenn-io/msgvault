package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestIdentityMatchJudgmentCursorUpgradePreservesProgress(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := storetest.New(t).Store
	var ids []int64
	for _, suffix := range []string{"upgrade-first", "upgrade-middle", "upgrade-last"} {
		ids = append(ids, createScoringCandidate(t, st, suffix))
		lease, err := st.ClaimNextIdentityMatchJudgmentContext(t.Context(), "worker", time.Minute)
		require.NoError(err)
		require.NotNil(lease)
		_, err = st.RecordIdentityMatchJudgmentContext(t.Context(), *lease, scoredJudgmentInput())
		require.NoError(err)
	}
	_, err := st.AddIdentityMatchEvidenceContext(t.Context(), ids[0], store.IdentityMatchEvidenceInput{
		EvidenceKind: "email", Source: store.ProvenanceArchiveObservation,
	})
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), `DROP TABLE person_match_judgment_cursor`)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), `CREATE TABLE person_match_judgment_cursor (
		singleton INTEGER PRIMARY KEY CHECK (singleton = 1), candidate_id BIGINT NOT NULL DEFAULT 0)`)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_match_judgment_cursor VALUES (1, ?)`), ids[1])
	require.NoError(err)
	require.NoError(st.InitSchemaContext(t.Context()))
	var cursor int64
	var started bool
	err = st.DB().QueryRowContext(t.Context(), `SELECT candidate_id, started_at_zero FROM person_match_judgment_cursor WHERE singleton = 1`).Scan(&cursor, &started)
	require.NoError(err)
	assert.Equal(ids[1], cursor)
	assert.False(started)
	lease, err := st.ClaimNextIdentityMatchJudgmentContext(t.Context(), "worker", time.Minute)
	require.NoError(err)
	require.NotNil(lease, "an upgraded cursor with unknown coverage must wrap before completion")
	assert.Equal(ids[0], lease.CandidateID)
	_, err = st.RecordIdentityMatchJudgmentContext(t.Context(), *lease, scoredJudgmentInput())
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_match_judgment_cursor SET candidate_id = ?, started_at_zero = TRUE`), ids[1])
	require.NoError(err)
	require.NoError(st.InitSchemaContext(t.Context()))
	err = st.DB().QueryRowContext(t.Context(), `SELECT candidate_id, started_at_zero FROM person_match_judgment_cursor WHERE singleton = 1`).Scan(&cursor, &started)
	require.NoError(err)
	assert.Equal(ids[1], cursor)
	assert.True(started)
}
