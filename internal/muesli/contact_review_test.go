package muesli

import (
	"context"
	"database/sql"
	"fmt"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAmbiguousContactsSupplyOnlyReviewPhones(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newResolveFixture(t,
		fixtureCard{uniqueID: "CARD-A:ABPerson", emails: []string{"attendee@example.com"}, phones: []string{"+1 604 555 0100"}},
		fixtureCard{uniqueID: "CARD-B:ABPerson", emails: []string{"attendee@example.com"}, phones: []string{"+16045550100", "+16045550101"}},
	)
	contacts, err := OpenContacts(t.Context(), f.contacts)
	requirements.NoError(err)
	m := Meeting{Participants: []Participant{{Email: "attendee@example.com", Identifier: "email:attendee@example.com"}}}
	requirements.NoError(f.imp.resolveParticipants(f.source.ID, &m, contacts, "", contacts.sharedAddresses("")))
	p := m.Participants[0]
	assertions.Equal([]string{"+16045550100", "+16045550101"}, p.ContactReviewPhones)
	assertions.Empty(p.Anchor)
	assertions.Empty(p.Resolution)
	assertions.Empty(p.archivePerson().OtherPhones)
	assertions.Empty(p.archivePerson().Phone)
	raw := p.raw(p.archivePerson())
	assertions.Equal(p.ContactReviewPhones, raw.ContactReviewPhones)
	remote := NewRemoteMeeting(m).toMeeting()
	assertions.Equal(p.ContactReviewPhones, remote.Participants[0].ContactReviewPhones)
	for _, state := range []ContactsState{ContactsPartial, ContactsUnavailable, ContactsOff} {
		contacts.state = state
		assertions.Empty(contacts.reviewPhones("attendee@example.com", ""))
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
			assertions := assert.New(t)
			requirements := require.New(t)
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
				requirements.NoError(err)
			} else {
				requirements.ErrorIs(err, ErrRemoteValidation)
				assertions.NotContains(err.Error(), "attendee@example.com")
			}
		})
	}
}

func TestContactReviewSurvivesDuplicateAttendees(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	m := remoteTestRequest().Meeting.toMeeting()
	m.Participants = []Participant{
		{Email: "attendee@example.com", Identifier: "calendar:synthetic"},
		{Email: "attendee@example.com", Identifier: "email:attendee@example.com", ContactReviewPhones: []string{"+16045550100"}},
	}
	people := dedupeParticipants(m.Participants)
	requirements.Len(people, 1)
	assertions.Equal([]string{"+16045550100"}, people[0].ContactReviewPhones)
	snapshot, err := m.ArchiveSnapshot(1, "mac", "you@example.com")
	requirements.NoError(err)
	assertions.NotContains(string(snapshot.Raw), "calendar:synthetic")
	assertions.Contains(string(snapshot.Raw), "contact_review_phones")
}

func TestResolvedDuplicateSupersedesCarriedContactEvidence(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newImporterFixture(t)
	_, err := f.st.EnsureParticipant("attendee@example.com", "Attendee", "example.com")
	requirements.NoError(err)
	_, err = f.st.EnsureParticipantByPhone("+16045550100", "Existing Chat", "imessage")
	requirements.NoError(err)

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
	requirements.Len(people, 1)
	assertions.Equal(resolutionResolved, people[0].Resolution)
	assertions.Equal("apple-contact:synthetic-card", people[0].Anchor)
	assertions.Equal([]string{"new@example.com"}, people[0].ContactEmails)
	assertions.Equal([]string{"+16045550102"}, people[0].ContactPhones)
	assertions.Empty(people[0].ContactReviewPhones)

	requirements.NoError(f.imp.recordContactReviewCandidates(t.Context(), f.source.ID, m))
	candidates, err := f.st.ListIdentityMatchReviewsContext(t.Context(), nil, 100, 0)
	requirements.NoError(err)
	assertions.Empty(candidates, "resolved duplicate evidence must not create a review candidate from carried phones")
}

func TestRemoteContactReviewRejectsUnnormalizedPhones(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	r := remoteTestRequest()
	r.Meeting.ContactsState = ContactsComplete
	for _, phone := range []string{"+1 604 555 0100", "secret-private-phone", "6045550100"} {
		r.Meeting.Participants[0].ContactReviewPhones = []string{phone}
		_, err := r.Normalize()
		requirements.ErrorIs(err, ErrRemoteValidation)
		assertions.NotContains(err.Error(), phone)
	}
}

func TestDuplicateContactsNativeReviewedAttribution(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newResolveFixture(t,
		fixtureCard{uniqueID: "CARD-A:ABPerson", emails: []string{"attendee@example.com"}, phones: []string{"+16045550100"}},
		fixtureCard{uniqueID: "CARD-B:ABPerson", emails: []string{"attendee@example.com"}, phones: []string{"+16045550101"}},
	)
	meetingID := insertRow(t, f.muesli, "meetings", completedMeeting(nil))
	insertRow(t, f.muesli, "meeting_participants", map[string]any{"meeting_id": meetingID, "participant_identifier": "email:attendee@example.com", "email_address": "attendee@example.com", "display_name": "Meeting Attendee", "source": "calendar", "insertion_order": 0})
	f.sync(t, ImportOptions{})
	candidates, err := f.st.ListIdentityMatchReviewsContext(t.Context(), nil, 100, 0)
	requirements.NoError(err)
	requirements.Len(candidates, 1)
	candidate := candidates[0]
	assertions.Equal(store.IdentityMatchStateCandidate, candidate.State)
	assertions.Equal(store.IdentityMatchEmail, candidate.Basis)
	requirements.Len(candidate.Evidence, 1)
	requirements.NotNil(candidate.Evidence[0].Detail)
	assertions.Contains(*candidate.Evidence[0].Detail, "attendee@example.com")
	assertions.Contains(*candidate.Evidence[0].Detail, "+16045550100")
	assertions.Contains(*candidate.Evidence[0].Detail, "historical")
	emailID, err := f.st.EnsureParticipant("attendee@example.com", "", "example.com")
	requirements.NoError(err)
	members, err := f.st.ClusterMembers(f.phoneID)
	requirements.NoError(err)
	assertions.NotContains(members, emailID)
	var phoneCount int
	requirements.NoError(f.st.DB().QueryRow(`SELECT count(*) FROM participants WHERE phone_number IS NOT NULL`).Scan(&phoneCount))
	assertions.Equal(1, phoneCount, "an unknown suggested phone must not create a participant")
	f.project(t)
	assertions.NotContains(f.meetingPersons(t), f.personID)
	summary := f.sync(t, ImportOptions{})
	assertions.Zero(summary.MeetingsUpdated)
	repeated, err := f.st.GetIdentityMatchReviewContext(t.Context(), candidate.ID)
	requirements.NoError(err)
	assertions.Equal(candidate.ReviewToken, repeated.ReviewToken)
	assertions.Len(repeated.Evidence, 1)
	_, _, err = f.st.DecideIdentityMatchReviewedContext(t.Context(), candidate.ID, candidate.ReviewToken, store.IdentityMatchStateAccepted, nil)
	requirements.NoError(err)
	f.project(t)
	assertions.Contains(f.meetingPersons(t), f.personID)
	f.sync(t, ImportOptions{})
	accepted, err := f.st.GetIdentityMatchReviewContext(t.Context(), candidate.ID)
	requirements.NoError(err)
	assertions.Equal(store.IdentityMatchStateAccepted, accepted.State)
}

func TestRemoteContactReviewRejectionAndLaterPhoneDiscovery(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newImporterFixture(t)
	registerRemoteFixture(t, f)
	r := remoteTestRequest()
	r.Meeting.ContactsState = ContactsComplete
	r.Meeting.Participants[0].ContactReviewPhones = []string{"+16045550100", "+16045550101"}
	first, err := f.imp.ImportRemote(t.Context(), r)
	requirements.NoError(err)
	assertions.True(first.Changed)
	candidates, err := f.st.ListIdentityMatchReviewsContext(t.Context(), nil, 100, 0)
	requirements.NoError(err)
	assertions.Empty(candidates)
	_, err = f.st.EnsureParticipantByPhone("+16045550100", "Existing Chat", "imessage")
	requirements.NoError(err)
	before, err := f.st.IdentityRevisionContext(t.Context())
	requirements.NoError(err)
	unchanged, err := f.imp.ImportRemote(t.Context(), r)
	requirements.NoError(err)
	assertions.False(unchanged.Changed)
	after, err := f.st.IdentityRevisionContext(t.Context())
	requirements.NoError(err)
	assertions.Equal(before, after)
	candidates, err = f.st.ListIdentityMatchReviewsContext(t.Context(), nil, 100, 0)
	requirements.NoError(err)
	requirements.Len(candidates, 1)
	candidate := candidates[0]
	_, _, err = f.st.DecideIdentityMatchReviewedContext(t.Context(), candidate.ID, candidate.ReviewToken, store.IdentityMatchStateRejected, nil)
	requirements.NoError(err)
	rejected, err := f.st.GetIdentityMatchReviewContext(t.Context(), candidate.ID)
	requirements.NoError(err)
	_, err = f.imp.ImportRemote(t.Context(), r)
	requirements.NoError(err)
	retry, err := f.st.GetIdentityMatchReviewContext(t.Context(), candidate.ID)
	requirements.NoError(err)
	assertions.Equal(store.IdentityMatchStateRejected, retry.State)
	assertions.Equal(rejected.ReviewToken, retry.ReviewToken)
	_, err = f.st.EnsureParticipantByPhone("+16045550101", "Other Chat", "imessage")
	requirements.NoError(err)
	unchanged, err = f.imp.ImportRemote(t.Context(), r)
	requirements.NoError(err)
	assertions.False(unchanged.Changed)
	candidates, err = f.st.ListIdentityMatchReviewsContext(t.Context(), nil, 100, 0)
	requirements.NoError(err)
	requirements.Len(candidates, 2)
	assertions.Equal(store.IdentityMatchStateRejected, candidates[0].State)
	assertions.Equal(store.IdentityMatchStateCandidate, candidates[1].State)
	r.Meeting.Participants[0].Email = "other@example.com"
	r.Meeting.Participants[0].Ref = participantRef("email:other@example.com")
	edited, err := f.imp.ImportRemote(t.Context(), r)
	requirements.NoError(err)
	assertions.True(edited.Changed)
	candidates, err = f.st.ListIdentityMatchReviewsContext(t.Context(), nil, 100, 0)
	requirements.NoError(err)
	requirements.Len(candidates, 4)
	assertions.Equal(store.IdentityMatchStateRejected, candidates[0].State)
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
			assertions := assert.New(t)
			requirements := require.New(t)
			f := newImporterFixture(t)
			registerRemoteFixture(t, f)
			_, err := f.st.EnsureParticipantByPhone("+16045550100", "Existing Chat", "imessage")
			requirements.NoError(err)
			if tc.ownerPhone {
				requirements.NoError(f.st.AddAccountIdentityContext(t.Context(), f.source.ID, "+16045550100", "self-id"))
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
			requirements.NoError(err)
			requirements.NoError(f.imp.recordContactReviewCandidates(t.Context(), f.source.ID, m))
			// The incomplete states are also rejected by transport before any upsert.
			if tc.state != ContactsComplete {
				r.Meeting.ContactsState = tc.state
				r.Meeting.Participants[0].ContactReviewPhones = []string{"+16045550100"}
				_, err = r.Normalize()
				requirements.ErrorIs(err, ErrRemoteValidation)
			}
			candidates, err := f.st.ListIdentityMatchReviewsContext(t.Context(), nil, 100, 0)
			requirements.NoError(err)
			assertions.Empty(candidates)
		})
	}
}

func TestContactReviewCombinedDuplicateLimit(t *testing.T) {
	requirements := require.New(t)
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
	requirements.ErrorIs(err, ErrRemoteValidation)
}

func TestContactReviewSuggestsAbsorbedPhoneAlias(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newImporterFixture(t)
	registerRemoteFixture(t, f)
	survivor, err := f.st.EnsureParticipantByPhone("+16045550100", "Existing Chat", "imessage")
	requirements.NoError(err)
	absorbed, err := f.st.EnsureParticipantByPhone("+16045550101", "Older Chat", "imessage")
	requirements.NoError(err)
	requirements.NoError(f.st.MergeParticipants(absorbed, survivor))
	r := remoteTestRequest()
	r.Meeting.ContactsState = ContactsComplete
	r.Meeting.Participants[0].ContactReviewPhones = []string{"+16045550101"}
	_, err = f.imp.ImportRemote(t.Context(), r)
	requirements.NoError(err)
	candidates, err := f.st.ListIdentityMatchReviewsContext(t.Context(), nil, 100, 0)
	requirements.NoError(err)
	requirements.Len(candidates, 1)
	assertions.Contains([]int64{candidates[0].LeftID, candidates[0].RightID}, survivor,
		"the suggestion points at the participant that kept the merged number")
}

func TestContactReviewCannotAutomaticallyMergePeople(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newImporterFixture(t)
	registerRemoteFixture(t, f)
	phoneID, err := f.st.EnsureParticipantByPhone("+16045550100", "Existing Chat", "imessage")
	requirements.NoError(err)
	_, _, err = f.st.CreatePersonFromParticipantContext(t.Context(), phoneID)
	requirements.NoError(err)
	r := remoteTestRequest()
	r.Meeting.ContactsState = ContactsComplete
	r.Meeting.Participants[0].ContactReviewPhones = []string{"+16045550100"}
	_, err = f.imp.ImportRemote(t.Context(), r)
	requirements.NoError(err)
	candidates, err := f.st.ListIdentityMatchReviewsContext(t.Context(), nil, 100, 0)
	requirements.NoError(err)
	requirements.Len(candidates, 1)
	_, _, err = f.st.AcceptIdentityMatchCandidateContext(t.Context(), candidates[0].ID, "system", nil)
	requirements.ErrorIs(err, store.ErrIdentityMatchNotAcceptable)
	emailID, err := f.st.EnsureParticipant("attendee@example.com", "", "example.com")
	requirements.NoError(err)
	_, _, err = f.st.CreatePersonFromParticipantContext(t.Context(), emailID)
	requirements.NoError(err)
	review, err := f.st.GetIdentityMatchReviewContext(t.Context(), candidates[0].ID)
	requirements.NoError(err)
	_, _, err = f.st.DecideIdentityMatchReviewedContext(t.Context(), review.ID, review.ReviewToken, store.IdentityMatchStateAccepted, nil)
	requirements.ErrorIs(err, store.ErrPersonBindingConflict)
	members, err := f.st.ClusterMembers(phoneID)
	requirements.NoError(err)
	assertions.NotContains(members, emailID)
}

func TestRemoteReviewBudgetIncludesCarriedIdentities(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
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
	requirements.NoError(err)
	before, err := f.st.GetMessageRaw(first.MessageID)
	requirements.NoError(err)
	p.Resolution = ""
	p.Phones = nil
	for i := 30; i < 60; i++ {
		p.ContactReviewPhones = append(p.ContactReviewPhones, fmt.Sprintf("+1604555%04d", 100+i))
	}
	_, err = r.Normalize()
	requirements.NoError(err, "the incoming evidence alone fits the budget")
	_, err = f.imp.ImportRemote(t.Context(), r)
	requirements.NoError(err, "over-budget suggestions must not reject the meeting")
	after, err := f.st.GetMessageRaw(first.MessageID)
	requirements.NoError(err)
	assertions.Equal(before, after, "carried identities stay; over-budget review phones are dropped")
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
		assertions := assert.New(t)
		requirements := require.New(t)
		f := newResolveFixture(t, cards...)
		insertMeeting(t, f)
		sum := f.sync(t, ImportOptions{})
		assertions.Equal(int64(1), sum.MeetingsAdded)
		assertions.Zero(sum.Errors)
		candidates, err := f.st.ListIdentityMatchReviewsContext(t.Context(), nil, 100, 0)
		requirements.NoError(err)
		assertions.Empty(candidates, "an email on cards with more than the budget of phones is too ambiguous to suggest")
	})
	t.Run("remote", func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		f := newResolveFixture(t, cards...)
		id := insertMeeting(t, f)
		var received []RemoteRequest
		sum, err := ScanRemote(t.Context(), ImportOptions{Identifier: "mac", AccountEmail: "you@example.com", DBPath: f.path, ContactsEnabled: true, ContactsPath: f.contacts, LockDir: t.TempDir()}, func(_ context.Context, r RemoteRequest) (RemoteResult, error) {
			received = append(received, r)
			return RemoteResult{Status: "created", Changed: true}, nil
		})
		requirements.NoError(err)
		requirements.Len(received, 1)
		assertions.Equal(id, received[0].Meeting.Record.ID)
		assertions.Empty(received[0].Meeting.Participants[0].ContactReviewPhones)
		assertions.Zero(sum.Errors)
	})
}

func TestOversizedReviewRowBesideResolvedDuplicateStillImports(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
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
	requirements.NoError(err)
	assertions.Zero(sum.Errors)
	requirements.Len(received, 1)
	requirements.Len(received[0].Meeting.Participants, 2)
	for _, p := range received[0].Meeting.Participants {
		assertions.Empty(p.ContactReviewPhones)
	}
}

func TestContactReviewDoesNotCreateInvalidEmailIdentity(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newResolveFixture(t,
		fixtureCard{uniqueID: "CARD-A:ABPerson", emails: []string{"not-an-email"}, phones: []string{"+16045550100"}},
		fixtureCard{uniqueID: "CARD-B:ABPerson", emails: []string{"not-an-email"}, phones: []string{"+16045550101"}},
	)
	meetingID := insertRow(t, f.muesli, "meetings", completedMeeting(nil))
	insertRow(t, f.muesli, "meeting_participants", map[string]any{"meeting_id": meetingID, "participant_identifier": "email:synthetic-invalid", "email_address": "not-an-email", "display_name": "Meeting Attendee", "source": "calendar", "insertion_order": 0})
	f.sync(t, ImportOptions{})
	candidates, err := f.st.ListIdentityMatchReviewsContext(t.Context(), nil, 100, 0)
	requirements.NoError(err)
	assertions.Empty(candidates)
	var count int
	requirements.NoError(f.st.DB().QueryRow(`SELECT count(*) FROM participants WHERE email_address='not-an-email'`).Scan(&count))
	assertions.Zero(count)
}

func TestContactReviewSurvivesUnrelatedConversationPolicyTransition(t *testing.T) {
	for _, support := range []string{"independent", "candidate", "evidence", "both"} {
		t.Run(support, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)

			st := testutil.NewTestStore(t)
			st.DB().SetMaxOpenConns(1)
			source, err := st.GetOrCreateSource(SourceType, "contact-review-account")
			requirements.NoError(err)
			_, err = st.EnsureParticipant("attendee@example.com", "Attendee", "example.com")
			requirements.NoError(err)
			_, err = st.EnsureParticipantByPhone("+16045550100", "Existing Chat", "imessage")
			requirements.NoError(err)
			m := remoteTestRequest().Meeting.toMeeting()
			m.ContactsState = ContactsComplete
			m.Participants = []Participant{{Email: "attendee@example.com", Identifier: "email:attendee@example.com", ContactReviewPhones: []string{"+16045550100"}}}
			requirements.NoError(NewImporter(st).recordContactReviewCandidates(t.Context(), source.ID, m))
			candidates, err := st.ListIdentityMatchCandidatesContext(t.Context(), nil, 100, 0)
			requirements.NoError(err)
			requirements.Len(candidates, 1)
			candidate := candidates[0]
			// Generic email evidence from the independent contact source must also
			// retain its existing contract without participant-observation pairs.
			independentEvidence, err := st.AddIdentityMatchEvidenceContext(t.Context(), candidate.ID, store.IdentityMatchEvidenceInput{EvidenceKind: "email", Detail: new("attendee@example.com"), Source: store.ProvenanceArchiveObservation, SourceID: &source.ID})
			requirements.NoError(err)
			before, err := st.GetIdentityMatchCandidateContext(t.Context(), candidate.ID)
			requirements.NoError(err)
			requirements.Len(before.Evidence, 2)
			chat, err := st.GetOrCreateSource("beeper", "unrelated-chat")
			requirements.NoError(err)
			_, err = st.EnsureConversation(chat.ID, "unrelated-room", "Unrelated")
			requirements.NoError(err)
			if support == "candidate" || support == "both" {
				requirements.NoError(st.AttachIdentityMatchCandidateSourceContext(t.Context(), candidate.ID, chat.ID))
			}
			if support == "evidence" || support == "both" {
				requirements.NoError(st.AttachIdentityMatchEvidenceSourceContext(t.Context(), independentEvidence.ID, chat.ID))
			}
			for _, mode := range []conversationpolicy.Tier{conversationpolicy.Full, conversationpolicy.Cold} {
				requirements.NoError(st.SetConversationPolicyOverride(t.Context(), chat.ID, "unrelated-room", mode))
				after, err := st.GetIdentityMatchCandidateContext(t.Context(), candidate.ID)
				requirements.NoError(err)
				assertions.Equal(store.IdentityMatchStateCandidate, after.State)
				assertions.Equal(before.Evidence, after.Evidence)
				var candidateSources, evidenceSources, independentCandidates, independentExplanations int
				requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*), COALESCE(SUM(CASE WHEN source_id=? THEN 1 ELSE 0 END),0) FROM identity_match_candidate_sources WHERE candidate_id=?`), source.ID, candidate.ID).Scan(&candidateSources, &independentCandidates))
				requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*), COALESCE(SUM(CASE WHEN source_id=? THEN 1 ELSE 0 END),0) FROM identity_match_evidence_sources WHERE evidence_id=?`), source.ID, independentEvidence.ID).Scan(&evidenceSources, &independentExplanations))
				assertions.Equal(1, candidateSources)
				assertions.Equal(1, evidenceSources)
				assertions.Equal(1, independentCandidates)
				assertions.Equal(1, independentExplanations)
			}
		})
	}
}

func TestContactReviewPreservesLaterIndependentSupportAfterPairedObservationDemotion(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	st := testutil.NewTestStore(t)
	st.DB().SetMaxOpenConns(1)
	chat, err := st.GetOrCreateSource("beeper", "paired-chat")
	requirements.NoError(err)
	room, err := st.EnsureConversation(chat.ID, "paired-room", "Paired")
	requirements.NoError(err)
	requirements.NoError(st.SetConversationPolicyOverride(t.Context(), chat.ID, "paired-room", conversationpolicy.Full))
	mail, err := st.GetOrCreateSource("gmail", "peer@example.com")
	requirements.NoError(err)
	contacts, err := st.GetOrCreateSource(SourceType, "later-contacts")
	requirements.NoError(err)
	left, err := st.EnsureParticipant("attendee@example.com", "Attendee", "example.com")
	requirements.NoError(err)
	right, err := st.EnsureParticipantByPhone("+16045550100", "Existing Chat", "imessage")
	requirements.NoError(err)
	input := store.ParticipantContactObservationInput{SourceID: &chat.ID, ConversationID: &room, AddressKind: store.ContactAddressEmail, OriginalValue: "attendee@example.com", ProviderUserID: new("shared-provider"), Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation}}
	_, err = st.RecordContactObservationContext(t.Context(), left, input)
	requirements.NoError(err)
	input.SourceID, input.ConversationID = &mail.ID, nil
	_, err = st.RecordContactObservationContext(t.Context(), right, input)
	requirements.NoError(err)
	count, err := st.EnsurePersonMatchScoringCandidatesContext(t.Context(), 10)
	requirements.NoError(err)
	requirements.Equal(1, count)
	candidates, err := st.ListIdentityMatchCandidatesContext(t.Context(), nil, 10, 0)
	requirements.NoError(err)
	requirements.Len(candidates, 1)
	candidateID := candidates[0].ID
	requirements.Len(candidates[0].Evidence, 1)
	pairedEvidenceID := candidates[0].Evidence[0].ID
	m := remoteTestRequest().Meeting.toMeeting()
	m.ContactsState = ContactsComplete
	m.Participants = []Participant{{Email: "attendee@example.com", Identifier: "email:attendee@example.com", ContactReviewPhones: []string{"+16045550100"}}}
	requirements.NoError(NewImporter(st).recordContactReviewCandidates(t.Context(), contacts.ID, m))
	independent, err := st.AddIdentityMatchEvidenceContext(t.Context(), candidateID, store.IdentityMatchEvidenceInput{EvidenceKind: "email", Detail: new("attendee@example.com"), Source: store.ProvenanceArchiveObservation, SourceID: &contacts.ID})
	requirements.NoError(err)
	requirements.NoError(st.AttachIdentityMatchEvidenceSourceContext(t.Context(), independent.ID, chat.ID))
	requirements.NoError(st.SetConversationPolicyOverride(t.Context(), chat.ID, "paired-room", conversationpolicy.Cold))
	after, err := st.GetIdentityMatchCandidateContext(t.Context(), candidateID)
	requirements.NoError(err)
	requirements.Len(after.Evidence, 2)
	independentKept := false
	for _, evidence := range after.Evidence {
		assertions.NotEqual(pairedEvidenceID, evidence.ID)
		independentKept = independentKept || evidence.ID == independent.ID
	}
	assertions.True(independentKept)
	assertions.Equal(store.IdentityMatchStateCandidate, after.State)
	var candidateSources, evidenceSources int
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM identity_match_candidate_sources WHERE candidate_id=? AND source_id<>?`), candidateID, contacts.ID).Scan(&candidateSources))
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM identity_match_evidence_sources WHERE evidence_id=? AND source_id<>?`), independent.ID, contacts.ID).Scan(&evidenceSources))
	assertions.Zero(candidateSources)
	assertions.Zero(evidenceSources)
}

func TestContactReviewRestoresCandidateAfterGeneratedConflictDemotion(t *testing.T) {
	for _, tc := range []struct {
		name                                                string
		contactsFirst, retainFull, userDecision, assessment bool
	}{
		{name: "conflict-first"},
		{name: "scored-generated-conflict", assessment: true},
		{name: "contacts-first", contactsFirst: true},
		{name: "remaining-full", retainFull: true, assessment: true},
		{name: "user-confirmed-conflict", userDecision: true, assessment: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)

			st := testutil.NewTestStore(t)
			st.DB().SetMaxOpenConns(1)
			chat, err := st.GetOrCreateSource("beeper", "conflict-chat")
			requirements.NoError(err)
			room, err := st.EnsureConversation(chat.ID, "conflict-room", "Conflict")
			requirements.NoError(err)
			requirements.NoError(st.SetConversationPolicyOverride(t.Context(), chat.ID, "conflict-room", conversationpolicy.Full))
			mail, err := st.GetOrCreateSource("gmail", "peer@example.com")
			requirements.NoError(err)
			contacts, err := st.GetOrCreateSource(SourceType, "independent-contacts")
			requirements.NoError(err)
			left, err := st.EnsureParticipant("attendee@example.com", "Attendee", "example.com")
			requirements.NoError(err)
			right, err := st.EnsureParticipantByPhone("+16045550100", "Existing Chat", "imessage")
			requirements.NoError(err)
			m := remoteTestRequest().Meeting.toMeeting()
			m.ContactsState = ContactsComplete
			m.Participants = []Participant{{Email: "attendee@example.com", Identifier: "email:attendee@example.com", ContactReviewPhones: []string{"+16045550100"}}}
			imp := NewImporter(st)
			if tc.contactsFirst {
				requirements.NoError(imp.recordContactReviewCandidates(t.Context(), contacts.ID, m))
			}
			input := store.ParticipantContactObservationInput{SourceID: &chat.ID, ConversationID: &room, AddressKind: store.ContactAddressEmail, OriginalValue: "attendee@example.com", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation}}
			_, err = st.RecordContactObservationContext(t.Context(), left, input)
			requirements.NoError(err)
			if tc.retainFull {
				alternate, err := st.EnsureConversation(chat.ID, "remaining-room", "Remaining")
				requirements.NoError(err)
				requirements.NoError(st.SetConversationPolicyOverride(t.Context(), chat.ID, "remaining-room", conversationpolicy.Full))
				input.ConversationID = &alternate
				_, err = st.RecordContactObservationContext(t.Context(), left, input)
				requirements.NoError(err)
			}
			input.SourceID, input.ConversationID = &mail.ID, nil
			_, err = st.RecordContactObservationContext(t.Context(), right, input)
			requirements.NoError(err)
			count, err := st.EnsurePersonMatchScoringCandidatesContext(t.Context(), 10)
			requirements.NoError(err)
			assertions.Zero(count, "the observation API already created or promoted the conflict")
			candidates, err := st.ListIdentityMatchCandidatesContext(t.Context(), nil, 10, 0)
			requirements.NoError(err)
			requirements.Len(candidates, 1)
			candidateID := candidates[0].ID
			requirements.Equal(store.IdentityMatchStateConflict, candidates[0].State)
			var origin, previousState sql.NullString
			requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT observation_conflict_origin, pre_conflict_state FROM identity_match_candidates WHERE id=?`), candidateID).Scan(&origin, &previousState))
			expectedOrigin := "generated"
			if tc.contactsFirst {
				expectedOrigin = "promoted"
			}
			requirements.Equal(sql.NullString{String: expectedOrigin, Valid: true}, origin)
			var pairedEvidenceID int64
			for _, evidence := range candidates[0].Evidence {
				if evidence.EvidenceKind == "email" {
					pairedEvidenceID = evidence.ID
				}
			}
			requirements.NotZero(pairedEvidenceID)
			// Preserve a persisted assessment as can arrive through candidate merging.
			// The conflict itself and its later independent support use real producers.
			confidence, reason := 0.83, "Generated collision assessment"
			if tc.assessment {
				_, err = st.DB().Exec(st.Rebind(`UPDATE identity_match_candidates SET confidence=?, notes=? WHERE id=?`), confidence, reason, candidateID)
				requirements.NoError(err)
			}
			requirements.NoError(imp.recordContactReviewCandidates(t.Context(), contacts.ID, m))
			independent, err := st.AddIdentityMatchEvidenceContext(t.Context(), candidateID, store.IdentityMatchEvidenceInput{EvidenceKind: "email", Detail: new("attendee@example.com"), Source: store.ProvenanceArchiveObservation, SourceID: &contacts.ID})
			requirements.NoError(err)
			requirements.NoError(st.AttachIdentityMatchEvidenceSourceContext(t.Context(), independent.ID, chat.ID))
			if tc.userDecision {
				_, err = st.DecideIdentityMatchCandidateContext(t.Context(), candidateID, store.IdentityMatchStateConflict, "user", new("retain reviewed conflict"))
				requirements.NoError(err)
			}
			requirements.NoError(st.SetConversationPolicyOverride(t.Context(), chat.ID, "conflict-room", conversationpolicy.Cold))
			after, err := st.GetIdentityMatchCandidateContext(t.Context(), candidateID)
			requirements.NoError(err)
			requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT observation_conflict_origin, pre_conflict_state FROM identity_match_candidates WHERE id=?`), candidateID).Scan(&origin, &previousState))
			if tc.retainFull {
				assertions.Equal(store.IdentityMatchStateConflict, after.State)
				assertions.Equal(sql.NullString{String: "generated", Valid: true}, origin)
				assertions.Len(after.Evidence, 3)
				assertions.Equal(&confidence, after.Confidence)
				assertions.Equal(&reason, after.Notes)
				return
			}
			assertions.False(origin.Valid, "unsupported generated conflict origin must be withdrawn")
			assertions.False(previousState.Valid, "no stale conflict state remains")
			if tc.userDecision {
				assertions.Equal(store.IdentityMatchStateConflict, after.State)
				assertions.Equal(new("user"), after.DecidedBy)
				assertions.Equal(new("retain reviewed conflict"), after.Notes)
				assertions.Equal(&confidence, after.Confidence)
			} else {
				assertions.Equal(store.IdentityMatchStateCandidate, after.State)
				assertions.Nil(after.DecidedBy)
				// Compare with the same independent producer's ordinary candidate, rather
				// than retaining a score or reason from the withdrawn generated conflict.
				baselineEmail := "baseline@example.com"
				_, err = st.EnsureParticipant(baselineEmail, "Baseline", "example.com")
				requirements.NoError(err)
				_, err = st.EnsureParticipantByPhone("+16045550101", "Baseline Phone", "imessage")
				requirements.NoError(err)
				baselineMeeting := m
				baselineMeeting.Participants = []Participant{{Email: baselineEmail, Identifier: "email:" + baselineEmail, ContactReviewPhones: []string{"+16045550101"}}}
				requirements.NoError(imp.recordContactReviewCandidates(t.Context(), contacts.ID, baselineMeeting))
				baselineCandidates, err := st.ListIdentityMatchCandidatesContext(t.Context(), nil, 10, 0)
				requirements.NoError(err)
				var baseline *store.IdentityMatchCandidate
				for i := range baselineCandidates {
					if baselineCandidates[i].NormalizedValue != nil && *baselineCandidates[i].NormalizedValue == baselineEmail {
						baseline = &baselineCandidates[i]
					}
				}
				requirements.NotNil(baseline)
				assertions.Equal(baseline.State, after.State)
				assertions.Equal(baseline.Confidence, after.Confidence)
				assertions.Equal(baseline.Notes, after.Notes)
			}
			requirements.Len(after.Evidence, 2)
			independentKept := false
			for _, evidence := range after.Evidence {
				assertions.NotEqual(pairedEvidenceID, evidence.ID)
				independentKept = independentKept || evidence.ID == independent.ID
			}
			assertions.True(independentKept)
			var candidateSources, evidenceSources, independentCandidates, independentExplanations int
			requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*), COALESCE(SUM(CASE WHEN source_id=? THEN 1 ELSE 0 END),0) FROM identity_match_candidate_sources WHERE candidate_id=?`), contacts.ID, candidateID).Scan(&candidateSources, &independentCandidates))
			requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*), COALESCE(SUM(CASE WHEN source_id=? THEN 1 ELSE 0 END),0) FROM identity_match_evidence_sources WHERE evidence_id=?`), contacts.ID, independent.ID).Scan(&evidenceSources, &independentExplanations))
			assertions.Equal(1, candidateSources)
			assertions.Equal(1, independentCandidates)
			assertions.Equal(1, evidenceSources)
			assertions.Equal(1, independentExplanations)
		})
	}
}
