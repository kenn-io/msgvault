package muesli

import (
	"context"
	"fmt"
	"go.kenn.io/msgvault/internal/store"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAmbiguousContactsSupplyOnlyReviewPhones(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newResolveFixture(t,
		fixtureCard{uniqueID: "CARD-A:ABPerson", emails: []string{"attendee@example.com"}, phones: []string{"+1 604 555 0100"}},
		fixtureCard{uniqueID: "CARD-B:ABPerson", emails: []string{"attendee@example.com"}, phones: []string{"+16045550100", "+16045550101"}},
	)
	contacts, err := OpenContacts(t.Context(), f.contacts)
	require.NoError(err)
	m := Meeting{Participants: []Participant{{Email: "attendee@example.com", Identifier: "email:attendee@example.com"}}}
	require.NoError(f.imp.resolveParticipants(f.source.ID, &m, contacts, "", contacts.sharedAddresses("")))
	p := m.Participants[0]
	assert.Equal([]string{"+16045550100", "+16045550101"}, p.ContactReviewPhones)
	assert.Empty(p.Anchor)
	assert.Empty(p.Resolution)
	assert.Empty(p.archivePerson().OtherPhones)
	assert.Empty(p.archivePerson().Phone)
	raw := p.raw(p.archivePerson())
	assert.Equal(p.ContactReviewPhones, raw.ContactReviewPhones)
	remote := NewRemoteMeeting(m).toMeeting()
	assert.Equal(p.ContactReviewPhones, remote.Participants[0].ContactReviewPhones)
	for _, state := range []ContactsState{ContactsPartial, ContactsUnavailable, ContactsOff} {
		contacts.state = state
		assert.Empty(contacts.reviewPhones("attendee@example.com", ""))
	}
}

func TestRemoteContactReviewValidation(t *testing.T) {
	for _, tc := range []struct {
		name              string
		count             int
		state             ContactsState
		resolution, email string
		duplicates, valid bool
	}{
		{"boundary", 49, ContactsComplete, "", "attendee@example.com", false, true},
		{"distinct overflow", 50, ContactsComplete, "", "attendee@example.com", false, false},
		{"raw overflow", 51, ContactsComplete, "", "attendee@example.com", true, false},
		{"resolved without anchor", 1, ContactsComplete, resolutionResolved, "attendee@example.com", false, false},
		{"carried", 1, ContactsComplete, resolutionCarried, "attendee@example.com", false, true},
		{"partial", 1, ContactsPartial, "", "attendee@example.com", false, false},
		{"off", 1, ContactsOff, "", "attendee@example.com", false, false},
		{"unavailable", 1, ContactsUnavailable, "", "attendee@example.com", false, false},
		{"no email", 1, ContactsComplete, "", "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			r := remoteTestRequest()
			r.Meeting.ContactsState = tc.state
			p := &r.Meeting.Participants[0]
			p.Email = tc.email
			p.Resolution = tc.resolution
			for i := range tc.count {
				n := i
				if tc.duplicates {
					n = 0
				}
				p.ContactReviewPhones = append(p.ContactReviewPhones, fmt.Sprintf("+1604555%04d", 100+n))
			}
			_, err := r.Normalize()
			if tc.valid {
				require.NoError(err)
			} else {
				require.ErrorIs(err, ErrRemoteValidation)
				assert.NotContains(err.Error(), "attendee@example.com")
			}
		})
	}
}

func TestContactReviewSurvivesDuplicateAttendees(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := remoteTestRequest().Meeting.toMeeting()
	m.Participants = []Participant{
		{Email: "attendee@example.com", Identifier: "calendar:synthetic"},
		{Email: "attendee@example.com", Identifier: "email:attendee@example.com", ContactReviewPhones: []string{"+16045550100"}},
	}
	people := dedupeParticipants(m.Participants)
	require.Len(people, 1)
	assert.Equal([]string{"+16045550100"}, people[0].ContactReviewPhones)
	snapshot, err := m.ArchiveSnapshot(1, "mac", "you@example.com")
	require.NoError(err)
	assert.NotContains(string(snapshot.Raw), "calendar:synthetic")
	assert.Contains(string(snapshot.Raw), "contact_review_phones")
}

func TestResolvedDuplicateSupersedesCarriedContactEvidence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newImporterFixture(t)
	_, err := f.st.EnsureParticipant("attendee@example.com", "Attendee", "example.com")
	require.NoError(err)
	_, err = f.st.EnsureParticipantByPhone("+16045550100", "Existing Chat", "imessage")
	require.NoError(err)

	m := remoteTestRequest().Meeting.toMeeting()
	m.ContactsState = ContactsComplete
	m.Participants = []Participant{
		{
			Name: "Calendar attendee", Email: "attendee@example.com", Identifier: "calendar:synthetic-event",
			ContactEmails: []string{"old@example.com"}, ContactPhones: []string{"+16045550101"},
			ContactReviewPhones: []string{"+16045550100"}, Resolution: resolutionCarried,
		},
		{
			Email: "attendee@example.com", Identifier: "contact:synthetic-card",
			ContactEmails: []string{"new@example.com"}, ContactPhones: []string{"+16045550102"},
			Anchor: "apple-contact:synthetic-card", Resolution: resolutionResolved,
		},
	}

	people := dedupeParticipants(m.Participants)
	require.Len(people, 1)
	assert.Equal(resolutionResolved, people[0].Resolution)
	assert.Equal("apple-contact:synthetic-card", people[0].Anchor)
	assert.Equal([]string{"new@example.com"}, people[0].ContactEmails)
	assert.Equal([]string{"+16045550102"}, people[0].ContactPhones)
	assert.Empty(people[0].ContactReviewPhones)

	require.NoError(f.imp.recordContactReviewCandidates(t.Context(), f.source.ID, m))
	candidates, err := f.st.ListIdentityMatchReviewsContext(t.Context(), nil, 100, 0)
	require.NoError(err)
	assert.Empty(candidates, "resolved duplicate evidence must not create a review candidate from carried phones")
}

func TestRemoteContactReviewRejectsUnnormalizedPhones(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	r := remoteTestRequest()
	r.Meeting.ContactsState = ContactsComplete
	for _, phone := range []string{"+1 604 555 0100", "secret-private-phone", "6045550100"} {
		r.Meeting.Participants[0].ContactReviewPhones = []string{phone}
		_, err := r.Normalize()
		require.ErrorIs(err, ErrRemoteValidation)
		assert.NotContains(err.Error(), phone)
	}
}

func TestDuplicateContactsNativeReviewedAttribution(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newResolveFixture(t,
		fixtureCard{uniqueID: "CARD-A:ABPerson", emails: []string{"attendee@example.com"}, phones: []string{"+16045550100"}},
		fixtureCard{uniqueID: "CARD-B:ABPerson", emails: []string{"attendee@example.com"}, phones: []string{"+16045550101"}},
	)
	meetingID := insertRow(t, f.muesli, "meetings", completedMeeting(nil))
	insertRow(t, f.muesli, "meeting_participants", map[string]any{"meeting_id": meetingID, "participant_identifier": "email:attendee@example.com", "email_address": "attendee@example.com", "display_name": "Meeting Attendee", "source": "calendar", "insertion_order": 0})
	f.sync(t, ImportOptions{})
	candidates, err := f.st.ListIdentityMatchReviewsContext(t.Context(), nil, 100, 0)
	require.NoError(err)
	require.Len(candidates, 1)
	candidate := candidates[0]
	assert.Equal(store.IdentityMatchStateCandidate, candidate.State)
	assert.Equal(store.IdentityMatchEmail, candidate.Basis)
	require.Len(candidate.Evidence, 1)
	require.NotNil(candidate.Evidence[0].Detail)
	assert.Contains(*candidate.Evidence[0].Detail, "attendee@example.com")
	assert.Contains(*candidate.Evidence[0].Detail, "+16045550100")
	assert.Contains(*candidate.Evidence[0].Detail, "historical")
	emailID, err := f.st.EnsureParticipant("attendee@example.com", "", "example.com")
	require.NoError(err)
	members, err := f.st.ClusterMembers(f.phoneID)
	require.NoError(err)
	assert.NotContains(members, emailID)
	var phoneCount int
	require.NoError(f.st.DB().QueryRow(`SELECT count(*) FROM participants WHERE phone_number IS NOT NULL`).Scan(&phoneCount))
	assert.Equal(1, phoneCount, "an unknown suggested phone must not create a participant")
	f.project(t)
	assert.NotContains(f.meetingPersons(t), f.personID)
	summary := f.sync(t, ImportOptions{})
	assert.Zero(summary.MeetingsUpdated)
	repeated, err := f.st.GetIdentityMatchReviewContext(t.Context(), candidate.ID)
	require.NoError(err)
	assert.Equal(candidate.ReviewToken, repeated.ReviewToken)
	assert.Len(repeated.Evidence, 1)
	_, _, err = f.st.DecideIdentityMatchReviewedContext(t.Context(), candidate.ID, candidate.ReviewToken, store.IdentityMatchStateAccepted, nil)
	require.NoError(err)
	f.project(t)
	assert.Contains(f.meetingPersons(t), f.personID)
	f.sync(t, ImportOptions{})
	accepted, err := f.st.GetIdentityMatchReviewContext(t.Context(), candidate.ID)
	require.NoError(err)
	assert.Equal(store.IdentityMatchStateAccepted, accepted.State)
}

func TestRemoteContactReviewRejectionAndLaterPhoneDiscovery(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newImporterFixture(t)
	registerRemoteFixture(t, f)
	r := remoteTestRequest()
	r.Meeting.ContactsState = ContactsComplete
	r.Meeting.Participants[0].ContactReviewPhones = []string{"+16045550100", "+16045550101"}
	first, err := f.imp.ImportRemote(t.Context(), r)
	require.NoError(err)
	assert.True(first.Changed)
	candidates, err := f.st.ListIdentityMatchReviewsContext(t.Context(), nil, 100, 0)
	require.NoError(err)
	assert.Empty(candidates)
	_, err = f.st.EnsureParticipantByPhone("+16045550100", "Existing Chat", "imessage")
	require.NoError(err)
	before, err := f.st.IdentityRevisionContext(t.Context())
	require.NoError(err)
	unchanged, err := f.imp.ImportRemote(t.Context(), r)
	require.NoError(err)
	assert.False(unchanged.Changed)
	after, err := f.st.IdentityRevisionContext(t.Context())
	require.NoError(err)
	assert.Equal(before, after)
	candidates, err = f.st.ListIdentityMatchReviewsContext(t.Context(), nil, 100, 0)
	require.NoError(err)
	require.Len(candidates, 1)
	candidate := candidates[0]
	_, _, err = f.st.DecideIdentityMatchReviewedContext(t.Context(), candidate.ID, candidate.ReviewToken, store.IdentityMatchStateRejected, nil)
	require.NoError(err)
	rejected, err := f.st.GetIdentityMatchReviewContext(t.Context(), candidate.ID)
	require.NoError(err)
	_, err = f.imp.ImportRemote(t.Context(), r)
	require.NoError(err)
	retry, err := f.st.GetIdentityMatchReviewContext(t.Context(), candidate.ID)
	require.NoError(err)
	assert.Equal(store.IdentityMatchStateRejected, retry.State)
	assert.Equal(rejected.ReviewToken, retry.ReviewToken)
	_, err = f.st.EnsureParticipantByPhone("+16045550101", "Other Chat", "imessage")
	require.NoError(err)
	unchanged, err = f.imp.ImportRemote(t.Context(), r)
	require.NoError(err)
	assert.False(unchanged.Changed)
	candidates, err = f.st.ListIdentityMatchReviewsContext(t.Context(), nil, 100, 0)
	require.NoError(err)
	require.Len(candidates, 2)
	assert.Equal(store.IdentityMatchStateRejected, candidates[0].State)
	assert.Equal(store.IdentityMatchStateCandidate, candidates[1].State)
	r.Meeting.Participants[0].Email = "other@example.com"
	r.Meeting.Participants[0].Ref = participantRef("email:other@example.com")
	edited, err := f.imp.ImportRemote(t.Context(), r)
	require.NoError(err)
	assert.True(edited.Changed)
	candidates, err = f.st.ListIdentityMatchReviewsContext(t.Context(), nil, 100, 0)
	require.NoError(err)
	require.Len(candidates, 4)
	assert.Equal(store.IdentityMatchStateRejected, candidates[0].State)
}

func TestContactReviewSkipsOwnerAndPartialEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, email string
		state       ContactsState
		ownerPhone  bool
	}{
		{"owner email", "you@example.com", ContactsComplete, false},
		{"owner phone", "attendee@example.com", ContactsComplete, true},
		{"partial", "attendee@example.com", ContactsPartial, false},
		{"off", "attendee@example.com", ContactsOff, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newImporterFixture(t)
			registerRemoteFixture(t, f)
			_, err := f.st.EnsureParticipantByPhone("+16045550100", "Existing Chat", "imessage")
			require.NoError(err)
			if tc.ownerPhone {
				require.NoError(f.st.AddAccountIdentityContext(t.Context(), f.source.ID, "+16045550100", "self-id"))
			}
			r := remoteTestRequest()
			r.Meeting.ContactsState = ContactsComplete
			r.Meeting.Participants[0].Email = tc.email
			r.Meeting.Participants[0].ContactReviewPhones = []string{"+16045550100"}
			m := r.Meeting.toMeeting()
			m.ContactsState = tc.state
			if tc.state != ContactsComplete {
				r.Meeting.Participants[0].ContactReviewPhones = nil
			}
			_, err = f.imp.ImportRemote(t.Context(), r)
			require.NoError(err)
			require.NoError(f.imp.recordContactReviewCandidates(t.Context(), f.source.ID, m))
			// The incomplete states are also rejected by transport before any upsert.
			if tc.state != ContactsComplete {
				r.Meeting.ContactsState = tc.state
				r.Meeting.Participants[0].ContactReviewPhones = []string{"+16045550100"}
				_, err = r.Normalize()
				require.ErrorIs(err, ErrRemoteValidation)
			}
			candidates, err := f.st.ListIdentityMatchReviewsContext(t.Context(), nil, 100, 0)
			require.NoError(err)
			assert.Empty(candidates)
		})
	}
}

func TestContactReviewCombinedDuplicateLimit(t *testing.T) {
	require := require.New(t)
	r := remoteTestRequest()
	r.Meeting.ContactsState = ContactsComplete
	p := r.Meeting.Participants[0]
	for i := range 30 {
		p.ContactReviewPhones = append(p.ContactReviewPhones, fmt.Sprintf("+1604555%04d", 100+i))
	}
	r.Meeting.Participants[0] = p
	other := p
	other.Ref = participantRef("calendar:synthetic")
	other.ContactReviewPhones = nil
	for i := 30; i < 60; i++ {
		other.ContactReviewPhones = append(other.ContactReviewPhones, fmt.Sprintf("+1604555%04d", 100+i))
	}
	r.Meeting.Participants = append(r.Meeting.Participants, other)
	_, err := r.Normalize()
	require.ErrorIs(err, ErrRemoteValidation)
}

func TestContactReviewSuggestsAbsorbedPhoneAlias(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newImporterFixture(t)
	registerRemoteFixture(t, f)
	survivor, err := f.st.EnsureParticipantByPhone("+16045550100", "Existing Chat", "imessage")
	require.NoError(err)
	absorbed, err := f.st.EnsureParticipantByPhone("+16045550101", "Older Chat", "imessage")
	require.NoError(err)
	require.NoError(f.st.MergeParticipants(absorbed, survivor))
	r := remoteTestRequest()
	r.Meeting.ContactsState = ContactsComplete
	r.Meeting.Participants[0].ContactReviewPhones = []string{"+16045550101"}
	_, err = f.imp.ImportRemote(t.Context(), r)
	require.NoError(err)
	candidates, err := f.st.ListIdentityMatchReviewsContext(t.Context(), nil, 100, 0)
	require.NoError(err)
	require.Len(candidates, 1)
	assert.Contains([]int64{candidates[0].LeftID, candidates[0].RightID}, survivor,
		"the suggestion points at the participant that kept the merged number")
}

func TestContactReviewCannotAutomaticallyMergePeople(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newImporterFixture(t)
	registerRemoteFixture(t, f)
	phoneID, err := f.st.EnsureParticipantByPhone("+16045550100", "Existing Chat", "imessage")
	require.NoError(err)
	_, _, err = f.st.CreatePersonFromParticipantContext(t.Context(), phoneID)
	require.NoError(err)
	r := remoteTestRequest()
	r.Meeting.ContactsState = ContactsComplete
	r.Meeting.Participants[0].ContactReviewPhones = []string{"+16045550100"}
	_, err = f.imp.ImportRemote(t.Context(), r)
	require.NoError(err)
	candidates, err := f.st.ListIdentityMatchReviewsContext(t.Context(), nil, 100, 0)
	require.NoError(err)
	require.Len(candidates, 1)
	_, _, err = f.st.AcceptIdentityMatchCandidateContext(t.Context(), candidates[0].ID, "system", nil)
	require.ErrorIs(err, store.ErrIdentityMatchNotAcceptable)
	emailID, err := f.st.EnsureParticipant("attendee@example.com", "", "example.com")
	require.NoError(err)
	_, _, err = f.st.CreatePersonFromParticipantContext(t.Context(), emailID)
	require.NoError(err)
	review, err := f.st.GetIdentityMatchReviewContext(t.Context(), candidates[0].ID)
	require.NoError(err)
	_, _, err = f.st.DecideIdentityMatchReviewedContext(t.Context(), review.ID, review.ReviewToken, store.IdentityMatchStateAccepted, nil)
	require.ErrorIs(err, store.ErrPersonBindingConflict)
	members, err := f.st.ClusterMembers(phoneID)
	require.NoError(err)
	assert.NotContains(members, emailID)
}

func TestRemoteReviewBudgetIncludesCarriedIdentities(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newImporterFixture(t)
	registerRemoteFixture(t, f)
	r := remoteTestRequest()
	r.Meeting.ContactsState = ContactsComplete
	p := &r.Meeting.Participants[0]
	p.Resolution = resolutionResolved
	for i := range 30 {
		p.Phones = append(p.Phones, fmt.Sprintf("+1604555%04d", 100+i))
	}
	first, err := f.imp.ImportRemote(t.Context(), r)
	require.NoError(err)
	before, err := f.st.GetMessageRaw(first.MessageID)
	require.NoError(err)
	p.Resolution = ""
	p.Phones = nil
	for i := 30; i < 60; i++ {
		p.ContactReviewPhones = append(p.ContactReviewPhones, fmt.Sprintf("+1604555%04d", 100+i))
	}
	_, err = r.Normalize()
	require.NoError(err, "the incoming evidence alone fits the budget")
	_, err = f.imp.ImportRemote(t.Context(), r)
	require.NoError(err, "over-budget suggestions must not reject the meeting")
	after, err := f.st.GetMessageRaw(first.MessageID)
	require.NoError(err)
	assert.Equal(before, after, "carried identities stay; over-budget review phones are dropped")
}

func TestOversizedContactReviewStillImportsMeeting(t *testing.T) {
	phones := make([]string, 50)
	for i := range phones {
		phones[i] = fmt.Sprintf("+1604555%04d", 100+i)
	}
	cards := []fixtureCard{
		{uniqueID: "CARD-A:ABPerson", emails: []string{"attendee@example.com"}, phones: phones},
		{uniqueID: "CARD-B:ABPerson", emails: []string{"attendee@example.com"}, phones: phones},
	}
	insertMeeting := func(t *testing.T, f resolveFixture) int64 {
		t.Helper()
		id := insertRow(t, f.muesli, "meetings", completedMeeting(nil))
		insertRow(t, f.muesli, "meeting_participants", map[string]any{"meeting_id": id, "participant_identifier": "email:attendee@example.com", "email_address": "attendee@example.com", "display_name": "Meeting Attendee", "source": "calendar", "insertion_order": 0})
		return id
	}
	t.Run("same host", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		f := newResolveFixture(t, cards...)
		insertMeeting(t, f)
		sum := f.sync(t, ImportOptions{})
		assert.Equal(int64(1), sum.MeetingsAdded)
		assert.Zero(sum.Errors)
		candidates, err := f.st.ListIdentityMatchReviewsContext(t.Context(), nil, 100, 0)
		require.NoError(err)
		assert.Empty(candidates, "an email on cards with more than the budget of phones is too ambiguous to suggest")
	})
	t.Run("remote", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		f := newResolveFixture(t, cards...)
		id := insertMeeting(t, f)
		var received []RemoteRequest
		sum, err := ScanRemote(t.Context(), ImportOptions{Identifier: "mac", AccountEmail: "you@example.com", DBPath: f.path, ContactsEnabled: true, ContactsPath: f.contacts, LockDir: t.TempDir()}, func(_ context.Context, r RemoteRequest) (RemoteResult, error) {
			received = append(received, r)
			return RemoteResult{Status: "created", Changed: true}, nil
		})
		require.NoError(err)
		require.Len(received, 1)
		assert.Equal(id, received[0].Meeting.Record.ID)
		assert.Empty(received[0].Meeting.Participants[0].ContactReviewPhones)
		assert.Zero(sum.Errors)
	})
}

func TestOversizedReviewRowBesideResolvedDuplicateStillImports(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var cardA, cardB []string
	for i := range 30 {
		cardA = append(cardA, fmt.Sprintf("+1604555%04d", 100+i))
		cardB = append(cardB, fmt.Sprintf("+1604555%04d", 200+i))
	}
	f := newResolveFixture(t,
		fixtureCard{uniqueID: "CARD-A:ABPerson", emails: []string{"attendee@example.com"}, phones: cardA},
		fixtureCard{uniqueID: "CARD-B:ABPerson", emails: []string{"attendee@example.com"}, phones: cardB},
	)
	id := insertRow(t, f.muesli, "meetings", completedMeeting(nil))
	// The picked card resolves; the calendar row for the same email is ambiguous.
	insertRow(t, f.muesli, "meeting_participants", map[string]any{"meeting_id": id, "participant_identifier": "contact:CARD-A:ABPerson", "email_address": "attendee@example.com", "display_name": "Meeting Attendee", "source": "manual", "insertion_order": 0})
	insertRow(t, f.muesli, "meeting_participants", map[string]any{"meeting_id": id, "participant_identifier": "email:attendee@example.com", "email_address": "attendee@example.com", "display_name": "Meeting Attendee", "source": "calendar", "insertion_order": 1})
	var received []RemoteRequest
	sum, err := ScanRemote(t.Context(), ImportOptions{Identifier: "mac", AccountEmail: "you@example.com", DBPath: f.path, ContactsEnabled: true, ContactsPath: f.contacts, LockDir: t.TempDir()}, func(_ context.Context, r RemoteRequest) (RemoteResult, error) {
		received = append(received, r)
		return RemoteResult{Status: "created", Changed: true}, nil
	})
	require.NoError(err)
	assert.Zero(sum.Errors)
	require.Len(received, 1)
	require.Len(received[0].Meeting.Participants, 2)
	for _, p := range received[0].Meeting.Participants {
		assert.Empty(p.ContactReviewPhones)
	}
}

func TestContactReviewDoesNotCreateInvalidEmailIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newResolveFixture(t,
		fixtureCard{uniqueID: "CARD-A:ABPerson", emails: []string{"not-an-email"}, phones: []string{"+16045550100"}},
		fixtureCard{uniqueID: "CARD-B:ABPerson", emails: []string{"not-an-email"}, phones: []string{"+16045550101"}},
	)
	meetingID := insertRow(t, f.muesli, "meetings", completedMeeting(nil))
	insertRow(t, f.muesli, "meeting_participants", map[string]any{"meeting_id": meetingID, "participant_identifier": "email:synthetic-invalid", "email_address": "not-an-email", "display_name": "Meeting Attendee", "source": "calendar", "insertion_order": 0})
	f.sync(t, ImportOptions{})
	candidates, err := f.st.ListIdentityMatchReviewsContext(t.Context(), nil, 100, 0)
	require.NoError(err)
	assert.Empty(candidates)
	var count int
	require.NoError(f.st.DB().QueryRow(`SELECT count(*) FROM participants WHERE email_address='not-an-email'`).Scan(&count))
	assert.Zero(count)
}
