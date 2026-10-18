package whoknows

import (
	"context"
	"database/sql"
	"errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/hybrid"
	"go.kenn.io/msgvault/internal/vector/personsearch"
	"math"
	"testing"
	"time"
)

type fixedRetriever struct {
	hits    []vector.FusedHit
	request hybrid.SearchRequest
}

func (r *fixedRetriever) Search(ctx context.Context, q hybrid.SearchRequest) ([]vector.FusedHit, hybrid.ResultMeta, error) {
	r.request = q
	if err := ctx.Err(); err != nil {
		return nil, hybrid.ResultMeta{}, err
	}
	return r.hits, hybrid.ResultMeta{Generation: vector.Generation{ID: 1, State: vector.GenerationActive}, PoolSaturated: true, QueryVector: []float32{1, 0, 0}}, nil
}

func TestServiceMetadataAndDeclarations(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	f := storetest.New(t)
	now := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	owner := f.EnsureParticipant("owner@example.net", "Owner", "example.net")
	author := f.EnsureParticipant("author@example.net", "Author", "example.net")
	declared := f.EnsureParticipant("declared@example.net", "Declared", "example.net")
	unsaved := f.EnsureParticipant("unsaved@example.net", "Unsaved", "example.net")
	ownerPerson, _, err := f.Store.CreatePersonFromParticipant(owner)
	must.NoError(err)
	authorPerson, _, err := f.Store.CreatePersonFromParticipant(author)
	must.NoError(err)
	declaredPerson, _, err := f.Store.CreatePersonFromParticipant(declared)
	must.NoError(err)
	must.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.net", "manual"))
	_, err = f.Store.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{PersonID: declaredPerson.ID, DefinitionSlug: store.AttributeSlugAskMeAbout, Value: store.AttributeValue{Type: store.AttributeValueText, Text: new("kubernetes")}, Source: store.ProvenanceUser})
	must.NoError(err)
	retriever := &fixedRetriever{}
	for i, sender := range []int64{author, owner, unsaved} {
		id, err := f.Store.UpsertMessage(&store.Message{ConversationID: f.ConvID, SourceID: f.Source.ID, SourceMessageID: []string{"author", "owner", "unsaved"}[i], MessageType: "email", SenderID: sql.NullInt64{Int64: sender, Valid: true}, SentAt: sql.NullTime{Time: now, Valid: true}})
		must.NoError(err)
		must.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{declared}, []string{"Declared"}))
		retriever.hits = append(retriever.hits, vector.FusedHit{MessageID: id, RRFScore: 1, VectorScore: .9})
	}
	s := Service{Retriever: retriever, Data: f.Store, KPerSignal: 100, Now: func() time.Time { return now }}
	got, err := s.Find(t.Context(), "kubernetes", DefaultParams())
	must.NoError(err)
	must.Len(got.People, 2)
	check.Equal(declaredPerson.ID, got.People[0].PersonID)
	check.Equal(authorPerson.ID, got.People[1].PersonID)
	check.Equal(1, got.Excluded.OwnerPeople)
	check.Equal(1, got.Excluded.OwnerMessages)
	must.Len(got.UnsavedCandidates, 1)
	check.Contains(got.UnsavedCandidates[0].PromoteHint, "person promote")
	check.True(retriever.request.Filter.IsEmpty())
	check.True(got.Retrieval.Underfilled)
	check.Equal("disabled", got.People[0].Breakdown.ProfileSearch)
	check.True(got.Coverage.Stale)
	p := DefaultParams()
	p.IncludeOwner = true
	got, err = s.Find(t.Context(), "kubernetes", p)
	must.NoError(err)
	ids := []int64{}
	for _, r := range got.People {
		ids = append(ids, r.PersonID)
	}
	check.Contains(ids, ownerPerson.ID)
	p.MinScore = 1
	got, err = s.Find(t.Context(), "unrelated", p)
	must.NoError(err)
	check.Empty(got.People)
	must.NotNil(got.Reason)
	check.Equal("no_relevant_evidence", *got.Reason)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = s.Find(ctx, "topic", p)
	must.ErrorIs(err, context.Canceled)
	retriever.hits[0].VectorScore = math.NaN()
	p = DefaultParams()
	p.IncludeOwner = true
	got, err = s.Find(t.Context(), "topic", p)
	must.NoError(err)
	for _, r := range got.People {
		for _, e := range r.Evidence {
			if e.MessageID == retriever.hits[0].MessageID {
				check.Nil(e.VectorScore)
				check.LessOrEqual(e.Relevance, .5)
			}
		}
	}
}

func TestServiceAppliesConfiguredLimits(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	f := storetest.New(t)
	now := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	authors := []int64{}
	retriever := &fixedRetriever{}
	for i, email := range []string{"limit-one@example.net", "limit-two@example.net"} {
		participant := f.EnsureParticipant(email, email, "example.net")
		_, _, err := f.Store.CreatePersonFromParticipant(participant)
		must.NoError(err)
		authors = append(authors, participant)
		id, err := f.Store.UpsertMessage(&store.Message{
			ConversationID: f.ConvID, SourceID: f.Source.ID, SourceMessageID: email,
			MessageType: "email", SenderID: sql.NullInt64{Int64: participant, Valid: true},
			SentAt: sql.NullTime{Time: now.Add(time.Duration(i) * time.Hour), Valid: true},
		})
		must.NoError(err)
		retriever.hits = append(retriever.hits, vector.FusedHit{MessageID: id, RRFScore: 1, VectorScore: .9})
	}
	rosterConversation, err := f.Store.EnsureConversationWithType(f.Source.ID, "bounded-roster", "group_chat", "Topic")
	must.NoError(err)
	for _, participant := range authors {
		must.NoError(f.Store.EnsureConversationParticipant(rosterConversation, participant, "member"))
	}
	rosterMessage, err := f.Store.UpsertMessage(&store.Message{
		ConversationID: rosterConversation, SourceID: f.Source.ID, SourceMessageID: "bounded-roster-message",
		MessageType: "chat", SenderID: sql.NullInt64{Int64: authors[0], Valid: true},
		SentAt: sql.NullTime{Time: now, Valid: true},
	})
	must.NoError(err)
	retriever.hits = append(retriever.hits, vector.FusedHit{MessageID: rosterMessage, RRFScore: 1, VectorScore: .9})

	s := Service{Retriever: retriever, Data: f.Store, KPerSignal: 100, Now: func() time.Time { return now }}
	p := DefaultParams()
	p.MaxRosterEdges = 1
	p.MaxCandidatePeople = 1
	got, err := s.Find(t.Context(), "topic", p)
	must.NoError(err)
	check.Equal(1, got.Params.MaxRosterEdges)
	check.Equal(1, got.Params.MaxCandidatePeople)
	check.Positive(got.Excluded.LargeRooms)
	check.Contains(got.LimitsApplied, "candidate_people")
	must.Len(got.People, 1)
}

func TestServiceExcludesAutomatedDeclaredPersonOutsideRetrievedMessages(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	f := storetest.New(t)
	bot := f.EnsureParticipant("newsletter@example.net", "Newsletter", "example.net")
	botPerson, _, err := f.Store.CreatePersonFromParticipant(bot)
	must.NoError(err)
	_, err = f.Store.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{
		PersonID: botPerson.ID, DefinitionSlug: store.AttributeSlugAskMeAbout,
		Value:  store.AttributeValue{Type: store.AttributeValueText, Text: new("distributed systems")},
		Source: store.ProvenanceUser,
	})
	must.NoError(err)
	botMessage, err := f.Store.UpsertMessage(&store.Message{
		ConversationID: f.ConvID, SourceID: f.Source.ID, SourceMessageID: "newsletter-archive",
		MessageType: "email", SenderID: sql.NullInt64{Int64: bot, Valid: true},
	})
	must.NoError(err)
	check.Positive(botMessage)

	human := f.EnsureParticipant("human@example.net", "Human", "example.net")
	humanPerson, _, err := f.Store.CreatePersonFromParticipant(human)
	must.NoError(err)
	humanMessage, err := f.Store.UpsertMessage(&store.Message{
		ConversationID: f.ConvID, SourceID: f.Source.ID, SourceMessageID: "human-topic",
		MessageType: "email", SenderID: sql.NullInt64{Int64: human, Valid: true},
	})
	must.NoError(err)

	retriever := &fixedRetriever{hits: []vector.FusedHit{{MessageID: humanMessage, RRFScore: 1, VectorScore: .9}}}
	s := Service{Retriever: retriever, Data: f.Store, KPerSignal: 100}
	got, err := s.Find(t.Context(), "distributed systems", DefaultParams())
	must.NoError(err)
	must.Len(got.People, 1)
	check.Equal(humanPerson.ID, got.People[0].PersonID)
	check.Equal(1, got.Excluded.AutomatedSenders)
}

func TestServiceExcludesUnsavedNativeOwnerOutsideRetrievalPool(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	f := storetest.New(t)
	now := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	source, err := f.Store.GetOrCreateSource("beeper", "beeper-account")
	must.NoError(err)
	conversation, err := f.Store.EnsureConversationWithType(source.ID, "direct-chat", "direct_chat", "Topic")
	must.NoError(err)
	owner := f.EnsureParticipant("native-owner@example.net", "Native Owner", "example.net")
	helper := f.EnsureParticipant("helper@example.net", "Helper", "example.net")
	_, _, err = f.Store.CreatePersonFromParticipant(helper)
	must.NoError(err)
	olderOutgoingID, err := f.Store.UpsertMessage(&store.Message{
		ConversationID: conversation, SourceID: source.ID, SourceMessageID: "older-outgoing",
		MessageType: "chat",
		IsFromMe:    true, SentAt: sql.NullTime{Time: now.Add(-time.Hour), Valid: true},
	})
	must.NoError(err)
	must.NoError(f.Store.ReplaceMessageRecipients(olderOutgoingID, "from", []int64{owner}, []string{"Native Owner"}))
	hitID, err := f.Store.UpsertMessage(&store.Message{
		ConversationID: conversation, SourceID: source.ID, SourceMessageID: "incoming-hit",
		MessageType: "chat", SenderID: sql.NullInt64{Int64: helper, Valid: true},
		SentAt: sql.NullTime{Time: now, Valid: true},
	})
	must.NoError(err)
	must.NoError(f.Store.ReplaceMessageRecipients(hitID, "to", []int64{owner}, []string{"Native Owner"}))
	s := Service{
		Retriever: &fixedRetriever{hits: []vector.FusedHit{{MessageID: hitID, RRFScore: 1, VectorScore: .9}}},
		Data:      f.Store,
		Now:       func() time.Time { return now },
	}
	got, err := s.Find(t.Context(), "topic", DefaultParams())
	must.NoError(err)
	check.Empty(got.UnsavedCandidates)
	check.Positive(got.Excluded.OwnerPeople)
}

func TestServiceExcludesSavedNativeOwnerOutsideRetrievalPool(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	f := storetest.New(t)
	now := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	source, err := f.Store.GetOrCreateSource("beeper", "beeper-account")
	must.NoError(err)
	conversation, err := f.Store.EnsureConversationWithType(source.ID, "saved-owner-chat", "direct_chat", "Topic")
	must.NoError(err)
	owner := f.EnsureParticipant("saved-native-owner@example.net", "Saved Native Owner", "example.net")
	helper := f.EnsureParticipant("saved-owner-helper@example.net", "Saved Owner Helper", "example.net")
	ownerPerson, _, err := f.Store.CreatePersonFromParticipant(owner)
	must.NoError(err)
	_, _, err = f.Store.CreatePersonFromParticipant(helper)
	must.NoError(err)
	olderOutgoingID, err := f.Store.UpsertMessage(&store.Message{
		ConversationID: conversation, SourceID: source.ID, SourceMessageID: "older-saved-outgoing",
		MessageType: "chat", IsFromMe: true, SentAt: sql.NullTime{Time: now.Add(-time.Hour), Valid: true},
	})
	must.NoError(err)
	must.NoError(f.Store.ReplaceMessageRecipients(olderOutgoingID, "from", []int64{owner}, []string{"Saved Native Owner"}))
	hitID, err := f.Store.UpsertMessage(&store.Message{
		ConversationID: conversation, SourceID: source.ID, SourceMessageID: "saved-owner-incoming-hit",
		MessageType: "chat", SenderID: sql.NullInt64{Int64: helper, Valid: true},
		SentAt: sql.NullTime{Time: now, Valid: true},
	})
	must.NoError(err)
	must.NoError(f.Store.ReplaceMessageRecipients(hitID, "to", []int64{owner}, []string{"Saved Native Owner"}))
	s := Service{
		Retriever: &fixedRetriever{hits: []vector.FusedHit{{MessageID: hitID, RRFScore: 1, VectorScore: .9}}},
		Data:      f.Store,
		Now:       func() time.Time { return now },
	}
	got, err := s.Find(t.Context(), "topic", DefaultParams())
	must.NoError(err)
	for _, person := range got.People {
		check.NotEqual(ownerPerson.ID, person.PersonID)
	}
	check.Positive(got.Excluded.OwnerPeople)

	p := DefaultParams()
	p.IncludeOwner = true
	got, err = s.Find(t.Context(), "topic", p)
	must.NoError(err)
	personIDs := make([]int64, 0, len(got.People))
	for _, person := range got.People {
		personIDs = append(personIDs, person.PersonID)
	}
	check.Contains(personIDs, ownerPerson.ID)
}

func TestServiceExcludesDeclaredNativeOwnerOutsideRetrievalPool(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	f := storetest.New(t)
	now := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	source, err := f.Store.GetOrCreateSource("beeper", "declared-owner-account")
	must.NoError(err)
	conversation, err := f.Store.EnsureConversationWithType(source.ID, "declared-owner-chat", "direct_chat", "Topic")
	must.NoError(err)
	owner := f.EnsureParticipant("declared-native-owner@example.net", "Declared Native Owner", "example.net")
	helper := f.EnsureParticipant("declared-owner-helper@example.net", "Declared Owner Helper", "example.net")
	ownerPerson, _, err := f.Store.CreatePersonFromParticipant(owner)
	must.NoError(err)
	_, err = f.Store.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{
		PersonID: ownerPerson.ID, DefinitionSlug: store.AttributeSlugAskMeAbout,
		Value:  store.AttributeValue{Type: store.AttributeValueText, Text: new("distributed systems")},
		Source: store.ProvenanceUser,
	})
	must.NoError(err)
	olderOutgoingID, err := f.Store.UpsertMessage(&store.Message{
		ConversationID: conversation, SourceID: source.ID, SourceMessageID: "older-declared-owner-outgoing",
		MessageType: "chat", IsFromMe: true, SentAt: sql.NullTime{Time: now.Add(-time.Hour), Valid: true},
	})
	must.NoError(err)
	must.NoError(f.Store.ReplaceMessageRecipients(olderOutgoingID, "from", []int64{owner}, []string{"Declared Native Owner"}))
	hitID, err := f.Store.UpsertMessage(&store.Message{
		ConversationID: conversation, SourceID: source.ID, SourceMessageID: "unrelated-helper-hit",
		MessageType: "chat", SenderID: sql.NullInt64{Int64: helper, Valid: true},
		SentAt: sql.NullTime{Time: now, Valid: true},
	})
	must.NoError(err)
	profileSearcher := &profileResults{results: []personsearch.Result{{Person: *ownerPerson, Score: .9}}}
	s := Service{
		Retriever:       &fixedRetriever{hits: []vector.FusedHit{{MessageID: hitID, RRFScore: 1, VectorScore: .9}}},
		Data:            f.Store,
		ProfileSearcher: profileSearcher,
		PeopleEnabled:   true,
		Now:             func() time.Time { return now },
	}
	got, err := s.Find(t.Context(), "distributed systems", DefaultParams())
	must.NoError(err)
	for _, person := range got.People {
		check.NotEqual(ownerPerson.ID, person.PersonID)
	}
	check.Positive(got.Excluded.OwnerPeople)

	p := DefaultParams()
	p.IncludeOwner = true
	got, err = s.Find(t.Context(), "distributed systems", p)
	must.NoError(err)
	personIDs := make([]int64, 0, len(got.People))
	for _, person := range got.People {
		personIDs = append(personIDs, person.PersonID)
	}
	check.Contains(personIDs, ownerPerson.ID)
}

func TestServiceKeepsExternalCoauthorOnOwnerOutboundMessage(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	f := storetest.New(t)
	now := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	source, err := f.Store.GetOrCreateSource("beeper", "beeper-account")
	must.NoError(err)
	conversation, err := f.Store.EnsureConversationWithType(source.ID, "coauthor-chat", "direct_chat", "Topic")
	must.NoError(err)
	owner := f.EnsureParticipant("owner@example.net", "Owner", "example.net")
	coauthor := f.EnsureParticipant("coauthor@example.net", "Coauthor", "example.net")
	ownerPerson, _, err := f.Store.CreatePersonFromParticipant(owner)
	must.NoError(err)
	messageID, err := f.Store.UpsertMessage(&store.Message{
		ConversationID: conversation, SourceID: source.ID, SourceMessageID: "owner-coauthored",
		MessageType: "chat", SenderID: sql.NullInt64{Int64: owner, Valid: true}, IsFromMe: true,
		SentAt: sql.NullTime{Time: now, Valid: true},
	})
	must.NoError(err)
	must.NoError(f.Store.ReplaceMessageRecipients(messageID, "from", []int64{owner, coauthor}, []string{"Owner", "Coauthor"}))
	s := Service{
		Retriever: &fixedRetriever{hits: []vector.FusedHit{{MessageID: messageID, RRFScore: 1, VectorScore: .9}}},
		Data:      f.Store,
		Now:       func() time.Time { return now },
	}
	got, err := s.Find(t.Context(), "topic", DefaultParams())
	must.NoError(err)
	must.Len(got.UnsavedCandidates, 1)
	check.Equal(coauthor, got.UnsavedCandidates[0].ParticipantID)
	for _, evidence := range got.UnsavedCandidates[0].Evidence {
		check.NotContains(evidence.Flags, "owner_authored")
	}
	p := DefaultParams()
	p.IncludeOwner = true
	got, err = s.Find(t.Context(), "topic", p)
	must.NoError(err)
	must.Len(got.People, 1)
	check.Equal(ownerPerson.ID, got.People[0].PersonID)
	check.Contains(got.People[0].Evidence[0].Flags, "owner_authored")
	must.Len(got.UnsavedCandidates, 1)
	check.Equal(coauthor, got.UnsavedCandidates[0].ParticipantID)
	for _, evidence := range got.UnsavedCandidates[0].Evidence {
		check.NotContains(evidence.Flags, "owner_authored")
	}
}

type profileResults struct {
	results                     []personsearch.Result
	err                         error
	vectorCalls, embeddingCalls int
}

func (p *profileResults) SearchVector(context.Context, []float32, vector.GenerationID, int) ([]personsearch.Result, error) {
	p.vectorCalls++
	return p.results, p.err
}
func (p *profileResults) Search(context.Context, string, int) ([]personsearch.Result, error) {
	p.embeddingCalls++
	return p.results, nil
}

func TestServiceOptionalProfilesContactStateAndExcerpts(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	f := storetest.New(t)
	now := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	participant := f.EnsureParticipant("expert@example.net", "Expert", "example.net")
	person, _, err := f.Store.CreatePersonFromParticipant(participant)
	must.NoError(err)
	id, err := f.Store.UpsertMessage(&store.Message{ConversationID: f.ConvID, SourceID: f.Source.ID, SourceMessageID: "expert", MessageType: "email", SenderID: sql.NullInt64{Int64: participant, Valid: true}, SentAt: sql.NullTime{Time: now, Valid: true}})
	must.NoError(err)
	_, err = f.Store.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{PersonID: person.ID, DefinitionSlug: store.AttributeSlugAskMeAbout, Value: store.AttributeValue{Type: store.AttributeValueText, Text: new("cloud")}, Source: store.ProvenanceUser})
	must.NoError(err)
	retriever := &fixedRetriever{hits: []vector.FusedHit{{MessageID: id, RRFScore: 1, VectorScore: .9}}}
	profiles := &profileResults{results: []personsearch.Result{{Person: *person, Score: .9}}}
	s := Service{Retriever: retriever, Data: f.Store, ProfileSearcher: profiles, PeopleEnabled: true, Now: func() time.Time { return now }, CoverageReader: func(context.Context) (vector.EmbeddingStatus, error) {
		return vector.EmbeddingStatus{Generation: vector.EmbeddingGenerationStatus{ID: 1, State: string(vector.GenerationActive)}, Pending: 1800, Eligible: 2000, Current: 200, CoverageSampledAt: now}, nil
	}}
	got, err := s.Find(t.Context(), "cloud", DefaultParams())
	must.NoError(err)
	must.Len(got.People, 1)
	check.Equal("ranked", got.People[0].Breakdown.ProfileSearch)
	check.InDelta(.2, got.People[0].Breakdown.Affinity, 1e-9)
	check.Equal(0, profiles.embeddingCalls)
	check.Contains(got.Coverage.StaleReasons, "embedding_backlog:1800")
	profiles.err = personsearch.ErrGenerationMismatch
	got, err = s.Find(t.Context(), "cloud", DefaultParams())
	must.NoError(err)
	check.Equal(1, profiles.embeddingCalls)
	check.Equal("ranked", got.People[0].Breakdown.ProfileSearch)
	profiles.err = vector.ErrSemanticPersonEmbeddingConsentRequired
	got, err = s.Find(t.Context(), "cloud", DefaultParams())
	must.NoError(err)
	check.Equal("disabled", got.People[0].Breakdown.ProfileSearch)
	check.False(got.Coverage.PeopleIndex.Consented)
	profiles.err = vector.ErrSemanticPersonEmbeddingPolicyUnavailable
	got, err = s.Find(t.Context(), "cloud", DefaultParams())
	must.NoError(err)
	check.Equal("unavailable", got.People[0].Breakdown.ProfileSearch)
	check.False(got.Coverage.PeopleIndex.Consented)
	check.Contains(got.Coverage.StaleReasons, "people_index_unavailable")
	profiles.err = &personsearch.CoverageIncompleteError{Generation: 1, Mismatched: 1}
	got, err = s.Find(t.Context(), "cloud", DefaultParams())
	must.NoError(err)
	check.Equal("unavailable", got.People[0].Breakdown.ProfileSearch)
	check.Contains(got.Coverage.StaleReasons, "people_index_incomplete")
	profiles.err = nil
	profiles.results[0].Score = .4
	got, err = s.Find(t.Context(), "cloud", DefaultParams())
	must.NoError(err)
	check.Equal("below_threshold", got.People[0].Breakdown.ProfileSearch)
	revisions, err := f.Store.ContactRevisionsContext(t.Context())
	must.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind("UPDATE activity_projection_queue SET processed_revision=revision"))
	must.NoError(err)
	must.NoError(f.Store.CompareAndSetActivityReconciledRevisionsContext(t.Context(), revisions))
	lastContact := now.Add(-24 * time.Hour)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind("INSERT INTO person_contact_state(person_id,last_contact_at,interaction_count,identity_revision,account_identity_revision,computed_at) VALUES (?,?,20,?,?,?)"), person.ID, lastContact, revisions.IdentityRevision, revisions.AccountIdentityRevision, now)
	must.NoError(err)
	got, err = s.Find(t.Context(), "cloud", DefaultParams())
	must.NoError(err)
	check.Contains(got.People[0].Flags, "one_way")
	must.NotNil(got.People[0].Tiebreak.LastContactAt)
	check.True(lastContact.Equal(*got.People[0].Tiebreak.LastContactAt))
	check.InDelta(.5, got.People[0].Breakdown.RawAuthored, 1e-9)
	check.InDelta(.75, got.People[0].Breakdown.Declared, 1e-9)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind("UPDATE person_contact_state SET dirty_at=? WHERE person_id=?"), now, person.ID)
	must.NoError(err)
	got, err = s.Find(t.Context(), "cloud", DefaultParams())
	must.NoError(err)
	check.NotContains(got.People[0].Flags, "one_way")
	check.Nil(got.People[0].Tiebreak.LastContactAt)
	check.Contains(got.Coverage.StaleReasons, "contact_state_dirty:1")
	outboundOwner := f.EnsureParticipant("owner@example.net", "Owner", "example.net")
	outboundID, err := f.Store.UpsertMessage(&store.Message{
		ConversationID: f.ConvID, SourceID: f.Source.ID, SourceMessageID: "pending-outbound",
		MessageType: "email", SenderID: sql.NullInt64{Int64: outboundOwner, Valid: true}, IsFromMe: true,
		SentAt: sql.NullTime{Time: now.Add(time.Hour), Valid: true},
	})
	must.NoError(err)
	must.NoError(f.Store.ReplaceMessageRecipients(outboundID, "to", []int64{participant}, []string{"Expert"}))
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind("UPDATE person_contact_state SET dirty_at=NULL WHERE person_id=?"), person.ID)
	must.NoError(err)
	got, err = s.Find(t.Context(), "cloud", DefaultParams())
	must.NoError(err)
	must.Len(got.People, 1)
	check.NotContains(got.People[0].Flags, "one_way")
	check.Nil(got.People[0].Tiebreak.LastContactAt)
	check.Zero(got.Coverage.ContactState.Dirty)
	check.Equal(1, got.Coverage.ContactState.Outdated)
	check.Contains(got.Coverage.StaleReasons, "contact_state_outdated:1")
	reads := 0
	s.ExcerptBuilder = func(context.Context, vector.GenerationID, []float32, int64, float64) (*Excerpt, error) {
		reads++
		return nil, errors.New("synthetic unavailable chunk")
	}
	_, err = s.Find(t.Context(), "cloud", DefaultParams())
	must.NoError(err)
	check.Zero(reads)
	p := DefaultParams()
	p.Excerpts = true
	got, err = s.Find(t.Context(), "cloud", p)
	must.NoError(err)
	check.Equal(1, reads)
	check.True(got.People[0].Evidence[0].ExcerptError)
	s.TemperatureReader = func(context.Context, map[int64][]int64) (map[int64]int, bool, error) {
		return map[int64]int{person.ID: 80}, true, nil
	}
	got, err = s.Find(t.Context(), "cloud", DefaultParams())
	must.NoError(err)
	check.Equal("temperature", got.Tiebreak.Applied)
	check.Equal(new(80), got.People[0].Tiebreak.Temperature)
	retriever.hits = nil
	profiles.results[0].Score = .9
	got, err = s.Find(t.Context(), "other topic", DefaultParams())
	must.NoError(err)
	must.Len(got.People, 1)
	check.Contains(got.People[0].Flags, "profile_only")
}

func TestServiceDoesNotUseBuildingGenerationCoverageForActiveRetrieval(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	f := storetest.New(t)
	active := vector.EmbeddingGenerationStatus{ID: 1, State: string(vector.GenerationActive)}
	s := Service{
		Retriever: &fixedRetriever{},
		Data:      f.Store,
		CoverageReader: func(context.Context) (vector.EmbeddingStatus, error) {
			return vector.EmbeddingStatus{
				Generation:       vector.EmbeddingGenerationStatus{ID: 2, State: string(vector.GenerationBuilding)},
				ActiveGeneration: &active,
				Eligible:         100,
				Current:          40,
				Pending:          60,
			}, nil
		},
	}

	got, err := s.Find(t.Context(), "topic", DefaultParams())
	must.NoError(err)

	check.Equal(int64(1), got.Coverage.Generation.ID)
	check.Zero(got.Coverage.Messages.Eligible)
	check.Zero(got.Coverage.Messages.Current)
	check.Zero(got.Coverage.Messages.Pending)
	check.True(got.Coverage.Messages.CoverageSampledAt.IsZero())
	check.Contains(got.Coverage.StaleReasons, "embedding_coverage_generation_mismatch")
	check.NotContains(got.Coverage.StaleReasons, "embedding_backlog:60")
}

func TestServiceSenderAuthenticationProvenance(t *testing.T) {
	f := storetest.New(t)
	participant := f.EnsureParticipant("author@example.net", "Author", "example.net")
	_, _, err := f.Store.CreatePersonFromParticipant(participant)
	require.NoError(t, err)
	for _, sourceType := range []string{"gmail", "beeper"} {
		t.Run(sourceType, func(t *testing.T) {
			must := require.New(t)
			source, err := f.Store.GetOrCreateSource(sourceType, sourceType+"-account")
			must.NoError(err)
			conv, err := f.Store.EnsureConversationWithType(source.ID, sourceType, "direct_chat", "Topic")
			must.NoError(err)
			id, err := f.Store.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: sourceType, MessageType: "chat", SenderID: sql.NullInt64{Int64: participant, Valid: true}})
			must.NoError(err)
			s := Service{Retriever: &fixedRetriever{hits: []vector.FusedHit{{MessageID: id, RRFScore: 1, VectorScore: .9}}}, Data: f.Store}
			got, err := s.Find(t.Context(), "topic", DefaultParams())
			must.NoError(err)
			must.Len(got.People, 1)
			must.Len(got.People[0].Evidence, 1)
			assert.Equal(t, sourceType == "beeper", got.People[0].Evidence[0].SenderAuthenticated)
		})
	}
}
