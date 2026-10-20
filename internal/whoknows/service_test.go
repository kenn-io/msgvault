package whoknows_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/hybrid"
	"go.kenn.io/msgvault/internal/whoknows"
)

type fixedRetriever struct {
	hits      []vector.FusedHit
	saturated bool
	err       error
	calls     int
	request   hybrid.SearchRequest
}

func (r *fixedRetriever) Search(_ context.Context, request hybrid.SearchRequest) ([]vector.FusedHit, hybrid.ResultMeta, error) {
	r.calls++
	r.request = request
	meta := hybrid.ResultMeta{
		Generation:    vector.Generation{ID: 7},
		QueryVector:   []float32{1, 0},
		PoolSaturated: r.saturated,
	}
	return r.hits, meta, r.err
}

// fixedScorer returns one chunk per message, or none for a message without a vector.
type fixedScorer struct {
	scores map[int64]float64
	calls  map[int64]vector.GenerationID
}

func (s *fixedScorer) ScoreMessageChunks(_ context.Context, gen vector.GenerationID, id int64, query []float32) ([]vector.ChunkHit, error) {
	if len(query) != 2 {
		return nil, errors.New("the search's query vector was not passed")
	}
	s.calls[id] = gen
	score, ok := s.scores[id]
	if !ok {
		return nil, nil
	}
	return []vector.ChunkHit{{ChunkIndex: 1, Score: score - .3}, {ChunkIndex: 0, Score: score}}, nil
}

var serviceNow = time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)

type serviceFixture struct {
	*storetest.Fixture

	retriever *fixedRetriever
	scorer    *fixedScorer
	service   *whoknows.Service
	owner     int64
}

func newServiceFixture(t *testing.T) *serviceFixture {
	t.Helper()
	f := storetest.New(t)
	require.NoError(t, f.Store.AddAccountIdentity(f.Source.ID, "owner@example.net", "manual"))
	retriever := &fixedRetriever{}
	scorer := &fixedScorer{scores: map[int64]float64{}, calls: map[int64]vector.GenerationID{}}
	return &serviceFixture{
		Fixture:   f,
		retriever: retriever,
		scorer:    scorer,
		owner:     f.EnsureParticipant("owner@example.net", "Owner", "example.net"),
		service: &whoknows.Service{
			Retriever: retriever, Scorer: scorer, Data: f.Store,
			Now: func() time.Time { return serviceNow },
		},
	}
}

// message stores one message from sender, scores it, and adds it to the hits.
func (f *serviceFixture) message(name string, sender int64, daysAgo int, similarity float64, change ...func(*store.Message)) int64 {
	f.T.Helper()
	m := f.NewMessage().WithSourceMessageID(name).WithSubject(name).
		WithSentAt(serviceNow.AddDate(0, 0, -daysAgo)).Build()
	m.SenderID = sql.NullInt64{Int64: sender, Valid: true}
	for _, c := range change {
		c(m)
	}
	id, err := f.Store.UpsertMessage(m)
	require.NoError(f.T, err)
	if similarity > 0 {
		f.scorer.scores[id] = similarity
	}
	f.retriever.hits = append(f.retriever.hits, vector.FusedHit{MessageID: id, BM25Score: math.NaN(), VectorScore: math.NaN()})
	return id
}

func (f *serviceFixture) saved(email string) (participantID, personID int64) {
	f.T.Helper()
	participantID = f.EnsureParticipant(email, email, "example.net")
	person, _, err := f.Store.CreatePersonFromParticipant(participantID)
	require.NoError(f.T, err)
	return participantID, person.ID
}

func TestFindCreditsOnlyWhoWroteAndCountsCosineMatches(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newServiceFixture(t)
	slack, err := f.Store.GetOrCreateSource("slack", "workspace")
	require.NoError(err)
	room, err := f.Store.EnsureConversation(slack.ID, "room", "Room")
	require.NoError(err)

	ana, anaPerson := f.saved("ana@example.net")
	ben := f.EnsureParticipant("ben@example.net", "Ben Contractor", "example.net")
	dana, _ := f.saved("dana@example.net")
	robot := f.EnsureParticipant("no_reply@example.net", "Robot", "example.net")
	botUser := f.EnsureParticipant("bot@example.net", "Chat Bot", "example.net")
	erin := f.EnsureParticipant("erin@example.net", "Erin", "example.net")
	frank := f.EnsureParticipant("frank@example.net", "Frank", "example.net")
	_ = dana

	answered := f.message("answered", ana, 5, .9)
	f.message("follow-up", ana, 30, .6)
	contractor := f.message("contractor", ben, 3, .8)
	copied := f.message("copied", ana, 2, .7)
	require.NoError(f.Store.ReplaceMessageRecipients(copied, "cc", []int64{dana}, []string{"Dana"}))
	f.message("outbound", f.owner, 1, .95)
	f.message("robot", robot, 1, .95)
	bot := f.message("bot", botUser, 1, .95, func(m *store.Message) { m.SourceID, m.ConversationID = slack.ID, room })
	require.NoError(f.Store.SetMessageMetadata(bot, sql.NullString{String: `{"author_kind":"bot","automated":true}`, Valid: true}))
	f.message("no vector", erin, 1, 0)
	f.message("weak", frank, 1, .49)

	got, err := f.service.Find(t.Context(), whoknows.Request{Topic: "Kubernetes   networking", After: "2026-01-01"})
	require.NoError(err)

	assert.Equal("Kubernetes   networking", got.Topic)
	assert.Equal(9, got.SearchedMessages)
	assert.Empty(got.Reason)
	require.Len(got.People, 1)
	person := got.People[0]
	assert.Equal(anaPerson, person.PersonID)
	assert.Equal(3, person.AuthoredMessages)
	assert.Zero(person.DeclaredBoost)
	assert.Greater(person.MessageScore, 0.0)
	require.Len(person.Citations, 3)
	assert.Equal(answered, person.Citations[0].MessageID, "highest weight first")
	assert.False(person.Citations[0].SenderAuthenticated, "an email From header is an assertion")
	require.Len(got.UnsavedContacts, 1)
	assert.Equal(ben, got.UnsavedContacts[0].ParticipantID)
	assert.Equal(contractor, got.UnsavedContacts[0].Citations[0].MessageID)

	assert.Equal(vector.GenerationID(7), f.scorer.calls[answered], "messages score against the search's generation")
	request := f.retriever.request
	assert.Equal(hybrid.ModeHybrid, request.Mode)
	assert.Equal("Kubernetes networking", request.FreeText)
	assert.Equal(200, request.Limit)
	assert.Equal([]string{"kubernetes", "networking"}, request.SubjectTerms)
	assert.Contains(request.Filter.MessageTypes, "email")
	assert.NotContains(request.Filter.MessageTypes, meetingarchive.MessageType)
	assert.NotContains(request.Filter.MessageTypes, "calendar_event")
	require.NotNil(request.Filter.After)
	assert.True(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Equal(*request.Filter.After))
}

func TestFindRestrictsMessageTypesToTheIndexScope(t *testing.T) {
	assert := assert.New(t)
	f := newServiceFixture(t)
	f.service.BuildScope = vector.NewBuildScope([]string{"email", "meeting_transcript"}, nil)
	_, err := f.service.Find(t.Context(), whoknows.Request{Topic: "topic"})
	require.NoError(t, err)
	assert.Equal([]string{"email"}, f.retriever.request.Filter.MessageTypes)

	f.retriever.calls = 0
	f.service.BuildScope = vector.NewBuildScope([]string{"meeting_transcript", "calendar_event"}, nil)
	got, err := f.service.Find(t.Context(), whoknows.Request{Topic: "topic"})
	require.NoError(t, err)
	assert.Zero(f.retriever.calls, "an index with only transcripts and events has no authors to search")
	assert.Equal(whoknows.ReasonNoEvidence, got.Reason)
}

func TestFindListsDeclaredOnlyPeopleAndBoostsAuthors(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newServiceFixture(t)
	author, authorPerson := f.saved("author@example.net")
	_, lurkerPerson := f.saved("lurker@example.net")
	_, otherPerson := f.saved("other@example.net")
	for personID, topic := range map[int64]string{
		authorPerson: "kubernetes", lurkerPerson: "kubernetes networking", otherPerson: "billing",
	} {
		_, err := f.Store.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{
			PersonID: personID, DefinitionSlug: store.AttributeSlugAskMeAbout, Ordinal: new(int64),
			Value:  store.AttributeValue{Type: store.AttributeValueText, Text: &topic},
			Source: store.ProvenanceUser,
		})
		require.NoError(err)
	}
	f.message("a", author, 5, .7)

	got, err := f.service.Find(t.Context(), whoknows.Request{Topic: "kubernetes networking"})
	require.NoError(err)
	require.Len(got.People, 2)
	assert.Equal(authorPerson, got.People[0].PersonID)
	assert.Equal([]string{"kubernetes"}, got.People[0].AskMeAbout)
	assert.InDelta(.75, got.People[0].DeclaredBoost, 1e-9)
	assert.Equal(lurkerPerson, got.People[1].PersonID)
	assert.Zero(got.People[1].AuthoredMessages)
	assert.InDelta(.75, got.People[1].Score, 1e-9)
}

func TestFindEmptyReasons(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newServiceFixture(t)
	ana, _ := f.saved("ana@example.net")
	find := func() whoknows.Response {
		got, err := f.service.Find(t.Context(), whoknows.Request{Topic: "sailboat insurance"})
		require.NoError(err)
		assert.Empty(got.People)
		assert.Empty(got.UnsavedContacts)
		assert.NotNil(got.People)
		assert.NotNil(got.UnsavedContacts)
		return got
	}

	assert.Equal(whoknows.ReasonNoEvidence, find().Reason, "no hits")

	f.message("weak", ana, 1, .3)
	assert.Equal(whoknows.ReasonNoEvidence, find().Reason, "nothing reaches the floor")

	f.message("mine", f.owner, 1, .99)
	f.retriever.saturated = false
	got := find()
	assert.Equal(whoknows.ReasonNoEvidence, got.Reason, "an unsaturated search that found only the owner's mail is complete")
	assert.Equal(2, got.SearchedMessages)

	f.retriever.saturated = true
	assert.Equal(whoknows.ReasonSearchLimit, find().Reason, "a saturated search may hide the answer")

	f.retriever.hits = f.retriever.hits[:1]
	assert.Equal(whoknows.ReasonNoEvidence, find().Reason, "saturation alone does not set the reason")
}

// The owner's mail can fill the whole pool. A real answer that made the pool
// is found; one beyond it is reported as a search limit, never as nobody.
func TestFindReportsSearchLimitWhenOwnerMailFillsThePool(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newServiceFixture(t)
	ana, anaPerson := f.saved("ana@example.net")
	for i := range 199 {
		f.message(fmt.Sprintf("mine-%d", i), f.owner, 1, .9)
	}
	answer := f.message("answer", ana, 2, .8)

	got, err := f.service.Find(t.Context(), whoknows.Request{Topic: "kubernetes networking"})
	require.NoError(err)
	require.Len(got.People, 1)
	assert.Equal(anaPerson, got.People[0].PersonID)
	assert.Equal(answer, got.People[0].Citations[0].MessageID)
	assert.Equal(200, got.SearchedMessages)

	// Without the answer, the same owner-only pool says where the search ended.
	f.retriever.hits = f.retriever.hits[:199]
	f.retriever.saturated = true
	got, err = f.service.Find(t.Context(), whoknows.Request{Topic: "kubernetes networking"})
	require.NoError(err)
	assert.Empty(got.People)
	assert.Equal(whoknows.ReasonSearchLimit, got.Reason)

	f.retriever.saturated = false
	got, err = f.service.Find(t.Context(), whoknows.Request{Topic: "kubernetes networking"})
	require.NoError(err)
	assert.Equal(whoknows.ReasonNoEvidence, got.Reason, "199 hits fill no pool")

	f.message("mine-last", f.owner, 1, .9)
	got, err = f.service.Find(t.Context(), whoknows.Request{Topic: "kubernetes networking"})
	require.NoError(err)
	assert.Equal(whoknows.ReasonSearchLimit, got.Reason, "a full pool of 200 is saturated")
	assert.Equal(200, got.SearchedMessages)
}

func TestFindBoundsListMailAndOneWaySenders(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newServiceFixture(t)
	ana, anaPerson := f.saved("ana@example.net")
	vendor, vendorPerson := f.saved("vendor@example.net")
	editor, editorPerson := f.saved("editor@example.net")
	other, otherPerson := f.saved("other@example.net")

	// The owner has written to Ana and Other, never to the vendor or the editor.
	reply := f.message("reply-to-ana", f.owner, 40, 0)
	require.NoError(f.Store.ReplaceMessageRecipients(reply, "to", []int64{ana, other}, []string{"Ana", "Other"}))
	f.retriever.hits = nil
	for i := range 10 {
		f.message(fmt.Sprintf("ana-%d", i), ana, 3+i, .8)
	}
	for i := range 30 {
		f.message(fmt.Sprintf("vendor-%d", i), vendor, 1, .9)
	}
	for i := range 12 {
		f.message(fmt.Sprintf("digest-%d", i), editor, 1, .9, func(m *store.Message) {
			m.ListID = sql.NullString{String: "<digest.example.net>", Valid: true}
		})
	}
	for i := range 2 {
		f.message(fmt.Sprintf("other-%d", i), other, 2, .8)
	}

	got, err := f.service.Find(t.Context(), whoknows.Request{Topic: "kubernetes networking"})
	require.NoError(err)
	var order []int64
	scores := map[int64]float64{}
	for _, p := range got.People {
		order = append(order, p.PersonID)
		scores[p.PersonID] = p.MessageScore
	}
	assert.Equal([]int64{anaPerson, otherPerson, vendorPerson, editorPerson}, order,
		"ten answers beat two answers; thirty unanswered messages and twelve digests earn one message each")
	assert.Greater(scores[otherPerson], scores[vendorPerson])
	assert.Equal(30, got.People[2].AuthoredMessages)
}

func TestFindRefusesSearchOperators(t *testing.T) {
	f := newServiceFixture(t)
	_, err := f.service.Find(t.Context(), whoknows.Request{Topic: "from:ana networking"})
	require.ErrorIs(t, err, whoknows.ErrInvalidParameter)
	assert.Zero(t, f.retriever.calls)
}

func TestFindMinScoreIsConfigurable(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newServiceFixture(t)
	ana, anaPerson := f.saved("ana@example.net")
	f.message("m", ana, 1, .42)

	got, err := f.service.Find(t.Context(), whoknows.Request{Topic: "topic"})
	require.NoError(err)
	assert.Empty(got.People)

	low := .4
	got, err = f.service.Find(t.Context(), whoknows.Request{Topic: "topic", MinScore: &low})
	require.NoError(err)
	require.Len(got.People, 1)
	assert.Equal(anaPerson, got.People[0].PersonID)
}

func TestFindErrors(t *testing.T) {
	require := require.New(t)
	f := newServiceFixture(t)
	failure := errors.New("search failed")
	f.retriever.err = failure
	_, err := f.service.Find(t.Context(), whoknows.Request{Topic: "topic"})
	require.ErrorIs(err, failure)

	_, err = (&whoknows.Service{Data: f.Store}).Find(t.Context(), whoknows.Request{Topic: "topic"})
	require.ErrorIs(err, vector.ErrNotEnabled)

	_, err = (&whoknows.Service{Retriever: f.retriever, Data: f.Store}).Find(t.Context(), whoknows.Request{Topic: "topic"})
	require.ErrorIs(err, whoknows.ErrChunkScoringUnavailable)
}
