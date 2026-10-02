package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestBindMeetingSourceIdentity(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("pocket", "personal")
	requirements.NoError(err)
	requirements.Error(st.BindMeetingSourceIdentity(src.ID, "", "user-a"))
	requirements.Error(st.BindMeetingSourceIdentity(src.ID, "owner@example.com", ""))
	requirements.NoError(st.BindMeetingSourceIdentity(src.ID, " OWNER@example.com ", "user-a"))
	requirements.NoError(st.BindMeetingSourceIdentity(src.ID, "owner@example.com", "user-a"))
	requirements.Error(st.BindMeetingSourceIdentity(src.ID, "other@example.com", "user-a"))
	requirements.Error(st.BindMeetingSourceIdentity(src.ID, "owner@example.com", "user-b"))
	bound, err := st.GetSourceByID(src.ID)
	requirements.NoError(err)
	assertions.JSONEq(`{"account_email":"owner@example.com","account_user_id":"user-a"}`, bound.SyncConfig.String)
	requirements.NoError(st.UpdateSourceSyncConfig(src.ID, `{"unrelated":true}`))
	requirements.Error(st.BindMeetingSourceIdentity(src.ID, "owner@example.com", "user-a"))
	bound, err = st.GetSourceByID(src.ID)
	requirements.NoError(err)
	assertions.JSONEq(`{"unrelated":true}`, bound.SyncConfig.String)
}
