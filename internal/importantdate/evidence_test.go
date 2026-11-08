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
			date, status := SelectDeclaredDate(tt.dates, store.PersonDateBirthday, now)
			assert.Equal(t, tt.want, status)
			if status == DateEligible {
				require.NotNil(t, date.Month)
				require.NotNil(t, date.Day)
				assert.Equal(t, 4, *date.Month)
				assert.Equal(t, 12, *date.Day)
			}
		})
	}
}
