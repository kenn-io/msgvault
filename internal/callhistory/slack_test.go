package callhistory

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const slackHuddleRecord = `{"type":"message","subtype":"huddle_thread","team":"T_EXAMPLE","ts":"1790841600.000001","room":{"id":"R_EXAMPLE","date_start":1790841600,"date_end":1790841660,"participant_history":["U_OWNER","U_PEER","U_PEER"],"participants":["U_OTHER"],"was_accepted":true}}`

func TestSlackHuddleObservation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	r := ImportRequest{Format: "slack_huddle", Account: "T_EXAMPLE:U_OWNER", Record: []byte(slackHuddleRecord), Bindings: map[string]int64{"U_PEER": 7, "U_OTHER": 7}}
	got, err := r.Observation()
	require.NoError(err)
	assert.Equal("slack", got.Provider)
	assert.Equal("R_EXAMPLE", got.RecordID)
	assert.Equal("R_EXAMPLE", got.NativeCallID)
	assert.Equal("call", got.Kind)
	assert.Equal("unknown", got.Direction)
	assert.Equal("unknown", got.Outcome)
	assert.Equal("unknown", got.OwnerParticipation)
	assert.Equal([]Participant{{ID: 7, Participation: "unknown"}}, got.Participants)
	assert.Equal(time.Unix(1790841600, 0).UTC(), got.OccurredAt)
	require.NotNil(got.EndedAt)
	assert.Equal(int64(1790841660), got.EndedAt.Unix())
	assert.Nil(got.DurationSeconds)
	assert.Empty(got.Timezone)
	assert.True(bytes.Equal([]byte(slackHuddleRecord), []byte(got.Raw)), "raw Slack JSON must be preserved byte-for-byte")
	body, err := json.Marshal(r)
	require.NoError(err)
	decoded, err := DecodeImport(bytes.NewReader(body))
	require.NoError(err)
	assert.Equal(got, decoded)
	r.Bindings["U_OTHER"] = 9
	distinct, err := r.Observation()
	require.NoError(err)
	assert.Equal([]Participant{{ID: 7, Participation: "unknown"}, {ID: 9, Participation: "unknown"}}, distinct.Participants)
}

func TestSlackHuddleRejectsInvalidEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, account, raw string
		bindings           map[string]int64
	}{
		{"fractional timestamp", "T:U", `{"type":"message","subtype":"huddle_thread","ts":"1","room":{"id":"R","date_start":1.5}}`, nil},
		{"string timestamp", "T:U", `{"type":"message","subtype":"huddle_thread","ts":"1","room":{"id":"R","date_start":"1"}}`, nil},
		{"empty room identity", "T:U", `{"type":"message","subtype":"huddle_thread","ts":"1","room":{"id":"","date_start":1}}`, nil},
		{"missing room", "T_EXAMPLE:U_OWNER", `{"type":"message","subtype":"huddle_thread","ts":"1"}`, nil},
		{"ordinary message", "T_EXAMPLE:U_OWNER", `{"type":"message","ts":"1","room":{"id":"R","date_start":1}}`, nil},
		{"wrong type", "T_EXAMPLE:U_OWNER", `{"type":"event","subtype":"huddle_thread","ts":"1","room":{"id":"R","date_start":1}}`, nil},
		{"no account owner", "T_EXAMPLE", slackHuddleRecord, nil},
		{"empty owner", "T_EXAMPLE:", slackHuddleRecord, nil},
		{"ambiguous account", "T_EXAMPLE:U_OWNER:extra", slackHuddleRecord, nil},
		{"wrong team", "T_OTHER:U_OWNER", slackHuddleRecord, nil},
		{"outsider binding", "T_EXAMPLE:U_OWNER", slackHuddleRecord, map[string]int64{"U_OUTSIDE": 7}},
		{"owner binding", "T_EXAMPLE:U_OWNER", slackHuddleRecord, map[string]int64{"U_OWNER": 7}},
		{"bad roster", "T_EXAMPLE:U_OWNER", `{"type":"message","subtype":"huddle_thread","ts":"1","room":{"id":"R","date_start":1,"participants":[null]}}`, nil},
		{"duplicate JSON key", "T_EXAMPLE:U_OWNER", `{"type":"message","type":"message","subtype":"huddle_thread","ts":"1","room":{"id":"R","date_start":1}}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := (ImportRequest{Format: "slack_huddle", Account: tc.account, Record: []byte(tc.raw), Bindings: tc.bindings}).Observation()
			require.ErrorIs(t, err, ErrInvalid)
		})
	}
}

// Reordering, duplication and owner/invite flags cannot establish an answer.
func FuzzSlackHuddleAssociation(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, choices []byte) {
		roster := []string{"U_PEER"}
		for _, b := range choices[:min(len(choices), 1000)] {
			roster = append(roster, []string{"U_PEER", "U_OWNER", "U_OTHER"}[b%3])
		}
		flags := byte(0)
		if len(choices) > 0 {
			flags = choices[0]
		}
		raw, err := json.Marshal(map[string]any{"type": "message", "subtype": "huddle_thread", "ts": "1", "user": "U_OWNER", "room": map[string]any{"id": "R", "date_start": 1, "participants": roster, "was_accepted": flags&1 != 0, "was_missed": flags&2 != 0, "was_rejected": flags&4 != 0, "created_by": "U_OWNER"}})
		require.NoError(t, err)
		o, err := (ImportRequest{Format: "slack_huddle", Account: "T:U_OWNER", Record: raw, Bindings: map[string]int64{"U_PEER": 7}}).Observation()
		require.NoError(t, err)
		assert.Equal(t, "unknown", o.Outcome)
		assert.Equal(t, "unknown", o.OwnerParticipation)
		assert.Equal(t, []Participant{{ID: 7, Participation: "unknown"}}, o.Participants)
	})
}

func FuzzSlackHuddleTimestamps(f *testing.F) {
	for _, n := range []int64{math.MinInt64, -1, 0, 1, 1790841600, 253402300799, 253402300800, math.MaxInt64} {
		f.Add(n, n)
	}
	f.Add(int64(10), int64(0))
	f.Add(int64(10), int64(9))
	f.Fuzz(func(t *testing.T, start, end int64) {
		raw := fmt.Sprintf(`{"type":"message","subtype":"huddle_thread","ts":"1","room":{"id":"R","date_start":%d,"date_end":%d}}`, start, end)
		o, err := (ImportRequest{Format: "slack_huddle", Account: "T:U", Record: []byte(raw)}).Observation()
		valid := start > 0 && start <= 253402300799 && (end == 0 || (end >= start && end <= 253402300799))
		if !valid {
			require.ErrorIs(t, err, ErrInvalid)
			return
		}
		require.NoError(t, err)
		assert.Equal(t, start, o.OccurredAt.Unix())
		if end == 0 {
			assert.Nil(t, o.EndedAt)
		} else {
			require.NotNil(t, o.EndedAt)
			assert.Equal(t, end, o.EndedAt.Unix())
		}
		assert.Nil(t, o.DurationSeconds)
	})
}

func FuzzSlackHuddleBindings(f *testing.F) {
	f.Add("U_PEER", int64(7))
	f.Add("U_OWNER", int64(7))
	f.Add("U_OTHER", int64(-1))
	f.Add("outsider", int64(1))
	f.Fuzz(func(t *testing.T, native string, id int64) {
		o, err := (ImportRequest{Format: "slack_huddle", Account: "T_EXAMPLE:U_OWNER", Record: []byte(slackHuddleRecord), Bindings: map[string]int64{native: id}}).Observation()
		if (native != "U_PEER" && native != "U_OTHER") || id <= 0 {
			require.ErrorIs(t, err, ErrInvalid)
			return
		}
		require.NoError(t, err)
		assert.Equal(t, []Participant{{ID: id, Participation: "unknown"}}, o.Participants)
	})
}

func FuzzSlackHuddlePeerSet(f *testing.F) {
	f.Add([]byte{1, 2, 3})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, choices []byte) {
		a, b := []string{"U_OWNER", "U_PEER"}, []string{"U_OTHER"}
		for _, v := range choices[:min(len(choices), 1000)] {
			id := []string{"U_OWNER", "U_PEER", "U_OTHER"}[v%3]
			if v%2 == 0 {
				a = append(a, id)
			} else {
				b = append(b, id)
			}
		}
		if len(choices) > 0 && choices[0]&128 != 0 {
			a, b = b, a
		}
		raw, err := json.Marshal(map[string]any{"type": "message", "subtype": "huddle_thread", "ts": "1", "room": map[string]any{"id": "R", "date_start": 1, "participants": a, "participant_history": b}})
		require.NoError(t, err)
		peers, err := SlackHuddlePeerIDs("T:U_OWNER", raw)
		require.NoError(t, err)
		assert.Equal(t, []string{"U_OTHER", "U_PEER"}, peers)
	})
}

func TestSlackHuddleLimitsAndOptionalEnd(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	for _, end := range []string{"", `,"date_end":null`, `,"date_end":0`} {
		o, err := (ImportRequest{Format: "slack_huddle", Account: "T:U", Record: []byte(`{"type":"message","subtype":"huddle_thread","ts":"1","room":{"id":"R","date_start":1` + end + `}}`)}).Observation()
		require.NoError(err)
		assert.Nil(o.EndedAt)
	}
	for _, n := range []int{100, 101} {
		peers := make([]string, n)
		bindings := map[string]int64{}
		for i := range peers {
			peers[i] = fmt.Sprintf("U%d", i)
			bindings[peers[i]] = 7
		}
		raw, err := json.Marshal(map[string]any{"type": "message", "subtype": "huddle_thread", "ts": "1", "room": map[string]any{"id": "R", "date_start": 1, "participants": peers}})
		require.NoError(err)
		r := ImportRequest{Format: "slack_huddle", Account: "T:OWNER", Record: raw, Bindings: bindings}
		o, err := r.Observation()
		if n == 100 {
			require.NoError(err)
			assert.Equal([]Participant{{ID: 7, Participation: "unknown"}}, o.Participants)
		} else {
			require.ErrorIs(err, ErrInvalid)
		}
		r.Bindings = nil
		_, err = r.Observation()
		require.NoError(err, "unbound room rosters may exceed 100")
	}
}
