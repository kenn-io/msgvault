package store_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestConversationSnapshotMemberCountPreservesMetadata(t *testing.T) {
	for _, tc := range []struct{ name, before, want string }{
		{"extension", `{"keep":"synthetic","member_count_unknown":true}`, `{"keep":"synthetic","member_count":3}`},
		{"JSON null", `null`, `{"member_count":3}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			f := storetest.New(t)
			require.NoError(f.Store.SetConversationMetadata(f.ConvID, sql.NullString{String: tc.before, Valid: true}))
			count := 3
			var err error
			require.NotPanics(func() {
				_, err = f.Store.ApplyConversationSnapshotContext(t.Context(), f.Source.ID, &store.ConversationPersistData{SourceConversationID: "default-thread", ConversationType: "group_chat", MemberCount: &count})
			})
			require.NoError(err)
			metadata, err := f.Store.GetConversationMetadata(f.ConvID)
			require.NoError(err)
			assert.JSONEq(t, tc.want, metadata.String)
		})
	}
}
