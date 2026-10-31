package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestXRestoreMissingSourceCannotDiscardFinancialAuthority(t *testing.T) {
	for _, active := range []bool{false, true} {
		name := "removal-only extra source"
		if active {
			name = "extra source with reservation"
		}
		t.Run(name, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			add := func(owner string) *store.Source {
				t.Helper()
				source, err := st.GetOrCreateSource("x", owner)
				require.NoError(t, err)
				generation := int64(1)
				_, err = st.UpdateXControlContext(t.Context(), source.ID, 0, store.XControlPatch{
					OwnerID: owner, Generation: &generation, InitializeInstance: true,
				})
				require.NoError(t, err)
				return source
			}
			add("1001")
			extra := add("1003")
			if active {
				_, err := st.ReserveXRequestContext(t.Context(), extra.ID, store.XRequestCost{
					Kind: store.XRequestTimeline, OwnerID: "1003", Generation: 1,
					GrantedScopes: []string{"tweet.read", "users.read"}, MonthlyBudgetMicros: 1_000_000,
					RequestedResults: 5, PostPriceMicros: 5_000, UserPriceMicros: 10_000,
					At: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC),
				})
				require.NoError(t, err)
			}
			authority, head, err := st.ExportXRemovalRecordContext(t.Context())
			require.NoError(t, err)
			require.Len(t, authority.Sources, 2)
			// The validator operates on restored scratch, not on the authority.
			// Removing this row constructs the earlier snapshot's exact source
			// inventory while retaining the independently captured current proof.
			require.NoError(t, st.RemoveSource(extra.ID))
			err = st.ValidateXRemovalRecordContext(t.Context(), authority, head)
			if active {
				assert.Error(t, err, "a snapshot cannot drop a source's durable request authority")
			} else {
				assert.NoError(t, err, "ID-only removals from post-snapshot sources remain replayable")
			}
		})
	}
}
