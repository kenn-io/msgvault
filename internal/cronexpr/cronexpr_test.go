package cronexpr

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParserReadsSevenAsSunday(t *testing.T) {
	// 2026-10-19 is a Monday.
	monday := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		expression string
		want       []time.Weekday
	}{
		{"0 9 * * 7", []time.Weekday{time.Sunday}},
		{"0 9 * * 5-7", []time.Weekday{time.Friday, time.Saturday, time.Sunday}},
		{"0 9 * * 5-7/2", []time.Weekday{time.Friday, time.Sunday}},
		{"CRON_TZ=UTC 0 9 * * 7", []time.Weekday{time.Sunday}},
		{"CRON_TZ=UTC 0 9 * * 5-7", []time.Weekday{time.Friday, time.Saturday, time.Sunday}},
		{"CRON_TZ=UTC 0 9 * * 5-7/2", []time.Weekday{time.Friday, time.Sunday}},
	}
	for _, tt := range tests {
		t.Run(tt.expression, func(t *testing.T) {
			schedule, err := Parser.Parse(tt.expression)
			require.NoError(t, err)
			from := monday.In(time.Local)
			var got []time.Weekday
			for next := schedule.Next(from); next.Before(from.AddDate(0, 0, 7)); next = schedule.Next(next) {
				got = append(got, next.Weekday())
			}
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParserDaylightSavingTransitions(t *testing.T) {
	t.Run("a time in the skipped hour runs after the jump", func(t *testing.T) {
		schedule, err := Parser.Parse("CRON_TZ=America/New_York 30 2 * * *")
		require.NoError(t, err)
		// Clocks in New York jump from 02:00 EST to 03:00 EDT on 2026-03-08.
		from := time.Date(2026, 3, 8, 5, 0, 0, 0, time.UTC) // 00:00 EST
		assert.Equal(t, time.Date(2026, 3, 8, 7, 0, 0, 0, time.UTC), schedule.Next(from).UTC())
	})
	t.Run("a time in the repeated hour runs once", func(t *testing.T) {
		schedule, err := Parser.Parse("CRON_TZ=America/New_York 30 1 * * *")
		require.NoError(t, err)
		// Clocks in New York fall back from 02:00 EDT to 01:00 EST on 2026-11-01.
		from := time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC) // 00:00 EDT
		first := schedule.Next(from)
		assert.Equal(t, time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC), first.UTC())                // 01:30 EDT
		assert.Equal(t, time.Date(2026, 11, 2, 6, 30, 0, 0, time.UTC), schedule.Next(first).UTC()) // 01:30 EST the next day
	})
}

func TestParserMatchesEitherRestrictedDayField(t *testing.T) {
	// Restricting both day fields matches a day that satisfies either one.
	schedule, err := Parser.Parse("CRON_TZ=UTC 0 0 1 * MON")
	require.NoError(t, err)
	from := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	var got []time.Time
	for next := schedule.Next(from); next.Before(time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)); next = schedule.Next(next) {
		got = append(got, next)
	}
	assert.Equal(t, []time.Time{
		time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC),
	}, got)
}
