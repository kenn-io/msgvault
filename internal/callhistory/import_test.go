package callhistory

import (
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
	}{
		{"Direct", "Accepted", "answered", "joined", "joined"},
		{"Direct", "Missed", "missed", "not_joined", "unknown"},
		{"Direct", "MissedNotificationProfile", "missed", "not_joined", "unknown"},
		{"Direct", "Declined", "declined", "not_joined", "unknown"},
		{"Direct", "Pending", "unknown", "unknown", "unknown"},
		{"Direct", "Deleted", "", "", ""},
		{"Direct", "FutureStatus", "unknown", "unknown", "unknown"},
		{"Group", "Accepted", "unknown", "unknown", ""},
		{"Group", "Joined", "answered", "joined", ""},
		{"Group", "Ringing", "unknown", "unknown", ""},
		{"Group", "GenericGroupCall", "unknown", "unknown", ""},
		{"Group", "OutgoingRing", "unknown", "unknown", ""},
		{"Group", "Missed", "missed", "not_joined", ""},
		{"Group", "Declined", "declined", "not_joined", ""},
		{"Group", "Deleted", "", "", ""},
		{"Adhoc", "JoinedAdhoc", "answered", "joined", ""},
		{"Adhoc", "GenericAdhocCall", "unknown", "unknown", ""},
		{"Adhoc", "Pending", "unknown", "unknown", ""},
		{"Adhoc", "Deleted", "", "", ""},
		{"Adhoc", "Unknown", "unknown", "unknown", ""},
		{"FutureMode", "Accepted", "unknown", "unknown", ""},
		{"", "Deleted", "", "", ""},
		{"FutureMode", "Deleted", "", "", ""},
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
			if tc.status == "Deleted" {
				requirements.ErrorIs(err, ErrInvalid)
				return
			}
			requirements.NoError(err)
			assertions.Equal(tc.outcome, got.Outcome)
			assertions.Equal(tc.owner, got.OwnerParticipation)
			if tc.mode == "Direct" && tc.status == "Accepted" {
				assertions.Equal("incoming", got.Direction)
				assertions.Nil(got.DurationSeconds, "end minus start is not talk duration")
				assertions.JSONEq(raw, string(got.Raw))
			}
			if tc.peer != "" {
				requirements.Equal([]Participant{{ID: 7, Participation: tc.peer}}, got.Participants)
			}
		})
	}

	t.Run("outgoing", func(t *testing.T) {
		got, err := (ImportRequest{
			Format: "signal_desktop", Account: "example",
			Record: []byte(`{"callId":"x","peerId":"peer","mode":"Direct","direction":"Outgoing","status":"Accepted","timestamp":1790841600000}`),
		}).Observation()
		require.NoError(t, err)
		assert.Equal(t, "outgoing", got.Direction)
	})
}

func TestNormalizedImportValidatesObservation(t *testing.T) {
	for _, field := range []string{"occurred_at", "ended_at"} {
		t.Run(field+" exceeds UTC year range", func(t *testing.T) {
			require := require.New(t)
			o := exampleObservation()
			at, err := time.Parse(time.RFC3339, "9999-12-31T23:30:00-01:00")
			require.NoError(err)
			if field == "occurred_at" {
				o.OccurredAt = at
			} else {
				o.EndedAt = &at
			}
			record, err := json.Marshal(o)
			require.NoError(err)
			_, err = (ImportRequest{Format: "normalized", Account: o.Account, Record: record}).Observation()
			require.ErrorIs(err, ErrInvalid)
			assert.ErrorContains(t, err, "timestamps")
		})
	}

	o := exampleObservation()
	o.OccurredAt = time.Date(2026, 10, 1, 12, 0, 0, 900, time.UTC)
	o.EndedAt = new(time.Date(2026, 10, 1, 12, 0, 0, 100, time.UTC))
	record, err := json.Marshal(o)
	require.NoError(t, err)
	_, err = (ImportRequest{Format: "normalized", Account: o.Account, Record: record}).Observation()
	require.ErrorIs(t, err, ErrInvalid, "reject an end before the start within the same microsecond")
}

func TestImportMissingAndUnsupportedFields(t *testing.T) {
	requirements := require.New(t)
	for _, tc := range []struct {
		format, raw string
	}{
		{"signal_desktop", `{"callId":"x","timestamp":0}`},
		{"signal_desktop", `{"callId":"x","timestamp":null}`},
		{"signal_desktop", `{"callId":"x"}`},
		{"signal_desktop", `{"callId":"x","timestamp":"1790841600000"}`},
		{"unsupported", `{}`},
	} {
		_, err := (ImportRequest{Format: tc.format, Account: "example", Record: []byte(tc.raw)}).Observation()
		requirements.ErrorIs(err, ErrInvalid)
	}
}

func TestImportRequestBoundsAccountMismatchAndTimestampNormalization(t *testing.T) {
	requirements := require.New(t)
	_, err := DecodeImport(strings.NewReader(strings.Repeat(" ", MaxRequestBytes+1)))
	requirements.ErrorIs(err, ErrImportTooLarge)
	o := exampleObservation()
	o.OccurredAt = time.Date(2026, 10, 1, 9, 0, 0, 123456789, time.FixedZone("", -7*3600))
	o.EndedAt = new(time.Date(2026, 10, 1, 9, 1, 0, 987654321, time.FixedZone("", -7*3600)))
	record, err := json.Marshal(o)
	requirements.NoError(err)
	got, err := (ImportRequest{Format: "normalized", Account: o.Account, Record: record}).Observation()
	requirements.NoError(err)
	o.OccurredAt = time.Date(2026, 10, 1, 16, 0, 0, 123456000, time.UTC)
	o.EndedAt = new(time.Date(2026, 10, 1, 16, 1, 0, 987654000, time.UTC))
	assert.Equal(t, o, got)
	_, err = (ImportRequest{Format: "normalized", Account: "different", Record: record}).Observation()
	requirements.ErrorIs(err, ErrInvalid)
	assert.ErrorContains(t, err, "account mismatch")
}

func TestSignalRequiresExactDirectPeerBinding(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	r := ImportRequest{
		Format: "signal_desktop", Account: "example",
		Record: []byte(`{"callId":"x","peerId":"peer","mode":"Direct","direction":"Outgoing","status":"Accepted","timestamp":1790841600000}`),
	}
	o, err := r.Observation()
	requirements.NoError(err)
	assertions.Empty(o.Participants, "missing explicit mapping does not guess a person")
	r.Bindings = map[string]int64{"another-peer": 1}
	_, err = r.Observation()
	requirements.ErrorIs(err, ErrInvalid)
	r.Record = []byte(`{"callId":"x","peerId":"peer","mode":"Group","status":"Joined","timestamp":1790841600000}`)
	r.Bindings = map[string]int64{"peer": 1}
	_, err = r.Observation()
	requirements.ErrorIs(err, ErrInvalid)
}

func TestSignalImportRejectsBlankNativePeer(t *testing.T) {
	_, err := (ImportRequest{
		Format: "signal_desktop", Account: "example",
		Record:   []byte(`{"callId":"x","peerId":" ","mode":"Direct","status":"Accepted","timestamp":1790841600000}`),
		Bindings: map[string]int64{" ": 1},
	}).Observation()
	require.ErrorIs(t, err, ErrInvalid)
}
