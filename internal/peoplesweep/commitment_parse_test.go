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
		Text: "I will send it. I will send it.", EventTime: now.Add(-time.Hour), ContentSHA256: "9372e6cad8b1cdad07b4aa9848f478a76f55144ee055c373975b1aeed38cd6d9"}
	window := CommitmentWindow{PersonID: 2, Sources: []CommitmentSource{source}, Now: now}
	output := jsontext.Value(`{"promises":[{"source_id":"m1","direction":"owed_by","quote":"I will send it.","start":0,"end":15,"summary":"Send the first item","due_on":null,"due_phrase":null},{"source_id":"m1","direction":"owed_by","quote":"I will send it.","start":16,"end":31,"summary":"Send the second item","due_on":null,"due_phrase":null}]}`)
	parsed, err := ParseCommitments(output, window)
	r.NoError(err)
	r.Len(parsed.Promises, 2)
	a.NotEqual(parsed.Promises[0].TargetKey, parsed.Promises[1].TargetKey)
	first := parsed.Promises[0].TargetKey
	// A different summary never changes promise identity.
	output = jsontext.Value(`{"promises":[{"source_id":"m1","direction":"owed_by","quote":"I will send it.","start":0,"end":15,"summary":"Another phrasing","due_on":null,"due_phrase":null}]}`)
	parsed, err = ParseCommitments(output, window)
	r.NoError(err)
	a.Equal(first, parsed.Promises[0].TargetKey)
	window.Sources[0].Text += " Note."
	window.Sources[0].ContentSHA256 = "81d2971a8cb79776e66a6931f3f0a4fcf94783eafddffa9b1c5c9d6e3f1f9c1a"
	parsed, err = ParseCommitments(output, window)
	r.NoError(err)
	a.NotEqual(first, parsed.Promises[0].TargetKey)
	window.Sources[0].Text = "Note. " + window.Sources[0].Text
	window.Sources[0].ContentSHA256 = "1edecdce9eca144e2be1372b7c87fc8f421a766ade855c459a529ba6b316355d"
	output = jsontext.Value(`{"promises":[{"source_id":"m1","direction":"owed_by","quote":"I will send it.","start":6,"end":21,"summary":"Send","due_on":null,"due_phrase":null}]}`)
	parsed, err = ParseCommitments(output, window)
	r.NoError(err)
	a.NotEqual(first, parsed.Promises[0].TargetKey)
	t.Run("overlapping quotes remain distinct", func(t *testing.T) {
		require := require.New(t)
		now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
		window := CommitmentWindow{PersonID: 2, Now: now, Sources: []CommitmentSource{{
			ID: "m1", MessageID: 1, Direction: CommitmentOwedTo, Text: "aaaa",
			ContentSHA256: "61be55a8e2f6b4e172338bddf184d6dbee29c98853e0a0485ecee7f27b9af0b4",
		}}}
		output := jsontext.Value(`{"promises":[{"source_id":"m1","direction":"owed_to","quote":"aaa","start":0,"end":3,"summary":"Send","due_on":null,"due_phrase":null}]}`)
		parsed, err := ParseCommitments(output, window)
		require.NoError(err)
		overlapping := parsed.Promises[0].TargetKey
		window.Sources[0].Text = "aaa"
		window.Sources[0].ContentSHA256 = "9834876dcfb05cb167a5c24953eba58c4ac89b1adf57f28f2f9d09af107ee8f0"
		parsed, err = ParseCommitments(output, window)
		require.NoError(err)
		require.NotEqual(overlapping, parsed.Promises[0].TargetKey)
	})
}

func TestParseCommitmentsRejectsWholeUnsupportedOutput(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	source := CommitmentSource{ID: "m1", MessageID: 1, Direction: CommitmentOwedTo,
		Text: "I will send it.", EventTime: time.Now().Add(-time.Hour)}
	window := CommitmentWindow{PersonID: 2, Sources: []CommitmentSource{source}, Now: time.Now()}
	cases := []struct{ name, output string }{
		{"unknown source", `{"promises":[{"source_id":"unknown","direction":"owed_to","quote":"I will send it.","start":0,"end":15,"summary":"Send","due_on":null,"due_phrase":null}]}`},
		{"wrong direction", `{"promises":[{"source_id":"m1","direction":"owed_by","quote":"I will send it.","start":0,"end":15,"summary":"Send","due_on":null,"due_phrase":null}]}`},
		{"quote mismatch", `{"promises":[{"source_id":"m1","direction":"owed_to","quote":"I sent it.","start":0,"end":10,"summary":"Send","due_on":null,"due_phrase":null}]}`},
		{"bad date", `{"promises":[{"source_id":"m1","direction":"owed_to","quote":"I will send it.","start":0,"end":15,"summary":"Send","due_on":"2026-02-30","due_phrase":null}]}`},
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

func TestCommitmentRequestOmitsInternalSourceFields(t *testing.T) {
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
		name, text, quote, due, timezone, phrase string
		event                                    time.Time
		keep                                     bool
	}{
		{name: "no date", text: "I will call.", quote: "I will call.", due: "2026-11-20"},
		{name: "outside quote", text: "I will call. Meeting is on 2026-11-20.", quote: "I will call.", due: "2026-11-20"},
		{name: "different date", text: "I will call by 2026-11-21.", quote: "I will call by 2026-11-21.", due: "2026-11-20"},
		{name: "embedded digits", text: "I will call about 12026-11-20.", quote: "I will call about 12026-11-20.", due: "2026-11-20"},
		{name: "ISO date", text: "I will call by 2026-11-20.", quote: "I will call by 2026-11-20.", due: "2026-11-20", keep: true},
		{name: "written date", text: "I will call by November 20, 2026.", quote: "I will call by November 20, 2026.", due: "2026-11-20", keep: true},
		{name: "weekday", text: "I will call by Friday.", quote: "I will call by Friday.", due: "2026-11-13", keep: true},
		{name: "bare weekday", text: "I will call Friday.", quote: "I will call Friday.", due: "2026-11-13", keep: true},
		{name: "this weekday", text: "I will call by this Friday.", quote: "I will call by this Friday.", due: "2026-11-13", keep: true},
		{name: "next weekday", text: "I will call by next Friday.", quote: "I will call by next Friday.", due: "2026-11-20", keep: true},
		{name: "weekday next week", text: "I will call by Monday next week.", quote: "I will call by Monday next week.", due: "2026-11-16", keep: true},
		{name: "weekday next week wrong date", text: "I will call by Monday next week.", quote: "I will call by Monday next week.", due: "2026-11-23"},
		{name: "yearless date", text: "I will call on November 20.", quote: "I will call on November 20.", due: "2026-11-20", keep: true},
		{name: "wrong weekday", text: "I will call by Friday.", quote: "I will call by Friday.", due: "2026-11-20"},
		{name: "unambiguous numeric date", text: "I will call on 11/20/2026.", quote: "I will call on 11/20/2026.", due: "2026-11-20", keep: true},
		{name: "two digit numeric year", text: "I will call by 11/20/26.", quote: "I will call by 11/20/26.", due: "2026-11-20", keep: true},
		{name: "two digit year wrong date", text: "I will call by 11/20/26.", quote: "I will call by 11/20/26.", due: "2026-11-21"},
		{name: "two digit year ambiguous date", text: "I will call by 11/12/26.", quote: "I will call by 11/12/26.", due: "2026-11-12"},
		{name: "ambiguous numeric date", text: "I will call on 11/12/2026.", quote: "I will call on 11/12/2026.", due: "2026-11-12"},
		{name: "unknown days", text: "I will call in several days.", quote: "I will call in several days.", due: "2026-11-13"},
		{name: "in three days", text: "I will call in three days.", quote: "I will call in three days.", due: "2026-11-13", keep: true},
		{name: "in two weeks", text: "I will call in two weeks.", quote: "I will call in two weeks.", due: "2026-11-24", keep: true},
		{name: "unrelated date", text: "We met Monday; I will call Tuesday.", quote: "We met Monday; I will call Tuesday.", due: "2026-11-16", phrase: `"I will call Tuesday"`},
		{name: "cited deadline", text: "We met Monday; I will call Tuesday.", quote: "We met Monday; I will call Tuesday.", due: "2026-11-10", phrase: `"I will call Tuesday"`, keep: true},
		{name: "null deadline phrase", text: "I will call Friday.", quote: "I will call Friday.", due: "2026-11-13", phrase: "null"},
		{name: "empty deadline phrase", text: "I will call Friday.", quote: "I will call Friday.", due: "2026-11-13", phrase: `""`},
		{name: "uncited deadline phrase", text: "I will call Friday.", quote: "I will call Friday.", due: "2026-11-13", phrase: `"Monday"`},
		{name: "in three days wrong date", text: "I will call in three days.", quote: "I will call in three days.", due: "2026-11-12"},
		{name: "tomorrow", text: "I will call tomorrow.", quote: "I will call tomorrow.", due: "2026-11-11", keep: true},
		{name: "today", text: "I will call today.", quote: "I will call today.", due: "2026-11-10", keep: true},
		{name: "today wrong date", text: "I will call today.", quote: "I will call today.", due: "2026-11-11"},
		{name: "day after tomorrow", text: "I will call the day after tomorrow.", quote: "I will call the day after tomorrow.", due: "2026-11-12", keep: true},
		{name: "day after tomorrow rejects tomorrow", text: "I will call the day after tomorrow.", quote: "I will call the day after tomorrow.", due: "2026-11-11"},
		{name: "wrong tomorrow", text: "I will call tomorrow.", quote: "I will call tomorrow.", due: "2026-11-12"},
		{name: "activity timezone", text: "I will call tomorrow.", quote: "I will call tomorrow.", due: "2026-11-11", timezone: "America/New_York", event: time.Date(2026, 11, 11, 2, 0, 0, 0, time.UTC), keep: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, a := require.New(t), assert.New(t)
			now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
			if !tc.event.IsZero() {
				now = tc.event
			}
			source := CommitmentSource{ID: "m1", MessageID: 1, Direction: CommitmentOwedTo, Text: tc.text, EventTime: now}
			window := CommitmentWindow{PersonID: 2, Sources: []CommitmentSource{source}, Now: now, Timezone: tc.timezone}
			phrase := tc.phrase
			if phrase == "" {
				phrase = fmt.Sprintf("%q", tc.quote)
			}
			output := jsontext.Value(fmt.Sprintf(`{"promises":[{"source_id":"m1","direction":"owed_to","quote":%q,"start":0,"end":%d,"summary":"Call","due_on":%q,"due_phrase":%s}]}`, tc.quote, len([]rune(tc.quote)), tc.due, phrase))
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
}
