// Package whoknows finds the people who wrote about a topic and cites the
// messages that show it.
package whoknows

import (
	"cmp"
	"context"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/gcal"
	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/hybrid"
)

// SearchPool is how many messages one search reads.
const SearchPool = 200

// Retriever runs the hybrid (keyword plus meaning) message search.
type Retriever interface {
	Search(ctx context.Context, request hybrid.SearchRequest) ([]vector.FusedHit, hybrid.ResultMeta, error)
}

// DataReader reads who wrote each message, who each sender is, and what
// people say to ask them about.
type DataReader interface {
	TopicAuthorsContext(ctx context.Context, messageIDs []int64) (map[int64]store.TopicAuthor, error)
	AuthorIdentitiesContext(ctx context.Context, senderIDs []int64) (map[int64]store.AuthorIdentity, error)
	AskMeAboutValuesContext(ctx context.Context) ([]store.PersonAskMeAbout, bool, error)
}

type Service struct {
	Retriever Retriever
	Data      DataReader
	// Index is the vector backend the search reads.
	Index      vector.FilteredCoverageBackend
	BuildScope vector.BuildScope
	Now        func() time.Time
}

// cosineScorer is an index whose search score is not cosine similarity, as
// sqlite-vec's 1 - L2 distance is not, so it scores the hits again.
type cosineScorer interface {
	MessageCosines(ctx context.Context, gen vector.GenerationID, queryVec []float32, messageIDs []int64) (map[int64]float64, error)
}

// searchTypes are the message types worth searching for authors: everything
// but meeting transcripts and calendar entries, which have organizers and
// attendees rather than authors. It also reports that the index scope leaves
// out some of those types or some accounts.
func searchTypes(scope vector.BuildScope) (types []string, narrowed bool) {
	allowed := slices.DeleteFunc(slices.Clone(query.KnownMessageTypes), func(t string) bool {
		return t == meetingarchive.MessageType || t == gcal.MessageTypeCalendarEvent
	})
	types, _ = search.IntersectValues(allowed, scope.MessageTypes)
	return types, len(types) < len(allowed) || len(scope.SourceIDs) > 0
}

// Find retrieves messages about the topic, keeps those whose cosine
// similarity to it reaches MinScore, credits each to the person who wrote it,
// and ranks the result.
func (s *Service) Find(ctx context.Context, req Request) (Response, error) {
	out := Response{Topic: req.Topic, Limits: []string{}, People: []Entry{}, UnsavedContacts: []Entry{}}
	p, err := req.Resolve()
	if err != nil {
		return out, err
	}
	out.Topic = p.Topic
	types, narrowed := searchTypes(s.BuildScope)
	if narrowed {
		out.Limits = append(out.Limits, LimitIndexScope)
	}
	candidates, err := s.searchAuthors(ctx, p, types, &out)
	if err != nil {
		return out, err
	}
	candidates, truncated, err := s.addDeclared(ctx, candidates, p.FreeText)
	if err != nil {
		return out, err
	}
	if truncated {
		out.Limits = append(out.Limits, LimitDeclaredTruncated)
	}
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	out.People, out.UnsavedContacts = Rank(candidates, p, now)
	return out, nil
}

// searchAuthors runs one hybrid search, credits the messages that reach the
// floor to their authors, and adds the limits the search ran into to out.
func (s *Service) searchAuthors(ctx context.Context, p Params, types []string, out *Response) ([]Candidate, error) {
	// An index limited to transcripts and calendar entries has no authors to
	// search, but people can still declare the topic.
	if len(types) == 0 {
		return nil, nil
	}
	request := hybrid.SearchRequest{
		Mode:     hybrid.ModeHybrid,
		FreeText: p.FreeText,
		Limit:    SearchPool,
		Filter:   vector.Filter{After: p.After, Before: p.Before, MessageTypes: types},
	}
	for _, term := range p.Terms {
		request.SubjectTerms = append(request.SubjectTerms, strings.ToLower(term))
	}
	hits, meta, err := s.Retriever.Search(ctx, request)
	if err != nil {
		return nil, err
	}
	scores, err := s.vectorScores(ctx, hits, meta)
	if err != nil {
		return nil, err
	}
	counted := similarities(scores, hits, p.MinScore)
	if len(counted) > 0 && (meta.PoolSaturated || len(hits) >= SearchPool) {
		out.Limits = append(out.Limits, LimitPoolFull)
	}
	unembedded, err := s.unembedded(ctx, hits, meta.Generation.ID)
	if err != nil {
		return nil, err
	}
	if unembedded {
		out.Limits = append(out.Limits, LimitNotEmbedded)
	}
	return s.attribute(ctx, counted)
}

// unembedded reports that a keyword-only hit has no vector in the searched
// generation, as mail synced since the last embedding run has none.
func (s *Service) unembedded(ctx context.Context, hits []vector.FusedHit, gen vector.GenerationID) (bool, error) {
	var keywordOnly []int64
	for _, hit := range hits {
		if math.IsNaN(hit.VectorScore) {
			keywordOnly = append(keywordOnly, hit.MessageID)
		}
	}
	if len(keywordOnly) == 0 {
		return false, nil
	}
	embedded, err := s.Index.EmbeddedMessageCountForIDs(ctx, gen, keywordOnly)
	return embedded < int64(len(keywordOnly)), err
}

type scoredHit struct {
	messageID  int64
	similarity float64
}

// vectorScores maps each hit the meaning search returned to its cosine
// similarity to the topic. A keyword-only hit has none.
func (s *Service) vectorScores(ctx context.Context, hits []vector.FusedHit, meta hybrid.ResultMeta) (map[int64]float64, error) {
	scores := map[int64]float64{}
	for _, hit := range hits {
		if !math.IsNaN(hit.VectorScore) {
			scores[hit.MessageID] = hit.VectorScore
		}
	}
	scorer, ok := s.Index.(cosineScorer)
	if !ok || len(scores) == 0 {
		return scores, nil
	}
	return scorer.MessageCosines(ctx, meta.Generation.ID, meta.QueryVector, slices.Collect(maps.Keys(scores)))
}

// similarities returns the hits whose cosine similarity to the topic reaches
// the floor, in search order. A hit with no score does not count.
func similarities(scores map[int64]float64, hits []vector.FusedHit, floor float64) []scoredHit {
	var out []scoredHit
	for _, hit := range hits {
		if similarity, ok := scores[hit.MessageID]; ok && similarity >= floor {
			out = append(out, scoredHit{messageID: hit.MessageID, similarity: similarity})
		}
	}
	return out
}

// attribute credits each counted message to its author's identity family,
// leaving out the owner, senders without an identity, and automated senders.
func (s *Service) attribute(ctx context.Context, counted []scoredHit) ([]Candidate, error) {
	if len(counted) == 0 {
		return nil, nil
	}
	ids := make([]int64, len(counted))
	for i, hit := range counted {
		ids[i] = hit.messageID
	}
	authors, err := s.Data.TopicAuthorsContext(ctx, ids)
	if err != nil {
		return nil, err
	}
	human := func(id int64) (store.TopicAuthor, bool) {
		a, ok := authors[id]
		return a, ok && !a.IsFromMe && a.SenderID != 0 && !a.Automated && !Automated(a.SenderEmail)
	}
	var senders []int64
	for _, id := range ids {
		if a, ok := human(id); ok && !slices.Contains(senders, a.SenderID) {
			senders = append(senders, a.SenderID)
		}
	}
	identities, err := s.Data.AuthorIdentitiesContext(ctx, senders)
	if err != nil {
		return nil, err
	}
	byKey := map[int64]int{}
	var out []Candidate
	for _, hit := range counted {
		a, ok := human(hit.messageID)
		identity := identities[a.SenderID]
		if !ok || identity.Owner {
			continue
		}
		i, exists := byKey[identity.Key]
		if !exists {
			c := Candidate{ParticipantID: a.SenderID, DisplayName: a.SenderName, EmailAddress: a.SenderEmail, OneWay: !identity.Addressed}
			if identity.PersonID != 0 {
				c = Candidate{PersonID: identity.PersonID, DisplayName: cmp.Or(identity.PersonName, a.SenderName), OneWay: !identity.Addressed}
			}
			out = append(out, c)
			i = len(out) - 1
			byKey[identity.Key] = i
		}
		out[i].Messages = append(out[i].Messages, Authored{
			Similarity: hit.similarity,
			ListMail:   a.ListMail,
			Citation: Citation{
				MessageID: a.MessageID, SentAt: a.SentAt, MessageType: a.MessageType,
				Subject: a.Subject, SenderAuthenticated: a.SenderAuthenticated,
			},
		})
	}
	return out, nil
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
	authored := map[int64]bool{}
	for i, c := range candidates {
		if c.PersonID != 0 {
			index[c.PersonID] = i
			authored[c.PersonID] = true
		}
	}
	for _, v := range values {
		// A person with matching messages gets a boost from a value that
		// shares words with the topic. A declared-only person needs a value
		// that names every topic word, so "sales" does not answer "sales tax".
		if !Matches(freeText, v.Value) || !authored[v.PersonID] && !Covers(v.Value, freeText) {
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
