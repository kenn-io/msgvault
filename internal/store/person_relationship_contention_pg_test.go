package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// TestPersonAndRelationshipWritesRetryDeadlock forces the lock cycle between
// the writes that bump both ends of a relationship edge. Person deletion locks
// the deleted person and then, through the counterpart bump, everyone they
// share an edge with; relationship writes bump both endpoints in whatever
// order the UPDATE visits them. The blocker plays the other side: it holds the
// higher-numbered person and asks for the lower one once the write under test
// is parked on the higher one. PostgreSQL's detector aborts one side; the
// write has to absorb that and finish once the blocker lets go.
func TestPersonAndRelationshipWritesRetryDeadlock(t *testing.T) {
	st := testutil.NewTestStore(t)
	if !st.IsPostgreSQL() {
		t.Skip("PostgreSQL row locks are required for the relationship deadlock regression")
	}
	type fixture struct {
		low, high *store.Person
		edge      *store.PersonRelationship
	}
	cases := []struct {
		name  string
		write func(ctx context.Context, f fixture) error
	}{{
		name: "person delete",
		write: func(ctx context.Context, f fixture) error {
			return st.DeletePersonContext(ctx, f.low.ID, f.low.Revision)
		},
	}, {
		name: "add relationship",
		write: func(ctx context.Context, f fixture) error {
			_, err := st.AddPersonRelationshipContext(ctx, store.PersonRelationshipInput{
				SourcePersonID: f.low.ID, TargetPersonID: f.high.ID,
				TypeSlug: "colleague", Source: store.ProvenanceUser, Actor: "test",
			})
			return err
		},
	}, {
		name: "patch relationship",
		write: func(ctx context.Context, f fixture) error {
			_, err := st.UpdatePersonRelationshipNotesContext(
				ctx, f.edge.ID, f.edge.Revision, new("retried"), "test")
			return err
		},
	}, {
		name: "delete relationship",
		write: func(ctx context.Context, f fixture) error {
			return st.DeletePersonRelationshipContext(ctx, f.edge.ID, f.edge.Revision)
		},
	}, {
		// A rename bumps every relationship counterpart after locking the
		// renamed person, so it meets edge writes on the same two rows.
		name: "display name",
		write: func(ctx context.Context, f fixture) error {
			_, err := st.UpdatePersonDisplayNameContext(ctx, f.low.ID, f.low.Revision, new("renamed"))
			return err
		},
	}}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			t.Cleanup(cancel)
			low := createEnvelopePerson(t, st, fmt.Sprintf("low%d@example.com", i))
			high := createEnvelopePerson(t, st, fmt.Sprintf("high%d@example.com", i))
			require.Less(low.ID, high.ID)
			edge, err := st.AddPersonRelationshipContext(ctx, store.PersonRelationshipInput{
				SourcePersonID: low.ID, TargetPersonID: high.ID,
				TypeSlug: "friend", Source: store.ProvenanceUser, Actor: "test",
			})
			require.NoError(err)
			low, err = st.GetPersonContext(ctx, low.ID)
			require.NoError(err)

			writeErr := forcePostgreSQLDeadlock(ctx, t, st,
				postgreSQLRowLock{table: "persons", id: high.ID},
				postgreSQLRowLock{table: "persons", id: low.ID},
				func(ctx context.Context) error {
					return tc.write(ctx, fixture{low: low, high: high, edge: edge})
				})
			require.NoError(writeErr, "%s must retry a transient PostgreSQL deadlock", tc.name)
		})
	}
}
