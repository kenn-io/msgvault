package discord

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSnowflakeTimestampConversions(t *testing.T) {
	tests := []struct {
		name string
		at   time.Time
	}{
		{name: "discord epoch", at: time.Date(2015, 1, 1, 0, 0, 0, 0, time.UTC)},
		{name: "millisecond precision", at: time.Date(2026, 7, 19, 12, 34, 56, 789_000_000, time.FixedZone("test", -5*60*60))},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snowflake, err := SnowflakeFromTimestamp(tt.at)
			require.NoError(t, err)
			got, err := TimestampFromSnowflake(snowflake)
			require.NoError(t, err)
			assert.Equal(t, tt.at.UTC().Truncate(time.Millisecond), got)
		})
	}
}

func TestSnowflakeTimestampBounds(t *testing.T) {
	at := time.Date(2026, 7, 19, 12, 34, 56, 789_654_321, time.UTC)
	lower, upper, err := SnowflakeBoundsForTimestamp(at)
	require.NoError(t, err)

	lowerTime, err := TimestampFromSnowflake(lower)
	require.NoError(t, err)
	upperTime, err := TimestampFromSnowflake(upper)
	require.NoError(t, err)
	assert.Equal(t, at.Truncate(time.Millisecond), lowerTime)
	assert.Equal(t, lowerTime, upperTime)

	lowerValue, err := ParseSnowflake(lower)
	require.NoError(t, err)
	upperValue, err := ParseSnowflake(upper)
	require.NoError(t, err)
	assert.Equal(t, uint64((1<<22)-1), upperValue-lowerValue)
}

func TestSnowflakeConversionRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name      string
		snowflake string
		at        time.Time
	}{
		{name: "malformed snowflake", snowflake: "not-a-snowflake"},
		{name: "negative snowflake", snowflake: "-1"},
		{name: "before discord epoch", at: time.Date(2014, 12, 31, 23, 59, 59, 999_000_000, time.UTC)},
		{name: "timestamp beyond snowflake range", at: time.UnixMilli(1420070400000 + int64(^uint64(0)>>22) + 1)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.snowflake != "" {
				_, err := TimestampFromSnowflake(tt.snowflake)
				require.Error(t, err)
				return
			}
			_, err := SnowflakeFromTimestamp(tt.at)
			require.Error(t, err)
		})
	}
}
