package personcard

import (
	"fmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSelectionBoundsAndExclusions(t *testing.T) {
	assertions := assert.New(t)
	in := baseInput()
	now := in.CollectedAt
	for i := 1; i <= 21; i++ {
		in.Loops.Loops = append(in.Loops.Loops, OpenLoop{ID: strconv.Itoa(i), Status: "open", Evidence: []EvidenceRef{{Ref: "message:1"}}, LastActivityAt: now})
	}
	in.Loops.Loops[0].Evidence[0].Ref = "message:1]\n[evil"
	in.Loops.Loops[1].KataRef = "example#dup"
	for i := 1; i <= 201; i++ {
		in.Actions = append(in.Actions, Action{MessageID: int64(i), Ordinal: 1, Status: "pending"})
	}
	for i := 1; i <= 101; i++ {
		in.Agenda = append(in.Agenda, AgendaItem{QualifiedRef: fmt.Sprintf("example#%03d", i)})
	}
	in.Agenda[0].QualifiedRef = "example#dup"
	in.Facts = []FactChange{{ID: 1, ChangedAt: now, Sensitive: true, Supported: true, EvidenceRef: "message:1"}, {ID: 2, ChangedAt: now, Supported: false, EvidenceRef: "message:1"}, {ID: 3, ChangedAt: now.Add(-91 * 24 * time.Hour), User: true}}
	card := Assemble(in, Options{MaxItems: 10})
	assertions.True(card.Coverage.LoopsTruncated)
	assertions.Equal(1, card.Coverage.LoopsInvalidEvidence)
	assertions.True(card.Coverage.Actions.Truncated)
	assertions.True(card.Coverage.AgendaTruncated)
	assertions.Contains(card.Gaps, "facts_none")
	assertions.Equal(308, card.ItemsDroppedByRank)
	for _, v := range card.Items {
		assertions.NotEqual("Kexample#dup", v.ID)
	}
}
func TestMeetingsAndCurrentSnapshotTimes(t *testing.T) {
	assertions := assert.New(t)
	in := baseInput()
	now := in.CollectedAt
	past := now.Add(-24 * time.Hour)
	future := now.Add(time.Hour)
	older := now.Add(-366 * 24 * time.Hour)
	in.Meetings = []Meeting{{Ref: "evt:1", Start: past, Basis: "invited", Title: "Invited"}, {Ref: "msg:2", Start: past.Add(-time.Hour), Basis: "transcript", Title: "Met", Participated: true}, {Ref: "msg:3", Start: future, Basis: "transcript"}, {Ref: "evt:4", Start: past, Basis: "invited", Cancelled: true}, {Ref: "evt:5", Start: older, Basis: "invited"}}
	in.ContactState = &ContactState{LastContactAt: &past, LastInboundAt: &past, LastOutboundAt: &past, CadenceStatus: "ok", CadenceDueAt: &future, ComputedAt: now}
	card := Assemble(in, Options{AsOf: now.Add(24 * time.Hour)})
	assertions.Equal(2, card.Meetings.Count)
	assertions.Equal(past, *card.Meetings.LastInvitedAt)
	assertions.Equal(past.Add(-time.Hour), *card.Meetings.LastMetAt)
	assertions.Equal("inbound", card.LastInteraction.Direction)
	assertions.True(card.LastInteraction.CadenceOverdueAtAsOf)
	assertions.Equal("ok", card.LastInteraction.CadenceStatus)
	in.ContactState.LastInboundAt = nil
	in.ContactState.LastOutboundAt = nil
	assertions.Equal("unknown", Assemble(in, Options{}).LastInteraction.Direction)
}
func TestFieldCapsAndBudgetTooSmall(t *testing.T) {
	assertions := assert.New(t)
	in := baseInput()
	long := strings.Repeat("界", 121)
	in.Person.UntrustedText = PersonText{DisplayName: long, OrganizationName: long, Title: long, Categories: []string{long, long, long, long, long, long}}
	in.Person.Emails = []string{long, long, long, long, long, long}
	in.Brief = &Brief{UntrustedText: &BriefText{Text: strings.Repeat("x", 301)}, Citations: make([]string, 21), Evidence: make([]string, 21)}
	card := Assemble(in, Options{})
	assertions.Len([]rune(card.Person.UntrustedText.DisplayName), 120)
	assertions.True(card.Person.UntrustedText.DisplayNameMore)
	assertions.True(card.Person.EmailsMore)
	assertions.True(card.Person.CategoriesMore)
	assertions.True(card.Brief.UntrustedText.TextMore)
	_, err := BuildResult("person:7", card, Options{MaxBytes: 2048})
	assertions.ErrorIs(err, ErrBudgetTooSmall)
}
func TestActionAssigneeAgendaTieOrder(t *testing.T) {
	in := baseInput()
	for i, c := range []string{"none", "other", "person", "owner"} {
		in.Actions = append(in.Actions, Action{MessageID: 1, Ordinal: i + 1, Status: "pending", AssigneeClass: c, OccurredAt: in.CollectedAt})
	}
	zero := 0
	one := 1
	in.Agenda = []AgendaItem{{QualifiedRef: "example#b", Priority: &one}, {QualifiedRef: "example#c"}, {QualifiedRef: "example#a", Priority: &zero}}
	card := Assemble(in, Options{MaxItems: 10})
	ids := []string{}
	for _, v := range card.Items {
		ids = append(ids, v.ID)
	}
	assert.Equal(t, []string{"A1:4", "A1:3", "A1:2", "A1:1", "Kexample#a", "Kexample#b", "Kexample#c"}, ids)
}
func TestValidatedCitations(t *testing.T) {
	for _, tc := range []struct {
		ref   string
		valid bool
	}{{"message:12", true}, {"msg:12", true}, {"evt:12", true}, {"evt:12@20261017T120000Z", true}, {"evt:12@20261017", true}, {"message:-1", false}, {"message:9007199254740992", false}, {"message:1]", false}, {"evt:1@hello", false}, {"kata:example#safe", true}, {"kata:example#safe]", false}} {
		t.Run(tc.ref, func(t *testing.T) { assert.Equal(t, tc.valid, ValidCitationRef(tc.ref)) })
	}
}
func TestBudgetPreservesStalenessAndCopies(t *testing.T) {
	assertions := assert.New(t)
	in := baseInput()
	old := in.CollectedAt.Add(-48 * time.Hour)
	for i := int64(1); i <= 10; i++ {
		in.Freshness.Sources = append(in.Freshness.Sources, SourceInput{SourceID: i, SourceType: "gmail", Identifier: fmt.Sprintf("%d@example.com", i), LastSuccessfulSyncAt: &old})
	}
	card := Assemble(in, Options{})
	limited, omitted := Budget(card, FormatMarkdown, 1)
	assertions.True(limited.Freshness.Stale)
	assertions.Equal(7, omitted.Sources)
	assertions.Len(card.Freshness.Sources, 10)
	assertions.Len(limited.Freshness.Sources, 3)
}
func TestResultOtherStatuses(t *testing.T) {
	assertions := assert.New(t)
	in := baseInput()
	r := ResultForStatus("ambiguous", "email:alex@example.com", in.CollectedAt, Options{}, []Candidate{{PersonID: 2, UntrustedText: CandidateText{DisplayName: "Alex\x1b[31m"}}, {PersonID: 1}}, nil)
	require.Len(t, r.Candidates, 2)
	assertions.Equal(int64(1), r.Candidates[0].PersonID)
	assertions.Nil(r.Card)
	assertions.Equal("source_controlled", r.Candidates[1].ContentTrust)
	assertions.Equal("Alex", r.Candidates[1].UntrustedText.DisplayName)
	assertions.NotNil(r.Freshness.Sources)
}
