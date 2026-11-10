package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestActivityProjectionCursorTokenSelection(t *testing.T) {
	for _, tc := range []struct {
		name, saved, requested string
		want                   int64
	}{
		{"legacy position", "12/34", "", 42},
		{"changed configuration", "UTC/2", "UTC/3", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			require.NoError(t, st.SetActivityProjectionCursorContext(t.Context(), "test", tc.saved, 42))
			got, err := st.ActivityProjectionCursorContext(t.Context(), "test", tc.requested)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
