package calevent

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodeICSContent(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	raw := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\n" +
		"UID:planning@example.test\r\nDTSTART:20270302T120000Z\r\nDTEND:20270302T130000Z\r\n" +
		"SUMMARY:Planning\r\nLOCATION:Room 4\r\nDESCRIPTION:Review agenda\r\n" +
		"URL:https://meet.example.test/planning\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")

	got, err := DecodeContent(t.Context(), "ics", raw, nil, "UTC")
	requirements.NoError(err)
	assertions.Equal(Content{Location: "Room 4", Description: "Review agenda", JoinURL: "https://meet.example.test/planning"}, got)
}

func TestDecodeCalDAVContentUsesStoredComponentSelector(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	raw := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\n" +
		"BEGIN:VEVENT\r\nUID:first@example.test\r\nDTSTART:20270302T120000Z\r\nDTEND:20270302T130000Z\r\n" +
		"LOCATION:First room\r\nDESCRIPTION:First agenda\r\nEND:VEVENT\r\n" +
		"BEGIN:VEVENT\r\nUID:second@example.test\r\nDTSTART:20270303T120000Z\r\nDTEND:20270303T130000Z\r\n" +
		"LOCATION:Second room\r\nDESCRIPTION:Second agenda\r\nURL:https://meet.example.test/second\r\nEND:VEVENT\r\n" +
		"END:VCALENDAR\r\n")
	events, err := ParseCalDAVResource(t.Context(), raw, "UTC")
	requirements.NoError(err)
	requirements.Len(events, 2)
	metadata, err := json.Marshal(CalDAVMetadata{EventID: events[1].Event.ProviderEventID})
	requirements.NoError(err)

	got, err := DecodeContent(t.Context(), CalDAVRawFormat, raw, metadata, "UTC")
	requirements.NoError(err)
	assertions.Equal(Content{Location: "Second room", Description: "Second agenda", JoinURL: "https://meet.example.test/second"}, got)
}
