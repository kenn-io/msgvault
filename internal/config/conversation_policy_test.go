package config

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
)

func TestConversationPolicyConfig(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "config.toml")
	requirements.NoError(os.WriteFile(file, []byte(`[beeper]
max_participants = 0
[conversation_policy]
activity_window_days = 30
[[conversation_policy.overrides]]
source_type = "beeper"
source_identifier = "test-account"
conversation_id = "test-room"
tier = "cold"
`), 0o600))
	loaded, err := LoadWithOverrides(file, dir, RuntimeOverrides{})
	requirements.NoError(err)
	assertions.Zero(loaded.Beeper.MaxParticipants)
	assertions.Equal(30, loaded.ConversationPolicy.ActivityWindowDays)
	assertions.Equal(conversationpolicy.Cold, loaded.ConversationPolicy.Overrides[0].Tier)
}

func TestConversationPolicyConfigValidation(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"negative cap", "[slack]\nmax_participants=-1"},
		{"invalid window", "[conversation_policy]\nactivity_window_days=-1"},
		{"overflow window", "[conversation_policy]\nactivity_window_days=10000000"},
		{"invalid override", `[[conversation_policy.overrides]]
source_type="beeper"
source_identifier="test-account"
conversation_id="room"
tier="unknown"`},
		{"unsupported stub", `[[conversation_policy.overrides]]
source_type="beeper"
source_identifier="test-account"
conversation_id="room"
tier="stub"`},
		{"missing account", `[[conversation_policy.overrides]]
source_type="beeper"
conversation_id="room"
tier="full"`},
		{"duplicate", `[[conversation_policy.overrides]]
source_type="beeper"
source_identifier="test-account"
conversation_id="room"
tier="full"
[[conversation_policy.overrides]]
source_type="beeper"
source_identifier="test-account"
conversation_id="room"
tier="cold"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			file := filepath.Join(dir, "config.toml")
			require.NoError(t, os.WriteFile(file, []byte(tc.body), 0o600))
			_, err := LoadWithOverrides(file, dir, RuntimeOverrides{})
			require.Error(t, err)
		})
	}
}

func TestConversationPolicyWindowBounds(t *testing.T) {
	for _, days := range []int{0, 1, 106751, 106752, -1} {
		t.Run(strconv.Itoa(days), func(t *testing.T) {
			requirements := require.New(t)
			cfg := NewDefaultConfig()
			cfg.ConversationPolicy.ActivityWindowDays = days
			err := cfg.validateConversationPolicy()
			if days < 0 || days > 106751 {
				requirements.Error(err)
				return
			}
			requirements.NoError(err)
			want := days
			if want == 0 {
				want = 180
			}
			assert.Equal(t, time.Duration(want)*24*time.Hour, cfg.ConversationPolicy.ActivityWindow())
		})
	}
}

func FuzzConversationOverrideIdentity(f *testing.F) {
	f.Add("beeper", "example-account", "room")
	f.Add("x/y", "a:b", "!room:example.com")
	f.Fuzz(func(t *testing.T, source, account, room string) {
		assertions := assert.New(t)
		cfg := NewDefaultConfig()
		cfg.ConversationPolicy.Overrides = []conversationpolicy.Override{{SourceType: source, SourceIdentifier: account, ConversationID: room, Tier: conversationpolicy.Cold}}
		assertions.Equal(conversationpolicy.Cold, cfg.ConversationOverride(source, account, room))
		assertions.Empty(cfg.ConversationOverride(source+"x", account, room))
		assertions.Empty(cfg.ConversationOverride(source, account+"x", room))
		assertions.Empty(cfg.ConversationOverride(source, account, room+"x"))
	})
}
