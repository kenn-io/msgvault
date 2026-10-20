package calevent

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newYorkTimezoneCalendar(year, hour, minute int, daylightRule string) []byte {
	start := fmt.Sprintf("%04d0101T%02d%02d00", year, hour, minute)
	return []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\n" +
		"BEGIN:VTIMEZONE\r\nTZID:America/New_York\r\n" +
		"BEGIN:STANDARD\r\nDTSTART:20071104T020000\r\nRRULE:FREQ=YEARLY;BYMONTH=11;BYDAY=1SU\r\nTZOFFSETFROM:-0400\r\nTZOFFSETTO:-0500\r\nEND:STANDARD\r\n" +
		"BEGIN:DAYLIGHT\r\nDTSTART:20070311T020000\r\nRRULE:FREQ=YEARLY;BYMONTH=3;BYDAY=" + daylightRule + "\r\nTZOFFSETFROM:-0500\r\nTZOFFSETTO:-0400\r\nEND:DAYLIGHT\r\n" +
		"END:VTIMEZONE\r\nBEGIN:VEVENT\r\nUID:generated-zone@example.test\r\n" +
		"DTSTART;TZID=America/New_York:" + start + "\r\nDURATION:PT30M\r\nRRULE:FREQ=DAILY;COUNT=365\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n")
}

func TestICSTimeZoneRetainsUnsupportedEventRecurrence(t *testing.T) {
	for _, tc := range []struct{ name, rule, diagnostic string }{
		{"unreachable secondly", "FREQ=SECONDLY;INTERVAL=2;BYSECOND=1;COUNT=1", "only daily, weekly, monthly and yearly recurrence is supported"},
		{"unreachable daily", "FREQ=DAILY;INTERVAL=7;BYDAY=MO;COUNT=1", "selectors and interval do not reach a recurrence date"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			raw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\n" +
				"BEGIN:VTIMEZONE\r\nTZID:Etc/UTC\r\n" +
				"BEGIN:STANDARD\r\nDTSTART:19700101T000000\r\nTZOFFSETFROM:+0000\r\nTZOFFSETTO:+0000\r\nEND:STANDARD\r\n" +
				"END:VTIMEZONE\r\nBEGIN:VEVENT\r\nUID:unsafe-rule@example.test\r\n" +
				"DTSTART;TZID=Etc/UTC:20270101T090000\r\nDURATION:PT1H\r\nRRULE:" + tc.rule + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
			components, err := ParseICS([]byte(raw), "", "UTC")
			requirements.NoError(err)
			requirements.Len(components, 1)
			assertions.Equal("series", components[0].Event.Kind)
			assertions.Contains(components[0].Event.RecurrenceError, tc.diagnostic)
		})
	}
}

func TestICSTimeZoneUnsupportedRecurrenceKeepsSibling(t *testing.T) {
	for _, recurrence := range []string{
		"RDATE;VALUE=PERIOD:20270102T090000Z/20270102T100000Z",
		"RDATE;VALUE=X-CUSTOM:20270102T090000Z",
		"EXDATE;VALUE=X-CUSTOM:20270102T090000Z",
		"EXRULE:FREQ=DAILY;COUNT=2",
		"RRULE:FREQ=YEARLY;RSCALE=CHINESE;COUNT=2",
	} {
		t.Run(recurrence, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			raw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\n" +
				"BEGIN:VTIMEZONE\r\nTZID:Etc/UTC\r\n" +
				"BEGIN:STANDARD\r\nDTSTART:19700101T000000\r\nTZOFFSETFROM:+0000\r\nTZOFFSETTO:+0000\r\nEND:STANDARD\r\n" +
				"END:VTIMEZONE\r\nBEGIN:VEVENT\r\nUID:unsupported@example.test\r\n" +
				"DTSTART;TZID=Etc/UTC:20270101T090000\r\nDURATION:PT1H\r\n" + recurrence + "\r\nEND:VEVENT\r\n" +
				"BEGIN:VEVENT\r\nUID:sibling@example.test\r\nDTSTART:20270102T120000Z\r\nSUMMARY:Readable sibling\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
			components, diagnostics, err := ParseICSWithDiagnostics(t.Context(), []byte(raw), "", "UTC", NewICSParseBudget())
			requirements.NoError(err)
			requirements.Len(components, 2)
			assertions.Equal("series", components[0].Event.Kind)
			assertions.Contains(components[0].Event.RecurrenceError, "unsupported recurrence")
			assertions.Equal([]string{recurrence}, components[0].Event.Recurrence)
			assertions.Contains(string(components[0].Raw), recurrence)
			assertions.Equal("sibling@example.test", components[1].UID)
			assertions.Empty(components[1].Event.RecurrenceError)
			requirements.Len(diagnostics, 1)
			assertions.Contains(diagnostics[0], "VEVENT 0:")
		})
	}
}

func TestICSTimeZoneMatchingTransitionRulesAcrossYears(t *testing.T) {
	for _, year := range []int{2027, 2063, 2099} {
		for _, clock := range []struct{ hour, minute int }{{0, 0}, {2, 0}, {23, 59}} {
			t.Run(fmt.Sprintf("%d/%02d:%02d", year, clock.hour, clock.minute), func(t *testing.T) {
				assert, require := assert.New(t), require.New(t)
				components, err := ParseICS(newYorkTimezoneCalendar(year, clock.hour, clock.minute, "2SU"), "", "UTC")
				require.NoError(err)
				require.Len(components, 1)
				assert.Equal("America/New_York", components[0].Event.EventTimeZone)
			})
		}
	}
}

func TestICSTimeZoneRejectsMismatchedTransitionRulesAcrossYears(t *testing.T) {
	for _, year := range []int{2027, 2063, 2099} {
		for _, clock := range []struct{ hour, minute int }{{0, 0}, {2, 0}, {23, 59}} {
			for _, rule := range []string{"1SU", "3SU", "4SU"} {
				t.Run(fmt.Sprintf("%d/%02d:%02d/%s", year, clock.hour, clock.minute, rule), func(t *testing.T) {
					components, err := ParseICS(newYorkTimezoneCalendar(year, clock.hour, clock.minute, rule), "", "UTC")
					require.ErrorContains(t, err, "VTIMEZONE transition dates contradict America/New_York")
					assert.Empty(t, components)
				})
			}
		}
	}
}

func TestICSTimeZoneDaylightOffsets(t *testing.T) {
	raw := "BEGIN:VCALENDAR\r\nBEGIN:VTIMEZONE\r\nTZID:America/New_York\r\nBEGIN:STANDARD\r\nDTSTART:20261101T020000\r\nRRULE:FREQ=YEARLY;BYMONTH=11;BYDAY=1SU\r\nTZOFFSETFROM:-0400\r\nTZOFFSETTO:-0500\r\nEND:STANDARD\r\nBEGIN:DAYLIGHT\r\nDTSTART:20270314T020000\r\nRRULE:FREQ=YEARLY;BYMONTH=3;BYDAY=2SU\r\nTZOFFSETFROM:-0500\r\nTZOFFSETTO:-0400\r\nEND:DAYLIGHT\r\nEND:VTIMEZONE\r\nBEGIN:VEVENT\r\nUID:winter@example.com\r\nDTSTART;TZID=America/New_York:20270102T090000\r\nRRULE:FREQ=DAILY;COUNT=365\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	for _, tc := range []struct{ name, from, to string }{
		{"valid", "-0500", "-0400"},
		{"wrong daylight offset", "-0500", "-0300"},
		{"wrong standard offset", "-0600", "-0400"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			input := strings.Replace(raw, "TZOFFSETFROM:-0500\r\nTZOFFSETTO:-0400", "TZOFFSETFROM:"+tc.from+"\r\nTZOFFSETTO:"+tc.to, 1)
			components, err := ParseICS([]byte(input), "", "UTC")
			if tc.name == "valid" {
				requirements.NoError(err)
				assertions.Len(components, 1)
			} else {
				requirements.ErrorContains(err, "VTIMEZONE DAYLIGHT contradicts America/New_York near DTSTART")
				assertions.Empty(components)
			}
		})
	}
}

func TestICSTimeZoneWinterSeriesRequiresDaylight(t *testing.T) {
	raw := "BEGIN:VCALENDAR\r\nBEGIN:VTIMEZONE\r\nTZID:America/New_York\r\nBEGIN:STANDARD\r\nDTSTART:19701101T020000\r\nTZOFFSETFROM:-0500\r\nTZOFFSETTO:-0500\r\nEND:STANDARD\r\nEND:VTIMEZONE\r\nBEGIN:VEVENT\r\nUID:winter@example.com\r\nDTSTART;TZID=America/New_York:20270102T090000\r\nRRULE:FREQ=DAILY;COUNT=365\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	components, err := ParseICS([]byte(raw), "", "UTC")
	require.ErrorContains(t, err, "VTIMEZONE DAYLIGHT")
	assert.Empty(t, components)
}

func TestICSTimeZoneRejectsTransitionDateMismatch(t *testing.T) {
	raw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\n" +
		"BEGIN:VTIMEZONE\r\nTZID:America/New_York\r\n" +
		"BEGIN:STANDARD\r\nDTSTART:20261101T020000\r\nRRULE:FREQ=YEARLY;BYMONTH=11;BYDAY=1SU\r\nTZOFFSETFROM:-0400\r\nTZOFFSETTO:-0500\r\nEND:STANDARD\r\n" +
		"BEGIN:DAYLIGHT\r\nDTSTART:20270404T020000\r\nRRULE:FREQ=YEARLY;BYMONTH=4;BYDAY=1SU\r\nTZOFFSETFROM:-0500\r\nTZOFFSETTO:-0400\r\nEND:DAYLIGHT\r\n" +
		"END:VTIMEZONE\r\nBEGIN:VEVENT\r\nUID:transition@example.test\r\n" +
		"DTSTART;TZID=America/New_York:20270102T090000\r\nRRULE:FREQ=DAILY;COUNT=365\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"

	components, err := ParseICS([]byte(raw), "", "UTC")
	require.ErrorContains(t, err, "VTIMEZONE transition dates contradict America/New_York")
	assert.Empty(t, components)
}

func TestICSTimeZoneRejectsTransitionHourMismatch(t *testing.T) {
	input := strings.Replace(
		string(newYorkTimezoneCalendar(2027, 9, 0, "2SU")),
		"RRULE:FREQ=YEARLY;BYMONTH=3;BYDAY=2SU",
		"RRULE:FREQ=YEARLY;BYMONTH=3;BYDAY=2SU;BYHOUR=3",
		1,
	)
	components, err := ParseICS([]byte(input), "", "UTC")
	require.ErrorContains(t, err, "VTIMEZONE transition dates contradict America/New_York")
	assert.Empty(t, components)
}

func TestICSTimeZoneAcceptsOccurrenceEndingAcrossFallBack(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	raw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\n" +
		"BEGIN:VTIMEZONE\r\nTZID:America/Los_Angeles\r\n" +
		"BEGIN:STANDARD\r\nDTSTART:20071104T020000\r\nRRULE:FREQ=YEARLY;BYMONTH=11;BYDAY=1SU\r\nTZOFFSETFROM:-0700\r\nTZOFFSETTO:-0800\r\nEND:STANDARD\r\n" +
		"BEGIN:DAYLIGHT\r\nDTSTART:20070311T020000\r\nRRULE:FREQ=YEARLY;BYMONTH=3;BYDAY=2SU\r\nTZOFFSETFROM:-0800\r\nTZOFFSETTO:-0700\r\nEND:DAYLIGHT\r\n" +
		"END:VTIMEZONE\r\nBEGIN:VEVENT\r\nUID:fall-back@example.test\r\n" +
		"DTSTART;TZID=America/Los_Angeles:20241027T003000\r\nDURATION:PT2H\r\n" +
		"RRULE:FREQ=WEEKLY;COUNT=2\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"

	components, err := ParseICS([]byte(raw), "", "UTC")
	require.NoError(err)
	require.Len(components, 1)
	assert.Equal("2024-10-27T07:30:00Z", components[0].Event.Start.UTC().Format(time.RFC3339))
	assert.Equal("2024-10-27T09:30:00Z", components[0].Event.End.UTC().Format(time.RFC3339))
}

func TestICSTimeZoneWithoutEmbeddedDefinitionAcceptsLargeRecurrence(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	raw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\n" +
		"BEGIN:VEVENT\r\nUID:large-series@example.test\r\n" +
		"DTSTART;TZID=America/New_York:20270102T090000\r\n" +
		"DTEND;TZID=America/New_York:20270102T100000\r\n" +
		"RRULE:FREQ=DAILY;COUNT=50001\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"

	components, err := ParseICS([]byte(raw), "", "UTC")
	requirements.NoError(err)
	requirements.Len(components, 1)
	assertions.Equal("America/New_York", components[0].Event.EventTimeZone)
}

func TestICSTimeZoneVerifiesEarlierRDate(t *testing.T) {
	raw := string(newYorkTimezoneCalendar(2027, 9, 0, "3SU"))
	raw = strings.Replace(raw, "20270101T090000", "20270401T090000", 1)
	raw = strings.Replace(raw, "RRULE:FREQ=DAILY;COUNT=365", "RDATE;TZID=America/New_York:20270301T090000", 1)
	components, err := ParseICS([]byte(raw), "", "UTC")
	require.ErrorContains(t, err, "VTIMEZONE transition dates contradict America/New_York")
	assert.Empty(t, components)
}

func TestICSTimeZoneVerifiesOtherReferencedZones(t *testing.T) {
	for _, property := range []string{"DTEND;TZID=America/New_York:20270401T100000", "DURATION:PT1H\r\nRDATE;TZID=America/New_York:20270401T090000"} {
		t.Run(property, func(t *testing.T) {
			raw := string(newYorkTimezoneCalendar(2027, 9, 0, "3SU"))
			raw = strings.Replace(raw, "DTSTART;TZID=America/New_York:20270101T090000", "DTSTART:20270301T140000Z", 1)
			raw = strings.Replace(raw, "DURATION:PT30M\r\nRRULE:FREQ=DAILY;COUNT=365", property, 1)
			components, err := ParseICS([]byte(raw), "", "UTC")
			require.ErrorContains(t, err, "VTIMEZONE transition dates contradict America/New_York")
			assert.Empty(t, components)
		})
	}
}

func TestICSTimeZoneRejectsFrequentObservance(t *testing.T) {
	raw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\n" +
		"BEGIN:VTIMEZONE\r\nTZID:Etc/UTC\r\n" +
		"BEGIN:STANDARD\r\nDTSTART:20270101T090000\r\nRRULE:FREQ=SECONDLY\r\nTZOFFSETFROM:+0000\r\nTZOFFSETTO:+0000\r\nEND:STANDARD\r\n" +
		"END:VTIMEZONE\r\nBEGIN:VEVENT\r\nUID:frequent-zone@example.test\r\n" +
		"DTSTART;TZID=Etc/UTC:20270101T090000\r\nDURATION:PT1S\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	for _, frequency := range []string{"SECONDLY", "MINUTELY", "HOURLY", "DAILY", "WEEKLY", "MONTHLY"} {
		t.Run(frequency, func(t *testing.T) {
			input := strings.Replace(raw, "FREQ=SECONDLY", "FREQ="+frequency, 1)
			input = strings.Replace(input, "DURATION:PT1S", "DURATION:PT1S\r\nRRULE:FREQ=DAILY", 1)
			components, err := ParseICS([]byte(input), "", "UTC")
			require.ErrorContains(t, err, "VTIMEZONE STANDARD RRULE must use FREQ=YEARLY")
			assert.Empty(t, components)
		})
	}
}

func TestICSTimeZoneBoundsEventCandidates(t *testing.T) {
	for _, rule := range []string{
		"FREQ=YEARLY;COUNT=2;BYHOUR=0,1,2,3,4,5,6,7,8,9,10,11;BYMINUTE=0,1,2,3,4,5,6,7,8,9,10,11;BYSECOND=0,1,2,3,4,5,6,7,8,9,10,11",
		"FREQ=YEARLY;UNTIL=20270101T100000Z;BYDAY=MO,TU,WE,TH,FR,SA,SU;BYMINUTE=0,1,2,3,4,5,6,7,8,9,10,11;BYSECOND=0,1,2,3,4,5,6,7,8,9,10,11",
	} {
		t.Run(rule, func(t *testing.T) {
			raw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VTIMEZONE\r\nTZID:Etc/UTC\r\n" +
				"BEGIN:STANDARD\r\nDTSTART:20270101T000000\r\nTZOFFSETFROM:+0000\r\nTZOFFSETTO:+0000\r\nEND:STANDARD\r\n" +
				"END:VTIMEZONE\r\nBEGIN:VEVENT\r\nUID:event-budget@example.test\r\n" +
				"DTSTART;TZID=Etc/UTC:20270101T090000\r\nDURATION:PT1S\r\nRRULE:" + rule + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
			components, err := ParseICS([]byte(raw), "", "UTC")
			require.NoError(t, err)
			require.Len(t, components, 1)
			assert.Contains(t, components[0].Event.RecurrenceError, "unsupported recurrence")
		})
	}
}

func TestICSTimeZoneBoundsObservanceWork(t *testing.T) {
	for _, tc := range []struct{ name, onset, rule, eventRule string }{
		{"interval", "20270101T090000", "FREQ=YEARLY;INTERVAL=401", ""},
		{"count", "20270101T090000", "FREQ=YEARLY;COUNT=50001", ""},
		{"year candidates", "20270101T090000", "FREQ=YEARLY;BYMINUTE=0,1,2,3,4,5,6,7,8,9,10,11;BYSECOND=0,1,2,3,4,5,6,7,8,9,10,11", ""},
		{"previous transitions", "00010101T090000", "FREQ=YEARLY;BYDAY=MO,TU,WE,TH,FR,SA,SU", ""},
		{"range transitions", "20270101T090000", "FREQ=YEARLY;BYDAY=MO,TU,WE,TH,FR,SA,SU", "RRULE:FREQ=DAILY\r\n"},
		{"event range", "20270101T090000", "FREQ=YEARLY", "RRULE:FREQ=DAILY;UNTIL=22270101T090000Z\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VTIMEZONE\r\nTZID:Etc/UTC\r\n" +
				"BEGIN:STANDARD\r\nDTSTART:" + tc.onset + "\r\nRRULE:" + tc.rule + "\r\nTZOFFSETFROM:+0000\r\nTZOFFSETTO:+0000\r\nEND:STANDARD\r\n" +
				"END:VTIMEZONE\r\nBEGIN:VEVENT\r\nUID:zone-budget@example.test\r\n" +
				"DTSTART;TZID=Etc/UTC:20270101T090000\r\nDURATION:PT1H\r\n" + tc.eventRule + "END:VEVENT\r\nEND:VCALENDAR\r\n"
			components, err := ParseICS([]byte(raw), "", "UTC")
			require.ErrorContains(t, err, "VTIMEZONE")
			assert.Contains(t, err.Error(), "budget")
			assert.Empty(t, components)
		})
	}
}

func TestICSTimeZoneAcceptsSupersededDaylight(t *testing.T) {
	raw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\n" +
		"BEGIN:VTIMEZONE\r\nTZID:Europe/Moscow\r\n" +
		"BEGIN:DAYLIGHT\r\nDTSTART:20100328T020000\r\nRRULE:FREQ=YEARLY;BYMONTH=3;BYDAY=-1SU;UNTIL=20100328T000000Z\r\nTZOFFSETFROM:+0300\r\nTZOFFSETTO:+0400\r\nEND:DAYLIGHT\r\n" +
		"BEGIN:STANDARD\r\nDTSTART:20141026T020000\r\nTZOFFSETFROM:+0400\r\nTZOFFSETTO:+0300\r\nEND:STANDARD\r\n" +
		"END:VTIMEZONE\r\nBEGIN:VEVENT\r\nUID:permanent-standard@example.test\r\n" +
		"DTSTART;TZID=Europe/Moscow:20270302T120000\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=365\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	for _, historical := range []bool{true, false} {
		t.Run(fmt.Sprintf("historical=%t", historical), func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			input := raw
			if !historical {
				input = strings.Replace(input, ";UNTIL=20100328T000000Z", "", 1)
			}
			components, err := ParseICS([]byte(input), "", "UTC")
			if !historical {
				require.ErrorContains(err, "VTIMEZONE active offset 14400 contradicts Europe/Moscow offset 10800")
				assert.Empty(components)
				return
			}
			require.NoError(err)
			require.Len(components, 1)
			assert.Equal("2027-03-02T09:00:00Z", components[0].Event.Start.UTC().Format(time.RFC3339))
		})
	}
}

func TestNearbyICSOffsetsFindsDaylightOnlyWhereItExists(t *testing.T) {
	for _, tc := range []struct {
		zone         string
		hasDaylight  bool
		standardSecs int
	}{
		{"America/Los_Angeles", true, -8 * 3600},
		{"Europe/Moscow", false, 3 * 3600},
	} {
		t.Run(tc.zone, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			loc, err := time.LoadLocation(tc.zone)
			requirements.NoError(err)
			standard, daylight := nearbyICSOffsets(time.Date(2024, time.October, 27, 0, 30, 0, 0, loc))
			requirements.NotNil(standard)
			assertions.Equal(tc.standardSecs, *standard)
			assertions.Equal(tc.hasDaylight, daylight != nil)
		})
	}
}

func TestICSTimeZoneObservanceUntilIncludesFinalTransition(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	raw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\n" +
		"BEGIN:VTIMEZONE\r\nTZID:Europe/Berlin\r\n" +
		"BEGIN:STANDARD\r\nDTSTART:20251026T030000\r\nRRULE:FREQ=YEARLY;BYMONTH=10;BYDAY=-1SU;UNTIL=20271031T010000Z\r\nTZOFFSETFROM:+0200\r\nTZOFFSETTO:+0100\r\nEND:STANDARD\r\n" +
		"BEGIN:DAYLIGHT\r\nDTSTART:20250330T020000\r\nRRULE:FREQ=YEARLY;BYMONTH=3;BYDAY=-1SU;UNTIL=20270328T010000Z\r\nTZOFFSETFROM:+0100\r\nTZOFFSETTO:+0200\r\nEND:DAYLIGHT\r\n" +
		"END:VTIMEZONE\r\nBEGIN:VEVENT\r\nUID:inclusive-transition@example.com\r\n" +
		"DTSTART;TZID=Europe/Berlin:20270101T090000\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=365\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	components, err := ParseICS([]byte(raw), "", "UTC")
	requirements.NoError(err)
	requirements.Len(components, 1)
	assertions.Equal("2027-01-01T08:00:00Z", components[0].Event.Start.UTC().Format(time.RFC3339))
}
