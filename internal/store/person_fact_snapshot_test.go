package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personfacts"
)

func TestPersonFactLedgerReadsOneRepeatableSnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	reader, personID := newPersonFactLedgerStore(t)
	evidenceKey := seedPersonFactEvidence(t, reader, personID, "snapshot")

	writer, err := OpenForTest(reader.dbPath)
	require.NoError(err)
	t.Cleanup(func() { _ = writer.Close() })

	var inside []personfacts.Evidence
	err = reader.withReadSnapshotContext(t.Context(), func(tx *loggedTx) error {
		inside, err = reader.queryPersonFactEvidenceTx(t.Context(), tx, personID,
			personfacts.EvidenceFilter{Limit: 50})
		if err != nil {
			return err
		}
		status := preparePersonFactLedgerGeneration(t, personID, "snapshot-status", nil,
			[]personfacts.EvidenceStatusChange{{
				EvidenceKey: evidenceKey, SourceVersion: "source-v1", Supported: false,
				Reason: personfacts.EvidenceStatusSourceEdited,
			}})
		persistPersonFactLedgerGeneration(t, writer, status, nil)
		return reader.hydratePersonFactEvidenceStatusTx(t.Context(), tx, inside)
	})
	require.NoError(err)
	require.Len(inside, 1)
	assert.True(inside[0].Supported,
		"the later status event must not leak into an already-established snapshot")
	assert.Nil(inside[0].LatestStatus)

	after, err := reader.ListPersonFactEvidenceContext(t.Context(), personID,
		personfacts.EvidenceFilter{})
	require.NoError(err)
	require.Len(after, 1)
	assert.False(after[0].Supported)
	require.NotNil(after[0].LatestStatus)
	assert.Equal(personfacts.EvidenceStatusSourceEdited, after[0].LatestStatus.Reason)
}
