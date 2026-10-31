package calevent

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestICSTimeZoneSparseCancellationReferences(t *testing.T) {
	for _, property := range []string{
		"RECURRENCE-ID;TZID=Europe/Moscow:20270302T090000",
		"EXDATE;TZID=Europe/Moscow:20270302T090000,20270303T090000",
		"RDATE;TZID=Europe/Moscow:20270302T090000,20270303T090000",
	} {
		for _, offset := range []string{"+0300", "+0400"} {
			t.Run(property+"/"+offset, func(t *testing.T) {
				assertions, requirements := assert.New(t), require.New(t)
				raw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:CANCEL\r\n" +
					"BEGIN:VTIMEZONE\r\nTZID:Europe/Moscow\r\n" +
					"BEGIN:STANDARD\r\nDTSTART:20141026T020000\r\nTZOFFSETFROM:" + offset + "\r\nTZOFFSETTO:" + offset + "\r\nEND:STANDARD\r\n" +
					"END:VTIMEZONE\r\nBEGIN:VEVENT\r\nUID:cancel@example.test\r\n" + property + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
				components, err := ParseICS([]byte(raw), "", "UTC")
				if offset == "+0400" {
					requirements.ErrorContains(err, "contradicts Europe/Moscow")
					assertions.Empty(components)
					return
				}
				requirements.NoError(err)
				requirements.Len(components, 1)
				event := components[0].Event
				assertions.Equal("cancelled", event.Status)
				assertions.Nil(event.Start)
				if strings.HasPrefix(property, "RECURRENCE-ID") {
					requirements.NotNil(event.Original)
					assertions.Equal("2027-03-02T06:00:00Z", event.Original.Instant.Format(time.RFC3339))
				} else {
					assertions.Equal([]string{property}, event.Recurrence)
				}
			})
		}
	}
}

func TestICSTimeZoneValidatesEachReferenceDate(t *testing.T) {
	for _, property := range []string{
		"RECURRENCE-ID;TZID=Europe/Moscow:20280302T090000",
		"EXDATE;TZID=Europe/Moscow:20270302T090000,20280302T090000",
		"RDATE;TZID=Europe/Moscow:20270302T090000,20280302T090000",
	} {
		t.Run(property, func(t *testing.T) {
			raw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\n" +
				"BEGIN:VTIMEZONE\r\nTZID:Europe/Moscow\r\n" +
				"BEGIN:STANDARD\r\nDTSTART:20141026T020000\r\nTZOFFSETFROM:+0400\r\nTZOFFSETTO:+0300\r\nEND:STANDARD\r\n" +
				"BEGIN:STANDARD\r\nDTSTART:20280101T020000\r\nTZOFFSETFROM:+0300\r\nTZOFFSETTO:+0400\r\nEND:STANDARD\r\n" +
				"END:VTIMEZONE\r\nBEGIN:VEVENT\r\nUID:reference@example.test\r\n" +
				"DTSTART;TZID=Europe/Moscow:20270302T090000\r\n" + property + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
			components, err := ParseICS([]byte(raw), "", "UTC")
			require.ErrorContains(t, err, "Europe/Moscow")
			assert.Empty(t, components)
		})
	}
}

func TestICSTimeZoneSparseCancellationTransitionRules(t *testing.T) {
	for _, rule := range []string{"2SU", "3SU"} {
		t.Run(rule, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			raw := string(newYorkTimezoneCalendar(2027, 9, 0, rule))
			raw = strings.Replace(raw, "VERSION:2.0\r\n", "VERSION:2.0\r\nMETHOD:CANCEL\r\n", 1)
			raw = strings.Replace(raw, "DTSTART;TZID=America/New_York:20270101T090000\r\nDURATION:PT30M\r\nRRULE:FREQ=DAILY;COUNT=365", "RECURRENCE-ID;TZID=America/New_York:20270315T090000", 1)
			components, err := ParseICS([]byte(raw), "", "UTC")
			if rule == "3SU" {
				requirements.ErrorContains(err, "contradicts America/New_York")
				assertions.Empty(components)
				return
			}
			requirements.NoError(err)
			requirements.Len(components, 1)
			requirements.NotNil(components[0].Event.Original)
			assertions.Equal("2027-03-15T13:00:00Z", components[0].Event.Original.Instant.Format(time.RFC3339))
		})
	}
}
