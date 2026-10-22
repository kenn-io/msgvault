package callhistory

import (
	"encoding/json/v2"
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func teamsRequest(t *testing.T, ended bool, fields map[string]any) ImportRequest {
	t.Helper()
	kind := "callStartedEventMessageDetail"
	if ended {
		kind = "callEndedEventMessageDetail"
	}
	record := map[string]any{
		"id": "event-1", "chatId": "chat-1", "messageType": "systemEventMessage",
		"createdDateTime": "2026-10-01T10:00:00Z",
		"eventDetail":     map[string]any{"@odata.type": "#microsoft.graph." + kind, "callId": "call-1", "callEventType": "call"},
	}
	maps.Copy(record, fields)
	raw, err := json.Marshal(record)
	require.NoError(t, err)
	return ImportRequest{Format: "teams", Account: "example-account", Record: raw}
}

func TestTeamsEndedEvidence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	r := teamsRequest(t, true, nil)
	r.Record = []byte(strings.Replace(string(r.Record), `"callId":"call-1"`, `"callId":"call-1","callDuration":"PT1M0.5S","callParticipants":[{"participant":{"user":{"id":"peer"}}},{"participant":{"user":{"id":"alias"}}}],"initiator":{"user":{"id":"initiator"}}`, 1))
	r.Bindings = map[string]int64{"peer": 7, "alias": 7}
	o, err := r.Observation()
	require.NoError(err)
	assert.Equal("teams", o.Provider)
	assert.Equal("call-1", o.NativeCallID)
	assert.Equal("call", o.Kind)
	assert.Equal([]Participant{{ID: 7, Participation: "joined"}}, o.Participants)
	require.NotNil(o.DurationSeconds)
	assert.InDelta(60.5, *o.DurationSeconds, 1e-9)
	require.NotNil(o.EndedAt)
	assert.Equal(time.Date(2026, 10, 1, 9, 58, 59, 500000000, time.UTC), o.OccurredAt)
	assert.Equal(string(r.Record), string(o.Raw))
	r.Bindings = map[string]int64{"initiator": 8}
	_, err = r.Observation()
	require.ErrorIs(err, ErrInvalid, "initiator alone is not roster evidence")
}

func TestTeamsInvalidRecords(t *testing.T) {
	for name, fields := range map[string]map[string]any{
		"ordinary":           {"messageType": "message"},
		"missing ID":         {"id": ""},
		"missing context":    {"chatId": nil},
		"ambiguous context":  {"channelIdentity": map[string]string{"teamId": "team", "channelId": "channel"}},
		"incomplete channel": {"chatId": nil, "channelIdentity": map[string]string{"teamId": "team"}},
		"missing time":       {"createdDateTime": nil},
		"offset required":    {"createdDateTime": "2026-10-01T10:00:00"},
		"unsupported event":  {"eventDetail": map[string]string{"@odata.type": "#microsoft.graph.callRecordingEventMessageDetail", "callId": "call"}},
		"no call ID":         {"eventDetail": map[string]string{"@odata.type": "#microsoft.graph.callEndedEventMessageDetail", "callEventType": "call"}},
		"invalid deletion":   {"deletedDateTime": "not-a-time"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := teamsRequest(t, true, fields).Observation()
			require.ErrorIs(t, err, ErrInvalid)
		})
	}
	for _, duration := range []string{"", "P", "PT", "P1Y", "P1M", "PT-1S", "-PT1S", "PT1Sx", "P1DT", "PT1.5M", "PT1e3S", "PTNaNS", "PT9999999999999999999999S"} {
		t.Run("duration/"+duration, func(t *testing.T) {
			r := teamsRequest(t, true, nil)
			r.Record = []byte(strings.Replace(string(r.Record), `"callId":"call-1"`, fmt.Sprintf(`"callId":"call-1","callDuration":%q`, duration), 1))
			_, err := r.Observation()
			require.ErrorIs(t, err, ErrInvalid)
		})
	}
	r := teamsRequest(t, true, map[string]any{"createdDateTime": "0001-01-01T00:00:00Z"})
	r.Record = []byte(strings.Replace(string(r.Record), `"callId":"call-1"`, `"callId":"call-1","callDuration":"P1D"`, 1))
	_, err := r.Observation()
	require.ErrorIs(t, err, ErrInvalid)
}

func FuzzTeamsDurationEquivalentUnits(f *testing.F) {
	f.Add(uint32(60), uint32(123456789))
	f.Add(uint32(0), uint32(0))
	f.Fuzz(func(t *testing.T, whole, fraction uint32) {
		// Materialize a duration representable in nanoseconds and exact days/seconds.
		nanos := fraction % 1000000000
		r := teamsRequest(t, true, nil)
		parse := func(value string) Observation {
			durationRequest := r
			durationRequest.Record = []byte(strings.Replace(string(r.Record), `"callId":"call-1"`, fmt.Sprintf(`"callId":"call-1","callDuration":%q`, value), 1))
			o, err := durationRequest.Observation()
			require.NoError(t, err)
			return o
		}
		a := parse(fmt.Sprintf("PT%d.%09dS", whole, nanos))
		b := parse(fmt.Sprintf("P%dDT%dH%dM%d.%09dS", whole/86400, whole%86400/3600, whole%3600/60, whole%60, nanos))
		want := time.Duration(whole)*time.Second + time.Duration(nanos)
		require.NotNil(t, a.DurationSeconds)
		assert.InDelta(t, want.Seconds(), *a.DurationSeconds, 1e-9)
		assert.Equal(t, a.OccurredAt, b.OccurredAt)
		assert.Equal(t, a.DurationSeconds, b.DurationSeconds)
	})
}

func FuzzTeamsStartedNeverClaimsAttendance(f *testing.F) {
	f.Add("call", "peer", "PT42S")
	f.Add("future", "", "garbage")
	f.Fuzz(func(t *testing.T, eventType, peer, duration string) {
		if !utf8.ValidString(eventType) || !utf8.ValidString(peer) || !utf8.ValidString(duration) {
			return
		} // JSON strings require UTF-8.
		r := teamsRequest(t, false, map[string]any{"eventDetail": map[string]any{
			"@odata.type": "#microsoft.graph.callStartedEventMessageDetail", "callId": "call-1", "callEventType": eventType,
			"callDuration": duration, "callParticipants": []any{map[string]any{"participant": map[string]any{"user": map[string]string{"id": peer}}}},
		}})
		o, err := r.Observation()
		if strings.TrimSpace(eventType) == "" || len(r.Record) > MaxRawBytes {
			require.ErrorIs(t, err, ErrInvalid)
			return
		}
		require.NoError(t, err)
		assert.Empty(t, o.Participants)
		assert.Nil(t, o.DurationSeconds)
		assert.Nil(t, o.EndedAt)
		assert.Equal(t, "unknown", o.Outcome)
		assert.Equal(t, "unknown", o.Direction)
		assert.Equal(t, "unknown", o.OwnerParticipation)
		r.Bindings = map[string]int64{"peer": 7}
		_, err = r.Observation()
		require.ErrorIs(t, err, ErrInvalid)
	})
}

func FuzzTeamsRecordIdentity(f *testing.F) {
	f.Add("room", "reply", "event")
	f.Add("a/b", "", "c:d")
	f.Add(strings.Repeat("r", 256), "reply", "event")
	f.Add("room", strings.Repeat("p", 256), "event")
	f.Add("room", "reply", strings.Repeat("m", 256))
	f.Fuzz(func(t *testing.T, room, reply, message string) {
		assert := assert.New(t)
		require := require.New(t)
		if !validID(room) || !validID(message) || (reply != "" && !validID(reply)) {
			return
		}
		r := teamsRequest(t, true, map[string]any{"chatId": room, "replyToId": reply, "id": message})
		a, err := r.Observation()
		require.NoError(err)
		changes := []struct {
			fields map[string]any
			valid  bool
		}{
			{fields: map[string]any{"chatId": room, "replyToId": reply, "id": message + "x"}, valid: validID(message + "x")},
			{fields: map[string]any{"chatId": room + "x", "replyToId": reply, "id": message}, valid: validID(room + "x")},
			{fields: map[string]any{"chatId": room, "replyToId": reply + "x", "id": message}, valid: validID(reply + "x")},
			{fields: map[string]any{"chatId": nil, "channelIdentity": map[string]string{"teamId": "team", "channelId": room}, "replyToId": reply, "id": message}, valid: true},
		}
		for _, change := range changes {
			other := teamsRequest(t, true, change.fields)
			b, err := other.Observation()
			if !change.valid { // validID enforces the 256-byte ID limit.
				require.ErrorIs(err, ErrInvalid)
				continue
			}
			require.NoError(err)
			assert.NotEqual(a.RecordID, b.RecordID)
			assert.Equal(a.CanonicalKey(), b.CanonicalKey())
		}
	})
}

func FuzzTeamsEndedBindingSet(f *testing.F) {
	f.Add([]byte{1, 2, 1, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		// Bound materialization to the documented 100-binding limit.
		if len(data) > 100 {
			data = data[:100]
		}
		bindings := map[string]int64{}
		roster := []any{}
		expected := map[int64]bool{}
		for i, value := range data {
			id := fmt.Sprintf("native-%d", i)
			participant := int64(value) + 1
			bindings[id] = participant
			expected[participant] = true
			entry := map[string]any{"participant": map[string]any{"user": map[string]string{"id": id}}}
			roster = append([]any{entry, entry}, roster...)
		}
		roster = append(roster, map[string]any{"participant": map[string]any{"application": map[string]string{"id": "application"}}})
		r := teamsRequest(t, true, map[string]any{"eventDetail": map[string]any{
			"@odata.type": "#microsoft.graph.callEndedEventMessageDetail", "callId": "call", "callEventType": "meeting", "callParticipants": roster,
		}})
		r.Bindings = bindings
		o, err := r.Observation()
		require.NoError(t, err)
		got := map[int64]bool{}
		for _, p := range o.Participants {
			assert.Equal(t, "joined", p.Participation)
			got[p.ID] = true
		}
		assert.Equal(t, expected, got)
		assert.Len(t, o.Participants, len(expected))
		assert.Equal(t, "unknown", o.Outcome)
		assert.Equal(t, "unknown", o.OwnerParticipation)
		if len(bindings) < 100 {
			r.Bindings["absent"] = 99
			_, err = r.Observation()
			require.ErrorIs(t, err, ErrInvalid)
		}
	})
}

func TestTeamsDurationAndTimestampBoundaries(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	for _, duration := range []string{"null", `"PT0S"`, `"PT0.0000000009S"`, `"PT9223372036.854775807S"`} {
		r := teamsRequest(t, true, nil)
		r.Record = []byte(strings.Replace(string(r.Record), `"callId":"call-1"`, `"callId":"call-1","callDuration":`+duration, 1))
		o, err := r.Observation()
		require.NoError(err)
		if duration == "null" {
			assert.Nil(o.DurationSeconds)
		} else {
			require.NotNil(o.DurationSeconds)
		}
	}
	for _, stamp := range []string{"2026-01-01T1:00:00Z", "2026-01-01T01:00:00,1Z", "2026-01-01T01:00:00+24:00", "2026-01-01T01:00:00+01:60"} {
		_, err := teamsRequest(t, true, map[string]any{"createdDateTime": stamp}).Observation()
		require.ErrorIs(err, ErrInvalid, stamp)
	}
	first, err := teamsRequest(t, true, map[string]any{"createdDateTime": "2026-10-01T10:00:00Z"}).Observation()
	require.NoError(err)
	second, err := teamsRequest(t, true, map[string]any{"createdDateTime": "2026-10-01T12:00:00+02:00", "lastModifiedDateTime": "2026-12-31T10:00:00Z"}).Observation()
	require.NoError(err)
	assert.Equal(first.OccurredAt, second.OccurredAt)
	assert.Equal(first.RecordID, second.RecordID)
}

func TestTeamsDurationExactLimit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	for _, text := range []string{"PT9223372036.854775807S", "P106751DT23H47M16.854775807S"} {
		got, err := teamsDuration(text)
		require.NoError(err)
		assert.Equal(time.Duration(1<<63-1), got)
	}
	for _, text := range []string{"PT9223372036.854775808S", "P106751DT23H47M16.854775808S", "P106752D"} {
		_, err := teamsDuration(text)
		require.ErrorIs(err, ErrInvalid)
	}
	for _, text := range []string{"P0D", "PT0S", "P0DT0H0M0S"} {
		got, err := teamsDuration(text)
		require.NoError(err)
		assert.Zero(got)
	}
}

func TestTeamsStartedOpaqueExtras(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	r := teamsRequest(t, false, map[string]any{"eventDetail": map[string]any{
		"@odata.type": "#microsoft.graph.callStartedEventMessageDetail", "callId": "call", "callEventType": "future",
		"callDuration": map[string]any{"future": true}, "callParticipants": 42,
	}})
	o, err := r.Observation()
	require.NoError(err)
	assert.Empty(o.Participants)
	assert.Nil(o.DurationSeconds)
	assert.Nil(o.EndedAt)
	_, err = (ImportRequest{Format: "teams", Account: r.Account, Record: []byte(strings.Replace(string(r.Record), "callStartedEventMessageDetail", "callEndedEventMessageDetail", 1))}).Observation()
	require.ErrorIs(err, ErrInvalid)
}

func FuzzTeamsUnrelatedFields(f *testing.F) {
	f.Add("Example", "hello", "future-time")
	f.Fuzz(func(t *testing.T, sender, text, modified string) {
		if !utf8.ValidString(sender) || !utf8.ValidString(text) || !utf8.ValidString(modified) {
			return // Only JSON-encodable strings materialize Graph records.
		}
		r := teamsRequest(t, true, map[string]any{"from": sender, "body": text, "lastModifiedDateTime": modified})
		o, err := r.Observation()
		if len(r.Record) > MaxRawBytes {
			require.ErrorIs(t, err, ErrInvalid)
			return
		}
		require.NoError(t, err)
		assert.Equal(t, "unknown", o.Outcome)
		assert.Equal(t, "unknown", o.Direction)
		assert.Equal(t, "unknown", o.OwnerParticipation)
		assert.Equal(t, time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC), o.OccurredAt)
		assert.Equal(t, string(r.Record), string(o.Raw))
		other := r
		other.Account += "/other"
		b, err := other.Observation()
		require.NoError(t, err)
		assert.NotEqual(t, o.CanonicalKey(), b.CanonicalKey())
		assert.NotEqual(t, o.SourceIdentifier(), b.SourceIdentifier())
		assert.Equal(t, o.RecordID, b.RecordID)
	})
}
