package loops

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseSnoozeUntil(t *testing.T) {
	location := time.FixedZone("UTC+2", 2*60*60)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, location)
	for _, tc := range []struct {
		input string
		want  time.Time
	}{
		{"48h", now.Add(48 * time.Hour)},
		{"2026-10-18", time.Date(2026, 10, 18, 0, 0, 0, 0, location)},
		{"2026-10-18T12:30:00.123Z", time.Date(2026, 10, 18, 12, 30, 0, 123000000, time.UTC)},
	} {
		t.Run(tc.input, func(t *testing.T) {
			got, err := ParseSnoozeUntil(tc.input, now)
			require.NoError(t, err)
			assert.True(t, tc.want.Equal(got))
		})
	}
	for _, input := range []string{"", "tomorrow", "0s", "-1h", "2026-10-17", "2026-02-30", "2026-10-17T10:00:00Z", "2020-01-01T00:00:00Z"} {
		t.Run("invalid "+input, func(t *testing.T) {
			_, err := ParseSnoozeUntil(input, now)
			require.Error(t, err)
		})
	}
}
