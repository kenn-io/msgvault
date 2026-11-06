package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestAccountIdentityAddressSourceScope(t *testing.T) {
	f := storetest.New(t)
	hidden, err := f.Store.GetOrCreateSource("gmail", "hidden@example.com")
	require.NoError(t, err)
	require.NoError(t, f.Store.AddAccountIdentity(hidden.ID, "owner@example.com", "manual"))
	for _, tc := range []struct {
		name string
		ids  []int64
		want bool
	}{
		{"unrestricted", nil, true},
		{"visible", []int64{hidden.ID}, true},
		{"hidden", []int64{f.Source.ID}, false},
		{"empty", []int64{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			if tc.ids != nil {
				ctx = sourcescope.WithSourceIDs(ctx, tc.ids)
			}
			got, err := f.Store.IsAccountIdentityAddressContext(ctx, "OWNER@example.com")
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
