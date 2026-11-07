package conversationpolicy_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
)

func TestConversationPolicySettingsRejectNegativeParticipantLimit(t *testing.T) {
	settings := conversationpolicy.RecommendedSettings()
	settings.MaxParticipants["beeper"] = -1
	require.Error(t, settings.Validate())
}
