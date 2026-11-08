package importantdate

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"hegel.dev/go/hegel"
)

func TestNextOccurrence(t *testing.T) {
	newYork, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	tests := []struct {
		name   string
		date   store.PartialDate
		now    time.Time
		policy LeapDayPolicy
		want   string
		wantOK bool
	}{
		{"yearless across year", store.PartialDate{Month: new(1), Day: new(2)}, time.Date(2026, 12, 31, 23, 0, 0, 0, newYork), LeapFebruary28, "2027-01-02", true},
		{"leap february", store.PartialDate{Month: new(2), Day: new(29)}, time.Date(2027, 1, 1, 0, 0, 0, 0, newYork), LeapFebruary28, "2027-02-28", true},
		{"leap march", store.PartialDate{Month: new(2), Day: new(29)}, time.Date(2027, 1, 1, 0, 0, 0, 0, newYork), LeapMarch1, "2027-03-01", true},
		{"leap only", store.PartialDate{Month: new(2), Day: new(29)}, time.Date(2027, 1, 1, 0, 0, 0, 0, newYork), LeapOnly, "2028-02-29", true},
		{"missing day", store.PartialDate{Month: new(2)}, time.Date(2027, 1, 1, 0, 0, 0, 0, newYork), LeapFebruary28, "", false},
		{"invalid date", store.PartialDate{Month: new(4), Day: new(31)}, time.Date(2027, 1, 1, 0, 0, 0, 0, newYork), LeapFebruary28, "", false},
		{"event before birth year", store.PartialDate{Year: new(2029), Month: new(5), Day: new(3)}, time.Date(2027, 1, 1, 0, 0, 0, 0, newYork), LeapFebruary28, "2029-05-03", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := NextOccurrence(tt.date, tt.now, newYork, tt.policy)
			assert.Equal(t, tt.wantOK, ok)
			if ok {
				assert.Equal(t, tt.want, got.Format(time.DateOnly))
			}
		})
	}
}

func TestDaysUntilUsesCalendarDaysAcrossDST(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	now := time.Date(2027, 3, 13, 23, 30, 0, 0, loc)
	occurrence := time.Date(2027, 3, 15, 0, 0, 0, 0, loc)
	assert.Equal(t, 2, DaysUntil(now, occurrence, loc))
}

// A valid non-leap date always returns the same month and day, no earlier
// than today, independent of the time of day used to request it.
func TestNextOccurrencePreservesMonthAndDay(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		month := hegel.Draw(ht, hegel.Integers(1, 12))
		day := hegel.Draw(ht, hegel.Integers(1, 28))
		year := hegel.Draw(ht, hegel.Integers(1900, 2200))
		now := time.Date(year, time.Month(month), day, 12, 0, 0, 0, time.UTC)
		got, ok := NextOccurrence(store.PartialDate{Month: &month, Day: &day}, now, time.UTC, LeapFebruary28)
		require.True(t, ok)
		assert.Equal(t, now.Format(time.DateOnly), got.Format(time.DateOnly))
	}, hegel.WithTestCases(200), hegel.WithDatabase(""))
}
