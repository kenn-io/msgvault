package store

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPersonVCardSnapshotTransactionDoesNotMixConcurrentSemanticWrites(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dbPath := filepath.Join(t.TempDir(), "snapshot.db")
	reader, err := OpenForTest(dbPath)
	require.NoError(err)
	t.Cleanup(func() { _ = reader.Close() })
	require.NoError(reader.InitSchema())
	participantID, err := reader.EnsureParticipant(
		"alice@example.com", "Alice", "example.com",
	)
	require.NoError(err)
	person, _, err := reader.CreatePersonFromParticipant(participantID)
	require.NoError(err)
	organization, err := reader.CreateOrganizationContext(
		t.Context(), OrganizationInput{Name: "Example Org"},
	)
	require.NoError(err)

	writer, err := OpenForTest(dbPath)
	require.NoError(err)
	t.Cleanup(func() { _ = writer.Close() })
	var insideSnapshot []Employment
	err = reader.withReadSnapshotContext(t.Context(), func(tx *loggedTx) error {
		_, readErr := reader.getPersonProfileTx(
			t.Context(), tx, person.ID, true,
		)
		if readErr != nil {
			return readErr
		}
		_, writeErr := writer.AddEmploymentContext(t.Context(), EmploymentInput{
			PersonID: person.ID, OrganizationID: organization.ID,
			Title: new("Engineer"), Source: ProvenanceUser,
		})
		if writeErr != nil {
			return writeErr
		}
		insideSnapshot, readErr = reader.listAllEmploymentsContext(
			t.Context(), tx, EmploymentFilter{PersonID: person.ID},
		)
		return readErr
	})
	require.NoError(err)
	assert.Empty(insideSnapshot,
		"later component reads must retain the transaction's first snapshot")
	afterCommit, err := reader.ListEmploymentsContext(
		t.Context(), EmploymentFilter{PersonID: person.ID},
	)
	require.NoError(err)
	assert.Len(afterCommit, 1)
}
