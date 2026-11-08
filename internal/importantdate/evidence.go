package importantdate

import (
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

// DateStatus explains whether a single usable declared date was found.
type DateStatus string

const (
	DateEligible   DateStatus = "eligible"
	DateNoEligible DateStatus = "no_eligible_date"
	DateConflict   DateStatus = "conflicting_dates"
)

// SelectDeclaredDate excludes derived, withdrawn, incomplete and non-Gregorian
// facts. Distinct current month/day values are ambiguous and cannot be routed.
// A known year is retained to avoid producing an occurrence before the event.
// Conflicting known years are ambiguous even when month and day agree.
func SelectDeclaredDate(dates []store.PersonDate, kind store.PersonDateKind, now time.Time) (store.PartialDate, DateStatus) {
	var selected store.PartialDate
	for _, date := range dates {
		env := date.Envelope
		if date.DateKind != kind || !env.Source.IsDeclared() || !env.IsCurrent() || (env.ActiveFrom != nil && env.ActiveFrom.After(now)) {
			continue
		}
		if date.CalendarScale != nil && strings.TrimSpace(*date.CalendarScale) != "" && !strings.EqualFold(strings.TrimSpace(*date.CalendarScale), "gregorian") {
			continue
		}
		if date.Date.Month == nil || date.Date.Day == nil || date.Date.Validate() != nil {
			continue
		}
		if selected.Month != nil && (*selected.Month != *date.Date.Month || *selected.Day != *date.Date.Day ||
			(selected.Year != nil && date.Date.Year != nil && *selected.Year != *date.Date.Year)) {
			return store.PartialDate{}, DateConflict
		}
		month, day := *date.Date.Month, *date.Date.Day
		year := selected.Year
		if date.Date.Year != nil {
			y := *date.Date.Year
			year = &y
		}
		selected = store.PartialDate{Year: year, Month: &month, Day: &day}
	}
	if selected.Month == nil {
		return store.PartialDate{}, DateNoEligible
	}
	return selected, DateEligible
}
