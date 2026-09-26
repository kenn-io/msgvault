package meetingarchive

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type linkFixture struct {
	st       *store.Store
	archiver *Archiver
	sourceID int64
}

func newLinkFixture(t *testing.T) linkFixture {
	t.Helper()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("meeting_import", "links")
	require.NoError(t, err)
	return linkFixture{st: st, archiver: New(st), sourceID: source.ID}
}

func (f linkFixture) emailParticipant(t *testing.T, email string) int64 {
	t.Helper()
	id, err := f.st.EnsureParticipant(email, "", emailDomain(email))
	require.NoError(t, err)
	return id
}

func (f linkFixture) phoneParticipant(t *testing.T, phone string) int64 {
	t.Helper()
	id, err := f.st.EnsureParticipantByPhone(phone, "", "imessage")
	require.NoError(t, err)
	return id
}

func (f linkFixture) person(t *testing.T, participantID int64) int64 {
	t.Helper()
	person, _, err := f.st.CreatePersonFromParticipantContext(t.Context(), participantID)
	require.NoError(t, err)
	return person.ID
}

func (f linkFixture) linked(t *testing.T, a, b int64) bool {
	t.Helper()
	members, err := f.st.ClusterMembers(a)
	require.NoError(t, err)
	return slices.Contains(members, b)
}

func (f linkFixture) boundPerson(t *testing.T, participantID int64) int64 {
	t.Helper()
	var personID int64
	err := f.st.DB().QueryRow(f.st.Rebind(
		`SELECT person_id FROM person_participants WHERE participant_id = ?`), participantID).Scan(&personID)
	if err != nil {
		return 0
	}
	return personID
}

func (f linkFixture) candidates(t *testing.T) []store.IdentityMatchCandidate {
	t.Helper()
	candidates, err := f.st.ListIdentityMatchCandidatesContext(t.Context(), nil, 500, 0)
	require.NoError(t, err)
	return candidates
}

func (f linkFixture) link(t *testing.T, people ...Person) LinkResult {
	t.Helper()
	result, err := f.archiver.LinkIdentities(t.Context(), f.sourceID, people)
	require.NoError(t, err)
	return result
}

func TestAnchorIsStableAndLengthPrefixed(t *testing.T) {
	assert := assert.New(t)

	first := Anchor("meeting-import", "a:b", "c")
	assert.Equal(first, Anchor("meeting-import", "a:b", "c"))
	assert.NotEqual(first, Anchor("meeting-import", "a", "b:c"))
	assert.NotEqual(first, Anchor("notion-user", "a:b", "c"))
	assert.Regexp(`^meeting-import:[0-9a-f]{64}$`, first)
	assert.NotContains(first, "a:b")
}

func TestLinkIdentitiesJoinsPhoneToExistingPerson(t *testing.T) {
	assert := assert.New(t)
	f := newLinkFixture(t)
	email := f.emailParticipant(t, "alex@example.com")
	personID := f.person(t, email)

	result := f.link(t, Person{
		Name: "Alex Example", Email: "alex@example.com", Phone: "+16045550100",
		OtherEmails: []string{"alex.work@example.com"},
		Anchor:      Anchor("apple-contact", "card-1"),
	})

	phone := f.phoneParticipant(t, "+16045550100")
	work := f.emailParticipant(t, "alex.work@example.com")
	assert.Equal(2, result.Linked)
	assert.True(f.linked(t, email, phone))
	assert.True(f.linked(t, email, work))
	assert.Equal(personID, f.boundPerson(t, phone), "the phone joins the person automatically")
	assert.Equal(personID, f.boundPerson(t, work))
}

func TestLinkIdentitiesLinksSightingsAcrossMeetings(t *testing.T) {
	f := newLinkFixture(t)
	anchor := Anchor("meeting-import", "crm", "42")

	f.link(t, Person{Email: "sam@example.com", Anchor: anchor})
	f.link(t, Person{Phone: "+16045550101", Anchor: anchor})

	assert.True(t, f.linked(t,
		f.emailParticipant(t, "sam@example.com"), f.phoneParticipant(t, "+16045550101")))
}

func TestLinkIdentitiesNeverJoinsTwoPeople(t *testing.T) {
	assert := assert.New(t)
	f := newLinkFixture(t)
	email := f.emailParticipant(t, "one@example.com")
	phone := f.phoneParticipant(t, "+16045550102")
	first := f.person(t, email)
	second := f.person(t, phone)

	result := f.link(t, Person{
		Email: "one@example.com", Phone: "+16045550102", Anchor: Anchor("notion-user", "u1"),
	})

	assert.Equal(1, result.Conflicts)
	assert.False(f.linked(t, email, phone))
	assert.Equal(first, f.boundPerson(t, email))
	assert.Equal(second, f.boundPerson(t, phone))
	candidates := f.candidates(t)
	require.Len(t, candidates, 1)
	assert.Equal(store.IdentityMatchStateConflict, candidates[0].State)
}

func TestLinkIdentitiesSendsReusedAddressToReview(t *testing.T) {
	assert := assert.New(t)
	f := newLinkFixture(t)
	f.link(t, Person{Email: "desk@example.com", Phone: "+16045550103", Anchor: Anchor("apple-contact", "card-a")})
	desk := f.emailParticipant(t, "desk@example.com")
	personID := f.person(t, desk)

	result := f.link(t, Person{Email: "desk@example.com", Phone: "+16045550104", Anchor: Anchor("apple-contact", "card-b")})

	other := f.phoneParticipant(t, "+16045550104")
	assert.Equal(1, result.Conflicts)
	assert.False(f.linked(t, desk, other), "a second card claiming the same email must not expand the person")
	assert.Zero(f.boundPerson(t, other))
	assert.Equal(personID, f.boundPerson(t, f.phoneParticipant(t, "+16045550103")))
	var conflict bool
	for _, candidate := range f.candidates(t) {
		if candidate.State == store.IdentityMatchStateConflict &&
			slices.Contains([]int64{candidate.LeftID, candidate.RightID}, other) {
			conflict = true
		}
	}
	assert.True(conflict, "the contradiction is reviewable")

	// A different provider namespace is independent evidence.
	f.link(t, Person{Email: "desk@example.com", Phone: "+16045550105", Anchor: Anchor("notion-user", "u9")})
	assert.True(f.linked(t, desk, f.phoneParticipant(t, "+16045550105")))
}

func TestLinkIdentitiesRespectsUserRejection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newLinkFixture(t)
	anchor := Anchor("meeting-import", "crm", "7")
	person := Person{Email: "kim@example.com", Phone: "+16045550106", Anchor: anchor}
	f.link(t, person)
	candidates := f.candidates(t)
	require.Len(candidates, 1)
	_, err := f.st.DecideIdentityMatchCandidateContext(
		t.Context(), candidates[0].ID, store.IdentityMatchStateRejected, "user", nil)
	require.NoError(err)

	f.link(t, person)

	email := f.emailParticipant(t, "kim@example.com")
	phone := f.phoneParticipant(t, "+16045550106")
	assert.False(f.linked(t, email, phone))
	current, err := f.st.GetIdentityMatchCandidateContext(t.Context(), candidates[0].ID)
	require.NoError(err)
	assert.Equal(store.IdentityMatchStateRejected, current.State)

	// Rejection is candidate-scoped: a later sighting led by a third identity
	// of the same anchor pairs it with both, which connects them through it.
	f.link(t, Person{Email: "kim.home@example.com", Anchor: anchor})
	assert.True(f.linked(t, email, phone))
}

func TestUpsertLinksOnUnchangedSnapshotsWithoutRewriting(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newLinkFixture(t)
	snapshot := testSnapshot(f.sourceID)
	snapshot.Attendees = []Person{{Name: "Pat Example", Email: "pat@example.com", Phone: "+16045550107"}}

	first, err := f.archiver.Upsert(t.Context(), snapshot, UpsertOptions{})
	require.NoError(err)
	email := f.emailParticipant(t, "pat@example.com")
	assert.False(f.linked(t, email, f.phoneParticipant(t, "+16045550107")), "no anchor, no link")

	// The same raw snapshot now carries a provider anchor, as after an
	// upgrade or a failed linking attempt.
	snapshot.Attendees[0].Anchor = Anchor("meeting-import", "crm", "9")
	second, err := f.archiver.Upsert(t.Context(), snapshot, UpsertOptions{})
	require.NoError(err)
	assert.False(second.Changed)
	assert.Equal(first.MessageID, second.MessageID)
	assert.Equal(1, second.Links.Linked)
	assert.True(f.linked(t, email, f.phoneParticipant(t, "+16045550107")))

	before := len(f.candidates(t))
	third, err := f.archiver.Upsert(t.Context(), snapshot, UpsertOptions{})
	require.NoError(err)
	assert.Equal(LinkResult{Settled: 1}, third.Links, "a settled person takes the read-only fast path")
	assert.Len(f.candidates(t), before)
}

func TestLinkIdentitiesNeverLinksTheAccountOwner(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newLinkFixture(t)
	require.NoError(f.st.AddAccountIdentity(f.sourceID, "owner@example.com", "account-email"))
	household := f.phoneParticipant(t, "+16045550110")
	spouse := f.person(t, household)

	f.link(t, Person{Email: "owner@example.com", Phone: "+16045550110", Anchor: Anchor("apple-contact", "my-card")})

	owner := f.emailParticipant(t, "owner@example.com")
	assert.False(f.linked(t, owner, household), "the owner's own card must not tie them to a shared number")
	assert.Zero(f.boundPerson(t, owner))
	assert.Equal(spouse, f.boundPerson(t, household))
}

func TestLinkIdentitiesAcrossSourcesIsNotAContradiction(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newLinkFixture(t)
	f.link(t, Person{Email: "lee@example.com", Phone: "+16045550111", Anchor: Anchor("meeting-import", "crm-a", "1")})
	other, err := f.st.GetOrCreateSource("meeting_import", "crm-b")
	require.NoError(err)

	result, err := f.archiver.LinkIdentities(t.Context(), other.ID, []Person{
		{Email: "lee@example.com", Phone: "+16045550112", Anchor: Anchor("meeting-import", "crm-b", "7")},
	})
	require.NoError(err)

	assert.Equal(0, result.Conflicts, "another source's anchor is independent evidence")
	assert.True(f.linked(t, f.emailParticipant(t, "lee@example.com"), f.phoneParticipant(t, "+16045550112")))
}
