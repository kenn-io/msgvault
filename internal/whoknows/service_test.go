package whoknows_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"slices"
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

// fixedRetriever caps each signal's hits and the fused pool, reporting
// saturation when either cap drops hits.
type fixedRetriever struct {
	hits    []vector.FusedHit
	err     error
	calls   int
	request hybrid.SearchRequest
}

func (r *fixedRetriever) Search(_ context.Context, request hybrid.SearchRequest) ([]vector.FusedHit, hybrid.ResultMeta, error) {
	r.calls++
	r.request = request
	var hits []vector.FusedHit
	var vectorHits, keywordHits int
	for _, hit := range r.hits {
		if !math.IsNaN(hit.VectorScore) {
			vectorHits++
			if vectorHits > request.KPerSignal {
				hit.VectorScore = math.NaN()
			}
		}
		if !math.IsNaN(hit.BM25Score) {
			keywordHits++
			if keywordHits > request.KPerSignal {
				hit.BM25Score = math.NaN()
			}
		}
		if len(hits) < request.Limit && (!math.IsNaN(hit.VectorScore) || !math.IsNaN(hit.BM25Score)) {
			hits = append(hits, hit)
		}
	}
	return hits, hybrid.ResultMeta{PoolSaturated: len(hits) < len(r.hits)}, r.err
}

// truncatedDeclared reports that the declared read hit its cap, as the store
// does by returning DeclaredValueLimit values, each for its own person.
type truncatedDeclared struct{ whoknows.DataReader }

func (truncatedDeclared) AskMeAboutValuesContext(context.Context, []int64) ([]store.PersonAskMeAbout, bool, error) {
	values := make([]store.PersonAskMeAbout, store.DeclaredValueLimit)
	for i := range values {
		values[i] = store.PersonAskMeAbout{
			PersonID: int64(i + 1), PersonName: fmt.Sprintf("Person %d", i+1), Value: fmt.Sprintf("hobby %d", i),
		}
	}
	return values, true, nil
}

// fixtureCount answers the vector-wait count from the fixture, since no
// embedding worker stamps the fixture's messages.
type fixtureCount struct {
	*store.Store

	f *serviceFixture
}

// fixedRetriever supplies cosine scores directly, so service unit tests can
// model a backend whose saturated results are already cosine ordered.
type fixtureCosineOrderBackend struct{ vector.Backend }

func (fixtureCosineOrderBackend) CosineSearchOrderGuaranteed() bool { return true }

func (c fixtureCount) CoverageCountsScoped(_ context.Context, _ int64, types []string, _ []int64) (int64, int64, int64, int64, error) {
	c.f.counted = append(c.f.counted, types)
	return c.f.waiting, 0, 0, c.f.waiting, c.f.countErr
}

var serviceNow = time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)

type serviceFixture struct {
	*storetest.Fixture

	retriever *fixedRetriever
	service   *whoknows.Service
	owner     int64
	// waiting is how many messages the vector-wait count reports, counted
	// the message types of each count, and countErr the error it returns.
	waiting  int64
	counted  [][]string
	countErr error
}

func newServiceFixture(t *testing.T) *serviceFixture {
	t.Helper()
	f := storetest.New(t)
	require.NoError(t, f.Store.AddAccountIdentity(f.Source.ID, "owner@example.net", "manual"))
	fixture := &serviceFixture{
		Fixture:   f,
		retriever: &fixedRetriever{},
		owner:     f.EnsureParticipant("owner@example.net", "Owner", "example.net"),
	}
	fixture.service = &whoknows.Service{
		Retriever: fixture.retriever, Data: fixtureCount{f.Store, fixture},
		Index: fixtureCosineOrderBackend{}, Now: func() time.Time { return serviceNow },
	}
	return fixture
}

// message stores one message from sender and adds it to the hits with its
// cosine similarity, or as a keyword-only hit when similarity is zero.
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
	hit := vector.FusedHit{MessageID: id, BM25Score: math.NaN(), VectorScore: math.NaN()}
	if similarity > 0 {
		hit.VectorScore = similarity
	} else {
		hit.BM25Score = 1
	}
	f.retriever.hits = append(f.retriever.hits, hit)
	return id
}

func (f *serviceFixture) saved(email string) (participantID, personID int64) {
	f.T.Helper()
	participantID = f.EnsureParticipant(email, email, "example.net")
	person, _, err := f.Store.CreatePersonFromParticipant(participantID)
	require.NoError(f.T, err)
	return participantID, person.ID
}

func (f *serviceFixture) declare(personID int64, topic string) {
	f.T.Helper()
	_, err := f.Store.SetPersonAttributeValueContext(f.T.Context(), store.PersonAttributeValueInput{
		PersonID: personID, DefinitionSlug: store.AttributeSlugAskMeAbout, Ordinal: new(int64),
		Value:  store.AttributeValue{Type: store.AttributeValueText, Text: &topic},
		Source: store.ProvenanceUser,
	})
	require.NoError(f.T, err)
}

func (f *serviceFixture) find(topic string) whoknows.Response {
	f.T.Helper()
	got, err := f.service.Find(f.T.Context(), whoknows.Request{Topic: topic})
	require.NoError(f.T, err)
	assert.NotNil(f.T, got.People)
	assert.NotNil(f.T, got.UnsavedContacts)
	return got
}

func TestFindCreditsOnlyWhoWroteAndCountsVectorMatches(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newServiceFixture(t)

	ana, anaPerson := f.saved("ana@example.net")
	ben := f.EnsureParticipant("ben@example.net", "Ben Contractor", "example.net")
	dana, _ := f.saved("dana@example.net")
	erin := f.EnsureParticipant("erin@example.net", "Erin", "example.net")
	frank := f.EnsureParticipant("frank@example.net", "Frank", "example.net")

	answered := f.message("answered", ana, 5, .9)
	f.message("follow-up", ana, 30, .6)
	contractor := f.message("contractor", ben, 3, .8)
	copied := f.message("copied", ana, 2, .7)
	require.NoError(f.Store.ReplaceMessageRecipients(copied, "cc", []int64{dana}, []string{"Dana"}))
	f.message("outbound", f.owner, 1, .95) // the store leaves out what the owner wrote
	f.message("no vector", erin, 1, 0)
	f.message("weak", frank, 1, .49)

	after := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	got, err := f.service.Find(t.Context(), whoknows.Request{Topic: "Kubernetes   networking", After: &after})
	require.NoError(err)

	assert.Equal("Kubernetes   networking", got.Topic)
	assert.Empty(got.Limits)
	require.Len(got.People, 1)
	person := got.People[0]
	assert.Equal(anaPerson, person.PersonID)
	assert.Equal(3, person.AuthoredMessages)
	assert.Zero(person.DeclaredBoost)
	require.Len(person.Citations, 3)
	assert.Equal(answered, person.Citations[0].MessageID, "highest weight first")
	assert.False(person.Citations[0].SenderAuthenticated, "an email From header is an assertion")
	require.Len(got.UnsavedContacts, 1)
	assert.Equal(ben, got.UnsavedContacts[0].ParticipantID)
	assert.Equal(contractor, got.UnsavedContacts[0].Citations[0].MessageID)

	request := f.retriever.request
	assert.Equal(hybrid.ModeHybrid, request.Mode)
	assert.Equal("Kubernetes networking", request.FreeText)
	assert.Equal(200, request.Limit)
	assert.Equal([]string{"kubernetes", "networking"}, request.SubjectTerms)
	assert.Contains(request.Filter.MessageTypes, "email")
	assert.NotContains(request.Filter.MessageTypes, meetingarchive.MessageType)
	assert.NotContains(request.Filter.MessageTypes, "calendar_event")
	require.NotNil(request.Filter.After)
	assert.True(after.Equal(*request.Filter.After))
}

func TestFindMatchesDeclaredTopicsInGo(t *testing.T) {
	assert := assert.New(t)
	f := newServiceFixture(t)
	author, authorPerson := f.saved("author@example.net")
	_, lurkerPerson := f.saved("lurker@example.net")
	_, otherPerson := f.saved("other@example.net")
	f.declare(authorPerson, "kubernetes")
	f.declare(lurkerPerson, "kubernetes networking")
	f.declare(otherPerson, "billing")
	f.message("a", author, 5, .7)

	got := f.find("kubernetes networking")
	require.Len(t, got.People, 2)
	assert.Equal(authorPerson, got.People[0].PersonID)
	assert.Equal([]string{"kubernetes"}, got.People[0].AskMeAbout)
	assert.Equal(lurkerPerson, got.People[1].PersonID)
	assert.Zero(got.People[1].AuthoredMessages)
	assert.Equal([]string{"kubernetes networking"}, got.People[1].AskMeAbout)

	f.retriever.hits = nil
	// A declared-only person needs every topic word in a value; an author's
	// boost needs only shared words.
	_, salesPerson := f.saved("sales@example.net")
	f.declare(salesPerson, "sales")
	got = f.find("sales tax filing")
	assert.Empty(got.People, "'sales' does not answer 'sales tax filing'")
	taxAuthor, taxPerson := f.saved("tax@example.net")
	f.declare(taxPerson, "sales")
	f.message("tax", taxAuthor, 2, .8)
	got = f.find("sales tax filing")
	require.Len(t, got.People, 1)
	assert.Equal(taxPerson, got.People[0].PersonID)
	assert.Equal([]string{"sales"}, got.People[0].AskMeAbout)
}

func TestFindRestrictsMessageTypesToTheIndexScope(t *testing.T) {
	assert := assert.New(t)
	f := newServiceFixture(t)
	f.service.BuildScope = vector.NewBuildScope([]string{"email", "meeting_transcript"}, nil)
	assert.Equal([]string{whoknows.LimitPartialIndex}, f.find("topic").Limits, "an email-only index leaves chat unsearched")
	assert.Equal([]string{"email"}, f.retriever.request.Filter.MessageTypes)

	// An index of some accounts leaves the others unsearched.
	f.service.BuildScope = vector.NewBuildScope(nil, []int64{f.Source.ID})
	assert.Equal([]string{whoknows.LimitPartialIndex}, f.find("topic").Limits)
	assert.Contains(f.retriever.request.Filter.MessageTypes, "slack")

	// An index of only transcripts and events has no authors to search, but
	// people can still declare the topic.
	_, person := f.saved("lurker@example.net")
	f.declare(person, "kubernetes networking")
	f.retriever.calls = 0
	f.service.BuildScope = vector.NewBuildScope([]string{"meeting_transcript", "calendar_event"}, nil)
	got := f.find("kubernetes networking")
	assert.Zero(f.retriever.calls)
	assert.Equal([]string{whoknows.LimitPartialIndex}, got.Limits)
	require.Len(t, got.People, 1)
	assert.Equal(person, got.People[0].PersonID)
}

func TestFindReportsDeclaredTruncation(t *testing.T) {
	assert := assert.New(t)
	f := newServiceFixture(t)
	f.service.Data = truncatedDeclared{f.service.Data}
	assert.Equal([]string{whoknows.LimitDeclaredTruncated}, f.find("sailboat insurance").Limits,
		"declared topics that were not all read may hide the answer")
}

// Before answering that nobody wrote about the topic, Find asks whether a
// message of the searched types still waits for its vector, as the search
// returns only messages with one or with a keyword hit.
func TestFindChecksCoverageBeforeAnsweringEmpty(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newServiceFixture(t)
	f.waiting = 1
	assert.Equal([]string{whoknows.LimitPartialIndex}, f.find("sailboat insurance").Limits)
	require.Len(f.counted, 1)
	assert.Equal(f.retriever.request.Filter.MessageTypes, f.counted[0], "the count covers the types the search read")

	ana, _ := f.saved("ana@example.net")
	f.message("answer", ana, 1, .8)
	got := f.find("sailboat insurance")
	assert.Len(got.People, 1)
	assert.Empty(got.Limits, "an answer does not wait on coverage")
	assert.Len(f.counted, 1)

	f.retriever.hits = nil
	f.service.BuildScope = vector.NewBuildScope([]string{"email"}, nil)
	assert.Equal([]string{whoknows.LimitPartialIndex}, f.find("sailboat insurance").Limits)
	assert.Len(f.counted, 1, "an index that leaves out chat is partial already")

	f.service.BuildScope = vector.BuildScope{}
	f.waiting = 0
	assert.Empty(f.find("sailboat insurance").Limits, "every message has its vector")
	f.countErr = errors.New("count failed")
	assert.Equal([]string{whoknows.LimitPartialIndex}, f.find("sailboat insurance").Limits,
		"a wait that can't be counted is partial")
}

// The owner's mail can fill the whole pool. A real answer that made the pool
// is found; one beyond it is reported as a limit, never as nobody.
func TestFindReportsAFullPoolWhenOwnerMailFillsIt(t *testing.T) {
	assert := assert.New(t)
	f := newServiceFixture(t)
	ana, anaPerson := f.saved("ana@example.net")
	for i := range 50 {
		f.message(fmt.Sprintf("mine-%d", i), f.owner, 1, .9)
	}
	answer := f.message("answer", ana, 2, .8)
	got := f.find("kubernetes networking")
	require.Len(t, got.People, 1)
	assert.Equal(anaPerson, got.People[0].PersonID)
	assert.Equal(answer, got.People[0].Citations[0].MessageID)
	assert.Empty(got.Limits)

	// More than 100 owner vector hits push the answer out of that signal's pool.
	for i := 50; i < 150; i++ {
		f.message(fmt.Sprintf("mine-%d", i), f.owner, 1, .9)
	}
	f.retriever.hits = append(
		slices.DeleteFunc(f.retriever.hits, func(h vector.FusedHit) bool { return h.MessageID == answer }),
		vector.FusedHit{MessageID: answer, BM25Score: math.NaN(), VectorScore: .8})
	got = f.find("kubernetes networking")
	assert.Empty(got.People)
	assert.Equal([]string{whoknows.LimitPoolFull}, got.Limits)

	// A full pool with a result inside it still says more may hide beyond it.
	f.retriever.hits = slices.DeleteFunc(f.retriever.hits, func(h vector.FusedHit) bool { return h.MessageID == answer })
	f.retriever.hits = slices.Insert(f.retriever.hits, 0, vector.FusedHit{MessageID: answer, BM25Score: math.NaN(), VectorScore: .8})
	got = f.find("kubernetes networking")
	require.Len(t, got.People, 1)
	assert.Equal([]string{whoknows.LimitPoolFull}, got.Limits)

	// A full pool of messages below the floor is an off-topic query, not a
	// short search.
	for i := range f.retriever.hits {
		f.retriever.hits[i].VectorScore = .2
	}
	assert.Empty(f.find("sailboat insurance").Limits)
}

// messageScores maps each listed person to their message score.
func messageScores(people []whoknows.Entry) map[int64]float64 {
	out := map[int64]float64{}
	for _, p := range people {
		out[p.PersonID] = p.MessageScore
	}
	return out
}

// The store's list-mail and one-way flags reach Rank. A list sender with
// twelve messages and a one-way sender with thirty each earn one message's
// worth, so they score alike and rank below the colleague who answered twice.
func TestFindBoundsListMailAndOneWayEmailSenders(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newServiceFixture(t)
	editor, editorPerson := f.saved("editor@example.net")
	vendor, vendorPerson := f.saved("vendor@example.net")
	other, otherPerson := f.saved("other@example.net")
	// The owner has written to the editor and to Other, never to the vendor.
	reply := f.message("reply", f.owner, 40, 0)
	require.NoError(f.Store.ReplaceMessageRecipients(reply, "to", []int64{editor, other}, []string{"Editor", "Other"}))
	f.retriever.hits = nil
	for i := range 12 {
		f.message(fmt.Sprintf("digest-%d", i), editor, 0, .9, func(m *store.Message) {
			m.ListID = sql.NullString{String: "<digest.example.net>", Valid: true}
		})
	}
	for i := range 30 {
		f.message(fmt.Sprintf("vendor-%d", i), vendor, 0, .9)
	}
	for i := range 2 {
		f.message(fmt.Sprintf("answer-%d", i), other, 0, .8)
	}

	got := f.find("kubernetes networking")
	require.Len(got.People, 3)
	assert.Equal(otherPerson, got.People[0].PersonID, "two answers beat twelve digests and thirty unanswered notes")
	scores := messageScores(got.People)
	assert.InDelta(scores[editorPerson], scores[vendorPerson], 1e-9)
	assert.Greater(scores[otherPerson], scores[editorPerson])
}

func TestFindSkipsSearchOnBadInputAndPropagatesSearchErrors(t *testing.T) {
	f := newServiceFixture(t)
	_, err := f.service.Find(t.Context(), whoknows.Request{Topic: "from:ana networking"})
	require.Error(t, err)
	assert.Zero(t, f.retriever.calls)

	f.retriever.err = errors.New("search failed")
	_, err = f.service.Find(t.Context(), whoknows.Request{Topic: "topic"})
	require.ErrorIs(t, err, f.retriever.err)
}
