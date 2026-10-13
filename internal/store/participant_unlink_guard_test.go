package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newUnlinkGuardStore(t *testing.T) (*Store, int64, int64) {
	t.Helper()
	var st *Store
	if dbURL := os.Getenv("MSGVAULT_TEST_DB"); IsPostgresURL(dbURL) {
		st = newPGStoreInternal(t, dbURL)
		require.True(t, st.IsPostgreSQL())
	} else {
		var err error
		st, err = OpenForTest(filepath.Join(t.TempDir(), "unlink.db"))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, st.Close()) })
		require.NoError(t, st.InitSchema())
	}
	a, err := st.EnsureParticipant("identity-a@example.test", "Synthetic A", "example.test")
	require.NoError(t, err)
	b, err := st.EnsureParticipant("identity-b@example.test", "Synthetic B", "example.test")
	require.NoError(t, err)
	return st, a, b
}

func TestUnlinkParticipantsContextCancellationPreservesEdge(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, a, b := newUnlinkGuardStore(t)
	before, err := st.LinkParticipants(a, b)
	requirements.NoError(err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = st.UnlinkParticipantsContext(ctx, b, a)
	requirements.ErrorIs(err, context.Canceled)
	revision, err := st.IdentityRevision()
	requirements.NoError(err)
	assertions.Equal(before, revision)
	edges, err := st.ClusterEdges(a)
	requirements.NoError(err)
	assertions.Len(edges, 1)
}

func TestUnlinkParticipantsContextKeepsDurablePersonBindings(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, a, b := newUnlinkGuardStore(t)
	_, err := st.LinkParticipants(a, b)
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(a)
	requirements.NoError(err)
	before, err := st.IdentityRevision()
	requirements.NoError(err)
	after, err := st.UnlinkParticipantsContext(t.Context(), b, a)
	requirements.NoError(err)
	assertions.Equal(before+1, after)
	edges, err := st.ClusterEdges(a)
	requirements.NoError(err)
	assertions.Empty(edges)
	members, err := st.ClusterMembers(a)
	requirements.NoError(err)
	assertions.Equal([]int64{a}, members)
	current, err := st.GetPerson(person.ID)
	requirements.NoError(err)
	assertions.ElementsMatch([]int64{a, b}, current.ParticipantIDs)
	again, err := st.UnlinkParticipants(a, b)
	requirements.NoError(err)
	assertions.Equal(after, again)
}

func TestGuardedParticipantUnlinkDenialRollsBack(t *testing.T) {
	for _, linked := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent edge", true: "existing edge"}[linked], func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			st, a, b := newUnlinkGuardStore(t)
			if linked {
				_, err := st.LinkParticipants(a, b)
				requirements.NoError(err)
			}
			before, err := st.IdentityRevision()
			requirements.NoError(err)
			var metadataBefore int
			requirements.NoError(st.db.QueryRow(`SELECT COUNT(*) FROM archive_metadata WHERE key=?`, identityRevisionKey).Scan(&metadataBefore))
			denied := errors.New("synthetic scope denied")
			calls := 0
			_, err = st.unlinkParticipantsContextGuarded(t.Context(), a, b, func(ctx context.Context, tx *loggedTx) error {
				calls++
				revision, err := st.currentIdentityRevisionTxContext(ctx, tx)
				require.NoError(t, err)
				assert.Equal(t, before, revision)
				return denied
			})
			requirements.ErrorIs(err, denied)
			assertions.Equal(1, calls)
			revision, err := st.IdentityRevision()
			requirements.NoError(err)
			assertions.Equal(before, revision)
			var metadataAfter int
			requirements.NoError(st.db.QueryRow(`SELECT COUNT(*) FROM archive_metadata WHERE key=?`, identityRevisionKey).Scan(&metadataAfter))
			assertions.Equal(metadataBefore, metadataAfter)
			edges, err := st.ClusterEdges(a)
			requirements.NoError(err)
			if linked {
				assertions.Len(edges, 1)
			} else {
				assertions.Empty(edges)
			}
		})
	}
}

func TestGuardedParticipantUnlinkCancellationAfterFence(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, a, b := newUnlinkGuardStore(t)
	before, err := st.LinkParticipants(a, b)
	requirements.NoError(err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, err = st.unlinkParticipantsContextGuarded(ctx, a, b, func(context.Context, *loggedTx) error { cancel(); return nil })
	requirements.ErrorIs(err, context.Canceled)
	revision, err := st.IdentityRevision()
	requirements.NoError(err)
	assertions.Equal(before, revision)
	edges, err := st.ClusterEdges(a)
	requirements.NoError(err)
	assertions.Len(edges, 1)
}
