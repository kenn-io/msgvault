package callhistory

import (
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"

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

func TestNormalizedCancellationRequiresTime(t *testing.T) {
	_, err := (ImportRequest{Format: "normalized", Account: "example",
		Record: []byte(`{"version":1,"provider":"signal","account":"example","record_id":"call","kind":"call","direction":"unknown","outcome":"cancelled","owner_participation":"unknown","precision":"instant","raw":{}}`),
	}).Observation()
	require.ErrorIs(t, err, ErrInvalid)
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
}

func TestImportRequestBoundsAndAccountMismatch(t *testing.T) {
	requirements := require.New(t)
	_, err := DecodeImport(strings.NewReader(strings.Repeat(" ", MaxRequestBytes+1)))
	requirements.ErrorIs(err, ErrImportTooLarge)
	o := exampleObservation()
	record, err := json.Marshal(o)
	requirements.NoError(err)
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

func TestSignalDeletedDropsEnd(t *testing.T) {
	o, err := (ImportRequest{Format: "signal_desktop", Account: "a", Record: []byte(`{"callId":"7","mode":"Direct","status":"Deleted","timestamp":1790841720000,"endedTimestamp":1790841660000}`)}).Observation()
	require.NoError(t, err)
	assert.True(t, o.Withdrawn)
	assert.Nil(t, o.EndedAt)
}
