package config

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConversationPolicySettingsContainsNoCredentials(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	cfg := NewDefaultConfig()
	dto := cfg.ConversationPolicySettings()
	requirements.NoError(dto.Validate())
	assertions.Len(dto.MaxParticipants, 5)
	raw, err := json.Marshal(dto)
	requirements.NoError(err)
	var fields map[string]any
	requirements.NoError(json.Unmarshal(raw, &fields))
	assertions.Len(fields, 2)
	assertions.Contains(fields, "max_participants")
	assertions.Contains(fields, "activity_window_days")
	dto.MaxParticipants["beeper"] = 0
	assertions.Equal(20, cfg.ConversationParticipantLimit("beeper"))
}
