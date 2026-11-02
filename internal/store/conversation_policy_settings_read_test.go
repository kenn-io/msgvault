package store_test

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/testutil"
	"testing"
	"time"
)

func TestConversationPolicySettingsContext(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	st := testutil.NewTestStore(t)
	defaults, err := st.ConversationPolicySettingsContext(t.Context())
	requirements.NoError(err)
	assertions.Equal(conversationpolicy.DefaultSettings(), defaults, "unsaved settings keep classification off")
	defaults.MaxParticipants["discord"] = 7
	_, err = st.ConfigureConversationPolicy(t.Context(), defaults, time.Now())
	requirements.NoError(err)
	got, err := st.ConversationPolicySettingsContext(t.Context())
	requirements.NoError(err)
	assertions.Equal(7, got.MaxParticipants["discord"])
	got.MaxParticipants["discord"] = 99
	fresh, err := st.ConversationPolicySettingsContext(t.Context())
	requirements.NoError(err)
	assertions.Equal(7, fresh.MaxParticipants["discord"])
	_, err = st.DB().Exec(`UPDATE archive_metadata SET value='{}' WHERE key='conversation_policy_settings'`)
	requirements.NoError(err)
	_, err = st.ConversationPolicySettingsContext(t.Context())
	requirements.Error(err)
}
