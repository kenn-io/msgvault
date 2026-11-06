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
				assertions.Empty(got.Timezone, "epoch milliseconds do not identify the source zone")
				assertions.JSONEq(raw, string(got.Raw))
			}
			if tc.peer == "" {
				assertions.Empty(got.Participants, "a group peer is not a joined roster")
			} else {
				requirements.Equal([]Participant{{ID: 7, Participation: tc.peer}}, got.Participants)
			}
		})
	}
}

func TestNormalizedImportRejectsDifferentNativeCallID(t *testing.T) {
	_, err := (ImportRequest{Format: "normalized", Account: "example",
		Record: []byte(`{"version":1,"provider":"signal","account":"example","record_id":"a","native_call_id":"n1","kind":"call","direction":"incoming","outcome":"answered","owner_participation":"joined","occurred_at":"2026-10-01T12:00:00Z","precision":"instant","participants":[{"participant_id":1,"participation":"joined"}],"raw":{}}`),
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
	got, err := (ImportRequest{Format: "normalized", Account: o.Account, Record: record}).Observation()
	requirements.NoError(err)
	o.OccurredAt = time.Date(2026, 10, 1, 16, 0, 0, 0, time.UTC)
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

func TestImportRejectsInvalidAccountAndBinding(t *testing.T) {
	for _, tc := range []struct {
		name, account, peer string
		id                  int64
	}{
		{"blank account", " ", "peer", 1},
		{"zero participant", "example", "peer", 0},
		{"blank native peer", "example", " ", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := (ImportRequest{
				Format: "signal_desktop", Account: tc.account,
				Record:   []byte(fmt.Sprintf(`{"callId":"x","peerId":%q,"mode":"Direct","status":"Accepted","timestamp":0}`, tc.peer)),
				Bindings: map[string]int64{tc.peer: tc.id},
			}).Observation()
			require.ErrorIs(t, err, ErrInvalid)
		})
	}
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
