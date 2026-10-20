package whoknows

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var rankNow = time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)

func authored(id int64, daysAgo int, similarity float64) Authored {
	at := rankNow.AddDate(0, 0, -daysAgo)
	return Authored{
		Similarity: similarity,
		Citation:   Citation{MessageID: id, SentAt: &at, MessageType: "email", Subject: "subject"},
	}
}

func person(id int64, messages ...Authored) Candidate {
	return Candidate{PersonID: id, DisplayName: "person", Messages: messages}
}

func rankParams() Params { return Params{Limit: 10, Evidence: 3, MinScore: .5} }

func TestRankRecencyOrdersEqualCounts(t *testing.T) {
	assert := assert.New(t)
	people, _ := Rank([]Candidate{
		person(1, authored(1, 720, .9), authored(2, 700, .9)),
		person(2, authored(3, 3, .9), authored(4, 5, .9)),
	}, rankParams(), rankNow)
	require.Len(t, people, 2)
	assert.Equal(int64(2), people[0].PersonID)
	assert.Equal(1, people[0].Rank)
	assert.Equal(int64(1), people[1].PersonID)
	assert.Equal(2, people[1].Rank)
}

func TestRankScoreIsLogOfWeightedMessages(t *testing.T) {
	people, _ := Rank([]Candidate{
		person(1, authored(1, 0, .8), authored(2, 180, .6)),
	}, rankParams(), rankNow)
	require.Len(t, people, 1)
	want := math.Log1p(.8*1 + .6*.5)
	assert.InDelta(t, want, people[0].Score, 1e-9)
	assert.Equal(t, 2, people[0].AuthoredMessages)
}

func TestRankRecencyFloor(t *testing.T) {
	undated := Authored{Similarity: 1, Citation: Citation{MessageID: 1}}
	people, _ := Rank([]Candidate{
		person(1, undated),
		person(2, authored(2, 20000, 1)),
	}, rankParams(), rankNow)
	require.Len(t, people, 2)
	assert.InDelta(t, math.Log1p(.1), people[0].Score, 1e-9)
	assert.InDelta(t, math.Log1p(.1), people[1].Score, 1e-9)
}

func TestRankAskMeAboutBoostsAndListsDeclaredOnlyPeople(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	declared := person(1, authored(1, 10, .9))
	declared.AskMeAbout = []string{"kubernetes"}
	plain := person(2, authored(2, 10, .9))
	only := Candidate{PersonID: 3, DisplayName: "declared only", AskMeAbout: []string{"kubernetes"}}
	people, _ := Rank([]Candidate{plain, only, declared}, rankParams(), rankNow)
	require.Len(people, 3)

	assert.Equal(int64(1), people[0].PersonID)
	assert.InDelta(people[2].MessageScore, people[0].MessageScore, 1e-9)
	assert.InDelta(.75, people[0].DeclaredBoost, 1e-9)
	assert.InDelta(people[0].MessageScore+.75, people[0].Score, 1e-9)

	assert.Equal(int64(3), people[1].PersonID)
	assert.Zero(people[1].AuthoredMessages)
	assert.Zero(people[1].MessageScore)
	assert.InDelta(.75, people[1].Score, 1e-9, "declared-only ranks on the boost alone")
	assert.Empty(people[1].Citations)
	assert.NotNil(people[1].Citations)
	assert.Equal([]string{"kubernetes"}, people[1].AskMeAbout)

	assert.Equal(int64(2), people[2].PersonID)
	assert.Zero(people[2].DeclaredBoost)
}

func TestRankListMailEarnsOneMessageAndDirectAnswersAllCount(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	digest := Candidate{PersonID: 1, DisplayName: "digest"}
	for i := range 12 {
		a := authored(int64(i+1), 1, .9)
		a.ListMail = true
		digest.Messages = append(digest.Messages, a)
	}
	answerer := person(2, authored(100, 3, .9), authored(101, 5, .9))
	people, _ := Rank([]Candidate{digest, answerer}, rankParams(), rankNow)
	require.Len(people, 2)
	assert.Equal(int64(2), people[0].PersonID, "two answers beat twelve digests")
	assert.Equal(12, people[1].AuthoredMessages)
	assert.InDelta(math.Log1p(.9*recency(digest.Messages[0].Citation.SentAt, rankNow)), people[1].MessageScore, 1e-9)

	mixed := person(3, authored(200, 3, .9), authored(201, 5, .9))
	listed := authored(202, 2, .9)
	listed.ListMail = true
	mixed.Messages = append(mixed.Messages, listed, listed)
	mixed.Messages[3].Citation.MessageID = 203
	people, _ = Rank([]Candidate{mixed}, rankParams(), rankNow)
	direct := .9*recency(mixed.Messages[0].Citation.SentAt, rankNow) + .9*recency(mixed.Messages[1].Citation.SentAt, rankNow)
	assert.InDelta(math.Log1p(direct+.9*recency(listed.Citation.SentAt, rankNow)), people[0].MessageScore, 1e-9)
}

func TestRankOneWaySenderEarnsOneMessage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	vendor := Candidate{PersonID: 1, DisplayName: "vendor", OneWay: true}
	for i := range 30 {
		vendor.Messages = append(vendor.Messages, authored(int64(i+1), 1, .9))
	}
	var twoWay []Authored
	for i := range 10 {
		twoWay = append(twoWay, authored(int64(100+i), 2, .9))
	}
	people, _ := Rank([]Candidate{vendor, person(2, twoWay...)}, rankParams(), rankNow)
	require.Len(people, 2)
	assert.Equal(int64(2), people[0].PersonID, "ten answers beat thirty unanswered messages")
	assert.Equal(30, people[1].AuthoredMessages)
	assert.InDelta(math.Log1p(.9*recency(vendor.Messages[0].Citation.SentAt, rankNow)), people[1].MessageScore, 1e-9)
}

func TestRankTiesAreDeterministic(t *testing.T) {
	assert := assert.New(t)
	// Equal scores from different counts: two weak messages against one strong one.
	two := person(7, authored(1, 0, .5), authored(2, 0, .5))
	one := person(5, authored(3, 0, 1))
	same := person(3, authored(4, 0, 1))
	forward, _ := Rank([]Candidate{two, one, same}, rankParams(), rankNow)
	backward, _ := Rank([]Candidate{same, one, two}, rankParams(), rankNow)
	require.Equal(t, forward, backward)
	require.Len(t, forward, 3)
	assert.Equal(int64(7), forward[0].PersonID, "equal score: more messages first")
	assert.Equal(int64(3), forward[1].PersonID, "equal score and count: lower ID first")
	assert.Equal(int64(5), forward[2].PersonID)
}

func TestRankCitationsAreHighestWeightFirstAndCapped(t *testing.T) {
	people, _ := Rank([]Candidate{
		person(1,
			authored(1, 0, .6), authored(2, 0, .9), authored(3, 400, .99),
			authored(4, 0, .7), authored(5, 0, .8)),
	}, Params{Limit: 10, Evidence: 3, MinScore: .5}, rankNow)
	require.Len(t, people, 1)
	var ids []int64
	for _, c := range people[0].Citations {
		ids = append(ids, c.MessageID)
	}
	assert.Equal(t, []int64{2, 5, 4}, ids)
	assert.Equal(t, 5, people[0].AuthoredMessages)
}

func TestRankListsPeopleAndContactsSeparately(t *testing.T) {
	assert := assert.New(t)
	var all []Candidate
	for i := int64(1); i <= 12; i++ {
		all = append(all, person(i, authored(i, 0, .9)))
	}
	for i := int64(1); i <= 7; i++ {
		all = append(all, Candidate{
			ParticipantID: 100 + i, EmailAddress: "contact@example.net",
			Messages: []Authored{authored(100+i, 0, .9)},
		})
	}
	people, contacts := Rank(all, Params{Limit: 4, Evidence: 1, MinScore: .5}, rankNow)
	assert.Len(people, 4)
	assert.Len(contacts, 5, "at most five unsaved contacts")
	assert.Equal(5, contacts[4].Rank)
	assert.Equal(int64(101), contacts[0].ParticipantID)
}

func TestAutomatedSenders(t *testing.T) {
	for _, address := range []string{
		"noreply@example.net", "no-reply+tag@example.net", "Bot <notifications@github.com>",
		"alerts-jira@example.net", "newsletter@example.net", "No_Reply@example.net", "mailer_daemon@example.net",
	} {
		assert.True(t, Automated(address), address)
	}
	for _, address := range []string{"ana@example.net", "newsome@example.net", "botanist@example.net", "jira@example.net", ""} {
		assert.False(t, Automated(address), address)
	}
}

func TestMatchesComparesWholeWords(t *testing.T) {
	assert := assert.New(t)
	assert.True(Matches("kubernetes networking", "Kubernetes"))
	assert.True(Matches("kubernetes", "kubernetes networking and storage"))
	assert.True(Matches("C++ templates", "c++ templates"))
	assert.False(Matches("C++ templates", "C templates"))
	assert.False(Matches("go", "google"))
	assert.False(Matches("the of", "the"), "stop words alone never match")
}
