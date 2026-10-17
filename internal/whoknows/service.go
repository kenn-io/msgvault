package whoknows

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/identityindex"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/hybrid"
	"go.kenn.io/msgvault/internal/vector/personsearch"
)

var ErrPersonScopeUnavailable = errors.New("people resolution is unavailable")

type Retriever interface {
	Search(ctx context.Context, request hybrid.SearchRequest) ([]vector.FusedHit, hybrid.ResultMeta, error)
}
type RoleReader interface {
	PeopleRolesForMessages(ctx context.Context, messageIDs []int64) (store.PeopleRoles, error)
	PeopleRolesForMessagesWithRosterLimit(ctx context.Context, messageIDs []int64, maxRosterEdges int) (store.PeopleRoles, error)
}
type DeclaredReader interface {
	PeopleDeclaredTopics(ctx context.Context) ([]store.PersonDeclaredTopic, error)
}
type ProfileReader interface {
	PeopleProfiles(ctx context.Context, personIDs []int64) (map[int64]store.PeopleProfile, error)
	PeopleEmployments(ctx context.Context, personIDs []int64) ([]store.PeopleEmployment, error)
}
type NativeOwnerReader interface {
	PeopleNativeOwnersContext(ctx context.Context, refs []store.SourceParticipantRef) (map[store.SourceParticipantRef]bool, error)
}
type DataReader interface {
	RoleReader
	DeclaredReader
	ProfileReader
	NativeOwnerReader
}
type ProfileSearcher interface {
	Search(ctx context.Context, topic string, limit int) ([]personsearch.Result, error)
	SearchVector(ctx context.Context, query []float32, generation vector.GenerationID, limit int) ([]personsearch.Result, error)
}
type Service struct {
	Retriever         Retriever
	Data              DataReader
	ProfileSearcher   ProfileSearcher
	PeopleEnabled     bool
	BuildScope        vector.BuildScope
	KPerSignal        int
	Now               func() time.Time
	CoverageReader    func(context.Context) (vector.EmbeddingStatus, error)
	TemperatureReader func(context.Context, map[int64][]int64) (map[int64]int, bool, error)
	ExcerptBuilder    func(context.Context, vector.GenerationID, []float32, int64, float64) (*Excerpt, error)
}
type candidateKey struct {
	person bool
	id     int64
}

func keyFor(e store.RoleEdge) candidateKey {
	if e.PersonID != nil {
		return candidateKey{true, *e.PersonID}
	}
	return candidateKey{false, e.ParticipantID}
}
func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
func generationView(g vector.Generation) Generation {
	return Generation{ID: int64(g.ID), State: string(g.State), Model: g.Model, Fingerprint: g.Fingerprint, Dimension: g.Dimension}
}

// Find runs one bounded retrieval and attributes evidence without reading bodies.
func (s *Service) Find(ctx context.Context, topic string, p Params) (Response, error) {
	start := time.Now()
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	out := Response{Topic: strings.TrimSpace(topic), GeneratedAt: now, Params: p, LimitsApplied: []string{}, People: []Ranked{}, UnsavedCandidates: []UnsavedCandidate{}, Organizations: nil, Tiebreak: Tiebreak{Requested: p.Tiebreak, Applied: p.Tiebreak}}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if err := p.Validate(); err != nil {
		return out, err
	}
	terms := search.Parse(topic).TextTerms
	freeText := strings.Join(terms, " ")
	if strings.TrimSpace(freeText) == "" {
		return out, ErrMissingFreeText
	}
	p.topic = freeText
	if s.Retriever == nil {
		return out, vector.ErrNotEnabled
	}
	if s.Data == nil {
		return out, ErrPersonScopeUnavailable
	}
	request := hybrid.SearchRequest{Mode: hybrid.ModeHybrid, FreeText: freeText, Limit: p.Candidates, Filter: vector.Filter{After: p.After, Before: p.Before, MessageTypes: slices.Clone(s.BuildScope.MessageTypes)}}
	for _, term := range terms {
		request.SubjectTerms = append(request.SubjectTerms, strings.ToLower(term))
	}
	hits, meta, err := s.Retriever.Search(ctx, request)
	if err != nil {
		return out, err
	}
	k := s.KPerSignal
	if k <= 0 {
		k = 100
	}
	effective := min(p.Candidates, 2*k)
	if len(hits) > effective {
		hits = hits[:effective]
	}
	out.Retrieval = Retrieval{Mode: "hybrid", CandidatesRequested: p.Candidates, CandidatesEffective: effective, PoolSaturated: meta.PoolSaturated, Generation: generationView(meta.Generation), Accelerator: meta.Accelerator, Timings: Timings{QueryEmbeddingMS: ms(meta.QueryEmbeddingDuration), RetrievalMS: ms(meta.RetrievalDuration)}}
	for _, hit := range hits {
		if !math.IsNaN(hit.VectorScore) && !math.IsInf(hit.VectorScore, 0) && (out.Retrieval.TopVectorScore == nil || hit.VectorScore > *out.Retrieval.TopVectorScore) {
			out.Retrieval.TopVectorScore = new(hit.VectorScore)
		}
	}
	ids := make([]int64, len(hits))
	for i, hit := range hits {
		ids[i] = hit.MessageID
	}
	phase := time.Now()
	roles, err := s.Data.PeopleRolesForMessagesWithRosterLimit(ctx, ids, p.MaxRosterEdges)
	if err != nil {
		return out, fmt.Errorf("attribute topic evidence: %w", err)
	}
	nativeOwnerRefs := []store.SourceParticipantRef{}
	for _, m := range roles.Messages {
		for _, edge := range m.Edges {
			if edge.PersonID == nil {
				nativeOwnerRefs = append(nativeOwnerRefs, store.SourceParticipantRef{SourceID: m.SourceID, ParticipantID: edge.ParticipantID})
			}
		}
	}
	nativeOwners, err := s.Data.PeopleNativeOwnersContext(ctx, nativeOwnerRefs)
	if err != nil {
		return out, fmt.Errorf("resolve native owners: %w", err)
	}
	ownerForEdge := func(m store.MessageRoles, edge store.RoleEdge) bool {
		if edge.IsOwner {
			return true
		}
		if edge.PersonID == nil {
			return nativeOwners[store.SourceParticipantRef{SourceID: m.SourceID, ParticipantID: edge.ParticipantID}]
		}
		return false
	}
	out.Retrieval.Timings.AttributionMS = ms(time.Since(phase))
	out.Excluded.LargeRooms = len(roles.LargeRoomMessageIDs)
	candidates := map[candidateKey]*Candidate{}
	owners := map[candidateKey]bool{}
	automated := map[int64]bool{}
	automatedPeople := map[candidateKey]bool{}
	get := func(key candidateKey) *Candidate {
		c := candidates[key]
		if c == nil {
			c = &Candidate{Evidence: []Evidence{}, AskMeAboutMatches: []string{}, Organizations: []Organization{}}
			if key.person {
				c.PersonID = key.id
			} else {
				c.ParticipantID = key.id
			}
			candidates[key] = c
		}
		return c
	}
	// Resolve all exclusions before attribution so input ordering cannot change ownership.
	for _, m := range roles.Messages {
		for _, e := range m.Edges {
			if ownerForEdge(m, e) {
				owners[keyFor(e)] = true
			}
			if e.Role == "from" && Automated(e.EmailAddress) {
				automated[e.ParticipantID] = true
				automatedPeople[keyFor(e)] = true
			}
		}
	}
	topRRF := 0.0
	if len(hits) > 0 {
		topRRF = hits[0].RRFScore
	}
	seenMessages := map[int64]bool{}
	for _, hit := range hits {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if seenMessages[hit.MessageID] {
			continue
		}
		seenMessages[hit.MessageID] = true
		m, ok := roles.Messages[hit.MessageID]
		if !ok {
			continue
		}
		if slices.Contains(p.ExcludedMessageTypes, m.MessageType) {
			out.Excluded.MessageTypes++
			continue
		}
		hasVector := !math.IsNaN(hit.VectorScore) && !math.IsInf(hit.VectorScore, 0)
		if p.MinScore != 0 && (!hasVector || hit.VectorScore < p.MinScore) {
			continue
		}
		auto, hasSender := false, false
		for _, e := range m.Edges {
			if e.Role == "from" {
				hasSender = true
				if automated[e.ParticipantID] {
					auto = true
				}
			}
		}
		if auto {
			continue
		}
		if !hasSender {
			out.Excluded.UnresolvedMessages++
		}
		if m.IsFromMe && !p.IncludeOwner {
			out.Excluded.OwnerMessages++
		}
		rel := 0.0
		if topRRF > 0 && !math.IsNaN(hit.RRFScore) && !math.IsInf(hit.RRFScore, 0) {
			rel = math.Max(0, math.Min(1, hit.RRFScore/topRRF))
		}
		if !hasVector {
			rel = math.Min(.5, rel)
		}
		retained := false
		for _, edge := range m.Edges {
			key := keyFor(edge)
			if owners[key] && !p.IncludeOwner || automatedPeople[key] {
				continue
			}
			if !key.person && !p.IncludeUnsaved {
				continue
			}
			role := string(edge.Role)
			c := get(key)
			c.DisplayName = edge.DisplayName
			c.EmailAddress = edge.EmailAddress
			evidence := Evidence{SenderAuthenticated: store.SourceAuthenticatesSender(m.SourceType), MessageID: m.MessageID, SourceMessageID: m.SourceMessageID, ConversationID: m.ConversationID, MessageType: m.MessageType, Role: role, SentAt: m.SentAt, Relevance: rel, Subject: m.Subject, Snippet: m.Snippet, Flags: []string{}, Channel: "other"}
			if hasVector {
				evidence.VectorScore = new(hit.VectorScore)
			} else {
				evidence.Flags = append(evidence.Flags, "bm25_only")
			}
			if m.MessageType == "email" {
				evidence.Channel = "email"
			} else if identityindex.IsChat(m.MessageType, m.ConversationType) {
				evidence.Channel = "chat"
			} else if m.MessageType == "meeting_transcript" {
				evidence.Channel = "meeting"
			}
			if role == "from" && m.ListID != "" {
				evidence.Flags = append(evidence.Flags, "via_list")
			}
			if role == "from" && ownerForEdge(m, edge) && p.IncludeOwner && m.MessageType != "meeting_transcript" {
				evidence.Flags = append(evidence.Flags, "owner_authored")
			}
			c.Evidence = append(c.Evidence, evidence)
			retained = true
		}
		if retained {
			out.Retrieval.CandidatesAfterFilters++
		}
	}
	phase = time.Now()
	declared, err := s.Data.PeopleDeclaredTopics(ctx)
	if err != nil {
		return out, fmt.Errorf("read topic declarations: %w", err)
	}
	if len(declared) > MaxDeclaredValues {
		out.LimitsApplied = append(out.LimitsApplied, "declared_values")
		declared = declared[:MaxDeclaredValues]
	}
	for _, v := range declared {
		key := candidateKey{true, v.PersonID}
		if Matches(freeText, v.Value) && (!owners[key] || p.IncludeOwner) && !automatedPeople[key] {
			c := get(key)
			c.AskMeAboutMatches = append(c.AskMeAboutMatches, v.Value)
		}
	}
	out.Retrieval.Timings.DeclaredMS = ms(time.Since(phase))
	profileState := "disabled"
	if s.PeopleEnabled {
		profileState = "unavailable"
	}
	profileScores := map[int64]float64{}
	out.Coverage.PeopleIndex = PeopleCoverage{Enabled: s.PeopleEnabled}
	phase = time.Now()
	if s.PeopleEnabled && s.ProfileSearcher != nil {
		profileState = "not_in_top"
		results, profileErr := s.ProfileSearcher.SearchVector(ctx, meta.QueryVector, meta.Generation.ID, 10)
		if errors.Is(profileErr, personsearch.ErrGenerationMismatch) {
			embeddingStart := time.Now()
			results, profileErr = s.ProfileSearcher.Search(ctx, freeText, 10)
			out.Retrieval.Timings.ProfileEmbeddingMS = ms(time.Since(embeddingStart))
		}
		if profileErr != nil {
			if err := ctx.Err(); err != nil {
				return out, err
			}
			if errors.Is(profileErr, vector.ErrSemanticPersonEmbeddingsDisabled) || errors.Is(profileErr, vector.ErrSemanticPersonEmbeddingConsentRequired) {
				profileState = "disabled"
			} else {
				profileState = "unavailable"
				out.Coverage.PeopleIndex.Consented = errors.Is(profileErr, personsearch.ErrPersonCoverageIncomplete)
			}
		}
		if profileErr == nil {
			out.Coverage.PeopleIndex.Consented = true
			out.Coverage.PeopleIndex.Complete = true
			for _, r := range results[:min(10, len(results))] {
				if math.IsNaN(r.Score) || math.IsInf(r.Score, 0) {
					continue
				}
				profileScores[r.Person.ID] = r.Score
				if r.Score > p.MinProfileScore || r.Score == 1 && p.MinProfileScore == 1 {
					key := candidateKey{true, r.Person.ID}
					if (!owners[key] || p.IncludeOwner) && !automatedPeople[key] {
						get(key)
					}
				}
			}
		}
	}
	for _, c := range candidates {
		c.ProfileSearch = profileState
		if score, ok := profileScores[c.PersonID]; ok && c.PersonID != 0 {
			c.ProfileScore = new(score)
			c.ProfileSearch = "below_threshold"
			if score >= p.MinProfileScore {
				c.ProfileSearch = "ranked"
			}
		}
	}
	// Bound all candidate-dependent reads; prefer authored evidence before boosts.
	bounded := make([]Candidate, 0, len(candidates))
	for _, c := range candidates {
		bounded = append(bounded, *c)
	}
	if len(bounded) > p.MaxCandidatePeople {
		raw := make(map[candidateKey][2]float64, len(bounded))
		for _, c := range bounded {
			ranked := Rank([]Candidate{c}, p, now)
			if len(ranked) > 0 {
				b := ranked[0].Breakdown
				key := candidateKey{c.PersonID != 0, c.PersonID}
				if c.PersonID == 0 {
					key.id = c.ParticipantID
				}
				raw[key] = [2]float64{b.RawAuthored, b.RawAuthored + b.RawReceived}
			}
		}
		slices.SortFunc(bounded, func(a, b Candidate) int {
			ka, kb := candidateKey{a.PersonID != 0, a.PersonID}, candidateKey{b.PersonID != 0, b.PersonID}
			if a.PersonID == 0 {
				ka.id = a.ParticipantID
			}
			if b.PersonID == 0 {
				kb.id = b.ParticipantID
			}
			for i := range 2 {
				if n := cmp.Compare(raw[kb][i], raw[ka][i]); n != 0 {
					return n
				}
			}
			if n := cmp.Compare(ka.id, kb.id); n != 0 {
				return n
			}
			if ka.person == kb.person {
				return 0
			}
			if ka.person {
				return -1
			}
			return 1
		})
		bounded = bounded[:p.MaxCandidatePeople]
		out.LimitsApplied = append(out.LimitsApplied, "candidate_people")
	}
	personIDs := []int64{}
	for _, c := range bounded {
		if c.PersonID != 0 {
			personIDs = append(personIDs, c.PersonID)
		}
	}
	slices.Sort(personIDs)
	profiles, err := s.Data.PeopleProfiles(ctx, personIDs)
	if err != nil {
		return out, fmt.Errorf("read candidate profiles: %w", err)
	}
	employments, err := s.Data.PeopleEmployments(ctx, personIDs)
	if err != nil {
		return out, fmt.Errorf("read candidate employment: %w", err)
	}
	byPerson := map[int64][]Organization{}
	for _, e := range employments {
		byPerson[e.PersonID] = append(byPerson[e.PersonID], Organization{OrganizationID: e.OrganizationID, Name: e.Name, NameNormalized: e.NameNormalized, PrimaryDomain: e.PrimaryDomain, Title: e.Title})
	}
	ready := make([]Candidate, 0, len(bounded))
	members := map[int64][]int64{}
	for _, c := range bounded {
		if c.PersonID != 0 {
			profile, exists := profiles[c.PersonID]
			if !exists {
				continue
			}
			key := candidateKey{true, c.PersonID}
			if profile.IsOwner {
				owners[key] = true
			}
			for participantID, email := range profile.ParticipantEmails {
				if Automated(email) {
					automated[participantID] = true
					automatedPeople[key] = true
				}
			}
			if automatedPeople[key] {
				continue
			}
			if owners[key] && !p.IncludeOwner {
				continue
			}
			c.VCardUID = profile.VCardUID
			c.DisplayName = profile.DisplayName
			c.Organizations = byPerson[c.PersonID]
			if profile.ContactStateFresh {
				c.OneWay = profile.InteractionCount >= 20 && profile.LastOutboundAt == nil
				c.LastContactAt = profile.LastContactAt
			}
			members[c.PersonID] = profile.ParticipantIDs
		}
		ready = append(ready, c)
	}
	out.Excluded.AutomatedSenders = len(automated)
	if !p.IncludeOwner {
		out.Excluded.OwnerPeople = len(owners)
	}
	out.Retrieval.Timings.ProfileMS = ms(time.Since(phase))
	phase = time.Now()
	if p.Tiebreak == "temperature" {
		published := false
		if s.TemperatureReader != nil {
			temperatures, available, tempErr := s.TemperatureReader(ctx, members)
			if err := ctx.Err(); err != nil {
				return out, err
			}
			published = available && tempErr == nil
			if published {
				for i := range ready {
					if temperature, ok := temperatures[ready[i].PersonID]; ok {
						ready[i].Temperature = new(temperature)
					}
				}
			}
		}
		if !published {
			p.Tiebreak = "last_contact"
			out.Tiebreak.Applied = "last_contact"
		}
	}
	out.Retrieval.Timings.TiebreakMS = ms(time.Since(phase))
	ranked := Rank(ready, p, now)
	for _, r := range ranked {
		if r.PersonID != 0 {
			if len(out.People) < p.Limit {
				r.Rank = len(out.People) + 1
				out.People = append(out.People, r)
			}
		} else if p.IncludeUnsaved && len(out.UnsavedCandidates) < 5 {
			r.Rank = len(out.UnsavedCandidates) + 1
			out.UnsavedCandidates = append(out.UnsavedCandidates, unsavedResult(r))
		}
	}
	out.Retrieval.Underfilled = out.Retrieval.CandidatesAfterFilters*2 < effective || (len(out.People) < p.Limit && meta.PoolSaturated)
	if len(out.People) == 0 && len(out.UnsavedCandidates) == 0 {
		out.Reason = new("no_relevant_evidence")
	}
	if p.IncludeOrganizations {
		out.Organizations = groupOrganizations(out.People)
	}
	if err := s.populateCoverage(ctx, &out, meta, profiles); err != nil {
		return out, err
	}
	if p.Excerpts {
		phase = time.Now()
		s.excerpts(ctx, &out, meta, p.MinScore)
		out.Retrieval.Timings.HydrationMS = ms(time.Since(phase))
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	out.Retrieval.TookMS = ms(time.Since(start))
	return out, nil
}

func groupOrganizations(people []Ranked) []OrganizationGroup {
	groups := map[int64]*OrganizationGroup{}
	for _, p := range people {
		seen := map[int64]bool{}
		for _, o := range p.Organizations {
			if seen[o.OrganizationID] {
				continue
			}
			seen[o.OrganizationID] = true
			g := groups[o.OrganizationID]
			if g == nil {
				g = &OrganizationGroup{OrganizationID: o.OrganizationID, Name: o.Name, People: []int64{}}
				groups[o.OrganizationID] = g
			}
			g.Score += p.Score
			g.People = append(g.People, p.PersonID)
		}
	}
	out := make([]OrganizationGroup, 0, len(groups))
	for _, g := range groups {
		slices.Sort(g.People)
		out = append(out, *g)
	}
	slices.SortFunc(out, func(a, b OrganizationGroup) int {
		if n := cmp.Compare(b.Score, a.Score); n != 0 {
			return n
		}
		return cmp.Compare(a.OrganizationID, b.OrganizationID)
	})
	return out[:min(5, len(out))]
}
func (s *Service) excerpts(ctx context.Context, out *Response, meta hybrid.ResultMeta, minScore float64) {
	type result struct {
		excerpt *Excerpt
		err     error
	}
	cache := map[int64]result{}
	visit := func(evidence []Evidence) {
		for j := range evidence {
			e := &evidence[j]
			if ctx.Err() != nil {
				return
			}
			r, exists := cache[e.MessageID]
			if !exists {
				if s.ExcerptBuilder == nil {
					r.err = errors.New("chunk scoring unavailable")
				} else {
					r.excerpt, r.err = s.ExcerptBuilder(ctx, meta.Generation.ID, meta.QueryVector, e.MessageID, minScore)
				}
				cache[e.MessageID] = r
			}
			e.Excerpt = r.excerpt
			e.ExcerptError = r.err != nil
		}
	}
	for i := range out.People {
		visit(out.People[i].Evidence)
	}
	for i := range out.UnsavedCandidates {
		visit(out.UnsavedCandidates[i].Evidence)
	}
}
