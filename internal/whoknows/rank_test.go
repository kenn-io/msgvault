package whoknows

import (
	"fmt"
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

func TestRankWeighsRecencyAndCitesTheBestMessages(t *testing.T) {
	assert := assert.New(t)
	people, _ := Rank([]Candidate{
		person(1, authored(1, 720, .9), authored(2, 700, .9)),
		person(2, authored(3, 0, .6), authored(4, 0, .9), authored(5, 400, .99), authored(6, 0, .7), authored(7, 180, 1)),
	}, rankParams(), rankNow)
	require.Len(t, people, 2)
	assert.Equal(int64(2), people[0].PersonID, "recent authors rank above old ones")
	// Recency halves every 180 days: 0.5 at 180 days, 2^(-400/180) = 0.2143 at 400.
	assert.InDelta(math.Log1p(.6+.9+.99*.2143109957+.7+1*.5), people[0].MessageScore, 1e-9)
	var ids []int64
	for _, c := range people[0].Citations {
		ids = append(ids, c.MessageID)
	}
	assert.Equal([]int64{4, 6, 3}, ids, "highest weight first, capped at the evidence count")
	assert.Equal(5, people[0].AuthoredMessages)
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

func TestRankListsPeopleAndContactsSeparately(t *testing.T) {
	assert := assert.New(t)
	var all []Candidate
	for i := int64(1); i <= 12; i++ {
		all = append(all, person(i, authored(i, 0, .9)))
	}
	for i := int64(1); i <= 7; i++ {
		all = append(all, Candidate{
			ParticipantID: 100 + i, EmailAddress: fmt.Sprintf("contact-%d@example.net", i),
			Messages: []Authored{authored(100+i, 0, .9)},
		})
	}
	people, contacts := Rank(all, Params{Limit: 4, Evidence: 1, MinScore: .5}, rankNow)
	assert.Len(people, 4)
	assert.Len(contacts, 5, "at most five unsaved contacts")
	assert.Equal(5, contacts[4].Rank)
	assert.Equal(int64(101), contacts[0].ParticipantID)
}

func TestMatchesComparesWholeWords(t *testing.T) {
	assert := assert.New(t)
	assert.True(Matches("kubernetes networking", "Kubernetes"))
	assert.True(Matches("kubernetes", "kubernetes networking and storage"))
	assert.True(Matches("C++ templates", "c++ templates"))
	assert.False(Matches("C++ templates", "C templates"))
	assert.False(Matches("go", "google"))
	assert.True(Matches("strasse", "Straße"), "case folding matches ss to ß")
	assert.False(Matches("the of", "the"), "stop words alone never match")
}
