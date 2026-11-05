package store_test

import (
	"context"
	"fmt"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"hegel.dev/go/hegel"
)

func TestSourceRemovalClearsOnlyItsRepairLedger(t *testing.T) {
	for _, serialized := range []bool{false, true} {
		name := "maintenance"
		if serialized {
			name = "serialized"
		}
		t.Run(name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewTestStore(t)
			src, err := st.GetOrCreateSource("outlook", "archive@example.net")
			require.NoError(err)
			removed := []string{
				"rederive:outlook:archive@example.net:email-v1",
				"rederive:outlook:archive@example.net:email-v2#c0000000000000001-0000000000000002",
				"rederive:outlook:archive@example.net:email-v2#f0000000000000001",
				"rederive:outlook:archive@example.net:email-v2#r0000000000000021-0000000000000042",
				"rederive:outlook:archive@example.net:email-v2#p",
			}
			retained := []string{
				"rederive:outlook:archive@example.net:other:email-v2#p",
				"rederive:outlook:archive@example.net/extra:email-v2",
				"rederive:gmail:archive@example.net:email-v2",
				"unrelated-migration",
			}
			for _, key := range append(removed, retained...) {
				require.NoError(st.MarkMigrationApplied(key))
			}
			require.NoError(removeRepairFixtureSource(t.Context(), st, src.ID, serialized))
			for _, key := range removed {
				applied, err := st.IsMigrationApplied(key)
				require.NoError(err)
				assert.False(applied, key)
			}
			for _, key := range retained {
				applied, err := st.IsMigrationApplied(key)
				require.NoError(err)
				assert.True(applied, key)
			}
		})
	}
}

func removeRepairFixtureSource(ctx context.Context, st *store.Store, id int64, serialized bool) error {
	if serialized {
		_, _, err := st.RemoveSourceSerialized(ctx, id)
		return err
	}
	return st.RemoveSource(id)
}

func TestSourceRemovalRollsBackRepairCleanup(t *testing.T) {
	for _, serialized := range []bool{false, true} {
		name := "maintenance"
		if serialized {
			name = "serialized"
		}
		t.Run(name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewTestStore(t)
			src, err := st.GetOrCreateSource("outlook", "archive@example.net")
			require.NoError(err)
			key := "rederive:outlook:archive@example.net:email-v2#p"
			require.NoError(st.MarkMigrationApplied(key))
			fault := `CREATE TRIGGER fail_source_removal BEFORE DELETE ON sources BEGIN SELECT RAISE(ABORT, 'removal fixture failure'); END`
			if st.IsPostgreSQL() {
				_, err := st.DB().Exec(`CREATE FUNCTION fail_source_removal() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'removal fixture failure'; RETURN OLD; END $$`)
				require.NoError(err)
				fault = `CREATE TRIGGER fail_source_removal BEFORE DELETE ON sources FOR EACH ROW EXECUTE FUNCTION fail_source_removal()`
			}
			_, err = st.DB().Exec(fault)
			require.NoError(err)
			require.ErrorContains(removeRepairFixtureSource(t.Context(), st, src.ID, serialized), "removal fixture failure")
			_, err = st.GetSourceByID(src.ID)
			require.NoError(err)
			applied, err := st.IsMigrationApplied(key)
			require.NoError(err)
			assert.True(applied, "repair cleanup must roll back with the failed source deletion")
		})
	}
}

func TestSourceRemovalRepairLedgerIsolation(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		require := require.New(ht)
		n := hegel.Draw(ht, hegel.Integers(uint64(0), uint64(math.MaxUint64)))
		serialized := hegel.Draw(ht, hegel.Booleans())
		st := testutil.NewTestStore(ht.T)
		defer func() { require.NoError(st.Close()) }()
		identifier := fmt.Sprintf("archive_%%%x#@example.net", n)
		src, err := st.GetOrCreateSource("outlook", identifier)
		require.NoError(err)
		key := "rederive:outlook:" + identifier + ":email-v2#p"
		other := "rederive:outlook:" + identifier + ":mirror:email-v2#p"
		require.NoError(st.MarkMigrationApplied(key))
		require.NoError(st.MarkMigrationApplied(other))
		require.NoError(removeRepairFixtureSource(t.Context(), st, src.ID, serialized))
		applied, err := st.IsMigrationApplied(key)
		require.NoError(err)
		require.False(applied)
		applied, err = st.IsMigrationApplied(other)
		require.NoError(err)
		require.True(applied, "an overlapping identifier must keep its repair state")
	}, hegel.WithTestCases(32), hegel.WithDatabase(""))
}
