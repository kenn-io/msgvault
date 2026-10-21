package callhistory

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func FuzzDiscordIdentityAgreement(f *testing.F) {
	f.Add(uint64(1), uint64(2), uint32(0))
	f.Add(^uint64(0), uint64(42), ^uint32(0))
	f.Fuzz(func(t *testing.T, message, owner uint64, seconds uint32) {
		if message == 0 {
			message = 1
		}
		if owner == 0 {
			owner = 1
		}
		id, account := strconv.FormatUint(message, 10), strconv.FormatUint(owner, 10)
		at := time.Unix(int64(seconds), 0).UTC()
		var observations []Observation
		for _, format := range []string{"discord", "discord_chat_exporter"} {
			kind := any(3)
			if format == "discord_chat_exporter" {
				kind = "Call"
			}
			raw, err := json.Marshal(map[string]any{"id": id, "channel_id": "100", "type": kind, "timestamp": at.Format(time.RFC3339Nano), "future": true})
			require.NoError(t, err)
			wire, err := json.Marshal(ImportRequest{Format: format, Account: account, Record: raw})
			require.NoError(t, err)
			o, err := DecodeImport(bytes.NewReader(wire))
			require.NoError(t, err)
			assert.Equal(t, id, o.NativeCallID)
			assert.True(t, at.Equal(o.OccurredAt))
			assert.Equal(t, string(raw), string(o.Raw))
			observations = append(observations, o)
		}
		assert.Equal(t, observations[0].CanonicalKey(), observations[1].CanonicalKey())
		assert.NotEqual(t, observations[0].RecordID, observations[1].RecordID)
	})
}

func FuzzDiscordNeverInventsEvidence(f *testing.F) {
	f.Add(uint64(2), true, true)
	f.Add(^uint64(0), false, false)
	f.Fuzz(func(t *testing.T, nativePeer uint64, ownerJoined, ended bool) {
		if nativePeer < 2 {
			nativePeer = 2
		}
		peer := strconv.FormatUint(nativePeer, 10)
		roster := []string{peer}
		if ownerJoined {
			roster = append(roster, "1")
		}
		call := map[string]any{"participants": roster}
		if ended {
			call["ended_timestamp"] = "2026-10-01T00:01:00Z"
		}
		raw, err := json.Marshal(map[string]any{"id": "100", "channel_id": "200", "type": 3, "timestamp": "2026-10-01T00:00:00Z", "call": call, "author": map[string]string{"id": "1"}, "content": "missed call"})
		require.NoError(t, err)
		o, err := (ImportRequest{Format: "discord", Account: "1", Record: raw, Bindings: map[string]int64{peer: 42}}).Observation()
		require.NoError(t, err)
		assert.Equal(t, "unknown", o.Direction)
		assert.Equal(t, "unknown", o.Outcome)
		assert.Nil(t, o.DurationSeconds)
		wantOwner := "unknown"
		if ownerJoined {
			wantOwner = "joined"
		}
		assert.Equal(t, wantOwner, o.OwnerParticipation)
		assert.Equal(t, []Participant{{ID: 42, Participation: "joined"}}, o.Participants)
	})
}

func FuzzDiscordRejectsNonCalls(f *testing.F) {
	f.Add(int32(0))
	f.Add(int32(-1))
	f.Add(int32(999))
	f.Fuzz(func(t *testing.T, kind int32) {
		if kind == 3 {
			return
		}
		raw := fmt.Sprintf(`{"id":"100","channel_id":"200","type":%d,"timestamp":"2026-10-01T00:00:00Z","call":{"participants":["1","2"]}}`, kind)
		_, err := (ImportRequest{Format: "discord", Account: "1", Record: []byte(raw)}).Observation()
		require.ErrorIs(t, err, ErrInvalid)
	})
}

func TestDiscordRosterAndValidation(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	const raw = `{"id":"100","channel_id":"200","type":3,"timestamp":"2026-10-01T00:00:00Z","call":{"participants":["1","2","2","3","4"],"ended_timestamp":"2026-10-01T00:01:00Z"}}`
	req := ImportRequest{Format: "discord", Account: "1", Record: []byte(raw), Bindings: map[string]int64{"2": 43, "3": 42, "4": 42}}
	o, err := req.Observation()
	requirements.NoError(err)
	checks.Equal("native/100", o.RecordID)
	checks.Equal([]Participant{{ID: 42, Participation: "joined"}, {ID: 43, Participation: "joined"}}, o.Participants)
	requirements.NotNil(o.EndedAt)
	checks.Equal(time.Date(2026, 10, 1, 0, 1, 0, 0, time.UTC), *o.EndedAt)
	for _, call := range []string{"", `,"call":null`, `,"call":{}`, `,"call":{"participants":null}`, `,"call":{"participants":[]}`} {
		req.Record = []byte(`{"id":"100","channel_id":"200","type":3,"timestamp":"2026-10-01T00:00:00Z"` + call + `}`)
		req.Bindings = nil
		got, err := req.Observation()
		requirements.NoError(err)
		checks.Equal("unknown", got.OwnerParticipation)
		checks.Empty(got.Participants)
	}
	for _, tc := range []struct{ name, old, replacement string }{
		{"leading zero message", "\"100\"", "\"0100\""},
		{"zero message", "\"100\"", "\"0\""},
		{"overflow message", "\"100\"", "\"18446744073709551616\""},
		{"missing channel", "\"200\"", "\"\""},
		{"bad timestamp", "2026-10-01T00:00:00Z", "invalid"},
		{"reversed end", "2026-10-01T00:01:00Z", "2026-09-30T23:59:00Z"},
		{"invalid roster ID", "\"2\"", "\"02\""},
		{"null roster member", "\"2\"", "null"},
		{"roster type", "[\"1\",\"2\",\"2\",\"3\",\"4\"]", "{}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			_, err := (ImportRequest{Format: "discord", Account: "1", Record: []byte(strings.ReplaceAll(raw, tc.old, tc.replacement))}).Observation()
			requirements.ErrorIs(err, ErrInvalid)
		})
	}
	for _, bindings := range []map[string]int64{{"1": 42}, {"5": 42}} {
		req.Record = []byte(raw)
		req.Bindings = bindings
		_, err := req.Observation()
		requirements.ErrorIs(err, ErrInvalid)
	}
	req.Bindings = nil
	for _, account := range []string{"01", "0", "-1", " 1", "18446744073709551616"} {
		req.Account = account
		_, err := req.Observation()
		requirements.ErrorIs(err, ErrInvalid)
	}
	req.Account = "1"
	req.Record = []byte(strings.Replace(raw, `["1","2","2","3","4"]`, `[`+strings.Repeat(`"2",`, 100)+`"2"]`, 1))
	_, err = req.Observation()
	requirements.ErrorIs(err, ErrInvalid)
}

func TestDiscordExporterHasNoAttendance(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte(`{"id":"100","type":"Call","timestamp":"2026-10-01T00:00:00Z","callEndedTimestamp":"2026-10-01T00:01:00Z","author":{"id":"1"},"mentions":[{"id":"2"}],"content":"Started a call"}`)
	req := ImportRequest{Format: "discord_chat_exporter", Account: "1", Record: raw}
	o, err := req.Observation()
	require.NoError(err)
	assert.Equal("exporter/100", o.RecordID)
	assert.Equal("unknown", o.OwnerParticipation)
	assert.Equal("unknown", o.Outcome)
	assert.Nil(o.DurationSeconds)
	assert.Empty(o.Participants)
	require.NotNil(o.EndedAt)
	req.Bindings = map[string]int64{"2": 42}
	_, err = req.Observation()
	require.ErrorIs(err, ErrInvalid)
	req.Bindings = nil
	req.Record = []byte(`{"ID":"100","Timestamp":"2026-10-01T00:00:00Z","Contents":"Started a call"}`)
	_, err = req.Observation()
	require.ErrorIs(err, ErrInvalid)
}

func TestDiscordArchiveReferenceValidation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	for _, record := range []string{`{"message_id":1}`, `{"message_id":9223372036854775807}`} {
		wire := `{"format":"discord_archived","account":"1","record":` + record + `}`
		req, err := DecodeImportRequest(strings.NewReader(wire))
		require.NoError(err)
		id, err := req.DiscordArchiveID()
		require.NoError(err)
		assert.Positive(id)
	}
	for _, record := range []string{`{}`, `null`, `{"message_id":0}`, `{"message_id":-1}`, `{"message_id":"1"}`, `{"message_id":1.5}`, `{"message_id":9223372036854775808}`, `{"message_id":1,"path":"example"}`} {
		_, err := DecodeImportRequest(strings.NewReader(`{"format":"discord_archived","account":"1","record":` + record + `}`))
		require.ErrorIs(err, ErrInvalid)
	}
	_, err := DecodeImportRequest(strings.NewReader(`{"format":"discord_archived","account":"01","record":{"message_id":1}}`))
	require.ErrorIs(err, ErrInvalid)
	_, err = DecodeImportRequest(strings.NewReader(`{"format":"discord","account":"1","record":{}}`))
	require.ErrorIs(err, ErrInvalid, "supplied records still validate locally")
}

func FuzzDiscordIgnoresOtherRepresentationFields(f *testing.F) {
	f.Add("not a timestamp", false)
	f.Add("unused native field", true)
	f.Fuzz(func(t *testing.T, extra string, channel bool) {
		if len(extra) > MaxRawBytes/2 {
			return
		} // materialized record must fit the public bound.
		for _, format := range []string{"discord", "discord_chat_exporter"} {
			kind := any(3)
			unused := "callEndedTimestamp"
			if format == "discord_chat_exporter" {
				kind = "Call"
				unused = "call"
				if channel {
					unused = "channel_id"
				}
			}
			raw, err := json.Marshal(map[string]any{"id": "100", "channel_id": "200", "type": kind, "timestamp": "2026-10-01T00:00:00Z", unused: []byte(extra)})
			require.NoError(t, err)
			o, err := (ImportRequest{Format: format, Account: "1", Record: raw}).Observation()
			require.NoError(t, err)
			assert.Equal(t, string(raw), string(o.Raw))
		}
	})
}

func TestDiscordRecordSizeBoundary(t *testing.T) {
	for _, format := range []string{"discord", "discord_chat_exporter"} {
		kind := `3`
		if format == "discord_chat_exporter" {
			kind = `"Call"`
		}
		base := `{"id":"100","channel_id":"200","type":` + kind + `,"timestamp":"2026-10-01T00:00:00Z","extra":""}`
		for _, over := range []int{0, 1} {
			raw := strings.Replace(base, `"extra":""`, `"extra":"`+strings.Repeat("x", MaxRawBytes-len(base)+over)+`"`, 1)
			req := ImportRequest{Format: format, Account: "1", Record: []byte(raw)}
			_, err := req.Observation()
			if over == 0 {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, ErrImportTooLarge)
			}
		}
	}
}
