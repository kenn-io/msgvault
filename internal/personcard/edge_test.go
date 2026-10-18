package personcard

import (
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestFreshnessSourcePolicy(t *testing.T) {
	assertions := assert.New(t)
	now := baseInput().CollectedAt
	old := now.Add(-48 * time.Hour)
	recent := now.Add(-time.Hour)
	for _, tc := range []struct {
		name                string
		source              SourceInput
		wantCommand, reason string
		wantStale           bool
	}{
		{"Graph mail", SourceInput{SourceID: 1, SourceType: "msmail", Identifier: "you@example.com", CanSync: true, LastSuccessfulSyncAt: &recent}, "msgvault sync-incremental you@example.com", "", false},
		{"calendar", SourceInput{SourceID: 1, SourceType: "gcal", Identifier: "you@example.com", CanSync: true, LastSuccessfulSyncAt: &recent}, "msgvault sync-calendar you@example.com", "", false},
		{"calendar archive key", SourceInput{SourceID: 1, SourceType: "gcal", Identifier: "you@example.com/primary", CanSync: true, LastSuccessfulSyncAt: &recent}, "msgvault sync-calendar you@example.com", "", false},
		{"cannot sync", SourceInput{SourceID: 1, SourceType: "gmail", Identifier: "you@example.com", CanSync: false, SyncUnavailableReason: "disabled", LastSuccessfulSyncAt: &old}, "", "disabled", true},
		{"unsupported", SourceInput{SourceID: 1, SourceType: "files", Identifier: "local", CanSync: true, LastSuccessfulSyncAt: &recent}, "", "use list_source_status", false},
		{"unmeasured", SourceInput{SourceID: 1, SourceType: "gmail", Identifier: "you@example.com"}, "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			in := baseInput()
			in.Freshness.Sources = []SourceInput{tc.source}
			card := Assemble(in, Options{})
			require.Len(t, card.Freshness.Sources, 1)
			assertions.Equal(tc.wantCommand, card.Freshness.Sources[0].SyncCommand)
			assertions.Equal(tc.reason, card.Freshness.Sources[0].SyncUnavailableReason)
			assertions.Equal(tc.wantStale, card.Freshness.Stale)
		})
	}
	in := baseInput()
	in.ContactState = &ContactState{Stale: true, ComputedAt: now}
	assertions.True(Assemble(in, Options{}).Freshness.Stale)
	in = baseInput()
	in.Freshness.SourcesOmitted = 5
	in.Freshness.OmittedStale = true
	card := Assemble(in, Options{})
	assertions.True(card.Freshness.Stale)
	assertions.Equal(5, card.Freshness.SourcesOmitted)
	in = baseInput()
	for i := int64(1); i <= 11; i++ {
		in.Freshness.Sources = append(in.Freshness.Sources, SourceInput{SourceID: i, Identifier: strconv.FormatInt(i, 10), LastSuccessfulSyncAt: &recent})
	}
	in.Freshness.Sources[10].LastSuccessfulSyncAt = nil
	card = Assemble(in, Options{})
	assertions.Len(card.Freshness.Sources, 10)
	assertions.True(card.Freshness.Stale)
	assertions.Equal(1, card.Freshness.SourcesOmitted)
	in = baseInput()
	in.Brief = &Brief{GeneratedAt: old}
	card = Assemble(in, Options{})
	assertions.False(card.Freshness.Stale)
	assertions.True(card.Freshness.Projections["brief"].Stale)
}
func TestGapLabelsAndSuppressedEmptyGaps(t *testing.T) {
	assertions := assert.New(t)
	in := baseInput()
	in.Gaps = []string{"contact_state_unavailable", "brief_unavailable", "actions_unavailable", "facts_unavailable", "agenda_not_configured", "meetings_unavailable", "open_loops_unavailable", "sources_unavailable", "unrecognized"}
	c := Assemble(in, Options{})
	assertions.NotContains(c.Gaps, "unrecognized")
	for _, g := range []string{"contact_state_none", "brief_none", "actions_none", "facts_none", "agenda_none", "meetings_none", "open_loops_none"} {
		assertions.NotContains(c.Gaps, g)
	}
	md := RenderMarkdown(c, "")
	assertions.Contains(md, "No data:")
	assertions.NotContains(md, "not enrolled")
}
func TestEvidenceKindDerivedAndOversizedIDsRejected(t *testing.T) {
	assertions := assert.New(t)
	in := baseInput()
	in.Loops.Loops = []OpenLoop{{ID: "1", Status: "open", Evidence: []EvidenceRef{{Ref: "message:1"}}}, {ID: "2", Status: "open", Evidence: []EvidenceRef{{Ref: "evt:2"}}}}
	in.Facts = []FactChange{{ID: 9007199254740992, User: true, ChangedAt: in.CollectedAt}}
	in.Agenda = []AgendaItem{{QualifiedRef: strings.Repeat("p", 200) + "#x"}}
	card := Assemble(in, Options{MaxItems: 10})
	require.Len(t, card.Items, 2)
	assertions.Equal("message", card.Items[0].Cite.Kind)
	assertions.Equal("meeting", card.Items[1].Cite.Kind)
	for _, i := range card.Items {
		assertions.NotEmpty(renderedCite(i.Cite))
	}
}
func TestMarkdownBudgetCountsAffiliation(t *testing.T) {
	assertions := assert.New(t)
	c := Assemble(baseInput(), Options{Affiliation: "external"})
	r, e := BuildResult("person:7", c, Options{Format: FormatMarkdown, Affiliation: "external", MaxBytes: 65536})
	require.NoError(t, e)
	assertions.Contains(*r.Content, " · external\n")
	assertions.Equal(len(*r.Content), r.ContentBytes)
	_, e = BuildResult("person:7", c, Options{Format: FormatMarkdown, Affiliation: "external", MaxBytes: r.ContentBytes - 1})
	assertions.ErrorIs(e, ErrBudgetTooSmall)
}
func TestJSONBudgetMeasuresCardOnly(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	c := Assemble(baseInput(), Options{})
	r, e := BuildResult("person:7", c, Options{})
	requirements.NoError(e)
	encoded, e := json.Marshal(r.Card)
	requirements.NoError(e)
	assertions.Equal(len(encoded), r.ContentBytes)
	assertions.Nil(r.Content)
	assertions.Equal("ok", r.Status)
}

func TestV2JSONOmitsZeroPersonTextTruncationFlags(t *testing.T) {
	assertions := assert.New(t)

	encoded, err := jsonv2.Marshal(PersonText{})
	require.NoError(t, err)

	assertions.NotContains(string(encoded), `"display_name_more"`)
	assertions.NotContains(string(encoded), `"organization_name_more"`)
	assertions.NotContains(string(encoded), `"title_more"`)
}

func TestPrimaryChannelPreservesInPerson(t *testing.T) {
	in := baseInput()
	in.Person.PrimaryChannel = "in_person"

	card := Assemble(in, Options{})

	assert.Equal(t, "in_person", card.Person.PrimaryChannel)
}

func TestInvitationOnlyMeetingDoesNotAdvanceLastMet(t *testing.T) {
	in := baseInput()
	at := in.CollectedAt.Add(-time.Hour)
	in.Meetings = []Meeting{{Ref: "evt:1", Start: at, Basis: "transcript", Invited: true}}

	card := Assemble(in, Options{})

	assert.Nil(t, card.Meetings.LastMetAt)
	require.NotNil(t, card.Meetings.LastInvitedAt)
	assert.Equal(t, at, *card.Meetings.LastInvitedAt)
}

func TestEvidenceURLsUseExistingMessageRoute(t *testing.T) {
	in := baseInput()
	for i, ref := range []string{"message:12", "msg:12", "evt:12", "evt:12@20261017T120000Z"} {
		in.Loops.Loops = append(in.Loops.Loops, OpenLoop{ID: strconv.Itoa(i + 1), Status: "open", Evidence: []EvidenceRef{{Ref: ref}}})
	}
	card := Assemble(in, Options{MaxItems: 10, WebBaseURL: "https://archive.example.test/"})
	require.Len(t, card.Items, 4)
	for _, item := range card.Items {
		assert.Equal(t, "https://archive.example.test/messages/12", item.Cite.WebURL)
	}
}

func TestInvitedMeetingWithNotesRetainsBothHistoryDates(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	in := baseInput()
	at := in.CollectedAt.Add(-time.Hour)
	in.Meetings = []Meeting{{Ref: "evt:1", Start: at, Basis: "transcript", Invited: true, Participated: true}, {Ref: "msg:2", Start: at.Add(-time.Hour), Basis: "notes", Participated: true}}
	card := Assemble(in, Options{})
	assertions.Equal(2, card.Meetings.Count)
	requirements.NotNil(card.Meetings.LastMetAt)
	requirements.NotNil(card.Meetings.LastInvitedAt)
	assertions.Equal(at, *card.Meetings.LastMetAt)
	assertions.Equal(at, *card.Meetings.LastInvitedAt)
	assertions.Equal(map[string]int{"transcript": 1, "notes": 1}, card.Meetings.CountByBasis)
}

func TestSupportedExternalFactCitesDecisionWithoutInventedEvidence(t *testing.T) {
	assertions := assert.New(t)
	// Enrichment evidence may have an opaque provider reference. The adapter
	// leaves EvidenceRef empty; the immutable decision remains a valid citation.
	in := baseInput()
	in.Facts = []FactChange{{ID: 1, Supported: true, ChangedAt: in.CollectedAt, Label: "Role", Text: "Engineer"}}
	card := Assemble(in, Options{})
	require.Len(t, card.Items, 1)
	assertions.Equal("[fact-decision:1]", renderedCite(card.Items[0].Cite))
	assertions.Empty(card.Items[0].Cite.EvidenceRef)
	assertions.Empty(card.Items[0].Cite.WebURL)
	in.Facts[0].EvidenceRef = "message:1][malicious]"
	assertions.Empty(Assemble(in, Options{}).Items)
	in.Facts[0].EvidenceRef = ""
	in.Facts[0].Supported = false
	assertions.Empty(Assemble(in, Options{}).Items)
}

func TestSourceOutcomePreservesLatestAttempt(t *testing.T) {
	for _, tc := range []struct {
		name, outcome, wantOutcome string
		errors                     int64
		warning                    string
	}{
		{name: "completed", outcome: "completed", wantOutcome: "completed"},
		{name: "known success without latest attempt", wantOutcome: "unknown"},
		{name: "newer failed attempt", outcome: "failed", wantOutcome: "failed", errors: 2, warning: "The latest sync failed for gmail source you@example.com."},
		{name: "invalid status", outcome: "failed\n[message:1]", wantOutcome: "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			in := baseInput()
			recent := in.CollectedAt.Add(-time.Hour)
			in.Freshness.Sources = []SourceInput{{SourceID: 1, SourceType: "gmail", Identifier: "you@example.com", LastSuccessfulSyncAt: &recent, Outcome: tc.outcome, LastSyncErrors: tc.errors}}
			card := Assemble(in, Options{})
			require.Len(t, card.Freshness.Sources, 1)
			assertions.Equal(tc.wantOutcome, card.Freshness.Sources[0].Outcome)
			assertions.Equal(tc.errors, card.Freshness.Sources[0].LastSyncErrors)
			assertions.False(card.Freshness.Stale)
			if tc.warning != "" {
				assertions.Contains(card.Freshness.Warnings, tc.warning)
			} else {
				assertions.Empty(card.Freshness.Warnings)
			}
		})
	}
}
