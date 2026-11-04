package calevent

import (
	"context"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodeContentICS(t *testing.T) {
	raw := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:content@example.org\r\nDTSTART:20270302T170000Z\r\nDURATION:PT1H\r\nSUMMARY:Planning\r\nDESCRIPTION:Discuss the launch\r\nLOCATION:Room 3\r\nURL:https://meet.example.org/launch\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")

	content, err := DecodeContent(t.Context(), "ics", raw, nil, "UTC")
	require.NoError(t, err)
	assert.Equal(t, Content{Location: "Room 3", Description: "Discuss the launch", JoinURL: "https://meet.example.org/launch"}, content)
}

func TestDecodeContentCalDAVSelectsComponent(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	raw := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\n" +
		"BEGIN:VEVENT\r\nUID:first@example.org\r\nDTSTART:20270302T170000Z\r\nDURATION:PT1H\r\nSUMMARY:First\r\nLOCATION:First room\r\nEND:VEVENT\r\n" +
		"BEGIN:VEVENT\r\nUID:second@example.org\r\nDTSTART:20270303T170000Z\r\nDURATION:PT1H\r\nSUMMARY:Second\r\nDESCRIPTION:Selected description\r\nLOCATION:Second room\r\nURL:https://meet.example.org/second\r\nEND:VEVENT\r\n" +
		"END:VCALENDAR\r\n")
	components, err := ParseCalDAVResource(t.Context(), raw, "UTC")
	requirements.NoError(err)
	requirements.Len(components, 2)
	metadata, err := json.Marshal(CalDAVMetadata{EventID: components[1].Event.ProviderEventID, TimeZone: "UTC"})
	requirements.NoError(err)

	content, err := DecodeContent(t.Context(), CalDAVRawFormat, raw, metadata, "UTC")
	requirements.NoError(err)
	assertions.Equal(Content{Location: "Second room", Description: "Selected description", JoinURL: "https://meet.example.org/second"}, content)
}

func TestDecodeContentCalDAVRequiresSelector(t *testing.T) {
	raw := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:event@example.org\r\nDTSTART:20270302T170000Z\r\nDURATION:PT1H\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")

	_, err := DecodeContent(t.Context(), CalDAVRawFormat, raw, nil, "UTC")
	require.ErrorContains(t, err, "component selector")
}

func TestDecodeContentHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := DecodeContent(ctx, "ics", []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nEND:VCALENDAR\r\n"), nil, "UTC")
	require.ErrorIs(t, err, context.Canceled)
}
