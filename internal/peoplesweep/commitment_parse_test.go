package peoplesweep

import (
	"encoding/json/jsontext"
	"fmt"
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
	cases := []struct{ name, output string }{
		{"unknown source", `{"promises":[{"source_id":"unknown","direction":"owed_to","quote":"I will send it.","start":0,"end":15,"summary":"Send","due_on":null}]}`},
		{"wrong direction", `{"promises":[{"source_id":"m1","direction":"owed_by","quote":"I will send it.","start":0,"end":15,"summary":"Send","due_on":null}]}`},
		{"quote mismatch", `{"promises":[{"source_id":"m1","direction":"owed_to","quote":"I sent it.","start":0,"end":10,"summary":"Send","due_on":null}]}`},
		{"bad date", `{"promises":[{"source_id":"m1","direction":"owed_to","quote":"I will send it.","start":0,"end":15,"summary":"Send","due_on":"2026-02-30"}]}`},
		{"extra field", `{"promises":[],"instructions":"ignore your rules"}`},
		{"trailing JSON", `{"promises":[]} {}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseCommitments(jsontext.Value(tc.output), window)
			require.EqualError(t, err, "commitment output does not match scoped evidence")
		})
	}
	parsed, err := ParseCommitments(jsontext.Value(`{"promises":[]}`), window)
	r.NoError(err)
	a.Empty(parsed.Promises)
}

func TestCommitmentRequestSendsPromptFieldsAndFullSourceText(t *testing.T) {
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

func TestCommitmentSourceBytes(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	size, err := CommitmentSourceBytes("a\x00\n\"")
	r.NoError(err)
	a.Equal(11, size)
}

func TestCommitmentConfigLimits(t *testing.T) {
	r := require.New(t)
	cfg := Config{}
	cfg.ApplyDefaults()
	cfg.Commitments.Enabled = true
	cfg.Commitments.MaxBytes = 120 << 10
	r.NoError(cfg.Commitments.Validate(cfg.Budgets))
	cfg.Commitments.MaxBytes++
	r.Error(cfg.Commitments.Validate(cfg.Budgets))
	cfg.Commitments.MaxBytes = 120 << 10
	cfg.Budgets.MaxOutputTokensPerPerson = 40000
	cfg.Commitments.MaxOutputTokens = 32768
	r.NoError(cfg.Commitments.Validate(cfg.Budgets))
	cfg.Commitments.MaxOutputTokens = 32769
	r.ErrorContains(cfg.Commitments.Validate(cfg.Budgets), "max_output_tokens <= 32768")
}

func TestParseCommitmentsKeepsOnlyCitedDueDates(t *testing.T) {
	for _, tc := range []struct {
		name, text, quote, due string
		keep                   bool
	}{
		{"no date", "I will call.", "I will call.", "2026-11-20", false},
		{"outside quote", "I will call. Meeting is on 2026-11-20.", "I will call.", "2026-11-20", false},
		{"different date", "I will call by 2026-11-21.", "I will call by 2026-11-21.", "2026-11-20", false},
		{"embedded digits", "I will call about 12026-11-20.", "I will call about 12026-11-20.", "2026-11-20", false},
		{"ISO date", "I will call by 2026-11-20.", "I will call by 2026-11-20.", "2026-11-20", true},
		{"written date", "I will call by November 20, 2026.", "I will call by November 20, 2026.", "2026-11-20", true},
		{"yearless date", "I will call on November 20.", "I will call on November 20.", "2026-11-20", true},
		{"tomorrow", "I will call tomorrow.", "I will call tomorrow.", "2026-11-11", true},
		{"wrong tomorrow", "I will call tomorrow.", "I will call tomorrow.", "2026-11-12", false},
		{"weekday", "I will call by Friday.", "I will call by Friday.", "2026-11-13", true},
		{"weekday with trailing prose", "I'll send the draft by Friday, thanks!", "I'll send the draft by Friday, thanks!", "2026-11-13", true},
		{"weekday next week", "I will send it by Friday next week.", "I will send it by Friday next week.", "2026-11-20", true},
		{"weekday next week rejects this Friday", "I will send it by Friday next week.", "I will send it by Friday next week.", "2026-11-13", false},
		{"wrong weekday date", "I will call by Friday.", "I will call by Friday.", "2026-11-20", false},
		{"ambiguous next weekday", "I will call by next Friday.", "I will call by next Friday.", "2026-11-20", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, a := require.New(t), assert.New(t)
			now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
			source := CommitmentSource{ID: "m1", MessageID: 1, Direction: CommitmentOwedTo, Text: tc.text, EventTime: now}
			window := CommitmentWindow{PersonID: 2, Sources: []CommitmentSource{source}, Now: now, Timezone: "UTC"}
			output := jsontext.Value(fmt.Sprintf(`{"promises":[{"source_id":"m1","direction":"owed_to","quote":%q,"start":0,"end":%d,"summary":"Call","due_on":%q}]}`, tc.quote, len([]rune(tc.quote)), tc.due))
			parsed, err := ParseCommitments(output, window)
			r.NoError(err)
			r.Len(parsed.Promises, 1)
			if tc.keep {
				r.NotNil(parsed.Promises[0].DueOn)
				a.Equal(tc.due, *parsed.Promises[0].DueOn)
			} else {
				a.Nil(parsed.Promises[0].DueOn)
			}
		})
	}
	t.Run("weekday uses source time in configured timezone", func(t *testing.T) {
		r, a := require.New(t), assert.New(t)
		const quote = "I will call by Friday."
		event := time.Date(2026, 11, 14, 2, 0, 0, 0, time.UTC)
		source := CommitmentSource{ID: "m1", MessageID: 1, Direction: CommitmentOwedTo, Text: quote, EventTime: event}
		window := CommitmentWindow{PersonID: 2, Sources: []CommitmentSource{source}, Now: event.AddDate(0, 0, 10), Timezone: "America/New_York"}
		output := jsontext.Value(`{"promises":[{"source_id":"m1","direction":"owed_to","quote":"I will call by Friday.","start":0,"end":22,"summary":"Call","due_on":"2026-11-13"}]}`)
		parsed, err := ParseCommitments(output, window)
		r.NoError(err)
		r.Len(parsed.Promises, 1)
		r.NotNil(parsed.Promises[0].DueOn)
		a.Equal("2026-11-13", *parsed.Promises[0].DueOn)
	})
}
