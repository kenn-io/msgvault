package cronexpr

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
