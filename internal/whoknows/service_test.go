package whoknows_test

import (
	"context"
	"database/sql"
	"errors"
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
	hits    []vector.FusedHit
	err     error
	request hybrid.SearchRequest
}

func (r *fixedRetriever) Search(_ context.Context, request hybrid.SearchRequest) ([]vector.FusedHit, hybrid.ResultMeta, error) {
	r.request = request
	return r.hits, hybrid.ResultMeta{}, r.err
}

func hit(id int64, similarity float64) vector.FusedHit {
	return vector.FusedHit{MessageID: id, RRFScore: 1, BM25Score: math.NaN(), VectorScore: similarity}
}

var serviceNow = time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)

type serviceFixture struct {
	*storetest.Fixture

	retriever *fixedRetriever
	service   *whoknows.Service
}

func newServiceFixture(t *testing.T) *serviceFixture {
	t.Helper()
	f := storetest.New(t)
	require.NoError(t, f.Store.AddAccountIdentity(f.Source.ID, "owner@example.net", "manual"))
	retriever := &fixedRetriever{}
	return &serviceFixture{
		Fixture:   f,
		retriever: retriever,
		service: &whoknows.Service{
			Retriever:  retriever,
			Data:       f.Store,
			BuildScope: vector.NewBuildScope([]string{"email", "meeting_transcript"}, nil),
			Now:        func() time.Time { return serviceNow },
		},
	}
}

// message stores one message from sender and returns its ID.
func (f *serviceFixture) message(name string, sender int64, daysAgo int, mutate ...func(*store.Message)) int64 {
	f.T.Helper()
	m := f.NewMessage().WithSourceMessageID(name).WithSubject(name).
		WithSentAt(serviceNow.AddDate(0, 0, -daysAgo)).Build()
	m.SenderID = sql.NullInt64{Int64: sender, Valid: true}
	for _, change := range mutate {
		change(m)
	}
	id, err := f.Store.UpsertMessage(m)
	require.NoError(f.T, err)
	return id
}

func (f *serviceFixture) saved(email string) (participantID, personID int64) {
	f.T.Helper()
	participantID = f.EnsureParticipant(email, email, "example.net")
	person, _, err := f.Store.CreatePersonFromParticipant(participantID)
	require.NoError(f.T, err)
	return participantID, person.ID
}

func TestFindCreditsOnlyWhoWroteAndCountsMeaningMatches(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newServiceFixture(t)

	ana, anaPerson := f.saved("ana@example.net")
	ben := f.EnsureParticipant("ben@example.net", "Ben Contractor", "example.net")
	dana, _ := f.saved("dana@example.net")
	owner, _ := f.saved("owner@example.net")
	alias := f.EnsureParticipant("owner.work@example.net", "Owner Work", "example.net")
	_, err := f.Store.LinkParticipants(owner, alias)
	require.NoError(err)
	robot, _ := f.saved("noreply@example.net")
	carol, _ := f.saved("carol@example.net")
	erin, _ := f.saved("erin@example.net")

	answered := f.message("answered", ana, 5)
	followUp := f.message("follow-up", ana, 30)
	contractor := f.message("contractor", ben, 3)
	copied := f.message("copied", ana, 2)
	require.NoError(f.Store.ReplaceMessageRecipients(copied, "cc", []int64{dana}, []string{"Dana"}))
	outbound := f.message("outbound", owner, 1)
	aliased := f.message("aliased", alias, 1)
	automated := f.message("automated", robot, 1)
	transcript := f.message("transcript", ana, 1, func(m *store.Message) { m.MessageType = meetingarchive.MessageType })
	keywordOnly := f.message("keyword", carol, 1)
	weak := f.message("weak", erin, 1)

	f.retriever.hits = []vector.FusedHit{
		hit(answered, .9), hit(followUp, .6), hit(contractor, .8), hit(copied, .7),
		hit(outbound, .95), hit(aliased, .95), hit(automated, .95), hit(transcript, .95),
		{MessageID: keywordOnly, RRFScore: 1, BM25Score: 3, VectorScore: math.NaN()},
		hit(weak, .49),
	}
	after := "2026-01-01"
	got, err := f.service.Find(t.Context(), whoknows.Request{Topic: "Kubernetes   networking", After: after})
	require.NoError(err)

	assert.Equal("Kubernetes   networking", got.Topic)
	assert.Empty(got.Reason)
	require.Len(got.People, 1)
	person := got.People[0]
	assert.Equal(anaPerson, person.PersonID)
	assert.Equal(1, person.Rank)
	assert.Equal(3, person.AuthoredMessages, "ana wrote three counted messages; the transcript is not authorship")
	require.Len(person.Citations, 3)
	assert.Equal(answered, person.Citations[0].MessageID, "highest weight first")
	assert.Equal("email", person.Citations[0].MessageType)
	assert.Equal("answered", person.Citations[0].Subject)
	require.Len(got.UnsavedContacts, 1)
	assert.Equal(ben, got.UnsavedContacts[0].ParticipantID)
	assert.Equal("ben@example.net", got.UnsavedContacts[0].EmailAddress)

	request := f.retriever.request
	assert.Equal(hybrid.ModeHybrid, request.Mode)
	assert.Equal("Kubernetes networking", request.FreeText)
	assert.Equal(200, request.Limit)
	assert.Equal([]string{"kubernetes", "networking"}, request.SubjectTerms)
	assert.Equal([]string{"email", "meeting_transcript"}, request.Filter.MessageTypes)
	require.NotNil(request.Filter.After)
	assert.True(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Equal(*request.Filter.After))
	assert.Nil(request.Filter.Before)
}

func TestFindBoostsAskMeAboutOnlyForAuthors(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newServiceFixture(t)
	author, authorPerson := f.saved("author@example.net")
	_, lurkerPerson := f.saved("lurker@example.net")
	plain, plainPerson := f.saved("plain@example.net")
	for personID, topic := range map[int64]string{authorPerson: "kubernetes", lurkerPerson: "kubernetes networking"} {
		_, err := f.Store.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{
			PersonID: personID, DefinitionSlug: store.AttributeSlugAskMeAbout, Ordinal: new(int64),
			Value:  store.AttributeValue{Type: store.AttributeValueText, Text: &topic},
			Source: store.ProvenanceUser,
		})
		require.NoError(err)
	}
	f.retriever.hits = []vector.FusedHit{hit(f.message("a", author, 5), .7), hit(f.message("b", plain, 5), .7)}

	got, err := f.service.Find(t.Context(), whoknows.Request{Topic: "kubernetes networking"})
	require.NoError(err)
	require.Len(got.People, 2, "a declared value adds no one")
	assert.Equal(authorPerson, got.People[0].PersonID)
	assert.Equal([]string{"kubernetes"}, got.People[0].AskMeAbout)
	assert.Equal(plainPerson, got.People[1].PersonID)
}

func TestFindIsEmptyWithoutAuthoredMeaningMatches(t *testing.T) {
	assert := assert.New(t)
	f := newServiceFixture(t)
	ana, _ := f.saved("ana@example.net")
	owner, _ := f.saved("owner@example.net")
	for name, hits := range map[string][]vector.FusedHit{
		"no hits":         nil,
		"below the floor": {hit(f.message("weak", ana, 1), .3)},
		"owner only":      {hit(f.message("mine", owner, 1), .99)},
		"keyword only": {{
			MessageID: f.message("words", ana, 1), RRFScore: 1, BM25Score: 4, VectorScore: math.NaN(),
		}},
	} {
		f.retriever.hits = hits
		got, err := f.service.Find(t.Context(), whoknows.Request{Topic: "sailboat insurance"})
		require.NoError(t, err, name)
		assert.Empty(got.People, name)
		assert.Empty(got.UnsavedContacts, name)
		assert.Equal(whoknows.ReasonNoEvidence, got.Reason, name)
		assert.NotNil(got.People, name)
		assert.NotNil(got.UnsavedContacts, name)
	}
}

func TestFindMinScoreIsConfigurable(t *testing.T) {
	require := require.New(t)
	f := newServiceFixture(t)
	ana, anaPerson := f.saved("ana@example.net")
	f.retriever.hits = []vector.FusedHit{hit(f.message("m", ana, 1), .42)}

	got, err := f.service.Find(t.Context(), whoknows.Request{Topic: "topic"})
	require.NoError(err)
	assert.Empty(t, got.People)

	low := .4
	got, err = f.service.Find(t.Context(), whoknows.Request{Topic: "topic", MinScore: &low})
	require.NoError(err)
	require.Len(got.People, 1)
	assert.Equal(t, anaPerson, got.People[0].PersonID)
}

func TestFindErrors(t *testing.T) {
	require := require.New(t)
	f := newServiceFixture(t)
	_, err := f.service.Find(t.Context(), whoknows.Request{Topic: "from:ana"})
	require.ErrorIs(err, whoknows.ErrMissingFreeText, "operators alone leave no free text")

	failure := errors.New("search failed")
	f.retriever.err = failure
	_, err = f.service.Find(t.Context(), whoknows.Request{Topic: "topic"})
	require.ErrorIs(err, failure)

	bare := &whoknows.Service{Data: f.Store}
	_, err = bare.Find(t.Context(), whoknows.Request{Topic: "topic"})
	require.ErrorIs(err, vector.ErrNotEnabled)
}
