// Package whoknows finds the people who wrote about a topic and cites the
// messages that show it.
package whoknows

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/gcal"
	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/slack"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/hybrid"
)

// retrieveLimit is how many messages one search reads.
const retrieveLimit = 200

// Retriever runs the hybrid (keyword plus meaning) message search.
type Retriever interface {
	Search(ctx context.Context, request hybrid.SearchRequest) ([]vector.FusedHit, hybrid.ResultMeta, error)
}

// DataReader reads who wrote each message and what people say to ask them about.
type DataReader interface {
	TopicAuthorsContext(ctx context.Context, messageIDs []int64) (map[int64]store.TopicAuthor, error)
	OwnerAddressedContext(ctx context.Context, participantIDs []int64) (map[int64]bool, error)
	AskMeAboutValuesContext(ctx context.Context) ([]store.PersonAskMeAbout, bool, error)
	GetMessageRawsContext(ctx context.Context, messageIDs []int64) (map[int64][]byte, error)
}

type Service struct {
	Retriever  Retriever
	Scorer     vector.ChunkScoringBackend
	Data       DataReader
	BuildScope vector.BuildScope
	Now        func() time.Time
}

type candidateKey struct {
	person bool
	id     int64
}

// searchTypes are the message types worth searching for authors: everything
// but meeting transcripts and calendar entries, which have organizers and
// attendees rather than authors.
func searchTypes(scope vector.BuildScope) ([]string, bool) {
	allowed := slices.DeleteFunc(slices.Clone(query.KnownMessageTypes), func(t string) bool {
		return t == meetingarchive.MessageType || t == gcal.MessageTypeCalendarEvent
	})
	return search.IntersectValues(allowed, scope.MessageTypes)
}

// Find retrieves messages about the topic, keeps those whose cosine
// similarity to it reaches MinScore, credits each to the person who wrote it,
// and ranks the result.
func (s *Service) Find(ctx context.Context, req Request) (Response, error) {
	out := Response{Topic: req.Topic, People: []Person{}, UnsavedContacts: []Contact{}}
	p, err := req.Resolve()
	if err != nil {
		return out, err
	}
	out.Topic = p.Topic
	var (
		similarity map[int64]float64
		candidates []Candidate
		meta       hybrid.ResultMeta
	)
	// An index limited to transcripts and calendar entries has no authors to
	// search, but people can still declare the topic.
	if types, empty := searchTypes(s.BuildScope); !empty {
		request := hybrid.SearchRequest{
			Mode:     hybrid.ModeHybrid,
			FreeText: p.FreeText,
			Limit:    retrieveLimit,
			Filter:   vector.Filter{After: p.After, Before: p.Before, MessageTypes: types},
		}
		for _, term := range p.Terms {
			request.SubjectTerms = append(request.SubjectTerms, strings.ToLower(term))
		}
		var hits []vector.FusedHit
		if hits, meta, err = s.Retriever.Search(ctx, request); err != nil {
			return out, err
		}
		out.SearchedMessages = len(hits)
		if similarity, err = s.similarities(ctx, hits, meta, p.MinScore); err != nil {
			return out, err
		}
		if candidates, err = s.attribute(ctx, hits, similarity); err != nil {
			return out, err
		}
	}
	candidates, out.DeclaredTruncated, err = s.addDeclared(ctx, candidates, p.FreeText)
	if err != nil {
		return out, err
	}
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	out.People, out.UnsavedContacts = Rank(candidates, p, now)
	if len(out.People) == 0 && len(out.UnsavedContacts) == 0 {
		out.Reason = ReasonNoEvidence
		poolFull := meta.PoolSaturated || out.SearchedMessages >= retrieveLimit
		if len(similarity) > 0 && poolFull || out.DeclaredTruncated {
			out.Reason = ReasonSearchLimit
		}
	}
	return out, nil
}

// similarities scores every hit exactly, by its best chunk, and keeps those at
// or above the floor. A hit with no embedded chunk does not count.
func (s *Service) similarities(
	ctx context.Context, hits []vector.FusedHit, meta hybrid.ResultMeta, floor float64,
) (map[int64]float64, error) {
	out := map[int64]float64{}
	for _, hit := range hits {
		chunks, err := s.Scorer.ScoreMessageChunks(ctx, meta.Generation.ID, hit.MessageID, meta.QueryVector)
		if err != nil {
			return nil, fmt.Errorf("score message %d: %w", hit.MessageID, err)
		}
		best := math.Inf(-1)
		for _, c := range chunks {
			if !math.IsNaN(c.Score) {
				best = max(best, c.Score)
			}
		}
		if best >= floor {
			out[hit.MessageID] = best
		}
	}
	return out, nil
}

// attribute credits each counted message to its author, leaving out the
// owner, senders without an identity, and automated senders.
func (s *Service) attribute(
	ctx context.Context, hits []vector.FusedHit, similarity map[int64]float64,
) ([]Candidate, error) {
	if len(similarity) == 0 {
		return nil, nil
	}
	ids := make([]int64, len(hits))
	for i, hit := range hits {
		ids[i] = hit.MessageID
	}
	authors, err := s.Data.TopicAuthorsContext(ctx, ids)
	if err != nil {
		return nil, err
	}
	counted := func(id int64) bool {
		a, ok := authors[id]
		_, scored := similarity[id]
		return ok && scored && !a.IsFromMe && !a.OwnerLinked && a.SenderID != 0 && !a.Automated &&
			!Automated(a.SenderEmail)
	}
	bots, err := s.slackBots(ctx, authors, counted)
	if err != nil {
		return nil, err
	}
	byKey := map[candidateKey]*Candidate{}
	participants := map[candidateKey][]int64{}
	var order []candidateKey
	for _, hit := range hits {
		if !counted(hit.MessageID) || bots[hit.MessageID] {
			continue
		}
		a := authors[hit.MessageID]
		sim := similarity[hit.MessageID]
		key := candidateKey{id: a.SenderID}
		fresh := &Candidate{ParticipantID: a.SenderID, DisplayName: a.SenderName, EmailAddress: a.SenderEmail}
		if a.PersonID != nil {
			key = candidateKey{person: true, id: *a.PersonID}
			fresh = &Candidate{PersonID: *a.PersonID, DisplayName: cmp.Or(a.PersonName, a.SenderName)}
		}
		c, exists := byKey[key]
		if !exists {
			c = fresh
			byKey[key] = c
			order = append(order, key)
		}
		if !slices.Contains(participants[key], a.SenderID) {
			participants[key] = append(participants[key], a.SenderID)
		}
		c.Messages = append(c.Messages, Authored{
			Similarity: sim,
			ListMail:   a.ListMail,
			Citation: Citation{
				MessageID: a.MessageID, SentAt: a.SentAt, MessageType: a.MessageType,
				Subject: a.Subject, Snippet: a.Snippet, SenderAuthenticated: a.SenderAuthenticated,
			},
		})
	}
	var senders []int64
	for _, key := range order {
		senders = append(senders, participants[key]...)
	}
	addressed, err := s.Data.OwnerAddressedContext(ctx, senders)
	if err != nil {
		return nil, err
	}
	out := make([]Candidate, len(order))
	for i, key := range order {
		c := byKey[key]
		c.OneWay = !slices.ContainsFunc(participants[key], func(id int64) bool { return addressed[id] })
		out[i] = *c
	}
	return out, nil
}

// slackBots returns the counted Slack messages whose archived payload marks a
// bot. The importer files a message carrying both "user" and "bot_id" under
// the user, so only the payload shows it. An unreadable payload leaves the
// message counted.
func (s *Service) slackBots(
	ctx context.Context, authors map[int64]store.TopicAuthor, counted func(int64) bool,
) (map[int64]bool, error) {
	var ids []int64
	for id, a := range authors {
		if a.MessageType == slack.MessageType && counted(id) {
			ids = append(ids, id)
		}
	}
	bots := map[int64]bool{}
	if len(ids) == 0 {
		return bots, nil
	}
	raws, err := s.Data.GetMessageRawsContext(ctx, ids)
	if err != nil {
		return nil, err
	}
	for id, raw := range raws {
		if slack.RawFromBot(raw) {
			bots[id] = true
		}
	}
	return bots, nil
}

// addDeclared adds each ask-me-about value that matches the topic to its
// person. A person with a match and no messages joins the candidates. It also
// reports that the declared read hit its cap.
func (s *Service) addDeclared(
	ctx context.Context, candidates []Candidate, freeText string,
) ([]Candidate, bool, error) {
	values, truncated, err := s.Data.AskMeAboutValuesContext(ctx)
	if err != nil {
		return nil, false, err
	}
	index := map[int64]int{}
	for i, c := range candidates {
		if c.PersonID != 0 {
			index[c.PersonID] = i
		}
	}
	for _, v := range values {
		if !Matches(freeText, v.Value) {
			continue
		}
		i, ok := index[v.PersonID]
		if !ok {
			candidates = append(candidates, Candidate{PersonID: v.PersonID, DisplayName: v.PersonName})
			i = len(candidates) - 1
			index[v.PersonID] = i
		}
		candidates[i].AskMeAbout = append(candidates[i].AskMeAbout, v.Value)
	}
	return candidates, truncated, nil
}
