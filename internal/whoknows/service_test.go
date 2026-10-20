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

// fixedRetriever returns its hits cut to the request limit, saturated when
// the cut dropped any, as the hybrid engine does for a full pool.
type fixedRetriever struct {
	hits    []vector.FusedHit
	err     error
	calls   int
	request hybrid.SearchRequest
}

func (r *fixedRetriever) Search(_ context.Context, request hybrid.SearchRequest) ([]vector.FusedHit, hybrid.ResultMeta, error) {
	r.calls++
	r.request = request
	hits := r.hits[:min(len(r.hits), request.Limit)]
	return hits, hybrid.ResultMeta{PoolSaturated: len(hits) < len(r.hits)}, r.err
}

// truncatedDeclared reports that the declared read hit its cap.
type truncatedDeclared struct{ whoknows.DataReader }

func (truncatedDeclared) AskMeAboutValuesContext(context.Context, []int64) ([]store.PersonAskMeAbout, bool, error) {
	return nil, true, nil
}

// fixtureCount answers the vector-wait count from the fixture, since no
// embedding worker stamps the fixture's messages.
type fixtureCount struct {
	*store.Store
	f *serviceFixture
}

func (c fixtureCount) CoverageCountsScoped(_ context.Context, _ int64, types []string, _ []int64) (int64, int64, int64, int64, error) {
	c.f.counted = append(c.f.counted, types)
	return 0, 0, 0, c.f.waiting, nil
}

var serviceNow = time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)

type serviceFixture struct {
	*storetest.Fixture

	retriever *fixedRetriever
	service   *whoknows.Service
	owner     int64
	// waiting is how many messages the vector-wait count reports, and
	// counted the message types of each count.
	waiting int64
	counted [][]string
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
		Now: func() time.Time { return serviceNow },
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

func TestFindCreditsOnlyWhoWroteAndCountsCosineMatches(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newServiceFixture(t)
	chat, err := f.Store.GetOrCreateSource("slack", "workspace")
	require.NoError(err)
	room, err := f.Store.EnsureConversation(chat.ID, "room", "Room")
	require.NoError(err)
	inRoom := func(m *store.Message) { m.SourceID, m.ConversationID, m.MessageType = chat.ID, room, "slack" }

	ana, anaPerson := f.saved("ana@example.net")
	ben := f.EnsureParticipant("ben@example.net", "Ben Contractor", "example.net")
	dana, _ := f.saved("dana@example.net")
	robot := f.EnsureParticipant("no_reply@example.net", "Robot", "example.net")
	relay := f.EnsureParticipant("relay@example.net", "Chat Relay", "example.net")
	erin := f.EnsureParticipant("erin@example.net", "Erin", "example.net")
	frank := f.EnsureParticipant("frank@example.net", "Frank", "example.net")
	alias := f.EnsureParticipant("owner.work@example.net", "Owner Work", "example.net")
	_, err = f.Store.LinkParticipants(f.owner, alias)
	require.NoError(err)
	_, _, err = f.Store.CreatePersonFromParticipant(f.owner)
	require.NoError(err)

	answered := f.message("answered", ana, 5, .9)
	f.message("follow-up", ana, 30, .6)
	contractor := f.message("contractor", ben, 3, .8)
	copied := f.message("copied", ana, 2, .7)
	require.NoError(f.Store.ReplaceMessageRecipients(copied, "cc", []int64{dana}, []string{"Dana"}))
	f.message("outbound", f.owner, 1, .95)
	f.message("alias", alias, 1, .95)
	f.message("robot", robot, 1, .95)
	bot := f.message("bot", relay, 1, .95, inRoom)
	require.NoError(f.Store.SetMessageMetadata(bot, sql.NullString{String: `{"automated":true}`, Valid: true}))
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
	assert.InDelta(.75, got.People[0].DeclaredBoost, 1e-9)
	assert.Equal(lurkerPerson, got.People[1].PersonID)
	assert.Zero(got.People[1].AuthoredMessages)
	assert.InDelta(.75, got.People[1].Score, 1e-9)

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
	assert.InDelta(.75, got.People[0].DeclaredBoost, 1e-9)
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

func TestFindReportsLimitsOnlyWhenTheSearchCameUpShort(t *testing.T) {
	assert := assert.New(t)
	f := newServiceFixture(t)
	ana, _ := f.saved("ana@example.net")

	assert.Empty(f.find("sailboat insurance").Limits, "no hits")
	f.message("weak", ana, 1, .3)
	f.message("mine", f.owner, 1, .99)
	assert.Empty(f.find("sailboat insurance").Limits, "an unsaturated search that found only the owner's mail is complete")

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
}

// The owner's mail can fill the whole pool. A real answer that made the pool
// is found; one beyond it is reported as a limit, never as nobody.
func TestFindReportsAFullPoolWhenOwnerMailFillsIt(t *testing.T) {
	assert := assert.New(t)
	f := newServiceFixture(t)
	ana, anaPerson := f.saved("ana@example.net")
	for i := range 150 {
		f.message(fmt.Sprintf("mine-%d", i), f.owner, 1, .9)
	}
	answer := f.message("answer", ana, 2, .8)
	got := f.find("kubernetes networking")
	require.Len(t, got.People, 1)
	assert.Equal(anaPerson, got.People[0].PersonID)
	assert.Equal(answer, got.People[0].Citations[0].MessageID)
	assert.Empty(got.Limits)

	// 300 owner messages ahead of the same answer push it out of the pool.
	for i := 150; i < 300; i++ {
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

// Linked addresses are one sender, and so are the addresses of one saved
// person, so each ranks once.
func TestFindRanksLinkedAddressesOnce(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newServiceFixture(t)
	home := f.EnsureParticipant("dee@example.net", "Dee", "example.net")
	work := f.EnsureParticipant("dee@work.example.net", "Dee Work", "work.example.net")
	_, err := f.Store.LinkParticipants(home, work)
	require.NoError(err)
	f.message("home", home, 1, .8)
	f.message("work", work, 2, .8)

	got := f.find("kubernetes networking")
	require.Len(got.UnsavedContacts, 1)
	assert.Equal(2, got.UnsavedContacts[0].AuthoredMessages)
	assert.Empty(got.People)

	person, _, err := f.Store.CreatePersonFromParticipant(work)
	require.NoError(err)
	got = f.find("kubernetes networking")
	require.Len(got.People, 1)
	assert.Equal(person.ID, got.People[0].PersonID)
	assert.Equal(2, got.People[0].AuthoredMessages)
	assert.Empty(got.UnsavedContacts)
}

// messageScores maps each listed person to their message score.
func messageScores(people []whoknows.Entry) map[int64]float64 {
	out := map[int64]float64{}
	for _, p := range people {
		out[p.PersonID] = p.MessageScore
	}
	return out
}

func TestFindBoundsListMail(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newServiceFixture(t)
	editor, editorPerson := f.saved("editor@example.net")
	other, otherPerson := f.saved("other@example.net")
	// The owner has written to both, so neither is one-way.
	reply := f.message("reply", f.owner, 40, 0)
	require.NoError(f.Store.ReplaceMessageRecipients(reply, "to", []int64{editor, other}, []string{"Editor", "Other"}))
	f.retriever.hits = nil
	for i := range 12 {
		f.message(fmt.Sprintf("digest-%d", i), editor, 0, .9, func(m *store.Message) {
			m.ListID = sql.NullString{String: "<digest.example.net>", Valid: true}
		})
	}
	for i := range 2 {
		f.message(fmt.Sprintf("answer-%d", i), other, 0, .8)
	}

	got := f.find("kubernetes networking")
	require.Len(got.People, 2)
	assert.Equal(otherPerson, got.People[0].PersonID, "two answers beat twelve digests")
	assert.Equal(map[int64]float64{otherPerson: math.Log1p(1.6), editorPerson: math.Log1p(.9)}, messageScores(got.People))
}

func TestFindBoundsOneWayEmailSenders(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newServiceFixture(t)
	vendor, vendorPerson := f.saved("vendor@example.net")
	other, otherPerson := f.saved("other@example.net")
	// The owner has written to Other, never to the vendor.
	reply := f.message("reply", f.owner, 40, 0)
	require.NoError(f.Store.ReplaceMessageRecipients(reply, "to", []int64{other}, []string{"Other"}))
	f.retriever.hits = nil
	for i := range 30 {
		f.message(fmt.Sprintf("vendor-%d", i), vendor, 0, .9)
	}
	for i := range 2 {
		f.message(fmt.Sprintf("answer-%d", i), other, 0, .8)
	}

	got := f.find("kubernetes networking")
	require.Len(got.People, 2)
	assert.Equal(otherPerson, got.People[0].PersonID, "a two-way answerer beats thirty unanswered notes")
	assert.Equal(map[int64]float64{otherPerson: math.Log1p(1.6), vendorPerson: math.Log1p(.9)}, messageScores(got.People))
	assert.Equal(30, got.People[1].AuthoredMessages)
}

// Chat has no direction: a colleague the owner reads in a channel but never
// writes to keeps every message.
func TestFindGivesChatSendersFullCredit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newServiceFixture(t)
	chat, err := f.Store.GetOrCreateSource("slack", "workspace")
	require.NoError(err)
	channel, err := f.Store.EnsureConversation(chat.ID, "channel", "Channel")
	require.NoError(err)
	priya, priyaPerson := f.saved("priya@example.net")
	for i := range 3 {
		f.message(fmt.Sprintf("post-%d", i), priya, 0, .8, func(m *store.Message) {
			m.SourceID, m.ConversationID, m.MessageType = chat.ID, channel, "slack"
		})
	}

	got := f.find("kubernetes networking")
	require.Len(got.People, 1)
	assert.Equal(priyaPerson, got.People[0].PersonID)
	assert.InDelta(math.Log1p(2.4), got.People[0].MessageScore, 1e-9)
}

// Declared topics match by whole words without regard to case.
func TestFindFoldsCaseInDeclaredTopics(t *testing.T) {
	f := newServiceFixture(t)
	_, person := f.saved("lurker@example.net")
	f.declare(person, "Straße networking")
	got := f.find("strasse networking")
	require.Len(t, got.People, 1)
	assert.Equal(t, person, got.People[0].PersonID)
	assert.Equal(t, []string{"Straße networking"}, got.People[0].AskMeAbout)
}

func TestFindStopsBeforeSearchingOnBadInput(t *testing.T) {
	f := newServiceFixture(t)
	_, err := f.service.Find(t.Context(), whoknows.Request{Topic: "from:ana networking"})
	require.ErrorIs(t, err, whoknows.ErrInvalidParameter)
	assert.Zero(t, f.retriever.calls)

	f.retriever.err = errors.New("search failed")
	_, err = f.service.Find(t.Context(), whoknows.Request{Topic: "topic"})
	require.ErrorIs(t, err, f.retriever.err)
}
