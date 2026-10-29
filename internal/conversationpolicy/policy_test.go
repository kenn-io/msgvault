package conversationpolicy

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEvaluate(t *testing.T) {
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		count    int
		known    bool
		activity time.Time
		override Tier
		want     Tier
	}{
		{"threshold", 20, true, time.Time{}, "", Full},
		{"large", 21, true, time.Time{}, "", Cold},
		{"unknown", 0, false, time.Time{}, "", Cold},
		{"cutoff", 200, true, now.Add(-180 * 24 * time.Hour), "", Full},
		{"expired", 200, true, now.Add(-180*24*time.Hour - time.Nanosecond), "", Cold},
		{"future cannot establish activity", 200, true, now.Add(time.Second), "", Cold},
		{"full override", 200, false, time.Time{}, Full, Full},
		{"cold override", 1, true, now, Cold, Cold},
		{"stub override", 1, true, now, Stub, Stub},
		{"skip override", 1, true, now, Skip, Skip},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Evaluate(Input{Participants: tc.count, KnownSize: tc.known, LastActivity: tc.activity, Override: tc.override, MaxParticipants: 20, ActivityWindow: 180 * 24 * time.Hour, Now: now})
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func FuzzAutomaticTier(f *testing.F) {
	f.Add(20, true, int64(0), 20, int64(86400))
	f.Add(21, false, int64(-1), 0, int64(-1))
	f.Fuzz(func(t *testing.T, count int, known bool, activity int64, limit int, window int64) {
		requirements := require.New(t)
		now := time.Unix(100000000, 0)
		got, err := Evaluate(Input{Participants: count, KnownSize: known, LastActivity: time.Unix(activity, 0), MaxParticipants: limit, ActivityWindow: time.Duration(window), Now: now})
		if count < 0 || limit < 0 || window <= 0 {
			requirements.Error(err)
			return
		}
		requirements.NoError(err)
		want := Cold
		active := activity <= now.Unix() && !time.Unix(activity, 0).Before(now.Add(-time.Duration(window)))
		if limit == 0 || (known && count <= limit) || active {
			want = Full
		}
		assert.Equal(t, want, got)
	})
}

func FuzzTierValidation(f *testing.F) {
	for _, s := range []string{"full", "cold", "stub", "skip", "", "FULL", " cold", "bogus"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		requirements := require.New(t)
		got, err := ParseTier(s)
		switch s {
		case "full", "cold", "stub", "skip":
			requirements.NoError(err)
			assert.Equal(t, Tier(s), got)
		default:
			requirements.Error(err)
		}
	})
}

func FuzzOverrideWins(f *testing.F) {
	f.Add(int64(0), false, uint8(0), int64(0))
	f.Fuzz(func(t *testing.T, count int64, known bool, choice uint8, limit int64) {
		// Keep the signed generator's complete magnitude, including MinInt64.
		if count < 0 {
			count = -(count + 1)
		}
		if limit < 0 {
			limit = -(limit + 1)
		}
		tier := []Tier{Full, Cold, Stub, Skip}[choice%4]
		got, err := Evaluate(Input{Participants: int(count), KnownSize: known, MaxParticipants: int(limit), ActivityWindow: time.Nanosecond, Now: time.Unix(1, 0), Override: tier})
		require.NoError(t, err)
		assert.Equal(t, tier, got)
	})
}
