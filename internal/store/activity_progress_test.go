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
		{"legacy position", `{"token":"12/34","after_id":42}`, "", 42},
		{"changed configuration", `{"token":"UTC/2","after_id":42}`, "UTC/3", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			_, err := st.DB().ExecContext(t.Context(), st.Rebind(`
				INSERT INTO archive_metadata (key, value) VALUES (?, ?)
			`), "activity_spine_cursor_test", tc.saved)
			require.NoError(t, err)
			got, err := st.ActivityProjectionCursorContext(t.Context(), "test", tc.requested)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
