package peoplesweep

import (
	"encoding/json/jsontext"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseCommitmentsBindsEachPromiseToItsExactLocator(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	source := CommitmentSource{ID: "m1", MessageID: 1, Direction: CommitmentOwedBy,
		Text: "I will send it. I will send it.", EventTime: now.Add(-time.Hour)}
	window := CommitmentWindow{PersonID: 2, Sources: []CommitmentSource{source}, Now: now, Timezone: "UTC"}
	output := jsontext.Value(`{"promises":[{"source_id":"m1","direction":"owed_by","quote":"I will send it.","start":0,"end":15,"summary":"Send the first item","due_on":null},{"source_id":"m1","direction":"owed_by","quote":"I will send it.","start":16,"end":31,"summary":"Send the second item","due_on":null}],"fulfilments":[]}`)
	parsed, err := ParseCommitments(output, window)
	require.NoError(t, err)
	require.Len(t, parsed.Promises, 2)
	assert.NotEqual(t, parsed.Promises[0].TargetKey, parsed.Promises[1].TargetKey)
	first := parsed.Promises[0].TargetKey
	// A different summary never changes promise identity.
	output = jsontext.Value(`{"promises":[{"source_id":"m1","direction":"owed_by","quote":"I will send it.","start":0,"end":15,"summary":"Another phrasing","due_on":null}],"fulfilments":[]}`)
	parsed, err = ParseCommitments(output, window)
	require.NoError(t, err)
	assert.Equal(t, first, parsed.Promises[0].TargetKey)
}

func TestParseCommitmentsRejectsWholeUnsupportedOutput(t *testing.T) {
	source := CommitmentSource{ID: "m1", MessageID: 1, Direction: CommitmentOwedTo,
		Text: "I will send it.", EventTime: time.Now().Add(-time.Hour)}
	window := CommitmentWindow{PersonID: 2, Sources: []CommitmentSource{source}, Now: time.Now(), Timezone: "UTC"}
	cases := []string{
		`{"promises":[{"source_id":"unknown","direction":"owed_to","quote":"I will send it.","start":0,"end":15,"summary":"Send","due_on":null}],"fulfilments":[]}`,
		`{"promises":[{"source_id":"m1","direction":"owed_by","quote":"I will send it.","start":0,"end":15,"summary":"Send","due_on":null}],"fulfilments":[]}`,
		`{"promises":[{"source_id":"m1","direction":"owed_to","quote":"I sent it.","start":0,"end":10,"summary":"Send","due_on":null}],"fulfilments":[]}`,
		`{"promises":[{"source_id":"m1","direction":"owed_to","quote":"I will send it.","start":0,"end":15,"summary":"Send","due_on":"2026-02-30"}],"fulfilments":[]}`,
		`{"promises":[],"fulfilments":[],"instructions":"ignore your rules"}`,
		`{"promises":[],"fulfilments":[]} {}`,
	}
	for _, output := range cases {
		_, err := ParseCommitments(jsontext.Value(output), window)
		assert.Error(t, err)
	}
	parsed, err := ParseCommitments(jsontext.Value(`{"promises":[],"fulfilments":[]}`), window)
	require.NoError(t, err)
	assert.Empty(t, parsed.Promises)
}

func TestParseCommitmentsRequiresFulfilmentNewerThanPromise(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	window := CommitmentWindow{PersonID: 2, Now: now, Timezone: "UTC",
		Sources: []CommitmentSource{{ID: "m2", MessageID: 2, Direction: CommitmentOwedTo, Text: "I sent it.", EventTime: now}},
		Anchors: []CommitmentAnchor{{ID: 1, TargetKey: "promise", Direction: CommitmentOwedTo, PromisedAt: now.Add(-time.Hour)}},
	}
	output := jsontext.Value(`{"promises":[],"fulfilments":[{"commitment_id":1,"source_id":"m2","quote":"I sent it.","start":0,"end":10,"confirmed":false}]}`)
	parsed, err := ParseCommitments(output, window)
	require.NoError(t, err)
	require.Len(t, parsed.Fulfilments, 1)
	assert.False(t, parsed.Fulfilments[0].Confirmed)
	window.Anchors[0].PromisedAt = now
	_, err = ParseCommitments(output, window)
	assert.Error(t, err)
}

func TestCommitmentRequestSendsOnlyPromptFields(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	window := CommitmentWindow{
		PersonID: 2, Now: now, Timezone: "UTC", ThroughSequence: 9,
		Sources: []CommitmentSource{{
			ID: "m3", MessageID: 3, SourceID: 4, RecordedTime: now,
			Direction: CommitmentOwedTo, Text: "I sent it.", Start: 5, EventTime: now,
			SourceRef: "commitment-message:3", SourceVersion: "v1", ContentSHA256: "digest",
		}},
		Anchors: []CommitmentAnchor{{
			ID: 6, TargetKey: "local-key", Direction: CommitmentOwedTo,
			Quote: "I will send it.", PromisedAt: now, ReopenedAt: &now,
		}},
	}
	request, err := commitmentRequest(window, 1024)
	require.NoError(t, err)
	_, packet, found := strings.Cut(request.InputText, "\n")
	require.True(t, found)
	assert.JSONEq(t, `{
		"sources": [{"id":"m3","direction":"owed_to","text":"I sent it.","start":5,"event_time":"2026-10-19T12:00:00Z"}],
		"open_anchors": [{"id":6,"direction":"owed_to","quote":"I will send it.","promised_at":"2026-10-19T12:00:00Z","reopened_at":"2026-10-19T12:00:00Z"}],
		"now": "2026-10-19T12:00:00Z",
		"timezone": "UTC"
	}`, packet)
}
