package store_test

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestSourceLifetimeHighestRemovalBeforeCounterInitialization(t *testing.T) {
	for _, serialized := range []bool{false, true} {
		for _, only := range []bool{false, true} {
			t.Run(strconv.FormatBool(serialized)+"/only="+strconv.FormatBool(only), func(t *testing.T) {
				requirements := require.New(t)

				st := testutil.NewTestStore(t)
				if !only {
					_, err := st.GetOrCreateSource("gmail", "retained@example.test")
					requirements.NoError(err)
				}
				before, err := st.GetOrCreateSource("gmail", "recreated@example.test")
				requirements.NoError(err)
				// A legacy archive has live source rows but no retained counter.
				_, err = st.DB().Exec("DELETE FROM archive_metadata WHERE key='source_id_high_water'")
				requirements.NoError(err)
				if serialized {
					_, _, err = st.RemoveSourceSerialized(t.Context(), before.ID)
				} else {
					err = st.RemoveSource(before.ID)
				}
				requirements.NoError(err)
				after, err := st.GetOrCreateSource("gmail", before.Identifier)
				requirements.NoError(err)
				assert.Greater(t, after.ID, before.ID, "a newly recreated lifetime cannot inherit an issued source ID")
			})
		}
	}
}

func TestSourceLifetimeCounterCorruptionAndExhaustionFailClosed(t *testing.T) {
	for _, value := range []string{"", "-1", "corrupt", "1.5", "9223372036854775808", "9223372036854775807"} {
		t.Run(value, func(t *testing.T) {
			assertions := assert.New(t)

			requirements := require.New(t)

			st := testutil.NewTestStore(t)
			if st.IsPostgreSQL() {
				t.Skip("PostgreSQL uses its native identity sequence; removal/convergence tests cover it")
			}
			before, err := st.GetOrCreateSource("gmail", "existing@example.test")
			requirements.NoError(err)
			_, err = st.DB().Exec(st.Rebind(`INSERT INTO archive_metadata(key,value) VALUES('source_id_high_water',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`), value)
			requirements.NoError(err)
			if value == "9223372036854775807" {
				got, err := st.GetOrCreateSource("gmail", before.Identifier)
				requirements.NoError(err, "an existing tuple does not need an available ID")
				assertions.Equal(before.ID, got.ID)
			}
			_, err = st.GetOrCreateSource("gmail", "new@example.test")
			requirements.Error(err, "never ignore a corrupt or exhausted retained ID supply")
			_, err = st.GetSourceByTypeAndIdentifier("gmail", "new@example.test")
			requirements.ErrorIs(err, store.ErrSourceNotFound)
			var retained string
			requirements.NoError(st.DB().QueryRow("SELECT value FROM archive_metadata WHERE key='source_id_high_water'").Scan(&retained))
			assertions.Equal(value, retained, "refused allocation must not repair or advance metadata")
		})
	}
}
