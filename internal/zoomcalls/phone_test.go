package zoomcalls

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const syntheticPhoneElement = `{"call_element_id":"element","call_id":"call","call_history_uuid":"history","direction":"inbound","event":"incoming","result":"answered","caller_ext_id":"peer","callee_ext_id":"owner","start_time":"2026-10-01T10:00:00Z","answer_time":"2026-10-01T10:00:05Z","end_time":"2026-10-01T10:01:00Z","talk_time":55}`

func TestPhoneConnectedOwner(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	o, err := PhoneObservation("account/owner", "owner", []byte(syntheticPhoneElement), map[string]int64{"ext/peer": 42})
	require.NoError(err)
	assert.Equal("element", o.RecordID)
	assert.Equal("call", o.NativeCallID)
	assert.Equal("zoom_phone", o.Provider)
	assert.Equal("answered", o.Outcome)
	assert.Equal("incoming", o.Direction)
	require.NotNil(o.DurationSeconds)
	assert.Equal(float64(55), *o.DurationSeconds) //nolint:testifylint // Integer source seconds must match exactly; a tolerance hides errors.
	require.Len(o.Participants, 1)
	assert.Equal("joined", o.Participants[0].Participation)
}

func TestPhoneMissingAnswerRemainsUnknown(t *testing.T) {
	raw := strings.ReplaceAll(strings.ReplaceAll(syntheticPhoneElement, `"2026-10-01T10:00:05Z"`, `""`), `"answered"`, `"no_answer"`)
	o, err := PhoneObservation("account", "owner", []byte(raw), nil)
	require.NoError(t, err)
	assert.Equal(t, "unknown", o.OwnerParticipation)
	assert.Nil(t, o.DurationSeconds)
}

func TestPhoneHistoryUUIDDoesNotOverwriteElements(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	a, err := PhoneObservation("account", "owner", []byte(syntheticPhoneElement), nil)
	require.NoError(err)
	b, err := PhoneObservation("account", "owner", []byte(strings.Replace(syntheticPhoneElement, `"element"`, `"second-element"`, 1)), nil)
	require.NoError(err)
	assert.NotEqual(a.RecordID, b.RecordID)
	assert.Equal(a.CanonicalKey(), b.CanonicalKey())
}

func TestPhoneOtherHandlerOrOwnerCannotAnswer(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	for _, raw := range []string{strings.Replace(syntheticPhoneElement, `"result":"answered"`, `"result":"answered","handler_ext_id":"other-handler"`, 1), strings.ReplaceAll(syntheticPhoneElement, `"owner"`, `"other-owner"`)} {
		o, err := PhoneObservation("account", "owner", []byte(raw), map[string]int64{"ext/peer": 42})
		require.NoError(err)
		assert.Equal("unknown", o.OwnerParticipation)
		assert.Nil(o.DurationSeconds)
		require.Len(o.Participants, 1)
		assert.Equal("unknown", o.Participants[0].Participation)
	}
}

func TestPhoneOptionalTimeForms(t *testing.T) {
	for _, value := range []string{`null`, `""`} {
		raw := strings.Replace(syntheticPhoneElement, `"2026-10-01T10:00:05Z"`, value, 1)
		o, err := PhoneObservation("account", "owner", []byte(raw), nil)
		require.NoError(t, err)
		assert.Equal(t, "unknown", o.Outcome)
	}
	_, err := PhoneObservation("account", "owner", []byte(strings.Replace(syntheticPhoneElement, `"2026-10-01T10:00:05Z"`, `"invalid"`, 1)), nil)
	require.Error(t, err)
}

func FuzzPhoneParserDoesNotPanic(f *testing.F) {
	f.Add([]byte(syntheticPhoneElement))
	f.Add([]byte(`{}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		_, _ = PhoneObservation("account", "owner", raw, nil)
	})
}

func FuzzPhoneNamespacesIncludeOwner(f *testing.F) {
	f.Add(uint64(0))
	f.Add(uint64(1))
	f.Fuzz(func(t *testing.T, seed uint64) {
		owner := fmt.Sprintf("owner-%x", seed)
		a, err := PhoneObservation("account", "owner", []byte(syntheticPhoneElement), nil)
		require.NoError(t, err)
		b, err := PhoneObservation("account", owner, []byte(strings.ReplaceAll(syntheticPhoneElement, `"owner"`, strconv.Quote(owner))), nil)
		require.NoError(t, err)
		assert.NotEqual(t, a.CanonicalKey(), b.CanonicalKey())
	})
}

func FuzzPhoneUnknownCannotAnswer(f *testing.F) {
	f.Add("future-state", "")
	f.Add("answered", "answered_by_other")
	f.Fuzz(func(t *testing.T, result, reason string) {
		var row map[string]jsontext.Value
		require.NoError(t, json.Unmarshal([]byte(syntheticPhoneElement), &row))
		value, err := json.Marshal(result, jsontext.AllowInvalidUTF8(true))
		require.NoError(t, err)
		row["result"] = value
		value, err = json.Marshal(reason, jsontext.AllowInvalidUTF8(true))
		require.NoError(t, err)
		row["result_reason"] = value
		raw, err := json.Marshal(row)
		require.NoError(t, err)
		o, err := PhoneObservation("account", "owner", raw, nil)
		require.NoError(t, err)
		if reason != "" || (result != "answered" && result != "connected") {
			assert.Equal(t, "unknown", o.Outcome)
			assert.Nil(t, o.DurationSeconds)
		}
	})
}

func TestPhoneFetchModernElements(t *testing.T) {
	day := time.Now().UTC().AddDate(0, 0, -2).Format(time.DateOnly)
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/phone/users/me/call_history", r.URL.Path)
		_, _ = fmt.Fprintf(w, `{"call_elements":[%s]}`, strings.ReplaceAll(syntheticPhoneElement, "2026-10-01", day))
	})
	items, err := c.Phone(t.Context(), day, time.Now().UTC().Format(time.DateOnly))
	require.NoError(t, err)
	require.Len(t, items, 1)
}

func TestPhoneStagingBoundAppliesAcrossMonths(t *testing.T) {
	now := time.Now().UTC()
	first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	padding := strings.Repeat("x", 510<<10)
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		i, _ := strconv.Atoi(r.URL.Query().Get("next_page_token"))
		next := ""
		if i < 33 {
			next = strconv.Itoa(i + 1)
		}
		day := first.AddDate(0, 0, -1).Format(time.DateOnly)
		if r.URL.Query().Get("from") == first.Format(time.DateOnly) {
			day = first.Format(time.DateOnly)
		}
		row := strings.ReplaceAll(syntheticPhoneElement, "2026-10-01", day)
		row = row[:len(row)-1] + `,"padding":"` + padding + `"}`
		_, _ = fmt.Fprintf(w, `{"next_page_token":%q,"call_elements":[%s]}`, next, row)
	})
	_, err := c.Phone(t.Context(), first.AddDate(0, 0, -1).Format(time.DateOnly), first.AddDate(0, 0, 1).Format(time.DateOnly))
	require.ErrorContains(t, err, "32 MiB")
}
