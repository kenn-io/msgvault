package importantdate

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestDeclaredDateSelection(t *testing.T) {
	now := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	base := store.PersonDate{DateKind: store.PersonDateBirthday, Date: store.PartialDate{Month: new(4), Day: new(12)}, Envelope: store.ValueEnvelope{Source: store.ProvenanceUser}}
	derived := base
	derived.Envelope.Source = store.ProvenanceExtraction
	conflict := base
	conflict.Date.Day = new(13)
	year1 := base
	year1.Date.Year = new(2001)
	year2 := base
	year2.Date.Year = new(2002)
	future := base
	future.Envelope.ActiveFrom = new(now.Add(time.Hour))
	closed := base
	closed.Envelope.SupersededAt = new(now)
	foreign := base
	foreign.CalendarScale = new("hebrew")
	incomplete := base
	incomplete.Date.Day = nil
	tests := []struct {
		name  string
		dates []store.PersonDate
		want  DateStatus
	}{
		{"manual", []store.PersonDate{base}, DateEligible},
		{"identical facts", []store.PersonDate{base, base}, DateEligible},
		{"conflict", []store.PersonDate{base, conflict}, DateConflict},
		{"conflicting years", []store.PersonDate{year1, year2}, DateConflict},
		{"derived", []store.PersonDate{derived}, DateNoEligible},
		{"future", []store.PersonDate{future}, DateNoEligible},
		{"superseded", []store.PersonDate{closed}, DateNoEligible},
		{"non-gregorian", []store.PersonDate{foreign}, DateNoEligible},
		{"incomplete", []store.PersonDate{incomplete}, DateNoEligible},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checks := assert.New(t)
			must := require.New(t)
			date, status := SelectDeclaredDate(tt.dates, store.PersonDateBirthday, now)
			checks.Equal(tt.want, status)
			if status == DateEligible {
				must.NotNil(date.Month)
				must.NotNil(date.Day)
				checks.Equal(4, *date.Month)
				checks.Equal(12, *date.Day)
			}
		})
	}
}
