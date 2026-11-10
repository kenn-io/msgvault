package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestEnsureConversationTypePreservationPolicy(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		t.Run(map[bool]string{false: "unscoped", true: "scoped"}[scoped], func(t *testing.T) {
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSourceContext(t.Context(), "google_chat", "owner@example.com")
			require.NoError(t, err)
			if scoped {
				run, err := st.StartSyncContext(t.Context(), source.ID, "test")
				require.NoError(t, err)
				st = st.ScopedToSync(source.ID, run)
			}
			id, err := st.EnsureConversationWithTypePolicyContext(t.Context(), source.ID, "DM synthetic", "direct_chat", "Original", true)
			require.NoError(t, err)
			again, err := st.EnsureConversationWithTypePolicyContext(t.Context(), source.ID, "DM synthetic", "chat", "", true)
			require.NoError(t, err)
			assert.Equal(t, id, again)
			var kind, title string
			require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT conversation_type,title FROM conversations WHERE id = ?`), id).Scan(&kind, &title))
			assert.Equal(t, "direct_chat", kind)
			assert.Equal(t, "Original", title)
			_, err = st.EnsureConversationWithTypeContext(t.Context(), source.ID, "DM synthetic", "group_chat", "Updated")
			require.NoError(t, err)
			require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT conversation_type,title FROM conversations WHERE id = ?`), id).Scan(&kind, &title))
			assert.Equal(t, "group_chat", kind)
			assert.Equal(t, "Updated", title)
		})
	}
}
