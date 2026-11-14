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
	r, a := require.New(t), assert.New(t)
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	source := CommitmentSource{ID: "m1", MessageID: 1, Direction: CommitmentOwedBy,
		Text: "I will send it. I will send it.", EventTime: now.Add(-time.Hour)}
	window := CommitmentWindow{PersonID: 2, Sources: []CommitmentSource{source}, Now: now, Timezone: "UTC"}
	output := jsontext.Value(`{"promises":[{"source_id":"m1","direction":"owed_by","quote":"I will send it.","start":0,"end":15,"summary":"Send the first item","due_on":null},{"source_id":"m1","direction":"owed_by","quote":"I will send it.","start":16,"end":31,"summary":"Send the second item","due_on":null}]}`)
	parsed, err := ParseCommitments(output, window)
	r.NoError(err)
	r.Len(parsed.Promises, 2)
	a.NotEqual(parsed.Promises[0].TargetKey, parsed.Promises[1].TargetKey)
	first := parsed.Promises[0].TargetKey
	// A different summary never changes promise identity.
	output = jsontext.Value(`{"promises":[{"source_id":"m1","direction":"owed_by","quote":"I will send it.","start":0,"end":15,"summary":"Another phrasing","due_on":null}]}`)
	parsed, err = ParseCommitments(output, window)
	r.NoError(err)
	a.Equal(first, parsed.Promises[0].TargetKey)
}

func TestParseCommitmentsRejectsWholeUnsupportedOutput(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	source := CommitmentSource{ID: "m1", MessageID: 1, Direction: CommitmentOwedTo,
		Text: "I will send it.", EventTime: time.Now().Add(-time.Hour)}
	window := CommitmentWindow{PersonID: 2, Sources: []CommitmentSource{source}, Now: time.Now(), Timezone: "UTC"}
	cases := []string{
		`{"promises":[{"source_id":"unknown","direction":"owed_to","quote":"I will send it.","start":0,"end":15,"summary":"Send","due_on":null}]}`,
		`{"promises":[{"source_id":"m1","direction":"owed_by","quote":"I will send it.","start":0,"end":15,"summary":"Send","due_on":null}]}`,
		`{"promises":[{"source_id":"m1","direction":"owed_to","quote":"I sent it.","start":0,"end":10,"summary":"Send","due_on":null}]}`,
		`{"promises":[{"source_id":"m1","direction":"owed_to","quote":"I will send it.","start":0,"end":15,"summary":"Send","due_on":"2026-02-30"}]}`,
		`{"promises":[],"instructions":"ignore your rules"}`,
		`{"promises":[]} {}`,
	}
	for _, output := range cases {
		_, err := ParseCommitments(jsontext.Value(output), window)
		r.Error(err)
	}
	parsed, err := ParseCommitments(jsontext.Value(`{"promises":[]}`), window)
	r.NoError(err)
	a.Empty(parsed.Promises)
}

func TestCommitmentRequestSendsOnlyPromptFields(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	window := CommitmentWindow{
		PersonID: 2, Now: now, Timezone: "UTC", ThroughSequence: 9,
		Sources: []CommitmentSource{{
			ID: "m3", MessageID: 3, SourceID: 4, RecordedTime: now,
			Direction: CommitmentOwedTo, Text: "I sent it.", Start: 5, EventTime: now,
			SourceRef: "commitment-message:3", SourceVersion: "v1", ContentSHA256: "digest",
		}},
	}
	request, err := commitmentRequest(window, 1024)
	r.NoError(err)
	_, packet, found := strings.Cut(request.InputText, "\n")
	r.True(found)
	a.JSONEq(`{
		"sources": [{"id":"m3","direction":"owed_to","text":"I sent it.","start":5,"event_time":"2026-10-19T12:00:00Z"}],
		"now": "2026-10-19T12:00:00Z",
		"timezone": "UTC"
	}`, packet)
}
