package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestTeamsMigrationTransfersAppreciationDismissal(t *testing.T) {
	for _, tc := range []struct {
		name                            string
		duplicate, destinationDismissed bool
	}{{name: "rename"}, {name: "duplicate-copy", duplicate: true}, {name: "duplicate-preserves-first", duplicate: true, destinationDismissed: true}} {
		t.Run(tc.name, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("teams", "owner@example.test")
			require.NoError(t, err)
			conversation, err := st.EnsureConversationWithType(source.ID, "direct", "direct_chat", "Direct")
			require.NoError(t, err)
			insertStoreTestMessage(t, st, source.ID, conversation, "legacy")
			if tc.duplicate {
				insertStoreTestMessage(t, st, source.ID, conversation, "scoped")
			}
			oldKey, err := peoplesweep.AppreciationContributionKey("teams", source.Identifier, "legacy")
			require.NoError(t, err)
			newKey, err := peoplesweep.AppreciationContributionKey("teams", source.Identifier, "scoped")
			require.NoError(t, err)
			at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
			_, err = st.RecordPersonAppreciationDismissalContext(t.Context(), oldKey, "person-test", "owner", at)
			require.NoError(t, err)
			if tc.destinationDismissed {
				_, err = st.RecordPersonAppreciationDismissalContext(t.Context(), newKey, "other-person", "first-owner", at.Add(-time.Hour))
				require.NoError(t, err)
			}
			require.NoError(t, st.MigrateSourceMessageID(t.Context(), source.ID, conversation, "legacy", "scoped"))
			states, err := st.GetPersonAppreciationStatesContext(t.Context(), []string{oldKey, newKey})
			require.NoError(t, err)
			require.Len(t, states, 2)
			require.NotNil(t, states[newKey].DismissedBy)
			wantActor := "owner"
			if tc.destinationDismissed {
				wantActor = "first-owner"
			}
			assert.Equal(t, wantActor, *states[newKey].DismissedBy)
			assert.Nil(t, states[newKey].ThanksBriefID)
			assert.NotNil(t, states[oldKey].DismissedAt)
		})
	}
}
