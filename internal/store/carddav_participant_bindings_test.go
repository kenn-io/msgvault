package store_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep constructors available in nested assertion scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep constructors available in nested assertion scopes.
	"go.kenn.io/msgvault/internal/store"
)

func applyParticipantBindingCards(t *testing.T, st *store.Store, account store.CardDAVAccount, book store.CardDAVAddressBook, cards ...store.CardDAVRemoteResource) {
	t.Helper()
	books, err := st.ListCardDAVAddressBooksContext(t.Context(), store.AllCardDAVAccounts)
	Require.NoError(t, err)
	for _, current := range books {
		if current.ID == book.ID {
			book = current
		}
	}
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision, Upserts: cards,
	})
	Require.NoError(t, err)
}

func TestCardDAVParticipantBindingExactAndIncremental(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	st, account, book := newCardDAVResourceStore(t)
	email, err := st.EnsureParticipant("alex@example.test", "Alex Example", "example.test")
	require.NoError(err)
	alias, err := st.EnsureParticipant("alias@example.test", "Alias Example", "example.test")
	require.NoError(err)
	_, err = st.LinkParticipants(email, alias)
	require.NoError(err)
	phone, err := st.EnsureParticipantByPhone(t.Context(), "+12025550100", "Alex Example", "imessage")
	require.NoError(err)
	unrelated, err := st.EnsureParticipant("other@example.test", "Other Example", "example.test")
	require.NoError(err)
	card := remoteResource(book.CanonicalURL+"alex.vcf", "card-alex", "Alex Example", "ALEX@EXAMPLE.TEST", "one")
	card.RemoteBody = []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:card-alex\r\nFN:Alex Example\r\nEMAIL:ALEX@EXAMPLE.TEST\r\nTEL;TYPE=cell:tel:+1-202-555-0100\r\nEND:VCARD\r\n")
	card.Phones = []string{"tel:+1-202-555-0100"}
	applyParticipantBindingCards(t, st, account, book, card)
	mapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, card.Href)
	require.NoError(err)
	require.NotNil(mapping.PersonID)
	person, err := st.GetPersonContext(t.Context(), *mapping.PersonID)
	require.NoError(err)
	assert.ElementsMatch([]int64{email, alias, phone}, person.ParticipantIDs)
	noPerson, err := st.PersonForParticipantsContext(t.Context(), []int64{unrelated})
	require.NoError(err)
	assert.Nil(noPerson)

	later, err := st.EnsureParticipant("later@example.test", "Later Example", "example.test")
	require.NoError(err)
	card.Emails = append(card.Emails, "later@example.test")
	card.RemoteBody = []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:card-alex\r\nFN:Alex Example\r\nEMAIL:alex@example.test\r\nEMAIL:later@example.test\r\nTEL:tel:+12025550100\r\nEND:VCARD\r\n")
	card.RemoteETag, card.SemanticHash = "two", "semantic-two"
	applyParticipantBindingCards(t, st, account, book, card)
	person, err = st.GetPersonContext(t.Context(), *mapping.PersonID)
	require.NoError(err)
	assert.ElementsMatch([]int64{email, alias, phone, later}, person.ParticipantIDs)
	before := person.Revision
	applyParticipantBindingCards(t, st, account, book)
	person, err = st.GetPersonContext(t.Context(), *mapping.PersonID)
	require.NoError(err)
	assert.Equal(before, person.Revision)
}

func bindingCardPerson(t *testing.T, st *store.Store, book store.CardDAVAddressBook, href string) *store.Person {
	t.Helper()
	resource, err := st.GetCardDAVResourceContext(t.Context(), book.ID, href)
	Require.NoError(t, err)
	Require.NotNil(t, resource.PersonID)
	person, err := st.GetPersonContext(t.Context(), *resource.PersonID)
	Require.NoError(t, err)
	return person
}

func bindingOwned(t *testing.T, st *store.Store, participantID int64) bool {
	t.Helper()
	var owned bool
	Require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT carddav_owned FROM person_participants WHERE participant_id = ?`), participantID).Scan(&owned))
	return owned
}

func TestCardDAVParticipantBindingPreviewUndoAndLaterArrival(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	st, account, book := newCardDAVResourceStore(t)
	card := remoteResource(book.CanonicalURL+"alex.vcf", "preview-alex", "Alex Example", "alex@example.test", "one")
	applyParticipantBindingCards(t, st, account, book, card)
	person := bindingCardPerson(t, st, book, card.Href)
	participant, err := st.EnsureParticipant("alex@example.test", "Alex Example", "example.test")
	require.NoError(err)
	before, err := st.IdentityRevision()
	require.NoError(err)
	preview, err := st.ReconcileCardDAVParticipantBindingsContext(t.Context(), store.CardDAVParticipantBindingOptions{DryRun: true})
	require.NoError(err)
	assert.Equal(1, preview.CardsScanned)
	assert.Equal(1, preview.ParticipantsBound)
	assert.Zero(preview.Conflicts)
	current, err := st.GetPersonContext(t.Context(), person.ID)
	require.NoError(err)
	assert.Empty(current.ParticipantIDs)
	assert.Equal(person.Revision, current.Revision)
	revision, err := st.IdentityRevision()
	require.NoError(err)
	assert.Equal(before, revision)
	applyParticipantBindingCards(t, st, account, book)
	assert.Equal([]int64{participant}, bindingCardPerson(t, st, book, card.Href).ParticipantIDs)
	assert.True(bindingOwned(t, st, participant))
	preview, err = st.ReconcileCardDAVParticipantBindingsContext(t.Context(), store.CardDAVParticipantBindingOptions{DryRun: true, Undo: true})
	require.NoError(err)
	assert.Equal(1, preview.ParticipantsUnbound)
	assert.Equal([]int64{participant}, bindingCardPerson(t, st, book, card.Href).ParticipantIDs)
	result, err := st.ReconcileCardDAVParticipantBindingsContext(t.Context(), store.CardDAVParticipantBindingOptions{Undo: true})
	require.NoError(err)
	assert.Equal(1, result.ParticipantsUnbound)
	assert.Empty(bindingCardPerson(t, st, book, card.Href).ParticipantIDs)
	applyParticipantBindingCards(t, st, account, book)
	assert.Equal([]int64{participant}, bindingCardPerson(t, st, book, card.Href).ParticipantIDs)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = st.ReconcileCardDAVParticipantBindingsContext(ctx, store.CardDAVParticipantBindingOptions{})
	assert.ErrorIs(err, context.Canceled)
}

func TestCardDAVParticipantBindingCardEditsRebaseAndRelease(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	st, account, book := newCardDAVResourceStore(t)
	oldID, err := st.EnsureParticipant("old@example.test", "Old Example", "example.test")
	require.NoError(err)
	newID, err := st.EnsureParticipant("new@example.test", "New Example", "example.test")
	require.NoError(err)
	card := remoteResource(book.CanonicalURL+"alex.vcf", "edit-alex", "Alex Example", "old@example.test", "one")
	applyParticipantBindingCards(t, st, account, book, card)
	person := bindingCardPerson(t, st, book, card.Href)
	require.Equal([]int64{oldID}, person.ParticipantIDs)
	card = remoteResource(card.Href, "edit-alex", "Alex Updated", "new@example.test", "two")
	card.SemanticHash = "semantic-two"
	applyParticipantBindingCards(t, st, account, book, card)
	person = bindingCardPerson(t, st, book, card.Href)
	assert.Equal([]int64{newID}, person.ParticipantIDs)
	require.NotNil(person.DisplayName)
	assert.Equal("Alex Updated", *person.DisplayName)
	books, err := st.ListCardDAVAddressBooksContext(t.Context(), store.AllCardDAVAccounts)
	require.NoError(err)
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration, SyncRevision: books[0].SyncRevision, RemovedHrefs: []string{card.Href}})
	require.NoError(err)
	bound, err := st.PersonForParticipantsContext(t.Context(), []int64{newID})
	require.NoError(err)
	assert.Nil(bound)
}

func TestCardDAVParticipantBindingConflictsNeverSteal(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	st, account, book := newCardDAVResourceStore(t)
	id, err := st.EnsureParticipant("alex@example.test", "Alex Example", "example.test")
	require.NoError(err)
	owner, _, err := st.CreatePersonFromParticipant(id)
	require.NoError(err)
	card := remoteResource(book.CanonicalURL+"alex.vcf", "conflict-alex", "Card Example", "alex@example.test", "one")
	applyParticipantBindingCards(t, st, account, book, card)
	person := bindingCardPerson(t, st, book, card.Href)
	require.NotEqual(owner.ID, person.ID)
	result, err := st.ReconcileCardDAVParticipantBindingsContext(t.Context(), store.CardDAVParticipantBindingOptions{})
	require.NoError(err)
	require.Len(result.ConflictDetails, 1)
	assert.Equal("existing_person", result.ConflictDetails[0].Reason)
	assert.ElementsMatch([]int64{owner.ID, person.ID}, result.ConflictDetails[0].PersonIDs)
	current, err := st.GetPersonContext(t.Context(), owner.ID)
	require.NoError(err)
	assert.Equal([]int64{id}, current.ParticipantIDs)
	assert.False(bindingOwned(t, st, id))
}

func TestCardDAVParticipantBindingSharedCardsWithdrawAutomaticOwnership(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(strconv.FormatBool(reverse), func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			st, account, book := newCardDAVResourceStore(t)
			id, err := st.EnsureParticipant("shared@example.test", "Shared Example", "example.test")
			require.NoError(err)
			first := remoteResource(book.CanonicalURL+"first.vcf", "shared-first", "First Example", "shared@example.test", "one")
			second := remoteResource(book.CanonicalURL+"second.vcf", "shared-second", "Second Example", "other@example.test", "one")
			applyParticipantBindingCards(t, st, account, book, first, second)
			a, b := bindingCardPerson(t, st, book, first.Href), bindingCardPerson(t, st, book, second.Href)
			require.NotEqual(a.ID, b.ID)
			second = remoteResource(second.Href, second.RemoteUID, "Second Example", "shared@example.test", "two")
			second.SemanticHash = "changed-shared"
			cards := []store.CardDAVRemoteResource{first, second}
			if reverse {
				cards[0], cards[1] = cards[1], cards[0]
			}
			applyParticipantBindingCards(t, st, account, book, cards...)
			bound, err := st.PersonForParticipantsContext(t.Context(), []int64{id})
			require.NoError(err)
			assert.Nil(bound)
			result, err := st.ReconcileCardDAVParticipantBindingsContext(t.Context(), store.CardDAVParticipantBindingOptions{DryRun: true})
			require.NoError(err)
			require.Len(result.ConflictDetails, 1)
			assert.Equal("shared_address", result.ConflictDetails[0].Reason)
			assert.ElementsMatch([]int64{a.ID, b.ID}, result.ConflictDetails[0].PersonIDs)
		})
	}
}

func TestCardDAVParticipantBindingPhoneAdmission(t *testing.T) {
	for _, raw := range []string{"202-555-0100 ext 99", "letters12025550100", "tel:+12025550100;ext=99", "tel:+12025550100;phone-context=example.test"} {
		t.Run(raw, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			st, account, book := newCardDAVResourceStore(t)
			// Normalization alone accepts malformed text; automatic ownership must not.
			phone := "+12025550100"
			if raw == "202-555-0100 ext 99" {
				phone = "+202555010099"
			}
			id, err := st.EnsureParticipantByPhone(t.Context(), phone, "Phone Example", "phone")
			require.NoError(err)
			card := remoteResource(book.CanonicalURL+"phone.vcf", "phone-card", "Phone Example", "unmatched@example.test", "one")
			card.RemoteBody = []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:phone-card\r\nFN:Phone Example\r\nTEL:" + raw + "\r\nEND:VCARD\r\n")
			card.Phones = []string{raw}
			card.Emails = nil
			applyParticipantBindingCards(t, st, account, book, card)
			bound, err := st.PersonForParticipantsContext(t.Context(), []int64{id})
			require.NoError(err)
			assert.Nil(bound)
		})
	}
}

func TestCardDAVParticipantBindingManualPromotionAndLinkConfirmOwnership(t *testing.T) {
	for _, action := range []string{"promote", "link", "repeat-link"} {
		t.Run(action, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			st, account, book := newCardDAVResourceStore(t)
			a, err := st.EnsureParticipant("alex@example.test", "Alex Example", "example.test")
			require.NoError(err)
			b, err := st.EnsureParticipant("alias@example.test", "Alias Example", "example.test")
			require.NoError(err)
			if action == "repeat-link" {
				_, err = st.LinkParticipants(a, b)
				require.NoError(err)
			}
			card := remoteResource(book.CanonicalURL+"manual.vcf", "manual-card", "Alex Example", "alex@example.test", "one")
			applyParticipantBindingCards(t, st, account, book, card)
			require.True(bindingOwned(t, st, a))
			if action == "promote" {
				_, _, err = st.CreatePersonFromParticipant(a)
			} else {
				_, err = st.LinkParticipants(a, b)
			}
			require.NoError(err)
			assert.False(bindingOwned(t, st, a))
			_, err = st.ReconcileCardDAVParticipantBindingsContext(t.Context(), store.CardDAVParticipantBindingOptions{Undo: true})
			require.NoError(err)
			person := bindingCardPerson(t, st, book, card.Href)
			assert.Contains(person.ParticipantIDs, a)
		})
	}
}

func TestCardDAVParticipantBindingImporterCollapsePreservesAutomaticOwnership(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	st, account, book := newCardDAVResourceStore(t)
	a, err := st.EnsureParticipant("alex@example.test", "Alex Example", "example.test")
	require.NoError(err)
	b, err := st.EnsureParticipant("survivor@example.test", "Survivor Example", "example.test")
	require.NoError(err)
	card := remoteResource(book.CanonicalURL+"collapse.vcf", "collapse-card", "Alex Example", "alex@example.test", "one")
	applyParticipantBindingCards(t, st, account, book, card)
	require.NoError(st.MergeParticipants(a, b))
	assert.True(bindingOwned(t, st, b))
	result, err := st.ReconcileCardDAVParticipantBindingsContext(t.Context(), store.CardDAVParticipantBindingOptions{})
	require.NoError(err)
	assert.Zero(result.ParticipantsBound)
	assert.Zero(result.ParticipantsUnbound)
	assert.True(bindingOwned(t, st, b))

	undo, err := st.ReconcileCardDAVParticipantBindingsContext(t.Context(), store.CardDAVParticipantBindingOptions{Undo: true})
	require.NoError(err)
	assert.Equal(1, undo.ParticipantsUnbound)
	assert.Empty(bindingCardPerson(t, st, book, card.Href).ParticipantIDs)
}

func TestCardDAVParticipantMergeDoesNotStealThirdPartyEmailIdentifier(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	st, _, _ := newCardDAVResourceStore(t)
	absorbed, err := st.EnsureParticipant("absorbed@example.test", "Absorbed Example", "example.test")
	require.NoError(err)
	survivor, err := st.EnsureParticipant("survivor@example.test", "Survivor Example", "example.test")
	require.NoError(err)
	third, err := st.EnsureParticipant("third@example.test", "Third Example", "example.test")
	require.NoError(err)
	require.NoError(st.SetParticipantIdentifier(third, "email", "absorbed@example.test"))

	require.NoError(st.MergeParticipants(absorbed, survivor))
	resolved, err := st.EnsureParticipantByIdentifier("email", "absorbed@example.test", "")
	require.NoError(err)
	assert.Equal(third, resolved)
}

func TestCardDAVParticipantBindingDecodesLegacyEncodedContact(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	st, account, book := newCardDAVResourceStore(t)
	participant, err := st.EnsureParticipant("josé@example.test", "Jose Example", "example.test")
	require.NoError(err)
	card := remoteResource(book.CanonicalURL+"encoded.vcf", "encoded-contact", "Jose Example", "encoded@example.test", "one")
	card.RemoteBody = []byte("BEGIN:VCARD\r\nVERSION:2.1\r\nUID:encoded-contact\r\nFN:Jose Example\r\nEMAIL;CHARSET=ISO-8859-1;ENCODING=QUOTED-PRINTABLE:jos=E9@example.test\r\nEND:VCARD\r\n")
	applyParticipantBindingCards(t, st, account, book, card)
	assert.Equal([]int64{participant}, bindingCardPerson(t, st, book, card.Href).ParticipantIDs)
}

func TestCardDAVParticipantBindingSystemLinkMakesMixedMembershipManual(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	st, account, book := newCardDAVResourceStore(t)
	a, err := st.EnsureParticipant("alex@example.test", "Alex Example", "example.test")
	require.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(a)
	require.NoError(err)
	b, err := st.EnsureParticipant("alias@example.test", "Alex Example", "example.test")
	require.NoError(err)
	card := remoteResource(book.CanonicalURL+"mixed-ownership.vcf", person.VCardUID, "Alex Example", "alex@example.test", "one")
	card.RemoteBody = []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:" + person.VCardUID + "\r\nFN:Alex Example\r\nEMAIL:alex@example.test\r\nEMAIL:alias@example.test\r\nEND:VCARD\r\n")
	card.Emails = []string{"alex@example.test", "alias@example.test"}
	applyParticipantBindingCards(t, st, account, book, card)
	assert.Equal(person.ID, bindingCardPerson(t, st, book, card.Href).ID)
	assert.False(bindingOwned(t, st, a))
	require.True(bindingOwned(t, st, b))

	candidate, _, err := st.UpsertIdentityMatchCandidateContext(t.Context(), store.IdentityMatchCandidateInput{
		LeftKind: store.IdentityMatchParticipant, LeftID: a,
		RightKind: store.IdentityMatchParticipant, RightID: b,
		Basis: store.IdentityMatchStableProviderID, NormalizedValue: new("synthetic-provider-id"),
		State: store.IdentityMatchStateCandidate, Source: store.ProvenanceArchiveObservation,
	})
	require.NoError(err)
	_, _, err = st.AcceptIdentityMatchCandidateContext(t.Context(), candidate.ID, "system", nil)
	require.NoError(err)
	assert.False(bindingOwned(t, st, b))

	result, err := st.ReconcileCardDAVParticipantBindingsContext(t.Context(), store.CardDAVParticipantBindingOptions{Undo: true})
	require.NoError(err)
	assert.Zero(result.ParticipantsUnbound)
	assert.ElementsMatch([]int64{a, b}, bindingCardPerson(t, st, book, card.Href).ParticipantIDs)
}

func TestCardDAVParticipantBindingMergedPhoneAliasRemainsClaimed(t *testing.T) {
	for _, identifierType := range []string{"imessage", "google_voice"} {
		t.Run(identifierType, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			st, account, book := newCardDAVResourceStore(t)
			absorbed, err := st.EnsureParticipantByPhone(t.Context(), "+12025550101", "Alex Example", identifierType)
			require.NoError(err)
			survivor, err := st.EnsureParticipantByPhone(t.Context(), "+12025550102", "Alex Example", "whatsapp")
			require.NoError(err)
			card := remoteResource(book.CanonicalURL+"phone-alias.vcf", "phone-alias-card", "Alex Example", "unmatched@example.test", "one")
			card.RemoteBody = []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:phone-alias-card\r\nFN:Alex Example\r\nTEL:+12025550101\r\nEND:VCARD\r\n")
			card.Emails = nil
			card.Phones = []string{"+12025550101"}
			applyParticipantBindingCards(t, st, account, book, card)
			require.True(bindingOwned(t, st, absorbed))
			require.NoError(st.MergeParticipants(absorbed, survivor))
			require.True(bindingOwned(t, st, survivor))

			result, err := st.ReconcileCardDAVParticipantBindingsContext(t.Context(), store.CardDAVParticipantBindingOptions{})
			require.NoError(err)
			assert.Zero(result.ParticipantsBound)
			assert.Zero(result.ParticipantsUnbound)
			assert.Equal([]int64{survivor}, bindingCardPerson(t, st, book, card.Href).ParticipantIDs)
			assert.True(bindingOwned(t, st, survivor))
		})
	}
}

func TestCardDAVParticipantBindingLifecycleCleanup(t *testing.T) {
	for _, action := range []string{"demote", "discovery", "conflict-edit", "conflict-tombstone"} {
		t.Run(action, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			st, account, book := newCardDAVResourceStore(t)
			id, err := st.EnsureParticipant("alex@example.test", "Alex Example", "example.test")
			require.NoError(err)
			card := remoteResource(book.CanonicalURL+"lifecycle.vcf", "lifecycle-card", "Alex Example", "alex@example.test", "one")
			applyParticipantBindingCards(t, st, account, book, card)
			require.True(bindingOwned(t, st, id))
			person := bindingCardPerson(t, st, book, card.Href)
			_, err = st.SetPersonTrackingContext(t.Context(), person.ID, true)
			require.NoError(err)
			switch action {
			case "demote":
				err = st.SetCardDAVBookRolesContext(t.Context(), book.ID, store.CardDAVBookRoles{IsLookupSource: true})
			case "discovery":
				allowed := true
				_, _, err = st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{BaseURL: "https://contacts.example/dav", Username: "alice", PrincipalURL: "https://contacts.example/principal/alice/", HomeURL: "https://contacts.example/books/alice/", Books: []store.CardDAVDiscoveredBook{{CanonicalURL: "https://contacts.example/books/alice/replacement/", DisplayName: "Replacement", CanCreate: &allowed}}})
			default:
				resource, readErr := st.GetCardDAVResourceContext(t.Context(), book.ID, card.Href)
				require.NoError(readErr)
				capture := store.CardDAVConflictCapture{AddressBookID: book.ID, Href: card.Href, ExpectedPersonID: *resource.PersonID, ExpectedMappingRevision: resource.MappingRevision, BaseLocalHash: resource.LocalHash, LocalHash: resource.LocalHash, BaseRemoteHash: resource.RemoteSemanticHash, BaseRemoteETag: resource.RemoteETag, RemoteETag: "two", LocalBody: card.RemoteBody, RemoteBody: []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Changed Example\r\nEND:VCARD\r\n")}
				if action == "conflict-tombstone" {
					capture.RemoteTombstone = true
					capture.RemoteBody = nil
				}
				_, err = st.RecordCardDAVConflictContext(t.Context(), capture)
			}
			require.NoError(err)
			bound, err := st.PersonForParticipantsContext(t.Context(), []int64{id})
			require.NoError(err)
			assert.Nil(bound)
		})
	}
}

func TestCardDAVParticipantBindingScopedAliasesAndTrueLocalEdit(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	st, account, book := newCardDAVResourceStore(t)
	a, err := st.EnsureParticipant("primary@example.test", "Primary Example", "example.test")
	require.NoError(err)
	b, err := st.EnsureParticipant("scoped@example.test", "Scoped Example", "example.test")
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO participant_identifiers (participant_id,identifier_type,identifier_value,scope_kind,scope_value) VALUES (?, 'email', 'card@example.test', 'server', 'synthetic')`), b)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO participant_identifiers (participant_id,identifier_type,identifier_value) VALUES (?, 'email', 'ALIAS@EXAMPLE.TEST')`), a)
	require.NoError(err)
	card := remoteResource(book.CanonicalURL+"alias.vcf", "alias-card", "Alias Example", "alias@example.test", "one")
	card.RemoteBody = []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:alias-card\r\nFN:Alias Example\r\nEMAIL:alias@example.test\r\nEMAIL:card@example.test\r\nEND:VCARD\r\n")
	card.Emails = append(card.Emails, "card@example.test")
	applyParticipantBindingCards(t, st, account, book, card)
	person := bindingCardPerson(t, st, book, card.Href)
	assert.Equal([]int64{a}, person.ParticipantIDs)
	name := "Local Example"
	_, err = st.UpdatePersonDisplayNameContext(t.Context(), person.ID, person.Revision, &name)
	require.NoError(err)
	card.DisplayName = "Remote Example"
	card.RemoteETag = "two"
	card.SemanticHash = "remote-edited"
	card.RemoteBody = []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:alias-card\r\nFN:Remote Example\r\nEMAIL:alias@example.test\r\nEND:VCARD\r\n")
	needsConflict, err := st.CardDAVRemoteUpdateNeedsConflictContext(t.Context(), book.ID, card.Href, card)
	require.NoError(err)
	assert.True(needsConflict)
	assert.Equal("Local Example", *bindingCardPerson(t, st, book, card.Href).DisplayName)
}

func TestCardDAVParticipantBindingSystemAndUserCandidateOwnership(t *testing.T) {
	for _, actor := range []string{"system", "user"} {
		t.Run(actor, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			st, account, book := newCardDAVResourceStore(t)
			a, err := st.EnsureParticipant("alex@example.test", "Alex Example", "example.test")
			require.NoError(err)
			b, err := st.EnsureParticipant("other@example.test", "Other Example", "example.test")
			require.NoError(err)
			card := remoteResource(book.CanonicalURL+"candidate.vcf", "candidate-card", "Alex Example", "alex@example.test", "one")
			applyParticipantBindingCards(t, st, account, book, card)
			candidate, _, err := st.UpsertIdentityMatchCandidateContext(t.Context(), store.IdentityMatchCandidateInput{LeftKind: store.IdentityMatchParticipant, LeftID: a, RightKind: store.IdentityMatchParticipant, RightID: b, Basis: store.IdentityMatchStableProviderID, NormalizedValue: new("synthetic-provider-id"), State: store.IdentityMatchStateCandidate, Source: store.ProvenanceUser})
			require.NoError(err)
			_, _, err = st.AcceptIdentityMatchCandidateContext(t.Context(), candidate.ID, actor, nil)
			require.NoError(err)
			assert.Equal(actor == "system", bindingOwned(t, st, a))
			assert.Equal(actor == "system", bindingOwned(t, st, b))
		})
	}
}

func TestCardDAVParticipantBindingPersonMergeConfirmsBothSides(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	st, account, book := newCardDAVResourceStore(t)
	a, err := st.EnsureParticipant("alex@example.test", "Alex Example", "example.test")
	require.NoError(err)
	b, err := st.EnsureParticipant("blair@example.test", "Blair Example", "example.test")
	require.NoError(err)
	first := remoteResource(book.CanonicalURL+"alex.vcf", "merge-first", "Alex Example", "alex@example.test", "one")
	second := remoteResource(book.CanonicalURL+"blair.vcf", "merge-second", "Blair Example", "blair@example.test", "one")
	applyParticipantBindingCards(t, st, account, book, first, second)
	left, right := bindingCardPerson(t, st, book, first.Href), bindingCardPerson(t, st, book, second.Href)
	merged, err := st.MergePersonsContext(t.Context(), store.PersonMergeRequest{SurvivorID: left.ID, AbsorbedID: right.ID, ExpectedSurvivorRevision: left.Revision, ExpectedAbsorbedRevision: right.Revision, IdempotencyKey: "carddav-binding-merge", Actor: "test"})
	require.NoError(err)
	assert.False(bindingOwned(t, st, a))
	assert.False(bindingOwned(t, st, b))
	result, err := st.ReconcileCardDAVParticipantBindingsContext(t.Context(), store.CardDAVParticipantBindingOptions{Undo: true})
	require.NoError(err)
	assert.Zero(result.ParticipantsUnbound)
	_, err = st.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{SourcePersonID: merged.Person.ID, MergeID: merged.Merge.ID, ParticipantIDs: []int64{b}, ExpectedSourceRevision: merged.Person.Revision, IdempotencyKey: "carddav-binding-split", Actor: "test"})
	require.NoError(err)
	assert.False(bindingOwned(t, st, a))
	assert.False(bindingOwned(t, st, b))
}

func TestCardDAVParticipantBindingActiveMergeReportsOnlyNewMembership(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	st, account, book := newCardDAVResourceStore(t)
	a, err := st.EnsureParticipant("alex@example.test", "Alex Example", "example.test")
	require.NoError(err)
	b, err := st.EnsureParticipant("blair@example.test", "Blair Example", "example.test")
	require.NoError(err)
	first := remoteResource(book.CanonicalURL+"merge-first.vcf", "merge-first-card", "Alex Example", "alex@example.test", "one")
	first.Emails = []string{"alex@example.test", "late@example.test"}
	first.RemoteBody = []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:merge-first-card\r\nFN:Alex Example\r\nEMAIL:alex@example.test\r\nEMAIL:late@example.test\r\nEND:VCARD\r\n")
	second := remoteResource(book.CanonicalURL+"merge-second.vcf", "merge-second-card", "Blair Example", "blair@example.test", "one")
	applyParticipantBindingCards(t, st, account, book, first, second)
	left, right := bindingCardPerson(t, st, book, first.Href), bindingCardPerson(t, st, book, second.Href)
	merged, err := st.MergePersonsContext(t.Context(), store.PersonMergeRequest{
		SurvivorID: left.ID, AbsorbedID: right.ID,
		ExpectedSurvivorRevision: left.Revision, ExpectedAbsorbedRevision: right.Revision,
		IdempotencyKey: "carddav-active-merge-noop", Actor: "test",
	})
	require.NoError(err)
	assert.False(bindingOwned(t, st, a))
	assert.False(bindingOwned(t, st, b))

	late, err := st.EnsureParticipant("late@example.test", "Late Example", "example.test")
	require.NoError(err)
	preview, err := st.ReconcileCardDAVParticipantBindingsContext(t.Context(), store.CardDAVParticipantBindingOptions{DryRun: true})
	require.NoError(err)
	require.Len(preview.ConflictDetails, 1)
	assert.Equal(1, preview.Conflicts)
	assert.Equal(late, preview.ConflictDetails[0].ParticipantID)
	assert.Equal([]int64{merged.Person.ID}, preview.ConflictDetails[0].PersonIDs)
	assert.Equal("active_merge", preview.ConflictDetails[0].Reason)
	assert.Zero(preview.ParticipantsBound)
	assert.Zero(preview.ParticipantsUnbound)
	unbound, err := st.PersonForParticipantsContext(t.Context(), []int64{late})
	require.NoError(err)
	assert.Nil(unbound, "new matches must not receive merge lineage implicitly")

	result, err := st.ReconcileCardDAVParticipantBindingsContext(t.Context(), store.CardDAVParticipantBindingOptions{})
	require.NoError(err)
	assert.Equal(1, result.Conflicts)
	assert.Zero(result.ParticipantsBound)
	unbound, err = st.PersonForParticipantsContext(t.Context(), []int64{late})
	require.NoError(err)
	assert.Nil(unbound)

	split, err := st.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
		SourcePersonID: merged.Person.ID, MergeID: merged.Merge.ID, ParticipantIDs: []int64{b},
		ExpectedSourceRevision: merged.Person.Revision, IdempotencyKey: "carddav-active-merge-split", Actor: "test",
	})
	require.NoError(err)
	assert.False(bindingOwned(t, st, a))
	assert.False(bindingOwned(t, st, b))
	result, err = st.ReconcileCardDAVParticipantBindingsContext(t.Context(), store.CardDAVParticipantBindingOptions{})
	require.NoError(err)
	assert.Zero(result.Conflicts)
	assert.Equal(1, result.ParticipantsBound)
	latePerson, err := st.PersonForParticipantsContext(t.Context(), []int64{late})
	require.NoError(err)
	require.NotNil(latePerson)
	assert.Equal(split.SourcePerson.ID, latePerson.ID)
	assert.True(bindingOwned(t, st, late))
	absorbedPerson, err := st.PersonForParticipantsContext(t.Context(), []int64{b})
	require.NoError(err)
	require.NotNil(absorbedPerson)
	assert.Equal(split.NewPerson.ID, absorbedPerson.ID)
}

func TestCardDAVParticipantBindingAcceptsNoBreakSpaceInPhone(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	st, account, book := newCardDAVResourceStore(t)
	participant, err := st.EnsureParticipantByPhone(t.Context(), "+12025550100", "Phone Example", "phone")
	require.NoError(err)
	card := remoteResource(book.CanonicalURL+"nbsp-phone.vcf", "nbsp-phone-card", "Phone Example", "unmatched@example.test", "one")
	card.Phones = []string{"+1\u00a0202-555-0100"}
	card.RemoteBody = []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:nbsp-phone-card\r\nFN:Phone Example\r\nEMAIL:unmatched@example.test\r\nTEL;TYPE=cell:+1\u00a0202-555-0100\r\nEND:VCARD\r\n")
	applyParticipantBindingCards(t, st, account, book, card)
	assert.Equal([]int64{participant}, bindingCardPerson(t, st, book, card.Href).ParticipantIDs)
}

func TestCardDAVParticipantBindingNumericPhoneContext(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	st, account, book := newCardDAVResourceStore(t)
	id, err := st.EnsureParticipantByPhone(t.Context(), "+12025550100", "Phone Example", "phone")
	require.NoError(err)
	card := remoteResource(book.CanonicalURL+"context.vcf", "context-card", "Phone Example", "unmatched@example.test", "one")
	card.RemoteBody = []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:context-card\r\nFN:Phone Example\r\nTEL;TYPE=cell;VALUE=uri:tel:555-0100;phone-context=+1-202\r\nEND:VCARD\r\n")
	card.Phones = []string{"tel:555-0100;phone-context=+1-202"}
	card.Emails = nil
	applyParticipantBindingCards(t, st, account, book, card)
	assert.Equal([]int64{id}, bindingCardPerson(t, st, book, card.Href).ParticipantIDs)
}

func TestCardDAVParticipantBindingLegacyOwnershipMigration(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	st, _, _ := newCardDAVResourceStore(t)
	id, err := st.EnsureParticipant("legacy@example.test", "Legacy Example", "example.test")
	require.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(id)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), `ALTER TABLE person_participants DROP COLUMN carddav_owned`)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), `UPDATE archive_metadata SET value = '25' WHERE key = 'schema_version'`)
	require.NoError(err)
	require.NoError(st.InitSchemaContext(t.Context()))
	assert.False(bindingOwned(t, st, id))
	current, err := st.GetPersonContext(t.Context(), person.ID)
	require.NoError(err)
	assert.Equal([]int64{id}, current.ParticipantIDs)
	version, err := st.SchemaVersionContext(t.Context())
	require.NoError(err)
	assert.Equal(store.SchemaVersion, version)
}

func installBindingWriteFailure(t *testing.T, st *store.Store, table, operation string) {
	t.Helper()
	// Fault injection covers rollback through real Store transactions on both
	// backends, including trigger side effects and resource/person persistence.
	if st.IsPostgreSQL() {
		_, err := st.DB().ExecContext(t.Context(), `CREATE FUNCTION reject_carddav_binding_write() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic binding failure'; END $$`)
		Require.NoError(t, err)
		_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER reject_carddav_binding_write BEFORE `+operation+` ON `+table+` FOR EACH ROW EXECUTE FUNCTION reject_carddav_binding_write()`)
		Require.NoError(t, err)
	} else {
		_, err := st.DB().ExecContext(t.Context(), `CREATE TRIGGER reject_carddav_binding_write BEFORE `+operation+` ON `+table+` BEGIN SELECT RAISE(ABORT, 'synthetic binding failure'); END`)
		Require.NoError(t, err)
	}
}

func TestCardDAVParticipantBindingDryRunNeverWrites(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	st, account, book := newCardDAVResourceStore(t)
	_, err := st.EnsureParticipant("alex@example.test", "Alex Example", "example.test")
	require.NoError(err)
	card := remoteResource(book.CanonicalURL+"readonly.vcf", "readonly-card", "Alex Example", "alex@example.test", "one")
	applyParticipantBindingCards(t, st, account, book, card)
	before := bindingCardPerson(t, st, book, card.Href)
	var journalBefore int64
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM person_sweep_changes`).Scan(&journalBefore))
	installBindingWriteFailure(t, st, "archive_metadata", "UPDATE")
	for _, undo := range []bool{false, true} {
		result, err := st.ReconcileCardDAVParticipantBindingsContext(t.Context(), store.CardDAVParticipantBindingOptions{DryRun: true, Undo: undo})
		require.NoError(err)
		if undo {
			assert.Equal(1, result.ParticipantsUnbound)
		} else {
			assert.Zero(result.ParticipantsBound)
		}
	}
	after := bindingCardPerson(t, st, book, card.Href)
	assert.Equal(before, after)
	var journalAfter int64
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM person_sweep_changes`).Scan(&journalAfter))
	assert.Equal(journalBefore, journalAfter)
}

func TestCardDAVParticipantBindingApplyRollsBackResourceAndMembership(t *testing.T) {
	for _, operation := range []string{"INSERT", "DELETE"} {
		t.Run(operation, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			st, account, book := newCardDAVResourceStore(t)
			id, err := st.EnsureParticipant("alex@example.test", "Alex Example", "example.test")
			require.NoError(err)
			card := remoteResource(book.CanonicalURL+"rollback.vcf", "rollback-card", "Alex Example", "alex@example.test", "one")
			plan := store.CardDAVSyncPlan{AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration, SyncRevision: book.SyncRevision, Upserts: []store.CardDAVRemoteResource{card}}
			if operation == "DELETE" {
				applyParticipantBindingCards(t, st, account, book, card)
				books, err := st.ListCardDAVAddressBooksContext(t.Context(), store.AllCardDAVAccounts)
				require.NoError(err)
				plan.SyncRevision = books[0].SyncRevision
				plan.Upserts = nil
				plan.RemovedHrefs = []string{card.Href}
			}
			installBindingWriteFailure(t, st, "person_participants", operation)
			_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), plan)
			require.Error(err)
			resource, err := st.GetCardDAVResourceContext(t.Context(), book.ID, card.Href)
			if operation == "INSERT" {
				assert.ErrorIs(err, store.ErrCardDAVResourceNotFound)
			} else {
				require.NoError(err)
				require.NotNil(resource.PersonID)
				assert.True(bindingOwned(t, st, id))
			}
		})
	}
}

func TestCardDAVParticipantBindingLegacyIMessageEmailAlias(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	st, account, book := newCardDAVResourceStore(t)
	id, err := st.EnsureParticipantByPhone(t.Context(), "+12025550100", "Alex Example", "imessage")
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO participant_identifiers (participant_id,identifier_type,identifier_value) VALUES (?, 'imessage', 'Alias@Example.test')`), id)
	require.NoError(err)
	card := remoteResource(book.CanonicalURL+"imessage.vcf", "imessage-card", "Alex Example", "alias@example.test", "one")
	applyParticipantBindingCards(t, st, account, book, card)
	assert.Equal([]int64{id}, bindingCardPerson(t, st, book, card.Href).ParticipantIDs)
}

func TestCardDAVParticipantBindingSynctechEmailIdentifier(t *testing.T) {
	for _, test := range []struct {
		name   string
		scoped bool
	}{
		{name: "unscoped address binds"},
		{name: "scoped handle stays excluded", scoped: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			st, account, book := newCardDAVResourceStore(t)
			id, err := st.EnsureParticipantByIdentifier("synctech_sms", "Alex@Example.Test", "Alex Example")
			require.NoError(err)
			if test.scoped {
				_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE participant_identifiers SET
					scope_kind = 'service', scope_value = 'synthetic-source'
					WHERE participant_id = ? AND identifier_type = 'synctech_sms' AND identifier_value = ?`),
					id, "Alex@Example.Test")
				require.NoError(err)
			}

			card := remoteResource(book.CanonicalURL+"synctech.vcf", "synctech-card", "Alex Example", "alex@example.test", "one")
			applyParticipantBindingCards(t, st, account, book, card)
			person, err := st.PersonForParticipantsContext(t.Context(), []int64{id})
			require.NoError(err)
			if test.scoped {
				assert.Nil(person)
				return
			}
			require.NotNil(person)
			assert.Equal(bindingCardPerson(t, st, book, card.Href).ID, person.ID)
		})
	}
}

func TestCardDAVParticipantBindingProvisionalCreateCollisionIsNotEvidence(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	st, _, book := newCardDAVResourceStore(t)
	id, err := st.EnsureParticipant("collision@example.test", "Collision Example", "example.test")
	require.NoError(err)
	var personID int64
	require.NoError(st.DB().QueryRowContext(t.Context(), `INSERT INTO persons (vcard_uid,display_name) VALUES ('collision-local','Local Example') RETURNING id`).Scan(&personID))
	snapshot, err := st.LoadPersonVCardSnapshotContext(t.Context(), personID)
	require.NoError(err)
	local := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:collision-local\r\nFN:Local Example\r\nEND:VCARD\r\n")
	pending, err := st.PrepareCardDAVPublicationContext(t.Context(), store.CardDAVPublicationPlan{PersonID: personID, Desired: true, AddressBookID: book.ID, Href: book.CanonicalURL + "collision.vcf", OutgoingBody: local, OutgoingSemanticHash: "local", LocalHash: snapshot.Fingerprint})
	require.NoError(err)
	remote := remoteResource(pending.Href, "collision-remote", "Remote Example", "collision@example.test", "remote")
	fenced, err := st.FenceCardDAVCreateCollisionContext(t.Context(), *pending, remote)
	require.NoError(err)
	// No conflict has been recorded yet: a stopped recovery must not turn the
	// provisional remote card into accepted evidence on the next reconciliation.
	result, err := st.ReconcileCardDAVParticipantBindingsContext(t.Context(), store.CardDAVParticipantBindingOptions{})
	require.NoError(err)
	assert.Zero(result.ParticipantsBound)
	person, err := st.PersonForParticipantsContext(t.Context(), []int64{id})
	require.NoError(err)
	assert.Nil(person)
	require.NoError(st.RollbackCardDAVPublicationContext(t.Context(), fenced))
	result, err = st.ReconcileCardDAVParticipantBindingsContext(t.Context(), store.CardDAVParticipantBindingOptions{})
	require.NoError(err)
	assert.Zero(result.ParticipantsBound)
	mapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, remote.Href)
	require.NoError(err)
	assert.Equal(store.CardDAVMappingUnbound, mapping.MappingStatus)
	assert.Nil(mapping.PersonID)
}

func TestCardDAVParticipantBindingSystemChangesKeepRemoteRebaseBaseline(t *testing.T) {
	for _, action := range []string{"system-link", "importer-collapse"} {
		t.Run(action, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			st, account, book := newCardDAVResourceStore(t)
			a, err := st.EnsureParticipant("alex@example.test", "Alex Example", "example.test")
			require.NoError(err)
			b, err := st.EnsureParticipant("other@example.test", "Other Example", "example.test")
			require.NoError(err)
			card := remoteResource(book.CanonicalURL+"baseline.vcf", "baseline-card", "Alex Example", "alex@example.test", "one")
			applyParticipantBindingCards(t, st, account, book, card)
			if action == "system-link" {
				candidate, _, err := st.UpsertIdentityMatchCandidateContext(t.Context(), store.IdentityMatchCandidateInput{LeftKind: store.IdentityMatchParticipant, LeftID: a, RightKind: store.IdentityMatchParticipant, RightID: b, Basis: store.IdentityMatchStableProviderID, NormalizedValue: new("synthetic-provider-id"), State: store.IdentityMatchStateCandidate, Source: store.ProvenanceArchiveObservation})
				require.NoError(err)
				_, _, err = st.AcceptIdentityMatchCandidateContext(t.Context(), candidate.ID, "system", nil)
				require.NoError(err)
			} else {
				require.NoError(st.MergeParticipants(a, b))
			}
			card = remoteResource(card.Href, card.RemoteUID, "Remote Updated", "alex@example.test", "two")
			card.SemanticHash = "updated-baseline"
			applyParticipantBindingCards(t, st, account, book, card)
			assert.Equal("Remote Updated", *bindingCardPerson(t, st, book, card.Href).DisplayName)
		})
	}
}

func TestCardDAVParticipantBindingPublicationConflictLockOrder(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	st, account, book := newCardDAVResourceStore(t)
	if !st.IsPostgreSQL() {
		t.Skip("PostgreSQL row-lock scheduling")
	}
	_, err := st.EnsureParticipant("alex@example.test", "Alex Example", "example.test")
	require.NoError(err)
	card := remoteResource(book.CanonicalURL+"lock-order.vcf", "lock-order-card", "Alex Example", "alex@example.test", "one")
	applyParticipantBindingCards(t, st, account, book, card)
	person := bindingCardPerson(t, st, book, card.Href)
	snapshot, err := st.LoadPersonVCardSnapshotContext(t.Context(), person.ID)
	require.NoError(err)
	pending, err := st.PrepareCardDAVPublicationContext(t.Context(), store.CardDAVPublicationPlan{PersonID: person.ID, Desired: true, AddressBookID: book.ID, Href: card.Href, OutgoingBody: card.RemoteBody, OutgoingSemanticHash: "local-update", LocalHash: snapshot.Fingerprint})
	require.NoError(err)
	mapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, card.Href)
	require.NoError(err)
	capture := store.CardDAVConflictCapture{AddressBookID: book.ID, Href: card.Href, ExpectedPersonID: person.ID, ExpectedMappingRevision: mapping.MappingRevision, BaseLocalHash: mapping.LocalHash, LocalHash: snapshot.Fingerprint, BaseRemoteHash: mapping.RemoteSemanticHash, BaseRemoteETag: mapping.RemoteETag, RemoteETag: "two", LocalBody: card.RemoteBody, RemoteBody: []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Changed Example\r\nEND:VCARD\r\n")}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	blocker, err := st.DB().BeginTx(ctx, nil)
	require.NoError(err)
	defer func() { _ = blocker.Rollback() }()
	var blockerPID int
	require.NoError(blocker.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID))
	_, err = blocker.ExecContext(ctx, `UPDATE archive_metadata SET value=value WHERE key='identity_revision'`)
	require.NoError(err)
	done := make(chan error, 1)
	go func() { _, err := st.RecordCardDAVPublicationConflictContext(ctx, *pending, capture); done <- err }()
	waitForPostgreSQLBlockedPID(ctx, t, st, blockerPID, "archive_metadata", "publication conflict must reach the identity lock")
	// While capture waits for identity serialization, another writer must still
	// be able to lock the resource. The reverse order deadlocks reconciliation.
	var resourceID int64
	probeErr := st.DB().QueryRowContext(ctx, st.Rebind(`SELECT id FROM carddav_resources WHERE id=? FOR UPDATE NOWAIT`), mapping.ID).Scan(&resourceID)
	assert.NoError(probeErr, "capture must not own the resource before identity")
	require.NoError(blocker.Rollback())
	require.NoError(<-done)
}

func TestCardDAVParticipantBindingSyncPreflightTakesIdentityLockFirst(t *testing.T) {
	require := Require.New(t)
	st, account, book := newCardDAVResourceStore(t)
	if !st.IsPostgreSQL() {
		t.Skip("PostgreSQL row-lock scheduling")
	}
	_, err := st.EnsureParticipant("alex@example.test", "Alex Example", "example.test")
	require.NoError(err)
	card := remoteResource(
		book.CanonicalURL+"preflight-lock-order.vcf", "preflight-lock-order-card",
		"Alex Example", "alex@example.test", "one",
	)
	applyParticipantBindingCards(t, st, account, book, card)
	mapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, card.Href)
	require.NoError(err)
	require.NotNil(mapping.PersonID)

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	blocker, err := st.DB().BeginTx(ctx, nil)
	require.NoError(err)
	defer func() { _ = blocker.Rollback() }()
	var blockerPID int
	require.NoError(blocker.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID))
	_, err = blocker.ExecContext(ctx, `UPDATE archive_metadata SET value=value WHERE key='identity_revision'`)
	require.NoError(err)
	var personID int64
	require.NoError(blocker.QueryRowContext(ctx,
		`SELECT id FROM persons WHERE id=$1 FOR UPDATE`, *mapping.PersonID).Scan(&personID))

	input := card
	input.SemanticHash = "remote-two"
	preflightDone := make(chan error, 1)
	go func() {
		_, err := st.CardDAVRemoteUpdateNeedsConflictContext(ctx, book.ID, card.Href, input)
		preflightDone <- err
	}()
	waitForPostgreSQLBlockedPID(ctx, t, st, blockerPID, "archive_metadata",
		"sync preflight must wait for identity serialization before taking CardDAV row locks")
	var resourceID int64
	require.NoError(blocker.QueryRowContext(ctx,
		`SELECT id FROM carddav_resources WHERE id=$1 FOR UPDATE`, mapping.ID).Scan(&resourceID))
	require.NoError(blocker.Commit())
	require.NoError(<-preflightDone)
}

func TestCardDAVParticipantBindingLastCardCleanupAdvancesIdentityRevision(t *testing.T) {
	for _, action := range []string{"delete", "demote"} {
		t.Run(action, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			st, account, book := newCardDAVResourceStore(t)
			id, err := st.EnsureParticipant("alex@example.test", "Alex Example", "example.test")
			require.NoError(err)
			card := remoteResource(book.CanonicalURL+"last.vcf", "last-card", "Alex Example", "alex@example.test", "one")
			applyParticipantBindingCards(t, st, account, book, card)
			person := bindingCardPerson(t, st, book, card.Href)
			before, err := st.IdentityRevision()
			require.NoError(err)
			if action == "demote" {
				require.NoError(st.SetCardDAVBookRolesContext(t.Context(), book.ID, store.CardDAVBookRoles{IsLookupSource: true}))
			} else {
				books, err := st.ListCardDAVAddressBooksContext(t.Context(), store.AllCardDAVAccounts)
				require.NoError(err)
				_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration, SyncRevision: books[0].SyncRevision, RemovedHrefs: []string{card.Href}})
				require.NoError(err)
			}
			after, err := st.IdentityRevision()
			require.NoError(err)
			assert.Greater(after, before)
			retained, err := st.GetPersonContext(t.Context(), person.ID)
			require.NoError(err)
			assert.Empty(retained.ParticipantIDs)
			bound, err := st.PersonForParticipantsContext(t.Context(), []int64{id})
			require.NoError(err)
			assert.Nil(bound)
		})
	}
}

func TestCardDAVParticipantBindingPublicationPreparationLockOrder(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	st, _, book := newCardDAVResourceStore(t)
	if !st.IsPostgreSQL() {
		t.Skip("PostgreSQL row-lock scheduling")
	}
	var personID int64
	require.NoError(st.DB().QueryRowContext(t.Context(), `INSERT INTO persons (vcard_uid,display_name) VALUES ('prepare-lock-person','Prepare Example') RETURNING id`).Scan(&personID))
	snapshot, err := st.LoadPersonVCardSnapshotContext(t.Context(), personID)
	require.NoError(err)
	body := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:prepare-lock-person\r\nFN:Prepare Example\r\nEND:VCARD\r\n")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	require.
		// Seed the same serialization row used by reconciliation, then hold it.
		NoError(st.DB().QueryRowContext(ctx, st.Rebind(`INSERT INTO archive_metadata(key,value) VALUES ('identity_revision','0') ON CONFLICT(key) DO UPDATE SET value=archive_metadata.value RETURNING value`)).Scan(new(string)))
	blocker, err := st.DB().BeginTx(ctx, nil)
	require.NoError(err)
	defer func() { _ = blocker.Rollback() }()
	var blockerPID int
	require.NoError(blocker.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID))
	_, err = blocker.ExecContext(ctx, `UPDATE archive_metadata SET value=value WHERE key='identity_revision'`)
	require.NoError(err)
	done := make(chan error, 1)
	go func() {
		_, err := st.PrepareCardDAVPublicationContext(ctx, store.CardDAVPublicationPlan{PersonID: personID, Desired: true, AddressBookID: book.ID, Href: book.CanonicalURL + "prepare-lock.vcf", OutgoingBody: body, OutgoingSemanticHash: "prepare-lock", LocalHash: snapshot.Fingerprint})
		done <- err
	}()
	waitForPostgreSQLBlockedPID(ctx, t, st, blockerPID, "archive_metadata", "publication preparation must wait for identity serialization")
	var id int64
	assert.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT id FROM persons WHERE id=? FOR UPDATE NOWAIT`), personID).Scan(&id), "preparation cannot own the person before identity")
	require.NoError(blocker.Rollback())
	require.NoError(<-done)
}
