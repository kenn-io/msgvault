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
	must := require.New(t)
	newYork, err := time.LoadLocation("America/New_York")
	must.NoError(err)
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
		{"century leap gap", store.PartialDate{Month: new(2), Day: new(29)}, time.Date(2097, 3, 1, 0, 0, 0, 0, newYork), LeapOnly, "2104-02-29", true},
		{"missing day", store.PartialDate{Month: new(2)}, time.Date(2027, 1, 1, 0, 0, 0, 0, newYork), LeapFebruary28, "", false},
		{"invalid date", store.PartialDate{Month: new(4), Day: new(31)}, time.Date(2027, 1, 1, 0, 0, 0, 0, newYork), LeapFebruary28, "", false},
		{"event before birth year", store.PartialDate{Year: new(2029), Month: new(5), Day: new(3)}, time.Date(2027, 1, 1, 0, 0, 0, 0, newYork), LeapFebruary28, "2029-05-03", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checks := assert.New(t)
			got, ok := NextOccurrence(tt.date, tt.now, newYork, tt.policy)
			checks.Equal(tt.wantOK, ok)
			if ok {
				checks.Equal(tt.want, got.Format(time.DateOnly))
			}
		})
	}
}

func TestDaysUntilUsesCalendarDaysAcrossDST(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	loc, err := time.LoadLocation("America/New_York")
	must.NoError(err)
	now := time.Date(2027, 3, 13, 23, 30, 0, 0, loc)
	occurrence := time.Date(2027, 3, 15, 0, 0, 0, 0, loc)
	checks.Equal(2, DaysUntil(now, occurrence, loc))
}

// A valid non-leap date always returns the same month and day, no earlier
// than today, independent of the time of day used to request it.
func TestNextOccurrencePreservesMonthAndDay(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		checks := assert.New(ht)
		must := require.New(ht)
		month := hegel.Draw(ht, hegel.Integers(1, 12))
		day := hegel.Draw(ht, hegel.Integers(1, 28))
		year := hegel.Draw(ht, hegel.Integers(1900, 2200))
		now := time.Date(year, time.Month(month), day, 12, 0, 0, 0, time.UTC)
		got, ok := NextOccurrence(store.PartialDate{Month: &month, Day: &day}, now, time.UTC, LeapFebruary28)
		must.True(ok)
		checks.Equal(now.Format(time.DateOnly), got.Format(time.DateOnly))
	}, hegel.WithTestCases(200), hegel.WithDatabase(""))
}

func TestNextOccurrenceOnSkippedMidnight(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	loc, err := time.LoadLocation("America/Sao_Paulo")
	must.NoError(err)
	now := time.Date(2018, 11, 3, 12, 0, 0, 0, loc)
	got, ok := NextOccurrence(store.PartialDate{Month: new(11), Day: new(4)}, now, loc, LeapFebruary28)
	must.True(ok)
	checks.Equal("2018-11-04", got.In(loc).Format(time.DateOnly))
	checks.Equal(1, DaysUntil(now, got, loc))
}

func TestNextOccurrenceSkipsNonexistentCivilDate(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	loc, err := time.LoadLocation("Pacific/Apia")
	must.NoError(err)
	now := time.Date(2011, 12, 29, 12, 0, 0, 0, loc)
	got, ok := NextOccurrence(store.PartialDate{Month: new(12), Day: new(30)}, now, loc, LeapFebruary28)
	must.True(ok)
	checks.Equal("2012-12-30", got.In(loc).Format(time.DateOnly))
}
