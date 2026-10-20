package whoknows

import (
	"cmp"
	"math"
	"slices"
	"time"
)

const (
	halfLifeDays  = 180.0
	recencyFloor  = 0.1
	declaredBoost = 0.75
	maxContacts   = 5
)

// Authored is one matching message its author wrote.
type Authored struct {
	Citation   Citation
	Similarity float64
}

// Candidate is a saved person (PersonID set) or an unsaved sender
// (ParticipantID set) with the matching messages they wrote.
type Candidate struct {
	PersonID      int64
	ParticipantID int64
	DisplayName   string
	EmailAddress  string
	Messages      []Authored
	AskMeAbout    []string
}

type weighted struct {
	Authored

	weight float64
}

type ranked struct {
	Candidate

	score     float64
	citations []Citation
}

func (c Candidate) id() int64 { return cmp.Or(c.PersonID, c.ParticipantID) }

// recency halves every 180 days and never drops below 0.1. A message with no
// date gets the floor.
func recency(sentAt *time.Time, now time.Time) float64 {
	if sentAt == nil {
		return recencyFloor
	}
	days := math.Max(0, now.Sub(*sentAt).Hours()/24)
	return math.Max(recencyFloor, math.Exp2(-days/halfLifeDays))
}

// Rank scores each candidate as log1p of the summed message weights, where a
// message weighs its similarity times its recency. A matching ask-me-about
// value adds a boost, but only to a candidate with authored messages. Equal
// scores order by authored count, then ID. Citations are the highest-weight
// messages first, at most p.Evidence of them. Rank does no I/O.
func Rank(candidates []Candidate, p Params, now time.Time) ([]Person, []Contact) {
	var all []ranked
	for _, c := range candidates {
		if len(c.Messages) == 0 {
			continue
		}
		messages := make([]weighted, len(c.Messages))
		for i, m := range c.Messages {
			messages[i] = weighted{m, m.Similarity * recency(m.Citation.SentAt, now)}
		}
		// Sum in message order so equal inputs give equal floats.
		slices.SortFunc(messages, func(a, b weighted) int {
			return cmp.Compare(a.Citation.MessageID, b.Citation.MessageID)
		})
		sum := 0.0
		for _, m := range messages {
			sum += m.weight
		}
		score := math.Log1p(sum)
		if len(c.AskMeAbout) > 0 {
			score += declaredBoost
		}
		slices.SortFunc(messages, func(a, b weighted) int {
			if n := cmp.Compare(b.weight, a.weight); n != 0 {
				return n
			}
			if a.Citation.SentAt != nil && b.Citation.SentAt != nil {
				if n := b.Citation.SentAt.Compare(*a.Citation.SentAt); n != 0 {
					return n
				}
			}
			return cmp.Compare(a.Citation.MessageID, b.Citation.MessageID)
		})
		r := ranked{Candidate: c, score: score}
		for _, m := range messages[:min(p.Evidence, len(messages))] {
			r.citations = append(r.citations, m.Citation)
		}
		all = append(all, r)
	}
	slices.SortFunc(all, func(a, b ranked) int {
		return cmp.Or(
			cmp.Compare(b.score, a.score),
			cmp.Compare(len(b.Messages), len(a.Messages)),
			cmp.Compare(a.id(), b.id()),
		)
	})
	people := []Person{}
	contacts := []Contact{}
	for _, r := range all {
		if r.PersonID != 0 {
			if len(people) < p.Limit {
				people = append(people, Person{
					Rank: len(people) + 1, PersonID: r.PersonID, DisplayName: r.DisplayName,
					Score: r.score, AuthoredMessages: len(r.Messages),
					AskMeAbout: r.AskMeAbout, Citations: r.citations,
				})
			}
		} else if len(contacts) < maxContacts {
			contacts = append(contacts, Contact{
				Rank: len(contacts) + 1, ParticipantID: r.ParticipantID, DisplayName: r.DisplayName,
				EmailAddress: r.EmailAddress, Score: r.score,
				AuthoredMessages: len(r.Messages), Citations: r.citations,
			})
		}
	}
	return people, contacts
}
