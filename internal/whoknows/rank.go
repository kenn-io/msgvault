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
	ListMail   bool
}

// Candidate is a saved person (PersonID set) or an unsaved sender
// (ParticipantID set) with the matching messages they wrote. OneWay means the
// owner never wrote to them.
type Candidate struct {
	PersonID      int64
	ParticipantID int64
	DisplayName   string
	EmailAddress  string
	Messages      []Authored
	AskMeAbout    []string
	OneWay        bool
}

type weighted struct {
	Authored

	weight float64
}

type ranked struct {
	Candidate

	messageScore float64
	boost        float64
	score        float64
	citations    []Citation
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

// total sums message weights, but list mail adds only its best message and a
// one-way sender adds only its best message overall. A newsletter or a
// vendor who writes often earns one message's worth, while a colleague who
// answers keeps all of their messages. Messages sum in ID order so equal
// inputs give equal floats.
func total(messages []weighted, oneWay bool) float64 {
	var direct, bestList, best float64
	for _, m := range messages {
		best = max(best, m.weight)
		if m.ListMail {
			bestList = max(bestList, m.weight)
		} else {
			direct += m.weight
		}
	}
	if oneWay {
		return best
	}
	return direct + bestList
}

// Rank scores each candidate as log1p of the total message weight, where a
// message weighs its cosine similarity times its recency, plus a boost for a
// matching ask-me-about value. A person with a matching value and no messages
// ranks on the boost alone. Equal scores order by authored count, then ID.
// Citations are the highest-weight messages first, at most p.Evidence of
// them. Rank does no I/O.
func Rank(candidates []Candidate, p Params, now time.Time) (people, contacts []Entry) {
	var all []ranked
	for _, c := range candidates {
		messages := make([]weighted, len(c.Messages))
		for i, m := range c.Messages {
			messages[i] = weighted{m, m.Similarity * recency(m.Citation.SentAt, now)}
		}
		slices.SortFunc(messages, func(a, b weighted) int {
			return cmp.Compare(a.Citation.MessageID, b.Citation.MessageID)
		})
		r := ranked{Candidate: c}
		if len(messages) > 0 {
			r.messageScore = math.Log1p(total(messages, c.OneWay))
		}
		if len(c.AskMeAbout) > 0 {
			r.boost = declaredBoost
		}
		r.score = r.messageScore + r.boost
		if r.score == 0 {
			continue
		}
		slices.SortFunc(messages, func(a, b weighted) int {
			if n := cmp.Compare(b.weight, a.weight); n != 0 {
				return n
			}
			return cmp.Compare(a.Citation.MessageID, b.Citation.MessageID)
		})
		r.citations = []Citation{}
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
	people, contacts = []Entry{}, []Entry{}
	for _, r := range all {
		entry := Entry{
			PersonID: r.PersonID, ParticipantID: r.ParticipantID, DisplayName: r.DisplayName,
			EmailAddress: r.EmailAddress, Score: r.score, MessageScore: r.messageScore, DeclaredBoost: r.boost,
			AuthoredMessages: len(r.Messages), AskMeAbout: r.AskMeAbout, Citations: r.citations,
		}
		if r.PersonID != 0 && len(people) < p.Limit {
			entry.Rank = len(people) + 1
			people = append(people, entry)
		} else if r.PersonID == 0 && len(contacts) < maxContacts {
			entry.Rank = len(contacts) + 1
			contacts = append(contacts, entry)
		}
	}
	return people, contacts
}
