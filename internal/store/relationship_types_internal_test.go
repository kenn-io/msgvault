package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCheckRelationshipTypeDeleteCASResultRejectsZeroAffectedRows(t *testing.T) {
	require := require.New(t)
	st, err := OpenForTest(filepath.Join(t.TempDir(), "relationship-types.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema())

	created, err := st.CreateRelationshipTypeContext(context.Background(), RelationshipTypeInput{
		Slug: "mentor", ForwardLabel: "mentor", ReverseLabel: "mentee",
	})
	require.NoError(err)

	// This is the real driver result of DELETE ... WHERE id = ? AND revision =
	// ? after a concurrent revision change: the statement succeeds but matches
	// no rows. It needs no Task 4 relationship-edge schema.
	_, err = st.DB().ExecContext(context.Background(),
		`UPDATE relationship_types SET revision = revision + 1 WHERE id = ?`, created.ID)
	require.NoError(err)

	result, err := st.DB().ExecContext(context.Background(),
		`DELETE FROM relationship_types WHERE id = ? AND revision = ?`, created.ID, created.Revision)
	require.NoError(err)

	require.ErrorIs(checkRelationshipTypeDeleteCASResult(result, created.ID), ErrRelationshipTypeRevisionConflict)
}
