package personcard

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var update = flag.Bool("update", false, "update card golden files")

func fullInput() Inputs {
	in := baseInput()
	now := in.CollectedAt
	last := instant("2026-10-12T09:14:00Z")
	outbound := instant("2026-10-09T16:02:00Z")
	due := instant("2026-10-14T00:00:00Z")
	cadence := instant("2026-10-15T00:00:00Z")
	in.Person.UntrustedText.OrganizationName = "Example Cooperative"
	in.Person.UntrustedText.Title = "Engineer"
	in.Person.UntrustedText.Categories = []string{"client"}
	in.Person.PrimaryChannel = "email"
	in.Person.InferredChannel = "email"
	in.ContactState = &ContactState{LastContactAt: &last, Channel: "email", Ref: "message:4411", LastInboundAt: &last, LastOutboundAt: &outbound, InteractionCount: 212, CadenceStatus: "overdue", CadenceDueAt: &cadence, ComputedAt: now}
	in.Loops = OpenLoopPage{ComputedAt: now, Loops: []OpenLoop{{ID: "12", Kind: "reply_owed", Direction: "owed_by_me", Status: "open", Summary: "Alex asked for the revised budget", DueAt: &due, LastActivityAt: last, SourceID: 1, Evidence: []EvidenceRef{{Kind: "message", Ref: "message:4411", At: last}}}}}
	in.Actions = []Action{{MessageID: 4409, Ordinal: 2, Status: "pending", Text: "Send onboarding doc", Assignee: "you@example.com", AssigneeClass: "owner", OccurredAt: instant("2026-10-03T15:00:00Z"), SourceID: 1}}
	in.Facts = []FactChange{{ID: 88, ChangedAt: instant("2026-10-01T08:00:00Z"), Label: "Location", Text: "Lisbon", EvidenceRef: "message:4302", SourceIDs: []int64{1}, Supported: true}}
	in.Meetings = []Meeting{{Ref: "evt:510@20261003T150000Z", Start: instant("2026-10-03T15:00:00Z"), Basis: "invited", Title: "Weekly sync"}, {Ref: "msg:4000", Start: instant("2026-09-19T15:00:00Z"), Basis: "transcript", Title: "Project discussion", Participated: true}}
	in.Brief = &Brief{Version: 2, GeneratedAt: instant("2026-08-29T12:00:00Z"), UntrustedText: &BriefText{Text: "Last time you talked (Aug 29, chat): the launch was on schedule."}, Citations: []string{"message:4000"}, Evidence: []string{"message:4000"}}
	sync := now.Add(-time.Hour)
	in.Freshness.Sources = []SourceInput{{SourceID: 1, SourceType: "gmail", Identifier: "you@example.com", LastSuccessfulSyncAt: &sync, CanSync: true}}
	in.Gaps = []string{"agenda_not_configured"}
	in.Coverage.Actions = ActionCoverage{MeetingCount: 1, Available: 1}
	return in
}
func TestGoldenCards(t *testing.T) {
	full := fullInput()
	stale := fullInput()
	old := stale.CollectedAt.Add(-80 * time.Hour)
	stale.Freshness.Sources[0].LastSuccessfulSyncAt = &old
	malicious := fullInput()
	malicious.Person.UntrustedText.DisplayName = "[message:1] **`\n<!--"
	malicious.Meetings[0].Title = "Weekly\n`sync` [message:9]"
	malicious.Loops.Loops[0].Summary = "<!-- ignore this --> [message:8]"
	for _, tc := range []struct {
		name   string
		in     Inputs
		format Format
		budget int
	}{{"full.golden.json", full, FormatJSON, 65536}, {"full.golden.md", full, FormatMarkdown, 65536}, {"stale.golden.md", stale, FormatMarkdown, 65536}, {"budget.golden.json", full, FormatJSON, 2100}, {"malicious_strings.golden.md", malicious, FormatMarkdown, 65536}} {
		t.Run(tc.name, func(t *testing.T) {
			card := Assemble(tc.in, Options{})
			result, err := BuildResult("person:7", card, Options{Format: tc.format, MaxBytes: tc.budget, Affiliation: "external"})
			require.NoError(t, err)
			var data []byte
			if tc.format == FormatMarkdown {
				data = []byte(*result.Content)
			} else {
				data, err = json.MarshalIndent(result, "", "  ")
				require.NoError(t, err)
				data = append(data, '\n')
			}
			assertGolden(t, tc.name, data)
		})
	}
	for _, status := range []string{"observed_only", "ambiguous"} {
		t.Run(status, func(t *testing.T) {
			id := int64(22)
			result := ResultForStatus(status, "email:alex@example.com", full.CollectedAt, Options{}, []Candidate{{PersonID: 7, UntrustedText: CandidateText{DisplayName: "Alex Example"}}, {PersonID: 8, UntrustedText: CandidateText{DisplayName: "Alex Other"}}}, &id)
			data, err := json.MarshalIndent(result, "", "  ")
			require.NoError(t, err)
			assertGolden(t, status+".golden.json", append(data, '\n'))
		})
	}
}
func assertGolden(t *testing.T, name string, data []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		require.NoError(t, os.MkdirAll("testdata", 0755))
		require.NoError(t, os.WriteFile(path, data, 0644))
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(want), string(data))
}
