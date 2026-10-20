// Package whoknows finds the people who wrote about a topic and cites the
// messages that show it.
package whoknows

import (
	"context"
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

// SearchPool caps how many fused hits one search reads. The hybrid engine
// draws at most SearchPool/2 hits from each of keyword and vector search,
// bounded further by k_per_signal, so both signals fit before fusion.
const SearchPool = 200

// Retriever runs the hybrid (keyword plus meaning) message search.
type Retriever interface {
	Search(ctx context.Context, request hybrid.SearchRequest) ([]vector.FusedHit, hybrid.ResultMeta, error)
}

// DataReader reads the owner's participants, who wrote each message, what
// people say to ask them about, and how many messages wait for a vector.
type DataReader interface {
	OwnerFamilyContext(ctx context.Context) ([]int64, error)
	TopicAuthorsContext(ctx context.Context, messageIDs, owner []int64) (map[int64]store.TopicAuthor, error)
	AskMeAboutValuesContext(ctx context.Context, owner []int64) ([]store.PersonAskMeAbout, bool, error)
	CoverageCountsScoped(ctx context.Context, activeGen int64, messageTypes []string, sourceIDs []int64) (live, stamped, blank, missing int64, err error)
}

type Service struct {
	Retriever Retriever
	Data      DataReader
	// Index is the vector backend the search reads.
	Index      vector.Backend
	BuildScope vector.BuildScope
	Now        func() time.Time
}

// cosineScorer scores retrieved messages from their stored vectors, including
// messages found only through keywords.
type cosineScorer interface {
	MessageCosines(ctx context.Context, gen vector.Generation, queryVec []float32, messageIDs []int64) (vector.CosineScores, error)
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

// searchResult is what one search found, whether it filled its pool, and
// the index generation it read.
type searchResult struct {
	candidates []Candidate
	poolFull   bool
	generation vector.GenerationID
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
	owner, err := s.Data.OwnerFamilyContext(ctx)
	if err != nil {
		return out, err
	}
	types, partial := searchTypes(s.BuildScope)
	found, err := s.searchAuthors(ctx, p, types, owner)
	if err != nil {
		return out, err
	}
	candidates, truncated, err := s.addDeclared(ctx, found.candidates, p.FreeText, owner)
	if err != nil {
		return out, err
	}
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	out.People, out.UnsavedContacts = Rank(candidates, p, now)
	if !partial && len(out.People) == 0 && len(out.UnsavedContacts) == 0 {
		partial = s.unembedded(ctx, found.generation, types)
	}
	if found.poolFull {
		out.Limits = append(out.Limits, LimitPoolFull)
	}
	if partial {
		out.Limits = append(out.Limits, LimitPartialIndex)
	}
	if truncated {
		out.Limits = append(out.Limits, LimitDeclaredTruncated)
	}
	return out, nil
}

// searchAuthors runs one hybrid search and credits the messages that reach
// the floor to their authors. The pool is full when the search filled it and
// either its lowest vector similarity reaches the floor or the index can't
// vouch that its order is cosine order, so more matches may lie beyond it.
func (s *Service) searchAuthors(ctx context.Context, p Params, types []string, owner []int64) (searchResult, error) {
	// An index limited to transcripts and calendar entries has no authors to
	// search, but people can still declare the topic.
	if len(types) == 0 {
		return searchResult{}, nil
	}
	request := hybrid.SearchRequest{
		Mode:       hybrid.ModeHybrid,
		FreeText:   p.FreeText,
		Limit:      SearchPool,
		KPerSignal: SearchPool / 2,
		Filter:     vector.Filter{After: p.After, Before: p.Before, MessageTypes: types},
	}
	for _, term := range p.Terms {
		request.SubjectTerms = append(request.SubjectTerms, strings.ToLower(term))
	}
	hits, meta, err := s.Retriever.Search(ctx, request)
	if err != nil {
		return searchResult{}, err
	}
	scores, err := s.vectorScores(ctx, hits, meta)
	if err != nil {
		return searchResult{}, err
	}
	counted := similarities(scores.Scores, hits, p.MinScore)
	lowest := math.Inf(1)
	for _, hit := range hits {
		if score, ok := scores.Scores[hit.MessageID]; ok && !math.IsNaN(hit.VectorScore) {
			lowest = math.Min(lowest, score)
		}
	}
	result := searchResult{generation: meta.Generation.ID}
	result.poolFull = (meta.PoolSaturated || len(hits) >= SearchPool) && (len(counted) > 0 && lowest >= p.MinScore || !scores.UnitLength)
	result.candidates, err = s.attribute(ctx, counted, owner)
	return result, err
}

// unembedded reports that a message of the searched types still waits for
// its vector in generation gen, or that the wait can't be counted. The search
// returns only messages with a vector or a keyword hit, so either way an
// empty answer can't say that nobody wrote about the topic.
func (s *Service) unembedded(ctx context.Context, gen vector.GenerationID, types []string) bool {
	live, stamped, _, _, err := s.Data.CoverageCountsScoped(ctx, int64(gen), types, nil)
	return err != nil || live > stamped
}

type scoredHit struct {
	messageID  int64
	similarity float64
}

// vectorScores maps each embedded hit to its cosine similarity to the topic.
// Backends without a cosine scorer use the meaning search's scores.
func (s *Service) vectorScores(ctx context.Context, hits []vector.FusedHit, meta hybrid.ResultMeta) (vector.CosineScores, error) {
	if scorer, ok := s.Index.(cosineScorer); ok {
		ids := make([]int64, len(hits))
		for i, hit := range hits {
			ids[i] = hit.MessageID
		}
		return scorer.MessageCosines(ctx, meta.Generation, meta.QueryVector, ids)
	}
	scores := map[int64]float64{}
	for _, hit := range hits {
		if !math.IsNaN(hit.VectorScore) {
			scores[hit.MessageID] = hit.VectorScore
		}
	}
	return vector.CosineScores{Scores: scores, UnitLength: true}, nil
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

// attribute credits each counted message to its author's contact. The store
// decides which messages count.
func (s *Service) attribute(ctx context.Context, counted []scoredHit, owner []int64) ([]Candidate, error) {
	if len(counted) == 0 {
		return nil, nil
	}
	ids := make([]int64, len(counted))
	for i, hit := range counted {
		ids[i] = hit.messageID
	}
	authors, err := s.Data.TopicAuthorsContext(ctx, ids, owner)
	if err != nil {
		return nil, err
	}
	byKey := map[int64]int{}
	var out []Candidate
	for _, hit := range counted {
		a, ok := authors[hit.messageID]
		if !ok || !a.Counts {
			continue
		}
		contact := a.Contact
		i, exists := byKey[contact.Key]
		if !exists {
			c := Candidate{PersonID: contact.PersonID, DisplayName: contact.Name, OneWay: contact.OneWay}
			if contact.PersonID == 0 {
				c.ParticipantID, c.EmailAddress = contact.Key, contact.Email
			}
			out = append(out, c)
			i = len(out) - 1
			byKey[contact.Key] = i
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
	ctx context.Context, candidates []Candidate, freeText string, owner []int64,
) ([]Candidate, bool, error) {
	values, truncated, err := s.Data.AskMeAboutValuesContext(ctx, owner)
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
		if authored[v.PersonID] && !Matches(freeText, v.Value) || !authored[v.PersonID] && !Covers(v.Value, freeText) {
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
