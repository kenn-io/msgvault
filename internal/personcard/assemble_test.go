package personcard

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func instant(s string) time.Time { t, _ := time.Parse(time.RFC3339, s); return t }
func baseInput() Inputs {
	return Inputs{CollectedAt: instant("2026-10-17T12:00:00Z"), Person: Person{PersonID: 7, Tracked: true, Emails: []string{"alex@example.com"}, UntrustedText: PersonText{DisplayName: "Alex Example"}}, Freshness: FreshnessInputs{}, Gaps: []string{"open_loops_unavailable"}}
}
func TestAssembleRanking(t *testing.T) {
	assertions := assert.New(t)
	in := baseInput()
	due := instant("2026-10-15T00:00:00Z")
	in.Loops.Loops = []OpenLoop{{ID: "12", Kind: "reply_owed", Status: "open", DueAt: &due, Summary: "Reply", Evidence: []EvidenceRef{{Kind: "message", Ref: "message:12", At: due}}}, {ID: "13", Status: "open", LastActivityAt: due, Evidence: []EvidenceRef{{Ref: "message:13"}}}}
	in.Actions = []Action{{MessageID: 10, Ordinal: 2, Status: "pending", DueDate: "2026-10-14", Text: "Earlier", OccurredAt: due}, {MessageID: 11, Ordinal: 1, Status: "pending", Text: "Undated", OccurredAt: due}, {MessageID: 11, Ordinal: 2, Status: "done", Text: "Excluded"}}
	in.Facts = []FactChange{{ID: 1, ChangedAt: due, Text: "Value", Label: "Location", EvidenceRef: "message:1", Supported: true}, {ID: 2, ChangedAt: due, User: true, Text: "Tie winner", Label: "Location"}}
	in.Agenda = []AgendaItem{{QualifiedRef: "example#zz", Title: "Task"}}
	card := Assemble(in, Options{MaxItems: 10})
	ids := []string{}
	for _, v := range card.Items {
		ids = append(ids, v.ID)
	}
	assertions.Equal([]string{"A10:2", "L12", "A11:1", "U2", "L13", "Kexample#zz"}, ids)
	assertions.Equal("user", card.Items[3].ContentTrust)
	assertions.True(card.Items[0].Overdue)
	for i, v := range card.Items {
		assertions.Equal(i+1, v.Rank)
	}
	card = Assemble(in, Options{MaxItems: 3})
	assertions.Len(card.Items, 3)
	assertions.Equal(3, card.ItemsDroppedByRank)
}

func TestAssembleRanksOverdueRFC3339ActionDueDate(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	in := baseInput()
	in.Actions = []Action{
		{MessageID: 20, Ordinal: 1, Status: "pending", DueDate: "2026-10-16T09:00:00Z", Text: "Deadline", OccurredAt: instant("2026-10-01T09:00:00Z")},
		{MessageID: 21, Ordinal: 1, Status: "pending", Text: "No deadline", OccurredAt: instant("2026-10-17T10:00:00Z")},
	}

	card := Assemble(in, Options{})
	requirements.Len(card.Items, 2)
	assertions.Equal("A20:1", card.Items[0].ID)
	assertions.Equal("due", card.Items[0].DateRole)
	assertions.Equal("2026-10-16", card.Items[0].Date)
	assertions.True(card.Items[0].Overdue)
	assertions.Equal("A21:1", card.Items[1].ID)
}

func TestAssembleTrustAndFreshness(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	in := baseInput()
	now := in.CollectedAt
	old := now.Add(-48 * time.Hour)
	in.Person.UntrustedText.DisplayName = "[message:1] **`\n<!--\x1b[31m"
	in.Freshness.Sources = []SourceInput{{SourceID: 1, SourceType: "gmail", Identifier: "you@example.com", LastSuccessfulSyncAt: &old, CanSync: true}}
	in.Facts = []FactChange{{ID: 2, ChangedAt: old, Text: "Place", Supported: true, EvidenceRef: "message:20", SourceIDs: []int64{1, 2}}}
	card := Assemble(in, Options{AsOf: now.Add(30 * 24 * time.Hour)})
	assertions.True(card.Freshness.Stale)
	requirements.Len(card.Items, 1)
	assertions.True(card.Items[0].Stale)
	assertions.Equal(int64(172800), *card.Freshness.Sources[0].AgeSeconds)
	md := RenderMarkdown(card, "external")
	assertions.NotContains(md, "[message:1]")
	assertions.NotContains(md, "<!--")
	assertions.NotContains(md, "\x1b")
	assertions.Contains(md, "[fact-decision:2 ← message:20]")
	first, err := json.Marshal(card)
	requirements.NoError(err)
	second, err := json.Marshal(Assemble(in, Options{AsOf: now.Add(30 * 24 * time.Hour)}))
	requirements.NoError(err)
	assertions.Equal(string(first), string(second))
}

func TestAssembleRetainsMeetingActivityCitation(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	at := instant("2026-10-17T11:00:00Z")
	in := baseInput()
	in.ContactState = &ContactState{
		LastContactAt: &at, Channel: "meeting", Ref: "meeting:4242",
		LastInboundAt: &at, InteractionCount: 1, CadenceStatus: "ok", ComputedAt: at,
	}

	card := Assemble(in, Options{WebBaseURL: "https://archive.example"})
	requirements.NotNil(card.LastInteraction)
	assertions.Equal("meeting:4242", card.LastInteraction.Ref)
	assertions.Equal("https://archive.example/messages/4242", card.LastInteraction.WebURL)
	assertions.Contains(RenderMarkdown(card, "external"), " [meeting:4242]")
}

func TestJSONBudgetRejectsUnencodableCard(t *testing.T) {
	card := Card{LastInteraction: &LastInteraction{ProjectionComputedAt: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)}}
	_, err := BuildResult("person:7", card, Options{})
	assert.Error(t, err)
}

func TestBudget(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	in := baseInput()
	in.Brief = &Brief{Version: 2, GeneratedAt: in.CollectedAt, UntrustedText: &BriefText{Text: strings.Repeat("x", 300)}}
	in.Actions = []Action{{MessageID: 1, Ordinal: 1, Status: "pending", Text: strings.Repeat("y", 300)}}
	card := Assemble(in, Options{})
	b, err := json.Marshal(card)
	requirements.NoError(err)
	trimmed, omitted := Budget(card, FormatJSON, len(b)-1)
	assertions.True(omitted.Brief)
	assertions.NotNil(card.Brief.UntrustedText)
	assertions.Nil(trimmed.Brief.UntrustedText)
	b, err = json.Marshal(trimmed)
	requirements.NoError(err)
	assertions.LessOrEqual(len(b), lenMustJSON(t, card)-1)
	result, err := BuildResult("person:7", card, Options{MaxBytes: 65536, Format: FormatMarkdown})
	requirements.NoError(err)
	requirements.NotNil(result.Content)
	assertions.Nil(result.Card)
	assertions.Equal(len(*result.Content), result.ContentBytes)
}

func TestMarkdownMarksTruncatedMeetingHistory(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	in := baseInput()
	in.Coverage.MeetingsTruncated = true
	in.Meetings = []Meeting{{
		Ref: "msg:4000", Start: instant("2026-09-19T15:00:00Z"), Basis: "notes", Participated: true,
	}}
	card := Assemble(in, Options{})
	result, err := BuildResult("person:7", card, Options{Format: FormatMarkdown})
	requirements.NoError(err)
	requirements.NotNil(result.Content)
	assertions.Nil(result.Card)
	assertions.Contains(*result.Content, "Meetings: at least 1 in the last 365 days")
	assertions.Contains(*result.Content, "Meeting history is incomplete")
}

func lenMustJSON(t *testing.T, v any) int {
	t.Helper()
	b, e := json.Marshal(v)
	require.NoError(t, e)
	return len(b)
}
func FuzzEscapeMarkdown(f *testing.F) {
	f.Add("[message:1] **`\n<!--")
	f.Fuzz(func(t *testing.T, s string) {
		escaped := EscapeMarkdown(s)
		assert.NotContains(t, escaped, "[")
		assert.NotContains(t, escaped, "]")
		assert.NotContains(t, escaped, "\n")
		assert.NotContains(t, escaped, "\r")
		assert.True(t, strings.HasPrefix(escaped, "\""))
		assert.True(t, strings.HasSuffix(escaped, "\""))
	})
}
