package whoknows

import (
	"encoding/json"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"math"
	"math/rand/v2"
	"testing"
	"time"
)

func TestRankAuthorshipReceptionAndDeclarations(t *testing.T) {
	check := assert.New(t)
	now := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	evidence := func(id int64, role string) Evidence {
		return Evidence{MessageID: id, ConversationID: id, MessageType: "email", Role: role, SentAt: &now, Relevance: 1}
	}
	received := make([]Evidence, 200)
	for i := range received {
		received[i] = evidence(int64(i+1), "to")
	}
	got := Rank([]Candidate{{PersonID: 1, Evidence: []Evidence{evidence(1, "from")}}, {PersonID: 2, Evidence: received}, {PersonID: 3, AskMeAboutMatches: []string{"go"}}, {PersonID: 4, Evidence: []Evidence{evidence(1, "from"), evidence(2, "from")}}}, DefaultParams(), now)
	require.Len(t, got, 4)
	check.Equal([]int64{4, 3, 1, 2}, []int64{got[0].PersonID, got[1].PersonID, got[2].PersonID, got[3].PersonID})
	check.InDelta(.3, got[3].Score, 1e-9)
	for _, p := range got {
		b := p.Breakdown
		check.InDelta(p.Score, b.Evidence+b.Declared+b.Affinity, 1e-9)
		check.InDelta(b.Evidence, b.Depth*b.Breadth, 1e-9)
		check.InDelta(b.RawAuthored+b.RawReceived, b.ShownContribution+b.OmittedContribution, 1e-9)
	}
}

func TestTopicTokens(t *testing.T) {
	for _, tt := range []struct {
		topic, value string
		want         bool
	}{{"go", "google", false}, {"c", "c++", false}, {"c++", "C++ templates", true}, {"Straße", "STRASSE", true}, {"about", "the", false}, {"go", "", false}, {"kubernetes networking", "kubernetes", true}} {
		assert.Equal(t, tt.want, Matches(tt.topic, tt.value), tt)
	}
}

func TestRankDecayFlagsAndDeterminism(t *testing.T) {
	check := assert.New(t)
	now := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	old := now.Add(-900 * 24 * time.Hour)
	future := now.Add(24 * time.Hour)
	candidates := []Candidate{{PersonID: 1, Evidence: []Evidence{{MessageID: 1, Role: "from", Relevance: 1, SentAt: &old}}}, {PersonID: 2, Evidence: []Evidence{{MessageID: 2, Role: "from", Relevance: 1, SentAt: &future}}}, {PersonID: 3, OneWay: true, AskMeAboutMatches: []string{"go"}, Evidence: []Evidence{{MessageID: 3, Role: "from", Relevance: 1, SentAt: &now, Flags: []string{"via_list"}}}}}
	params := DefaultParams()
	params.Tiebreak = "none"
	got := Rank(candidates, params, now)
	check.Equal(int64(3), got[0].PersonID)
	check.InDelta(math.Log1p(.25)+.75, got[0].Score, 1e-9)
	check.InDelta(math.Log1p(.1), got[2].Score, 1e-9)
	for range 10 {
		rand.Shuffle(len(candidates), func(i, j int) { candidates[i], candidates[j] = candidates[j], candidates[i] }) //nolint:gosec // Synthetic shuffling checks deterministic ranking.
		check.Equal(got, Rank(candidates, params, now))
	}
}

func TestParamsValidation(t *testing.T) {
	must := require.New(t)
	for _, change := range []func(*Params){func(p *Params) { p.Limit = 0 }, func(p *Params) { p.Evidence = 11 }, func(p *Params) { p.Candidates = 1001 }, func(p *Params) { p.MinScore = math.NaN() }, func(p *Params) { p.MinProfileScore = 1.1 }, func(p *Params) { p.HalfLifeDays = -1 }, func(p *Params) { p.Tiebreak = "random" }} {
		p := DefaultParams()
		change(&p)
		must.Error(p.Validate())
	}
	for _, change := range []func(*Params){
		func(p *Params) { p.MaxRosterEdges = 0 },
		func(p *Params) { p.MaxRosterEdges = 65 },
		func(p *Params) { p.MaxCandidatePeople = 0 },
		func(p *Params) { p.MaxCandidatePeople = MaxCandidatePeople + 1 },
	} {
		p := DefaultParams()
		change(&p)
		must.Error(p.Validate())
	}
	p := DefaultParams()
	must.NoError(p.Validate())
	p.Evidence = 0
	p.HalfLifeDays = 0
	must.NoError(p.Validate())
	p.MaxRosterEdges = 1
	p.MaxCandidatePeople = 1
	must.NoError(p.Validate())
}

func TestRankWorkedExample(t *testing.T) {
	check := assert.New(t)
	now := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	ev := func(id int64, age float64, rel float64, role, kind, channel string, thread int64) Evidence {
		at := now.Add(-time.Duration(age * float64(24*time.Hour)))
		return Evidence{MessageID: id, SentAt: &at, Relevance: rel, Role: role, MessageType: kind, Channel: channel, ConversationID: thread}
	}
	meeting := ev(113, 3, .75, "from", "meeting_transcript", "meeting", 8)
	alice := Candidate{PersonID: 7, AskMeAboutMatches: []string{"kubernetes"}, ProfileScore: new(.78), Evidence: []Evidence{ev(101, 30, 1, "from", "email", "email", 1), ev(102, 400, .8, "from", "email", "email", 1), ev(103, 10, .6, "from", "chat", "chat", 2), ev(104, 5, .9, "cc", "email", "email", 3), meeting}}
	bob := Candidate{PersonID: 12, ProfileScore: new(.42), Evidence: []Evidence{ev(102, 400, .8, "cc", "email", "email", 1), ev(103, 10, .6, "conversation_member", "chat", "chat", 2), ev(104, 5, .9, "to", "email", "email", 3), meeting}}
	for i := range 6 {
		bob.Evidence = append(bob.Evidence, ev(int64(105+i), 20, .7, "to", "email", "email", 4))
	}
	carol := Candidate{PersonID: 3, Evidence: []Evidence{ev(111, 900, .95, "from", "email", "email", 6), meeting}}
	dave := Candidate{PersonID: 21, AskMeAboutMatches: []string{"kubernetes networking"}, ProfileScore: new(.81)}
	got := Rank([]Candidate{alice, bob, carol, dave}, DefaultParams(), now)
	require.Len(t, got, 4)
	for i, want := range []float64{2.305, .905, .300, .127} {
		check.InDelta(want, got[i].Score, .001)
	}
	check.Equal([]string{"chat", "email"}, got[0].Breakdown.ChannelsAuthored)
	check.Equal(2, got[0].Breakdown.ThreadsAuthored)
	check.InDelta(1.35, got[0].Breakdown.Breadth, 1e-9)
}

func TestRankStableAccumulationAndRoleDedup(t *testing.T) {
	now := time.Now().UTC()
	p := DefaultParams()
	p.Evidence = 10
	evidence := []Evidence{}
	for i := range 300 {
		evidence = append(evidence, Evidence{MessageID: int64(i + 1), Role: "from", Relevance: 1 / float64(i+1), SentAt: &now})
	}
	evidence = append(evidence, Evidence{MessageID: 1, Role: "to", Relevance: 1, SentAt: &now})
	baseline := Rank([]Candidate{{PersonID: 1, Evidence: evidence}}, p, now)
	assert.Equal(t, 300, baseline[0].Breakdown.AuthoredMessages)
	for range 5 {
		rand.Shuffle(len(evidence), func(i, j int) { evidence[i], evidence[j] = evidence[j], evidence[i] }) //nolint:gosec // Synthetic shuffling checks deterministic ranking.
		assert.Equal(t, baseline, Rank([]Candidate{{PersonID: 1, Evidence: evidence}}, p, now))
	}
}

func TestRankMeetingsTiebreakAndZeroEvidence(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	now := time.Now().UTC()
	old := now.Add(-24 * time.Hour)
	for _, mode := range []string{"none", "temperature", "last_contact"} {
		p := DefaultParams()
		p.Tiebreak = mode
		p.Evidence = 0
		meeting := []Evidence{{MessageID: 1, Role: "from", MessageType: "meeting_transcript", Relevance: 1, SentAt: &now, Channel: "meeting", ConversationID: 1}}
		c := []Candidate{{PersonID: 1, Evidence: meeting, Temperature: new(10), LastContactAt: &old}, {PersonID: 2, Evidence: meeting, Temperature: new(50), LastContactAt: &now}}
		got := Rank(c, p, now)
		must.Len(got, 2)
		want := int64(2)
		if mode == "none" {
			want = 1
		}
		check.Equal(want, got[0].PersonID)
		for _, r := range got {
			check.Zero(r.Breakdown.AuthoredMessages)
			check.Zero(r.Breakdown.ThreadsAuthored)
			check.InDelta(1.0, r.Breakdown.Breadth, 0)
			check.Empty(r.Evidence)
			check.InDelta(.05, r.Breakdown.OmittedContribution, 1e-9)
		}
	}
	for _, tt := range []struct {
		email string
		want  bool
	}{{"newsletter+tag@example.net", true}, {"GitHub-notify@example.net", true}, {"notifications@example.net", true}, {"githubber@example.net", false}, {"support@example.net", false}, {"team@example.net", false}} {
		check.Equal(tt.want, Automated(tt.email))
	}
	p := DefaultParams()
	p.topic = "example cloud networking"
	got := Rank([]Candidate{{PersonID: 1, AskMeAboutMatches: []string{"cloud"}, Organizations: []Organization{{OrganizationID: 1, Name: "Example Cloud", PrimaryDomain: "cloud.example.co.uk"}}}}, p, now)
	must.Len(got, 1)
	check.InDelta(1.0, got[0].Breakdown.OrgMatch, 0)
	check.InDelta(1.0, got[0].Score, 1e-9)
}

func TestUnsavedResponseOmitsPersonBoosts(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	now := time.Now().UTC()
	ranked := Rank([]Candidate{{ParticipantID: 7, Evidence: []Evidence{{MessageID: 1, Role: "from", SentAt: &now, Relevance: 1}}}}, DefaultParams(), now)
	must.Len(ranked, 1)
	value := unsavedResult(ranked[0])
	raw, err := json.Marshal(value)
	must.NoError(err)
	var decoded map[string]any
	must.NoError(json.Unmarshal(raw, &decoded))
	check.NotContains(decoded, "tiebreak")
	check.NotContains(decoded, "person_id")
	breakdown, ok := decoded["breakdown"].(map[string]any)
	must.True(ok)
	check.NotContains(breakdown, "declared")
	check.NotContains(breakdown, "affinity")
}

func TestRankBoostCeilingAndUnknownDates(t *testing.T) {
	check := assert.New(t)
	now := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	p := DefaultParams()
	p.topic = "example"
	got := Rank([]Candidate{{PersonID: 1, AskMeAboutMatches: []string{"example"}, ProfileScore: new(1.0), Organizations: []Organization{{OrganizationID: 1, Name: "Example", NameNormalized: "example"}}}, {PersonID: 2, Evidence: []Evidence{{MessageID: 1, ConversationID: 1, Role: "from", SentAt: &now, Relevance: 1}, {MessageID: 2, ConversationID: 2, Role: "from", SentAt: &now, Relevance: 1}, {MessageID: 3, Role: "to", SentAt: &now, Relevance: 1}}}}, p, now)
	require.Len(t, got, 2)
	check.Equal(int64(2), got[0].PersonID)
	check.InDelta(1.25, got[1].Score, 1e-9)
	old := now.Add(-400 * 24 * time.Hour)
	c := []Candidate{{PersonID: 1, Evidence: []Evidence{{MessageID: 1, Role: "from", Relevance: 1}}}, {PersonID: 2, Evidence: []Evidence{{MessageID: 2, Role: "from", Relevance: 1, SentAt: &old}}}}
	got = Rank(c, p, now)
	check.Equal(int64(2), got[0].PersonID)
	check.InDelta(math.Log1p(.1), got[1].Score, 1e-9)
	p.HalfLifeDays = 0
	p.Tiebreak = "none"
	got = Rank(c, p, now)
	check.Equal(int64(1), got[0].PersonID)
	check.InDelta(got[0].Score, got[1].Score, 1e-9)
}
