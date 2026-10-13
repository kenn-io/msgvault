package store_test

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"path/filepath"
)

// Every UTF-8 principal/key pair within the native admission bounds is accepted;
// all other pairs are rejected. The binding grants no resource authority.
func FuzzPublicationReceiptBindingBounds(f *testing.F) {
	for _, seed := range [][2]string{{"synthetic-owner", "synthetic-key"}, {"", "key"}, {"owner", " "}, {"\xff", "key"}, {strings.Repeat("p", 256), strings.Repeat("k", 256)}, {"owner", strings.Repeat("k", 257)}} {
		f.Add(seed[0], seed[1])
	}
	st, err := store.Open(filepath.Join(f.TempDir(), "receipt-binding.db"))
	require.NoError(f, err)
	f.Cleanup(func() { require.NoError(f, st.Close()) })
	require.NoError(f, st.InitSchema())
	binding, ok := any(st).(interface {
		WithCardDAVPublicationReceipt(ctx context.Context, principal, key string) (context.Context, error)
	})
	require.True(f, ok, "receipt binding must validate the complete principal/key domain")
	f.Fuzz(func(t *testing.T, principal, key string) {
		ctx, err := binding.WithCardDAVPublicationReceipt(t.Context(), principal, key)
		valid := strings.TrimSpace(principal) != "" && len(principal) <= 256 && utf8.ValidString(principal) && strings.TrimSpace(key) != "" && len(key) <= 256 && utf8.ValidString(key)
		if valid {
			require.NoError(t, err)
			assert.NotNil(t, ctx)
		} else {
			require.Error(t, err)
			assert.Nil(t, ctx)
		}
	})
}
