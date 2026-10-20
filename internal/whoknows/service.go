// Package whoknows finds the people who wrote about a topic and cites the
// messages that show it.
package whoknows

import (
	"cmp"
	"context"
	"errors"
	"math"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/hybrid"
)

// retrieveLimit is how many messages one search reads.
const retrieveLimit = 200

var ErrPersonScopeUnavailable = errors.New("people resolution is unavailable")

// Retriever runs the hybrid (keyword plus meaning) message search.
type Retriever interface {
	Search(ctx context.Context, request hybrid.SearchRequest) ([]vector.FusedHit, hybrid.ResultMeta, error)
}

// DataReader reads who wrote each message and what people say to ask them about.
type DataReader interface {
	TopicAuthorsContext(ctx context.Context, messageIDs []int64) (map[int64]store.TopicAuthor, error)
	AskMeAboutValuesContext(ctx context.Context, personIDs []int64) ([]store.PersonAskMeAbout, error)
}

type Service struct {
	Retriever  Retriever
	Data       DataReader
	BuildScope vector.BuildScope
	Now        func() time.Time
}

type candidateKey struct {
	person bool
	id     int64
}

// Find retrieves messages about the topic, keeps those whose meaning
// similarity reaches MinScore, credits each to the person who wrote it, and
// ranks the result. Keyword-only hits carry no similarity and do not count.
func (s *Service) Find(ctx context.Context, req Request) (Response, error) {
	p, err := req.Resolve()
	out := Response{Topic: strings.TrimSpace(req.Topic), People: []Person{}, UnsavedContacts: []Contact{}}
	if err != nil {
		return out, err
	}
	terms := search.Parse(p.Topic).TextTerms
	freeText := strings.Join(terms, " ")
	if strings.TrimSpace(freeText) == "" {
		return out, ErrMissingFreeText
	}
	if s.Retriever == nil {
		return out, vector.ErrNotEnabled
	}
	if s.Data == nil {
		return out, ErrPersonScopeUnavailable
	}
	request := hybrid.SearchRequest{
		Mode:     hybrid.ModeHybrid,
		FreeText: freeText,
		Limit:    retrieveLimit,
		Filter: vector.Filter{
			After: p.After, Before: p.Before,
			MessageTypes: slices.Clone(s.BuildScope.MessageTypes),
		},
	}
	for _, term := range terms {
		request.SubjectTerms = append(request.SubjectTerms, strings.ToLower(term))
	}
	hits, _, err := s.Retriever.Search(ctx, request)
	if err != nil {
		return out, err
	}
	similarity := map[int64]float64{}
	ids := []int64{}
	for _, hit := range hits {
		v := hit.VectorScore
		if math.IsNaN(v) || math.IsInf(v, 0) || v < p.MinScore {
			continue
		}
		if _, seen := similarity[hit.MessageID]; !seen {
			ids = append(ids, hit.MessageID)
		}
		similarity[hit.MessageID] = max(similarity[hit.MessageID], v)
	}
	if len(ids) > 0 {
		authors, err := s.Data.TopicAuthorsContext(ctx, ids)
		if err != nil {
			return out, err
		}
		candidates, err := s.attribute(ctx, ids, authors, similarity, freeText)
		if err != nil {
			return out, err
		}
		now := time.Now().UTC()
		if s.Now != nil {
			now = s.Now().UTC()
		}
		out.People, out.UnsavedContacts = Rank(candidates, p, now)
	}
	if len(out.People) == 0 && len(out.UnsavedContacts) == 0 {
		out.Reason = ReasonNoEvidence
	}
	return out, nil
}

// attribute credits each counted message to its author, leaving out the
// owner, meeting transcripts, and automated senders.
func (s *Service) attribute(
	ctx context.Context, ids []int64, authors map[int64]store.TopicAuthor,
	similarity map[int64]float64, freeText string,
) ([]Candidate, error) {
	byKey := map[candidateKey]*Candidate{}
	var order []candidateKey
	for _, id := range ids {
		a, ok := authors[id]
		if !ok || a.IsFromMe || a.OwnerLinked || a.SenderID == 0 ||
			a.MessageType == meetingarchive.MessageType || Automated(a.SenderEmail) {
			continue
		}
		key := candidateKey{id: a.SenderID}
		c := &Candidate{
			ParticipantID: a.SenderID,
			DisplayName:   a.SenderName,
			EmailAddress:  a.SenderEmail,
		}
		if a.PersonID != nil {
			key = candidateKey{person: true, id: *a.PersonID}
			c = &Candidate{PersonID: *a.PersonID, DisplayName: cmp.Or(a.PersonName, a.SenderName)}
		}
		if existing, ok := byKey[key]; ok {
			c = existing
		} else {
			byKey[key] = c
			order = append(order, key)
		}
		c.Messages = append(c.Messages, Authored{
			Similarity: similarity[id],
			Citation: Citation{
				MessageID: id, SentAt: a.SentAt, MessageType: a.MessageType,
				Subject: a.Subject, Snippet: a.Snippet,
			},
		})
	}
	var personIDs []int64
	for _, key := range order {
		if key.person {
			personIDs = append(personIDs, key.id)
		}
	}
	if len(personIDs) > 0 {
		values, err := s.Data.AskMeAboutValuesContext(ctx, personIDs)
		if err != nil {
			return nil, err
		}
		for _, v := range values {
			if c := byKey[candidateKey{person: true, id: v.PersonID}]; c != nil && Matches(freeText, v.Value) {
				c.AskMeAbout = append(c.AskMeAbout, v.Value)
			}
		}
	}
	out := make([]Candidate, len(order))
	for i, key := range order {
		out[i] = *byKey[key]
	}
	return out, nil
}
