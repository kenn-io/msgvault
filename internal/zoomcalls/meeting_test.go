package zoomcalls

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/callhistory"
)

const roster = `{"participants":[
 {"id":"owner","user_id":"1","status":"in_meeting","join_time":"2026-10-01T10:00:00Z","leave_time":"2026-10-01T10:10:00Z"},
 {"id":"peer","user_id":"2","status":"in_meeting","join_time":"2026-10-01T10:05:00Z","leave_time":"2026-10-01T10:15:00Z"}]}`

func TestMeetingAttendance(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	o, err := Meeting("example/owner", "owner", "instance", []byte(roster), map[string]int64{"peer": 42})
	require.NoError(err)
	require.NotNil(o)
	assert.Equal("answered", o.Outcome)
	assert.Equal("zoom_meetings", o.Provider)
	assert.Equal("joined", o.OwnerParticipation)
	assert.Equal("unknown", o.Direction)
	assert.Nil(o.DurationSeconds)
	require.Len(o.Participants, 1)
	assert.Equal(int64(42), o.Participants[0].ID)
	assert.Equal("joined", o.Participants[0].Participation)
}

func TestMeetingRejectsNumericMeetingID(t *testing.T) {
	o, err := Meeting("example/owner", "owner", "1234567890", []byte(roster), nil)
	require.ErrorContains(t, err, "UUID")
	assert.Nil(t, o)
}

func TestMeetingRejectsIncompleteOrInvalidRoster(t *testing.T) {
	for _, raw := range []string{`{"next_page_token":"more","participants":[]}`, `null`, `[]`, `{"participants":[{"id":"peer","join_time":"bad"}]}`} {
		_, err := Meeting("account", "owner", "instance", []byte(raw), nil)
		require.Error(t, err, raw)
	}
}

func TestWaitingRoomWithoutJoinDoesNotBlockAttendance(t *testing.T) {
	raw := []byte(roster[:len(roster)-2] + `,{"id":"waiting","status":"in_waiting_room"}]}`)
	o, err := Meeting("account", "owner", "instance", raw, nil)
	require.NoError(t, err)
	require.NotNil(t, o)
}

func TestOwnerAnonymousAliasCannotBecomePeer(t *testing.T) {
	raw := `{"participants":[{"id":"owner","user_id":"one","status":"in_meeting","join_time":"2026-10-01T10:00:00Z","leave_time":"2026-10-01T10:10:00Z"},{"id":"","user_id":"one","status":"in_meeting","join_time":"2026-10-01T10:00:00Z","leave_time":"2026-10-01T10:10:00Z"}]}`
	o, err := Meeting("account", "owner", "instance", []byte(raw), nil)
	require.Error(t, err)
	assert.Nil(t, o)
}

func TestEveryRosterPageTotalMustBeComplete(t *testing.T) {
	raw := `[{"total_records":1,"next_page_token":"more","participants":[]},` + roster[:len(roster)-1] + `,"total_records":2}]`
	_, err := Meeting("account", "owner", "instance", []byte(raw), nil)
	require.Error(t, err)
}

func TestMeetingNamespacesIncludeOwner(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	a, err := Meeting("account", "owner", "instance", []byte(roster), nil)
	require.NoError(err)
	require.NotNil(a)
	b, err := Meeting("account", "second-owner", "instance", []byte(strings.ReplaceAll(roster, `"owner"`, `"second-owner"`)), nil)
	require.NoError(err)
	require.NotNil(b)
	assert.NotEqual(a.CanonicalKey(), b.CanonicalKey())
}

func TestLargestMeetingFitsNormalizedImport(t *testing.T) {
	require := require.New(t)
	var page struct {
		Participants []map[string]any `json:"participants"`
		Padding      string           `json:"padding"`
	}
	require.NoError(json.Unmarshal([]byte(roster), &page))
	peer := page.Participants[1]
	page.Participants = page.Participants[:1]
	bindings := map[string]int64{}
	for i := 1; i <= 100; i++ {
		row := maps.Clone(peer)
		id := fmt.Sprintf("peer-%d", i)
		row["id"] = id
		delete(row, "user_id")
		page.Participants = append(page.Participants, row)
		bindings[id] = int64(i)
	}
	raw, err := json.Marshal(page)
	require.NoError(err)
	page.Padding = strings.Repeat("x", MaxRosterBytes-len(raw))
	raw, err = json.Marshal(page)
	require.NoError(err)
	o, err := Meeting("account", "owner", "instance", raw, bindings)
	require.NoError(err)
	require.NotNil(o)
	record, err := json.Marshal(o)
	require.NoError(err)
	envelope, err := json.Marshal(callhistory.ImportRequest{Format: "normalized", Account: o.Account, Record: record})
	require.NoError(err)
	_, err = callhistory.DecodeImport(bytes.NewReader(envelope))
	require.NoError(err)
}

func TestMeetingDoesNotArchivePageTokens(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := `[{"participants":[],"next_page_token":"synthetic-page-secret"},` + roster + `]`
	o, err := Meeting("account", "owner", "instance", []byte(raw), nil)
	require.NoError(err)
	require.NotNil(o)
	assert.NotContains(string(o.Raw), "synthetic-page-secret")
	assert.NotContains(string(o.Raw), "next_page_token")
}

func TestReconnectGapIsNotAttendance(t *testing.T) {
	raw := `{"participants":[
{"id":"owner","status":"in_meeting","join_time":"2026-10-01T10:00:00Z","leave_time":"2026-10-01T10:05:00Z"},
{"id":"owner","status":"in_meeting","join_time":"2026-10-01T10:10:00Z","leave_time":"2026-10-01T10:15:00Z"},
{"id":"peer","status":"in_meeting","join_time":"2026-10-01T10:06:00Z","leave_time":"2026-10-01T10:09:00Z"}]}`
	o, err := Meeting("account", "owner", "instance", []byte(raw), nil)
	require.NoError(t, err)
	assert.Nil(t, o)
}

func TestMissingOwnerStillRejectsInvalidBindings(t *testing.T) {
	for _, bindings := range []map[string]int64{{"peer": 0}, {"owner": 1}, {"": 1}} {
		_, err := Meeting("account", "owner", "instance", []byte(`{"participants":[]}`), bindings)
		require.Error(t, err)
	}
}

func FuzzMeetingPermutationAndDuplicates(f *testing.F) {
	f.Add(uint64(0), false)
	f.Add(uint64(1), true)
	f.Fuzz(func(t *testing.T, seed uint64, duplicate bool) {
		var page struct {
			Participants []jsontext.Value `json:"participants"`
		}
		require.NoError(t, json.Unmarshal([]byte(roster), &page))
		if seed%2 != 0 {
			page.Participants[0], page.Participants[1] = page.Participants[1], page.Participants[0]
		}
		if duplicate {
			page.Participants = append(page.Participants, page.Participants[seed%2])
		}
		raw, err := json.Marshal(page)
		require.NoError(t, err)
		o, err := Meeting("account", "owner", "instance", raw, map[string]int64{"peer": 42})
		require.NoError(t, err)
		require.NotNil(t, o)
		assert.Equal(t, "answered", o.Outcome)
		require.Len(t, o.Participants, 1)
		assert.Equal(t, int64(42), o.Participants[0].ID)
	})
}

func FuzzMeetingParserDoesNotPanic(f *testing.F) {
	f.Add([]byte(roster))
	f.Add([]byte(`{"participants":[]}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		_, _ = Meeting("account", "owner", "instance", raw, nil)
	})
}

func FuzzMeetingRequiresActualOverlap(f *testing.F) {
	f.Add(int32(0), uint32(10), int32(5), uint32(10), true)
	f.Add(int32(0), uint32(10), int32(10), uint32(10), true)
	f.Add(int32(0), uint32(10), int32(5), uint32(10), false)
	f.Fuzz(func(t *testing.T, a int32, la uint32, b int32, lb uint32, owner bool) {
		base := int64(1790841600)
		row := func(id string, start int32, length uint32) map[string]any {
			return map[string]any{"id": id, "status": "in_meeting", "join_time": time.Unix(base+int64(start), 0).UTC(), "leave_time": time.Unix(base+int64(start)+int64(length), 0).UTC()}
		}
		rows := []map[string]any{row("peer", b, lb)}
		if owner {
			rows = append(rows, row("owner", a, la))
		}
		raw, err := json.Marshal(map[string]any{"participants": rows})
		require.NoError(t, err)
		o, err := Meeting("account", "owner", "instance", raw, nil)
		require.NoError(t, err)
		want := owner && la > 0 && lb > 0 && int64(a) < int64(b)+int64(lb) && int64(b) < int64(a)+int64(la)
		assert.Equal(t, want, o != nil)
	})
}
