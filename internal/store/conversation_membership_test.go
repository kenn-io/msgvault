package store_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestListUnresolvedConversationIDs(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("beeper", "roster-test")
	requirements.NoError(err)
	for _, tc := range []struct{ id, metadata string }{
		{"legacy", `{}`},
		{"unknown", `{"member_count_unknown":true}`},
		{"stale", `{"member_count":2,"member_count_unknown":true}`},
		{"pending", `{"member_count":2,"roster_repair_pending":true}`},
		{"empty", `{"member_count":0}`},
		{"known", `{"member_count":5}`},
	} {
		id, err := st.EnsureConversation(source.ID, tc.id, "Synthetic room")
		requirements.NoError(err)
		requirements.NoError(st.SetConversationMetadata(id, sql.NullString{String: tc.metadata, Valid: true}))
	}
	other, err := st.GetOrCreateSource("beeper", "other-roster-test")
	requirements.NoError(err)
	_, err = st.EnsureConversation(other.ID, "other-room", "Other room")
	requirements.NoError(err)
	ids, err := st.ListUnresolvedConversationIDs(t.Context(), source.ID)
	requirements.NoError(err)
	assertions.Equal([]string{"legacy", "unknown", "stale", "pending"}, ids)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = st.ListUnresolvedConversationIDs(ctx, source.ID)
	assertions.ErrorIs(err, context.Canceled)
}
