// Package importantdate contains deterministic calendar rules for annual dates.
package importantdate

import (
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/calrecur"
	"go.kenn.io/msgvault/internal/store"
)

// LeapDayPolicy controls February 29 in years without that date.
type LeapDayPolicy string

const (
	LeapFebruary28 LeapDayPolicy = "february_28"
	LeapMarch1     LeapDayPolicy = "march_1"
	LeapOnly       LeapDayPolicy = "leap_years_only"
)

// NextOccurrence returns the next local calendar occurrence, including today.
// An incomplete, invalid, or unsupported date has no occurrence.
func NextOccurrence(date store.PartialDate, now time.Time, location *time.Location, policy LeapDayPolicy) (time.Time, bool) {
	if location == nil || date.Month == nil || date.Day == nil || date.Validate() != nil {
		return time.Time{}, false
	}
	if policy != LeapFebruary28 && policy != LeapMarch1 && policy != LeapOnly {
		return time.Time{}, false
	}
	local := now.In(location)
	today := local.Format(time.DateOnly)
	year := local.Year()
	if date.Year != nil && *date.Year > year {
		year = *date.Year
	}
	// A Gregorian leap year always occurs within eight years, including the
	// century exception. Stop at the archive's maximum supported year.
	for last := min(year+8, 9999); year <= last; year++ {
		month, day := time.Month(*date.Month), *date.Day
		if month == time.February && day == 29 && !isLeapYear(year) {
			switch policy {
			case LeapOnly:
				continue
			case LeapFebruary28:
				day = 28
			case LeapMarch1:
				month, day = time.March, 1
			}
		}
		civil := fmt.Sprintf("%04d-%02d-%02d", year, month, day)
		if civil < today {
			continue
		}
		candidate, exists, err := calrecur.CivilDateBoundary(civil, location)
		if err != nil {
			return time.Time{}, false
		}
		if exists {
			return candidate, true
		}
	}
	return time.Time{}, false
}

func isLeapYear(year int) bool {
	return year%4 == 0 && (year%100 != 0 || year%400 == 0)
}

// DaysUntil counts calendar days in location, not elapsed 24-hour periods.
func DaysUntil(now, occurrence time.Time, location *time.Location) int {
	y1, m1, d1 := now.In(location).Date()
	y2, m2, d2 := occurrence.In(location).Date()
	a := time.Date(y1, m1, d1, 0, 0, 0, 0, time.UTC).Unix()
	b := time.Date(y2, m2, d2, 0, 0, 0, 0, time.UTC).Unix()
	return int((b - a) / 86400)
}
