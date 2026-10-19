package calrecur

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCivilDateResolution(t *testing.T) {
	for _, tc := range []struct {
		name, zone, start  string
		lines              []string
		wantDates          []string
		wantStart, wantEnd string // First generated occurrence, in UTC.
		exception          *Exception
		originalMatches    bool
		wantException      [4]string // Civil start, civil end, UTC start, UTC end.
	}{
		{
			name: "Apia skipped DTSTART alone", zone: "Pacific/Apia", start: "2011-12-30",
		},
		{
			name: "Apia skipped DTSTART COUNT", zone: "Pacific/Apia", start: "2011-12-30",
			lines:     []string{"RRULE:FREQ=DAILY;COUNT=3"},
			wantDates: []string{"2011-12-31", "2012-01-01", "2012-01-02"},
		},
		{
			name: "Apia skipped DTSTART unmatched BYDAY COUNT", zone: "Pacific/Apia", start: "2011-12-30",
			lines:     []string{"RRULE:FREQ=WEEKLY;BYDAY=SA;COUNT=1"},
			wantDates: []string{"2011-12-31"},
		},
		{
			name: "Apia skipped DTSTART matching BYDAY COUNT", zone: "Pacific/Apia", start: "2011-12-30",
			lines:     []string{"RRULE:FREQ=WEEKLY;BYDAY=FR;COUNT=1"},
			wantDates: []string{"2012-01-06"},
		},
		{
			name: "Apia skipped DTSTART BYDAY UNTIL", zone: "Pacific/Apia", start: "2011-12-30",
			lines:     []string{"RRULE:FREQ=WEEKLY;BYDAY=SA;UNTIL=20120107"},
			wantDates: []string{"2011-12-31", "2012-01-07"},
		},
		{
			name: "Apia generated dates COUNT", zone: "Pacific/Apia", start: "2011-12-28",
			lines:     []string{"RRULE:FREQ=DAILY;COUNT=4"},
			wantDates: []string{"2011-12-28", "2011-12-29", "2011-12-31", "2012-01-01"},
		},
		{
			name: "Apia BYDAY before skipping", zone: "Pacific/Apia", start: "2011-12-30",
			lines:     []string{"RRULE:FREQ=WEEKLY;BYDAY=FR,SA;COUNT=2"},
			wantDates: []string{"2011-12-31", "2012-01-06"},
		},
		{
			name: "Apia BYMONTHDAY before skipping", zone: "Pacific/Apia", start: "2011-12-30",
			lines:     []string{"RRULE:FREQ=MONTHLY;BYMONTHDAY=30;COUNT=1"},
			wantDates: []string{"2012-01-30"},
		},
		{
			name: "Apia skipped RDATE", zone: "Pacific/Apia", start: "2011-12-29",
			lines: []string{"RDATE;VALUE=DATE:20111230"}, wantDates: []string{"2011-12-29"},
		},
		{
			name: "Apia skipped RDATE and EXDATE", zone: "Pacific/Apia", start: "2011-12-29",
			lines:     []string{"RDATE;VALUE=DATE:20111230,20111231", "EXDATE;VALUE=DATE:20111230"},
			wantDates: []string{"2011-12-29", "2011-12-31"},
			wantStart: "2011-12-29T10:00:00Z", wantEnd: "2011-12-30T10:00:00Z",
		},
		{
			name: "Apia skipped EXDATE COUNT", zone: "Pacific/Apia", start: "2011-12-29",
			lines:     []string{"RRULE:FREQ=DAILY;COUNT=3", "EXDATE;VALUE=DATE:20111230"},
			wantDates: []string{"2011-12-29", "2011-12-31", "2012-01-01"},
		},
		{
			name: "Apia exception target skipped", zone: "Pacific/Apia", start: "2011-12-29",
			wantDates: []string{"2011-12-29"}, originalMatches: true,
			exception:     &Exception{Original: Identity{Date: "2011-12-29", AllDay: true}, StartDate: "2011-12-30", EndDate: "2011-12-31"},
			wantException: [4]string{"2011-12-30", "2011-12-31", "2011-12-30T10:00:00Z", "2011-12-30T10:00:00Z"},
		},
		{
			name: "Apia exception end skipped", zone: "Pacific/Apia", start: "2011-12-29",
			wantDates: []string{"2011-12-29"}, originalMatches: true,
			exception:     &Exception{Original: Identity{Date: "2011-12-29", AllDay: true}, StartDate: "2011-12-29", EndDate: "2011-12-30"},
			wantException: [4]string{"2011-12-29", "2011-12-30", "2011-12-29T10:00:00Z", "2011-12-30T10:00:00Z"},
		},
		{
			name: "Apia exception original skipped", zone: "Pacific/Apia", start: "2011-12-29",
			lines: []string{"RRULE:FREQ=DAILY;COUNT=2"}, wantDates: []string{"2011-12-29", "2011-12-31"},
			exception:     &Exception{Original: Identity{Date: "2011-12-30", AllDay: true}, StartDate: "2011-12-31", EndDate: "2012-01-01"},
			wantException: [4]string{"2011-12-31", "2012-01-01", "2011-12-30T10:00:00Z", "2011-12-31T10:00:00Z"},
		},
		{
			name: "Apia timed exception inherits skipped original start", zone: "Pacific/Apia", start: "2011-12-29",
			wantDates:     []string{"2011-12-29"},
			exception:     &Exception{Original: Identity{Date: "2011-12-30", AllDay: true}, End: new(instant(t, "2011-12-31T10:00:00Z"))},
			wantException: [4]string{"", "", "2011-12-30T10:00:00Z", "2011-12-31T10:00:00Z"},
		},
		{
			name: "Sao Paulo DTSTART COUNT", zone: "America/Sao_Paulo", start: "2018-11-04",
			lines: []string{"RRULE:FREQ=DAILY;COUNT=2"}, wantDates: []string{"2018-11-04", "2018-11-05"},
			wantStart: "2018-11-04T03:00:00Z", wantEnd: "2018-11-05T02:00:00Z",
		},
		{
			name: "Sao Paulo unmatched BYDAY COUNT", zone: "America/Sao_Paulo", start: "2018-11-04",
			lines: []string{"RRULE:FREQ=WEEKLY;BYDAY=MO;COUNT=1"}, wantDates: []string{"2018-11-04"},
		},
		{
			name: "Sao Paulo matching BYDAY COUNT", zone: "America/Sao_Paulo", start: "2018-11-04",
			lines: []string{"RRULE:FREQ=WEEKLY;BYDAY=SU;COUNT=2"}, wantDates: []string{"2018-11-04", "2018-11-11"},
		},
		{
			name: "Sao Paulo BYDAY before resolution", zone: "America/Sao_Paulo", start: "2018-11-04",
			lines: []string{"RRULE:FREQ=WEEKLY;BYDAY=SU,MO;COUNT=2"}, wantDates: []string{"2018-11-04", "2018-11-05"},
		},
		{
			name: "Sao Paulo RDATE", zone: "America/Sao_Paulo", start: "2018-11-03",
			lines: []string{"RDATE;VALUE=DATE:20181104"}, wantDates: []string{"2018-11-03", "2018-11-04"},
			wantStart: "2018-11-03T03:00:00Z", wantEnd: "2018-11-04T03:00:00Z",
		},
		{
			name: "Sao Paulo EXDATE COUNT", zone: "America/Sao_Paulo", start: "2018-11-03",
			lines: []string{"RRULE:FREQ=DAILY;COUNT=3", "EXDATE;VALUE=DATE:20181104"}, wantDates: []string{"2018-11-03", "2018-11-05"},
		},
		{
			name: "Sao Paulo exception target", zone: "America/Sao_Paulo", start: "2018-11-03",
			wantDates: []string{"2018-11-03"}, originalMatches: true,
			exception:     &Exception{Original: Identity{Date: "2018-11-03", AllDay: true}, StartDate: "2018-11-04", EndDate: "2018-11-05"},
			wantException: [4]string{"2018-11-04", "2018-11-05", "2018-11-04T03:00:00Z", "2018-11-05T02:00:00Z"},
		},
		{
			name: "Sao Paulo exception original", zone: "America/Sao_Paulo", start: "2018-11-04",
			wantDates: []string{"2018-11-04"}, originalMatches: true,
			exception:     &Exception{Original: Identity{Date: "2018-11-04", AllDay: true}, StartDate: "2018-11-05", EndDate: "2018-11-06"},
			wantException: [4]string{"2018-11-05", "2018-11-06", "2018-11-05T02:00:00Z", "2018-11-06T02:00:00Z"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			zone, err := time.LoadLocation(tc.zone)
			requirements.NoError(err)
			s, err := ParseSet(time.Time{}, true, tc.start, zone, tc.lines)
			requirements.NoError(err)
			from := instant(t, tc.start+"T00:00:00Z").Add(-24 * time.Hour)
			got, _, err := Expand(t.Context(), s, 0, 1, from, from.AddDate(0, 2, 0), 100)
			requirements.NoError(err)
			var dates []string
			for _, o := range got {
				dates = append(dates, o.StartDate)
			}
			assertions.Equal(tc.wantDates, dates)
			if tc.wantStart != "" {
				requirements.NotEmpty(got)
				assertions.Equal(instant(t, tc.wantStart), got[0].Start.UTC())
				assertions.Equal(instant(t, tc.wantEnd), got[0].End.UTC())
			}
			if tc.exception != nil {
				matches := false
				for _, o := range got {
					matches = matches || o.Original == tc.exception.Original
				}
				assertions.Equal(tc.originalMatches, matches)
				o, err := EffectiveException(s, *tc.exception, 0, 1)
				requirements.NoError(err)
				assertions.Equal(tc.exception.Original, o.Original)
				assertions.Equal(tc.wantException, [4]string{o.StartDate, o.EndDate, o.Start.UTC().Format(time.RFC3339), o.End.UTC().Format(time.RFC3339)})
				assertions.True(o.HasException)
				assertions.Equal(tc.wantException[0] != "", o.AllDay)
			}
		})
	}
}
