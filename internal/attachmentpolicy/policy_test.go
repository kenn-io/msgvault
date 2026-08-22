package attachmentpolicy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPolicyEvaluate(t *testing.T) {
	tests := []struct {
		name         string
		policy       Policy
		conversation Conversation
		size         int64
		want         SkipReason
	}{
		{
			name:         "zero value permits a channel",
			conversation: Conversation{Type: "channel", ParticipantCount: 100},
		},
		{
			name:         "none excludes a direct chat",
			policy:       Policy{Scope: ScopeNone},
			conversation: Conversation{Type: "direct_chat", ParticipantCount: 2},
			want:         SkipPolicyScope,
		},
		{
			name:         "direct excludes a channel",
			policy:       Policy{Scope: ScopeDirect},
			conversation: Conversation{Type: "channel", ParticipantCount: 2},
			want:         SkipPolicyScope,
		},
		{
			name:         "direct permits a group DM",
			policy:       Policy{Scope: ScopeDirect},
			conversation: Conversation{Type: "group_chat", ParticipantCount: 4},
		},
		{
			name:         "participant cap excludes a large chat",
			policy:       Policy{MaxParticipants: 3},
			conversation: Conversation{Type: "group_chat", ParticipantCount: 4},
			want:         SkipParticipantThreshold,
		},
		{
			name:         "unknown participant count remains eligible",
			policy:       Policy{MaxParticipants: 3},
			conversation: Conversation{Type: "group_chat"},
		},
		{
			name:   "account exclusion wins",
			policy: Policy{DisabledReason: SkipAccountPolicy},
			want:   SkipAccountPolicy,
		},
		{
			name:   "known oversize item is excluded",
			policy: Policy{MaxBytes: 1024},
			size:   1025,
			want:   SkipSizeCap,
		},
		{
			name:   "unknown size remains eligible",
			policy: Policy{MaxBytes: 1024},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.policy.Evaluate(tt.conversation, tt.size))
			assert.Equal(t, tt.want == "", tt.policy.Allows(tt.conversation, tt.size))
		})
	}
}

func TestPolicyValidate(t *testing.T) {
	require := require.New(t)
	require.NoError((Policy{}).Validate())
	require.NoError((Policy{Scope: ScopeAll}).Validate())
	require.NoError((Policy{Scope: ScopeDirect}).Validate())
	require.NoError((Policy{Scope: ScopeNone}).Validate())
	require.ErrorContains((Policy{Scope: "rooms"}).Validate(), "media_scope")
	require.ErrorContains((Policy{MaxParticipants: -1}).Validate(), "media_max_participants")
	require.ErrorContains((Policy{MaxBytes: -1}).Validate(), "max_media_mb")
}

func TestRetryEligible(t *testing.T) {
	tests := []struct {
		state DownloadState
		want  bool
	}{
		{state: "", want: true},
		{state: StatePending, want: true},
		{state: StateFailed, want: true},
		{state: StateSkipped, want: false},
		{state: StateStored, want: false},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, RetryEligible(tt.state), string(tt.state))
	}
}

func TestOversizeMarkerSize(t *testing.T) {
	assert.Equal(t, 11, OversizeMarkerSize(10, 0))
	assert.Equal(t, 12, OversizeMarkerSize(10, 12))
}
