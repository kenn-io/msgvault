package callhistory

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSignalNativeStatuses(t *testing.T) {
	for _, tc := range []struct {
		mode, status, outcome, owner, peer string
		withdrawn                          bool
	}{
		{"Direct", "Accepted", "answered", "joined", "joined", false},
		{"Direct", "Missed", "missed", "not_joined", "unknown", false},
		{"Direct", "MissedNotificationProfile", "missed", "not_joined", "unknown", false},
		{"Direct", "Declined", "declined", "not_joined", "unknown", false},
		{"Direct", "Pending", "unknown", "unknown", "unknown", false},
		{"Direct", "Deleted", "unknown", "unknown", "unknown", true},
		{"Direct", "FutureStatus", "unknown", "unknown", "unknown", false},
		{"Group", "Accepted", "unknown", "unknown", "", false},
		{"Group", "Joined", "answered", "joined", "", false},
		{"Group", "Ringing", "unknown", "unknown", "", false},
		{"Group", "GenericGroupCall", "unknown", "unknown", "", false},
		{"Group", "OutgoingRing", "unknown", "unknown", "", false},
		{"Group", "Missed", "missed", "not_joined", "", false},
		{"Group", "Declined", "declined", "not_joined", "", false},
		{"Group", "Deleted", "unknown", "unknown", "", true},
		{"Adhoc", "JoinedAdhoc", "answered", "joined", "", false},
		{"Adhoc", "GenericAdhocCall", "unknown", "unknown", "", false},
		{"Adhoc", "Pending", "unknown", "unknown", "", false},
		{"Adhoc", "Deleted", "unknown", "unknown", "", true},
		{"Adhoc", "Unknown", "unknown", "unknown", "", false},
		{"FutureMode", "Accepted", "unknown", "unknown", "", false},
	} {
		t.Run(tc.mode+"/"+tc.status, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			raw := fmt.Sprintf(`{"callId":"native-1","peerId":"peer","mode":%q,"status":%q,"direction":"Incoming","timestamp":1790841600000,"endedTimestamp":1790841660000,"futureField":null}`, tc.mode, tc.status)
			req := ImportRequest{Format: "signal_desktop", Account: "example", Record: []byte(raw)}
			if tc.mode == "Direct" {
				req.Bindings = map[string]int64{"peer": 7}
			}
			got, err := req.Observation()
			requirements.NoError(err)
			assertions.Equal(tc.outcome, got.Outcome)
			assertions.Equal(tc.owner, got.OwnerParticipation)
			assertions.Equal(tc.withdrawn, got.Withdrawn)
			assertions.Equal("incoming", got.Direction)
			assertions.Nil(got.DurationSeconds, "end minus start is not talk duration")
			assertions.Empty(got.Timezone, "epoch milliseconds do not identify the source zone")
			assertions.JSONEq(raw, string(got.Raw))
			if tc.peer == "" {
				assertions.Empty(got.Participants, "a group peer is not a joined roster")
			} else {
				requirements.Equal([]Participant{{ID: 7, Participation: tc.peer}}, got.Participants)
			}
		})
	}
}

func TestCalendarSchedulingIsNeverAttendance(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	raw := `{"id":"event-1","status":"confirmed","start":{"dateTime":"2026-10-01T09:00:00-07:00","timeZone":"America/Los_Angeles"},"end":{"dateTime":"2026-10-01T10:00:00-07:00"},"attendees":[{"email":"peer@example.com","responseStatus":"accepted"}],"conferenceData":{"entryPoints":[{"entryPointType":"phone","uri":"tel:+12025550101"},{"entryPointType":"video","uri":"https://meet.google.com/abc-defg-hij"}]},"description":"https://example.zoom.us/j/123 https://teams.microsoft.com/l/meetup-join/example https://cal.com/example/intro https://meet.google.com.evil.example/fake https://meet.google.com@evil.example/fake"}`
	got, err := (ImportRequest{
		Format: "google_calendar", Account: "calendar@example.com", Record: []byte(raw),
		Bindings: map[string]int64{"peer@example.com": 7},
	}).Observation()
	requirements.NoError(err)
	assertions.Equal("scheduled_meeting", got.Kind)
	assertions.Equal("unknown", got.Outcome)
	assertions.Equal("unknown", got.OwnerParticipation)
	assertions.Equal([]Participant{{ID: 7, Participation: "invited"}}, got.Participants)
	assertions.Empty(got.NativeCallID)
	assertions.Nil(got.DurationSeconds)
	assertions.Equal("America/Los_Angeles", got.Timezone)
	_, offset := got.OccurredAt.Zone()
	assertions.Equal(-7*3600, offset)
	assertions.Len(got.AccessPoints, 5)
	for _, point := range got.AccessPoints {
		assertions.NotContains(point.Value, "evil.example")
	}
	assertions.JSONEq(raw, string(got.Raw))
}

func TestImportMissingAndUnsupportedFields(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	for _, tc := range []struct {
		format, raw string
	}{
		{"signal_desktop", `{"callId":"x","timestamp":null}`},
		{"signal_desktop", `{"callId":"x"}`},
		{"signal_desktop", `{"callId":"x","timestamp":"1790841600000"}`},
		{"google_calendar", `{"id":"x","status":"cancelled"}`},
		{"google_calendar", `{"id":"x","start":{"date":"2026-10-01"},"recurrence":["RRULE:FREQ=WEEKLY"]}`},
		{"unsupported", `{}`},
	} {
		_, err := (ImportRequest{Format: tc.format, Account: "example", Record: []byte(tc.raw)}).Observation()
		requirements.ErrorIs(err, ErrInvalid)
	}
	for _, end := range []string{"", `,"endedTimestamp":null`, `,"endedTimestamp":0`} {
		got, err := (ImportRequest{Format: "signal_desktop", Account: "example", Record: []byte(`{"callId":"x","timestamp":0` + end + `}`)}).Observation()
		requirements.NoError(err, "explicit epoch zero differs from a missing timestamp")
		assertions.Equal(int64(0), got.OccurredAt.UnixMilli())
	}
	got, err := (ImportRequest{Format: "google_calendar", Account: "example", Record: []byte(`{"id":"x","status":"cancelled","start":{"date":"2026-10-01"}}`)}).Observation()
	requirements.NoError(err)
	assertions.Equal("day", got.Precision)
	assertions.Equal("cancelled", got.Outcome)
	assertions.Empty(got.Timezone)
}

func FuzzImportNeverInventsScheduledAttendance(f *testing.F) {
	f.Add([]byte(`{"format":"google_calendar","account":"example","record":{"id":"x","start":{"date":"2026-10-01"}}}`))
	f.Add([]byte(`{"format":"signal_desktop","account":"example","record":{"callId":"x","timestamp":0}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		assertions := assert.New(t)
		requirements := require.New(t)
		o, err := DecodeImport(bytes.NewReader(data))
		if err != nil {
			return
		}
		requirements.NoError(o.Validate())
		if o.Kind == "scheduled_meeting" {
			assertions.NotEqual("answered", o.Outcome)
			assertions.NotEqual("joined", o.OwnerParticipation)
			assertions.Nil(o.DurationSeconds)
			for _, p := range o.Participants {
				assertions.NotEqual("joined", p.Participation)
			}
		}
	})
}

func TestImportRequestBoundsAndNormalizedRoundtrip(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	_, err := DecodeImport(strings.NewReader(strings.Repeat(" ", MaxRequestBytes+1)))
	requirements.ErrorIs(err, ErrImportTooLarge)
	o := exampleObservation()
	record, err := json.Marshal(o)
	requirements.NoError(err)
	request, err := json.Marshal(ImportRequest{Format: "normalized", Account: o.Account, Record: record})
	requirements.NoError(err)
	got, err := DecodeImport(bytes.NewReader(request))
	requirements.NoError(err)
	assertions.Equal(o.RecordID, got.RecordID)
	assertions.Equal(o.Participants, got.Participants)
	_, err = (ImportRequest{Format: "normalized", Account: "different", Record: record}).Observation()
	requirements.ErrorIs(err, ErrInvalid)
}

func TestSignalRequiresExactDirectPeerBinding(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	r := ImportRequest{
		Format: "signal_desktop", Account: "example",
		Record: []byte(`{"callId":"x","peerId":"peer","mode":"Direct","direction":"Outgoing","status":"Accepted","timestamp":0}`),
	}
	o, err := r.Observation()
	requirements.NoError(err)
	assertions.Equal("outgoing", o.Direction)
	assertions.Empty(o.Participants, "missing explicit mapping does not guess a person")
	r.Bindings = map[string]int64{"another-peer": 1}
	_, err = r.Observation()
	requirements.ErrorIs(err, ErrInvalid)
	r.Record = []byte(`{"callId":"x","peerId":"peer","mode":"Group","status":"Joined","timestamp":0}`)
	r.Bindings = map[string]int64{"peer": 1}
	_, err = r.Observation()
	requirements.ErrorIs(err, ErrInvalid)
}

func FuzzSourceIdentifierSeparatesAccounts(f *testing.F) {
	f.Add("a", "bc", "ab", "c")
	f.Fuzz(func(t *testing.T, providerA, accountA, providerB, accountB string) {
		assertions := assert.New(t)
		a := (Evidence{Provider: providerA, Account: accountA}).SourceIdentifier()
		b := (Evidence{Provider: providerB, Account: accountB}).SourceIdentifier()
		if providerA == providerB && accountA == accountB {
			assertions.Equal(a, b)
		} else {
			assertions.NotEqual(a, b)
		}
	})
}

func FuzzValidCalendarImportPreservesEvidence(f *testing.F) {
	f.Add(uint32(0), uint8(0), false, false)
	f.Add(uint32(1790841600), uint8(255), true, true)
	f.Fuzz(func(t *testing.T, seconds uint32, peer uint8, cancelled, allDay bool) {
		assertions := assert.New(t)
		requirements := require.New(t)
		at := time.Unix(int64(seconds), 0).UTC()
		start := map[string]string{"dateTime": at.Format(time.RFC3339)}
		precision := "instant"
		if allDay {
			start = map[string]string{"date": at.Format(time.DateOnly)}
			at = time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC)
			precision = "day"
		}
		status, outcome := "confirmed", "unknown"
		if cancelled {
			status, outcome = "cancelled", "cancelled"
		}
		record, err := json.Marshal(map[string]any{
			"id": "generated-event", "status": status, "start": start,
			"attendees": []map[string]string{{"email": "peer@example.com", "responseStatus": "accepted"}},
		})
		requirements.NoError(err)
		id := int64(peer) + 1
		wire, err := json.Marshal(ImportRequest{
			Format: "google_calendar", Account: "example", Record: record,
			Bindings: map[string]int64{"peer@example.com": id},
		})
		requirements.NoError(err)
		o, err := DecodeImport(bytes.NewReader(wire))
		requirements.NoError(err, "constructive valid inputs must be accepted")
		assertions.Equal("generated-event", o.RecordID)
		assertions.Equal("scheduled_meeting", o.Kind)
		assertions.Equal(outcome, o.Outcome)
		assertions.Equal("unknown", o.OwnerParticipation)
		assertions.Equal(precision, o.Precision)
		assertions.True(at.Equal(o.OccurredAt))
		assertions.Equal([]Participant{{ID: id, Participation: "invited"}}, o.Participants)
		assertions.JSONEq(string(record), string(o.Raw))
		assertions.Nil(o.DurationSeconds)
	})
}

func FuzzScheduledContradictionsAreRejected(f *testing.F) {
	f.Add(uint8(0))
	f.Add(uint8(1))
	f.Add(uint8(2))
	f.Fuzz(func(t *testing.T, variant uint8) {
		requirements := require.New(t)
		o := exampleObservation()
		o.Kind, o.Outcome, o.OwnerParticipation = "scheduled_meeting", "unknown", "unknown"
		o.Participants[0].Participation = "invited"
		switch variant % 3 {
		case 0:
			o.OwnerParticipation = "joined"
		case 1:
			o.Participants[0].Participation = "joined"
		case 2:
			duration := float64(variant)
			o.DurationSeconds = &duration
		}
		record, err := json.Marshal(o)
		requirements.NoError(err)
		wire, err := json.Marshal(ImportRequest{Format: "normalized", Account: o.Account, Record: record})
		requirements.NoError(err)
		_, err = DecodeImport(bytes.NewReader(wire))
		requirements.ErrorIs(err, ErrInvalid)
	})
}
