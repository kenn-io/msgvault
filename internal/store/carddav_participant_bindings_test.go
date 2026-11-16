package store_test

import (
	"strconv"
	"testing"

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
	t.Run("preview is read only", func(t *testing.T) {
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
		installBindingWriteFailure(t, st, "archive_metadata", "UPDATE")
		preview, err := st.ReconcileCardDAVParticipantBindingsContext(t.Context(), store.CardDAVParticipantBindingOptions{DryRun: true})
		require.NoError(err)
		assert.Equal(1, preview.CardsScanned)
		assert.Equal(1, preview.ParticipantsBound)
		assert.Zero(preview.Conflicts)
		bound, err := st.PersonForParticipantsContext(t.Context(), []int64{participant})
		require.NoError(err)
		assert.Nil(bound)
		current, err := st.GetPersonContext(t.Context(), person.ID)
		require.NoError(err)
		assert.Equal(person.Revision, current.Revision)
		revision, err := st.IdentityRevision()
		require.NoError(err)
		assert.Equal(before, revision)
	})
	t.Run("undo", func(t *testing.T) {
		assert := Assert.New(t)
		require := Require.New(t)
		st, account, book := newCardDAVResourceStore(t)
		participant, err := st.EnsureParticipant("alex@example.test", "Alex Example", "example.test")
		require.NoError(err)
		card := remoteResource(book.CanonicalURL+"alex.vcf", "undo-alex", "Alex Example", "alex@example.test", "one")
		applyParticipantBindingCards(t, st, account, book, card)
		require.True(bindingOwned(t, st, participant))
		preview, err := st.ReconcileCardDAVParticipantBindingsContext(t.Context(), store.CardDAVParticipantBindingOptions{DryRun: true, Undo: true})
		require.NoError(err)
		assert.Equal(1, preview.ParticipantsUnbound)
		assert.Equal([]int64{participant}, bindingCardPerson(t, st, book, card.Href).ParticipantIDs)
		result, err := st.ReconcileCardDAVParticipantBindingsContext(t.Context(), store.CardDAVParticipantBindingOptions{Undo: true})
		require.NoError(err)
		assert.Equal(1, result.ParticipantsUnbound)
		assert.Empty(bindingCardPerson(t, st, book, card.Href).ParticipantIDs)
		applyParticipantBindingCards(t, st, account, book)
		assert.Equal([]int64{participant}, bindingCardPerson(t, st, book, card.Href).ParticipantIDs)
	})
	t.Run("later arrival", func(t *testing.T) {
		assert := Assert.New(t)
		require := Require.New(t)
		st, account, book := newCardDAVResourceStore(t)
		card := remoteResource(book.CanonicalURL+"alex.vcf", "later-alex", "Alex Example", "alex@example.test", "one")
		applyParticipantBindingCards(t, st, account, book, card)
		participant, err := st.EnsureParticipant("alex@example.test", "Alex Example", "example.test")
		require.NoError(err)
		applyParticipantBindingCards(t, st, account, book)
		assert.Equal([]int64{participant}, bindingCardPerson(t, st, book, card.Href).ParticipantIDs)
		assert.True(bindingOwned(t, st, participant))
	})
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
	t.Run("manual alias after binding", func(t *testing.T) {
		assert := Assert.New(t)
		require := Require.New(t)
		st, account, book := newCardDAVResourceStore(t)
		primary, err := st.EnsureParticipant("shared@example.test", "Primary", "example.test")
		require.NoError(err)
		alias, err := st.EnsureParticipant("other@example.test", "Other", "example.test")
		require.NoError(err)
		card := remoteResource(book.CanonicalURL+"order.vcf", "order", "Card", "shared@example.test", "one")
		applyParticipantBindingCards(t, st, account, book, card)
		person := bindingCardPerson(t, st, book, card.Href)
		require.Equal([]int64{primary}, person.ParticipantIDs)
		require.True(bindingOwned(t, st, primary))
		owner, _, err := st.CreatePersonFromParticipant(alias)
		require.NoError(err)
		require.NoError(st.SetParticipantIdentifier(alias, "email", "SHARED@example.test"))
		card.RemoteETag, card.SemanticHash = "two", "two"
		applyParticipantBindingCards(t, st, account, book, card)
		result, err := st.ReconcileCardDAVParticipantBindingsContext(t.Context(), store.CardDAVParticipantBindingOptions{})
		require.NoError(err)
		assert.ElementsMatch([]store.CardDAVParticipantBindingConflict{
			{ParticipantID: primary, PersonIDs: []int64{person.ID, owner.ID}, Reason: "existing_person"},
			{ParticipantID: alias, PersonIDs: []int64{person.ID, owner.ID}, Reason: "existing_person"},
		}, result.ConflictDetails)
		bound, err := st.PersonForParticipantsContext(t.Context(), []int64{primary})
		require.NoError(err)
		assert.Nil(bound)
		owner, err = st.GetPersonContext(t.Context(), owner.ID)
		require.NoError(err)
		assert.Equal([]int64{alias}, owner.ParticipantIDs)
	})
	t.Run("other address holders", func(t *testing.T) {
		for _, kind := range []string{"phone", "email aliases"} {
			t.Run(kind, func(t *testing.T) {
				assert := Assert.New(t)
				require := Require.New(t)
				st, account, book := newCardDAVResourceStore(t)
				var ownerID, otherID int64
				var err error
				card := remoteResource(book.CanonicalURL+"shared-holder.vcf", "shared-holder", "Shared", "shared@example.test", "one")
				if kind == "phone" {
					old, err := st.EnsureParticipantByPhone(t.Context(), "+12025550101", "Old", "imessage")
					require.NoError(err)
					ownerID, err = st.EnsureParticipantByPhone(t.Context(), "+12025550102", "Owner", "whatsapp")
					require.NoError(err)
					require.NoError(st.MergeParticipants(old, ownerID))
					otherID, err = st.EnsureParticipantByPhone(t.Context(), "+12025550101", "Other", "imessage")
					require.NoError(err)
					card.RemoteBody = []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:shared-holder\r\nFN:Shared\r\nTEL:+12025550101\r\nEND:VCARD\r\n")
					card.Emails = nil
					card.Phones = []string{"+12025550101"}
				} else {
					ownerID, err = st.EnsureParticipant("owner@example.test", "Owner", "example.test")
					require.NoError(err)
					otherID, err = st.EnsureParticipant("other@example.test", "Other", "example.test")
					require.NoError(err)
					require.NoError(st.SetParticipantIdentifier(ownerID, "imessage", "shared@example.test"))
					require.NoError(st.SetParticipantIdentifier(otherID, "email", "shared@example.test"))
				}
				require.NotEqual(ownerID, otherID)
				owner, _, err := st.CreatePersonFromParticipant(ownerID)
				require.NoError(err)
				applyParticipantBindingCards(t, st, account, book, card)
				person := bindingCardPerson(t, st, book, card.Href)
				result, err := st.ReconcileCardDAVParticipantBindingsContext(t.Context(), store.CardDAVParticipantBindingOptions{})
				require.NoError(err)
				require.NotEmpty(result.ConflictDetails)
				assert.Equal("existing_person", result.ConflictDetails[0].Reason)
				assert.ElementsMatch([]int64{person.ID, owner.ID}, result.ConflictDetails[0].PersonIDs)
				other, err := st.PersonForParticipantsContext(t.Context(), []int64{otherID})
				require.NoError(err)
				assert.Nil(other)
			})
		}
	})
}

func TestCardDAVParticipantBindingPrimaryAndAliasHoldersBindIndependently(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	st, account, book := newCardDAVResourceStore(t)
	absorbed, err := st.EnsureParticipant("absorbed@example.test", "Absorbed", "example.test")
	require.NoError(err)
	third, err := st.EnsureParticipant("third@example.test", "Third", "example.test")
	require.NoError(err)
	require.NoError(st.SetParticipantIdentifier(third, "email", "absorbed@example.test"))
	card := remoteResource(book.CanonicalURL+"collision.vcf", "email-collision", "Absorbed", "ABSORBED@example.test", "one")
	applyParticipantBindingCards(t, st, account, book, card)
	person := bindingCardPerson(t, st, book, card.Href)
	assert.ElementsMatch([]int64{absorbed, third}, person.ParticipantIDs)
	var links int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM participant_links`).Scan(&links))
	assert.Zero(links)
}

func TestCardDAVParticipantBindingSharedCardsWithdrawAutomaticOwnership(t *testing.T) {
	for _, test := range []struct {
		name       string
		unreadable bool
	}{{"forward", false}, {"unreadable phone", true}} {
		t.Run(test.name, func(t *testing.T) {
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
			if test.unreadable {
				second.RemoteBody = []byte("BEGIN:VCARD\r\nVERSION:2.1\r\nUID:shared-second\r\nFN:Second Example\r\nTEL;CHARSET=NO-SUCH-CHARSET:123\r\nEMAIL:shared@example.test\r\nEND:VCARD\r\n")
			}
			applyParticipantBindingCards(t, st, account, book, first, second)
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
	for _, tc := range []struct {
		raw, phone string
		bound      bool
	}{
		{"202/555-0100", "+12025550100", false},
		{"+1 202/555-0100", "+12025550100", true},
		{"0012025550100", "+12025550100", true},
		{"12025550100", "+12025550100", false},
		{"20 7946 0958", "+12079460958", false},
		{"+202-555-0100 ext 99", "+202555010099", false},
		{"+letters12025550100", "+12025550100", false},
		{"tel:+12025550100;ext=99", "+12025550100", false},
		{"tel:+12025550100;phone-context=example.test", "+12025550100", false},
		{"+1\u00a0202-555-0100", "+12025550100", true},
		{"tel:555-0100;phone-context=+1-202", "+12025550100", true},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			st, account, book := newCardDAVResourceStore(t)
			id, err := st.EnsureParticipantByPhone(t.Context(), tc.phone, "Phone Example", "phone")
			require.NoError(err)
			card := remoteResource(book.CanonicalURL+"phone.vcf", "phone-card", "Phone Example", "unmatched@example.test", "one")
			card.RemoteBody = []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:phone-card\r\nFN:Phone Example\r\nTEL:" + tc.raw + "\r\nEND:VCARD\r\n")
			card.Phones, card.Emails = []string{tc.raw}, nil
			applyParticipantBindingCards(t, st, account, book, card)
			person, err := st.PersonForParticipantsContext(t.Context(), []int64{id})
			require.NoError(err)
			if tc.bound {
				require.NotNil(person)
				assert.Equal(bindingCardPerson(t, st, book, card.Href).ID, person.ID)
			} else {
				assert.Nil(person)
			}
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

func TestCardDAVParticipantBindingEmailIdentifiersAndEncoding(t *testing.T) {
	for _, tc := range []struct {
		name, kind, value, email, property string
	}{
		{"legacy encoding", "email", "josé@example.test", "josé@example.test", "EMAIL;CHARSET=ISO-8859-1;ENCODING=QUOTED-PRINTABLE:jos=E9@example.test"},
		{"imessage alias", "imessage", "Alias@Example.test", "alias@example.test", "EMAIL:alias@example.test"},
		{"synctech", "synctech_sms", "Alex@Example.Test", "alex@example.test", "EMAIL:alex@example.test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			st, account, book := newCardDAVResourceStore(t)
			var id int64
			var err error
			switch tc.kind {
			case "email":
				id, err = st.EnsureParticipant(tc.value, "Alex", "example.test")
			case "imessage":
				id, err = st.EnsureParticipantByPhone(t.Context(), "+12025550100", "Alex", "imessage")
				require.NoError(err)
				err = st.SetParticipantIdentifier(id, "imessage", tc.value)
			default:
				id, err = st.EnsureParticipantByIdentifier(tc.kind, tc.value, "Alex")
			}
			require.NoError(err)
			card := remoteResource(book.CanonicalURL+"email.vcf", "email-card", "Alex", tc.email, "one")
			card.RemoteBody = []byte("BEGIN:VCARD\r\nVERSION:2.1\r\nUID:email-card\r\nFN:Alex\r\n" + tc.property + "\r\nEND:VCARD\r\n")
			applyParticipantBindingCards(t, st, account, book, card)
			person, err := st.PersonForParticipantsContext(t.Context(), []int64{id})
			require.NoError(err)
			require.NotNil(person)
			assert.Equal(bindingCardPerson(t, st, book, card.Href).ID, person.ID)
		})
	}
}

func TestCardDAVParticipantBindingCandidateOwnership(t *testing.T) {
	t.Run("automatic membership", func(t *testing.T) {
		for _, mode := range []string{"system", "user", "stale-system", "null-actor"} {
			t.Run(mode, func(t *testing.T) {
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
				switch mode {
				case "system", "user":
					_, _, err = st.AcceptIdentityMatchCandidateContext(t.Context(), candidate.ID, mode, nil)
					require.NoError(err)
				case "stale-system":
					stale, err := st.DecideIdentityMatchCandidateContext(t.Context(), candidate.ID, store.IdentityMatchStateAccepted, "system", nil)
					require.NoError(err)
					_, err = st.DecideIdentityMatchCandidateContext(t.Context(), candidate.ID, store.IdentityMatchStateAccepted, "user", nil)
					require.NoError(err)
					require.NoError(st.ApplyAcceptedIdentityMatchSnapshotForTest(t.Context(), stale, "system"))
				case "null-actor":
					_, err = st.DecideIdentityMatchCandidateContext(t.Context(), candidate.ID, store.IdentityMatchStateAccepted, "system", nil)
					require.NoError(err)
					_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE identity_match_candidates SET decided_by = NULL, decided_at = NULL WHERE id = ?`), candidate.ID)
					require.NoError(err)
					_, _, linked, err := st.ResumeAcceptedIdentityMatchCandidateContext(t.Context(), candidate.ID)
					require.NoError(err)
					assert.True(linked)
				}
				automatic := mode == "system" || mode == "null-actor"
				assert.Equal(automatic, bindingOwned(t, st, a))
				assert.Equal(automatic, bindingOwned(t, st, b))
			})
		}
	})
	t.Run("mixed membership", func(t *testing.T) {
		for _, actor := range []string{"system", "user"} {
			t.Run(actor, func(t *testing.T) {
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
				_, _, err = st.AcceptIdentityMatchCandidateContext(t.Context(), candidate.ID, actor, nil)
				require.NoError(err)
				assert.False(bindingOwned(t, st, a))
				assert.Equal(actor == "system", bindingOwned(t, st, b))

				result, err := st.ReconcileCardDAVParticipantBindingsContext(t.Context(), store.CardDAVParticipantBindingOptions{Undo: true})
				require.NoError(err)
				if actor == "system" {
					assert.Equal(1, result.ParticipantsUnbound)
					assert.Equal([]int64{a}, bindingCardPerson(t, st, book, card.Href).ParticipantIDs)
				} else {
					assert.Zero(result.ParticipantsUnbound)
					assert.ElementsMatch([]int64{a, b}, bindingCardPerson(t, st, book, card.Href).ParticipantIDs)
				}
			})
		}
	})
}

func TestCardDAVParticipantBindingMergedAliasRemainsClaimed(t *testing.T) {
	t.Run("email", func(t *testing.T) {
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
		card = remoteResource(card.Href, card.RemoteUID, "Remote Updated", "alex@example.test", "two")
		card.SemanticHash = "updated-baseline"
		applyParticipantBindingCards(t, st, account, book, card)
		assert.Equal("Remote Updated", *bindingCardPerson(t, st, book, card.Href).DisplayName)

		undo, err := st.ReconcileCardDAVParticipantBindingsContext(t.Context(), store.CardDAVParticipantBindingOptions{Undo: true})
		require.NoError(err)
		assert.Equal(1, undo.ParticipantsUnbound)
		assert.Empty(bindingCardPerson(t, st, book, card.Href).ParticipantIDs)
	})
	t.Run("phone", func(t *testing.T) {
		for _, identifierType := range []string{"imessage", "google_voice", "imazing_csv", "beeper", "chatwoot"} {
			t.Run(identifierType, func(t *testing.T) {
				assert := Assert.New(t)
				require := Require.New(t)
				st, account, book := newCardDAVResourceStore(t)
				absorbed, err := st.EnsureParticipantByPhone(t.Context(), "+12025550101", "Alex Example", identifierType)
				require.NoError(err)
				survivor, err := st.EnsureParticipantByPhone(t.Context(), "+12025550102", "Alex Example", "beeper")
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
	})
	t.Run("scoped beeper identifier", func(t *testing.T) {
		require := Require.New(t)
		st, account, book := newCardDAVResourceStore(t)
		_, err := st.EnsureParticipantByIdentifier("beeper", "+12025550101", "Alex Example")
		require.NoError(err)
		require.NoError(st.ClassifyParticipantIdentifierServiceContext(t.Context(), "beeper", "+12025550101", nil, new("account"), new("example")))
		card := remoteResource(book.CanonicalURL+"scoped.vcf", "scoped-card", "Alex Example", "unmatched@example.test", "one")
		card.RemoteBody = []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:scoped-card\r\nFN:Alex Example\r\nTEL:+12025550101\r\nEND:VCARD\r\n")
		card.Emails = nil
		card.Phones = []string{"+12025550101"}
		applyParticipantBindingCards(t, st, account, book, card)
		require.Empty(bindingCardPerson(t, st, book, card.Href).ParticipantIDs)
	})
}

func TestCardDAVParticipantBindingLifecycleCleanup(t *testing.T) {
	for _, test := range []struct {
		action  string
		tracked bool
	}{{"delete", false}, {"demote", true}, {"discovery", true}, {"conflict-edit", true}, {"publication-commit", true}} {
		t.Run(test.action+"/tracked="+strconv.FormatBool(test.tracked), func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			st, account, book := newCardDAVResourceStore(t)
			id, err := st.EnsureParticipant("alex@example.test", "Alex Example", "example.test")
			require.NoError(err)
			card := remoteResource(book.CanonicalURL+"lifecycle.vcf", "lifecycle-card", "Alex Example", "alex@example.test", "one")
			applyParticipantBindingCards(t, st, account, book, card)
			require.True(bindingOwned(t, st, id))
			person := bindingCardPerson(t, st, book, card.Href)
			if test.tracked {
				_, err = st.SetPersonTrackingContext(t.Context(), person.ID, true)
				require.NoError(err)
			}
			before, err := st.IdentityRevision()
			require.NoError(err)
			switch test.action {
			case "delete":
				books, readErr := st.ListCardDAVAddressBooksContext(t.Context(), store.AllCardDAVAccounts)
				require.NoError(readErr)
				_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration, SyncRevision: books[0].SyncRevision, RemovedHrefs: []string{card.Href}})
			case "demote":
				err = st.SetCardDAVBookRolesContext(t.Context(), book.ID, store.CardDAVBookRoles{IsLookupSource: true})
			case "discovery":
				allowed := true
				_, _, err = st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{BaseURL: "https://contacts.example/dav", Username: "alice", PrincipalURL: "https://contacts.example/principal/alice/", HomeURL: "https://contacts.example/books/alice/", Books: []store.CardDAVDiscoveredBook{{CanonicalURL: "https://contacts.example/books/alice/replacement/", DisplayName: "Replacement", CanCreate: &allowed}}})
			case "publication-commit":
				snapshot, readErr := st.LoadPersonVCardSnapshotContext(t.Context(), person.ID)
				require.NoError(readErr)
				card.RemoteBody = []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:lifecycle-card\r\nFN:Alex Example\r\nEND:VCARD\r\n")
				card.Emails = nil
				card.RemoteETag, card.SemanticHash = "two", "changed"
				pending, readErr := st.PrepareCardDAVPublicationContext(t.Context(), store.CardDAVPublicationPlan{PersonID: person.ID, Desired: true, AddressBookID: book.ID, Href: card.Href, OutgoingBody: card.RemoteBody, OutgoingSemanticHash: card.SemanticHash, LocalHash: snapshot.Fingerprint})
				require.NoError(readErr)
				err = st.CommitCardDAVPublicationContext(t.Context(), store.CardDAVCanonicalMutation{Publication: *pending, Remote: card})
			default:
				resource, readErr := st.GetCardDAVResourceContext(t.Context(), book.ID, card.Href)
				require.NoError(readErr)
				capture := store.CardDAVConflictCapture{AddressBookID: book.ID, Href: card.Href, ExpectedPersonID: *resource.PersonID, ExpectedMappingRevision: resource.MappingRevision, BaseLocalHash: resource.LocalHash, LocalHash: resource.LocalHash, BaseRemoteHash: resource.RemoteSemanticHash, BaseRemoteETag: resource.RemoteETag, RemoteETag: "two", LocalBody: card.RemoteBody, RemoteBody: []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Changed Example\r\nEND:VCARD\r\n")}
				_, err = st.RecordCardDAVConflictContext(t.Context(), capture)
			}
			require.NoError(err)
			bound, err := st.PersonForParticipantsContext(t.Context(), []int64{id})
			require.NoError(err)
			assert.Nil(bound)
			if test.tracked {
				_, err = st.GetPersonContext(t.Context(), person.ID)
				assert.NoError(err)
			} else {
				after, err := st.IdentityRevision()
				require.NoError(err)
				assert.Greater(after, before)
				_, err = st.GetPersonContext(t.Context(), person.ID)
				assert.ErrorIs(err, store.ErrPersonNotFound)
			}
		})
	}
}

func TestCardDAVParticipantBindingActiveMergeAllowsIndependentMembership(t *testing.T) {
	for _, mode := range []string{"independent", "system link", "confirm system link", "confirm bound system link"} {
		t.Run(mode, func(t *testing.T) {
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
			undo, err := st.ReconcileCardDAVParticipantBindingsContext(t.Context(), store.CardDAVParticipantBindingOptions{Undo: true})
			require.NoError(err)
			assert.Zero(undo.ParticipantsUnbound)

			late, err := st.EnsureParticipant("late@example.test", "Late Example", "example.test")
			require.NoError(err)
			preview, err := st.ReconcileCardDAVParticipantBindingsContext(t.Context(), store.CardDAVParticipantBindingOptions{DryRun: true})
			require.NoError(err)
			assert.Empty(preview.ConflictDetails)
			assert.Zero(preview.Conflicts)
			assert.Equal(1, preview.ParticipantsBound)
			assert.Zero(preview.ParticipantsUnbound)

			result, err := st.ReconcileCardDAVParticipantBindingsContext(t.Context(), store.CardDAVParticipantBindingOptions{})
			require.NoError(err)
			assert.Equal(preview, result)
			assert.True(bindingOwned(t, st, late))
			current, err := st.GetPersonContext(t.Context(), merged.Person.ID)
			require.NoError(err)
			assert.ElementsMatch([]int64{a, b, late}, current.ParticipantIDs)
			var lineageCount int
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM person_merge_participants WHERE participant_id = ?`), late).Scan(&lineageCount))
			assert.Zero(lineageCount, "automatic bindings must not inherit merge lineage")
			noop, err := st.ReconcileCardDAVParticipantBindingsContext(t.Context(), store.CardDAVParticipantBindingOptions{DryRun: true})
			require.NoError(err)
			assert.Zero(noop.Conflicts)
			assert.Zero(noop.ParticipantsBound)

			if mode != "independent" {
				candidate, _, err := st.UpsertIdentityMatchCandidateContext(t.Context(), store.IdentityMatchCandidateInput{LeftKind: store.IdentityMatchParticipant, LeftID: b, RightKind: store.IdentityMatchParticipant, RightID: late, Basis: store.IdentityMatchStableProviderID, NormalizedValue: new("synthetic-provider-id"), State: store.IdentityMatchStateCandidate, Source: store.ProvenanceArchiveObservation})
				require.NoError(err)
				_, _, err = st.AcceptIdentityMatchCandidateContext(t.Context(), candidate.ID, "system", nil)
				require.NoError(err)
				if mode != "confirm bound system link" {
					_, err = st.ReconcileCardDAVParticipantBindingsContext(t.Context(), store.CardDAVParticipantBindingOptions{Undo: true})
					require.NoError(err)
				}
				current, err = st.GetPersonContext(t.Context(), merged.Person.ID)
				require.NoError(err)
				if mode != "confirm bound system link" {
					assert.ElementsMatch([]int64{a, b}, current.ParticipantIDs)
				}
				if mode == "confirm system link" || mode == "confirm bound system link" {
					before := current.Revision
					identityBefore, err := st.IdentityRevision()
					require.NoError(err)
					_, err = st.LinkParticipants(b, late)
					require.NoError(err)
					current, err = st.GetPersonContext(t.Context(), merged.Person.ID)
					require.NoError(err)
					assert.ElementsMatch([]int64{a, b, late}, current.ParticipantIDs)
					if mode == "confirm system link" {
						assert.Greater(current.Revision, before)
						identityAfter, err := st.IdentityRevision()
						require.NoError(err)
						assert.Greater(identityAfter, identityBefore)
					}
				}
			}

			participants := []int64{b}
			if mode == "confirm system link" || mode == "confirm bound system link" {
				participants = append(participants, late)
			}
			split, err := st.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
				SourcePersonID: merged.Person.ID, MergeID: merged.Merge.ID, ParticipantIDs: participants,
				ExpectedSourceRevision: current.Revision, IdempotencyKey: "carddav-active-merge-split", Actor: "test",
			})
			require.NoError(err)
			if mode == "independent" {
				assert.False(bindingOwned(t, st, a))
				assert.False(bindingOwned(t, st, b))
				result, err = st.ReconcileCardDAVParticipantBindingsContext(t.Context(), store.CardDAVParticipantBindingOptions{})
				require.NoError(err)
				assert.Zero(result.Conflicts)
				assert.Zero(result.ParticipantsBound)
				latePerson, err := st.PersonForParticipantsContext(t.Context(), []int64{late})
				require.NoError(err)
				require.NotNil(latePerson)
				assert.Equal(split.SourcePerson.ID, latePerson.ID)
				assert.True(bindingOwned(t, st, late))
				absorbedPerson, err := st.PersonForParticipantsContext(t.Context(), []int64{b})
				require.NoError(err)
				require.NotNil(absorbedPerson)
				assert.Equal(split.NewPerson.ID, absorbedPerson.ID)
				return
			}
			assert.Equal([]int64{a}, split.SourcePerson.ParticipantIDs)
			assert.ElementsMatch(participants, split.NewPerson.ParticipantIDs)
			if mode == "system link" {
				unbound, err := st.PersonForParticipantsContext(t.Context(), []int64{late})
				require.NoError(err)
				assert.Nil(unbound)
			}
		})
	}
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
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE archive_metadata SET value = ? WHERE key = 'schema_version'`), strconv.Itoa(store.SchemaVersion-1))
	require.NoError(err)
	require.NoError(st.InitSchemaContext(t.Context()))
	assert.False(bindingOwned(t, st, id))
	current, err := st.GetPersonContext(t.Context(), person.ID)
	require.NoError(err)
	assert.Equal([]int64{id}, current.ParticipantIDs)
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

func TestCardDAVParticipantBindingCreateCollision(t *testing.T) {
	for _, scenario := range []string{"provisional collision", "recorded conflict"} {
		t.Run(scenario, func(t *testing.T) {
			require := Require.New(t)
			assert := Assert.New(t)
			st, account, book := newCardDAVResourceStore(t)
			id, err := st.EnsureParticipant("shared@example.test", "Shared", "example.test")
			require.NoError(err)
			if scenario == "recorded conflict" {
				card := remoteResource(book.CanonicalURL+"owner.vcf", "owner", "Owner", "shared@example.test", "one")
				applyParticipantBindingCards(t, st, account, book, card)
				owner := bindingCardPerson(t, st, book, card.Href)
				require.Equal([]int64{id}, owner.ParticipantIDs)
			}
			localID, err := st.EnsureParticipant("local@example.test", "Local", "example.test")
			require.NoError(err)
			localPerson, _, err := st.CreatePersonFromParticipant(localID)
			require.NoError(err)
			snapshot, err := st.LoadPersonVCardSnapshotContext(t.Context(), localPerson.ID)
			require.NoError(err)
			local := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:local\r\nFN:Local\r\nEMAIL:local@example.test\r\nEND:VCARD\r\n")
			pending, err := st.PrepareCardDAVPublicationContext(t.Context(), store.CardDAVPublicationPlan{PersonID: localPerson.ID, Desired: true, AddressBookID: book.ID, Href: book.CanonicalURL + "collision.vcf", OutgoingBody: local, OutgoingSemanticHash: "local", LocalHash: snapshot.Fingerprint})
			require.NoError(err)
			remote := remoteResource(pending.Href, "collision", "Remote", "shared@example.test", "remote")
			fenced, err := st.FenceCardDAVCreateCollisionContext(t.Context(), *pending, remote)
			require.NoError(err)
			if scenario == "provisional collision" {
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
				return
			}
			mapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, remote.Href)
			require.NoError(err)
			conflict, err := st.RecordCardDAVPublicationConflictRetainingIntentContext(t.Context(), *fenced, store.CardDAVConflictCapture{
				AddressBookID: book.ID, Href: remote.Href, ExpectedMappingRevision: mapping.MappingRevision,
				BaseLocalHash: mapping.LocalHash, LocalHash: fenced.LocalHash,
				BaseRemoteHash: mapping.RemoteSemanticHash, BaseRemoteETag: mapping.RemoteETag,
				LocalBody: local, RemoteETag: remote.RemoteETag, RemoteBody: remote.RemoteBody,
			})
			require.NoError(err)
			require.NotNil(conflict)
			require.Equal(store.CardDAVConflictUnresolved, conflict.Status)
			publication, err := st.GetCardDAVPublicationContext(t.Context(), localPerson.ID)
			require.NoError(err)
			require.Empty(publication.PendingOperation)
			bound, err := st.PersonForParticipantsContext(t.Context(), []int64{id})
			require.NoError(err)
			assert.Nil(bound)
		})
	}
}

func TestCardDAVParticipantBindingMovedAddressRebinds(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	st, account, book := newCardDAVResourceStore(t)
	id, err := st.EnsureParticipant("alex@example.test", "Alex", "example.test")
	require.NoError(err)
	a := remoteResource(book.CanonicalURL+"a.vcf", "card-a", "A", "alex@example.test", "one")
	b := remoteResource(book.CanonicalURL+"b.vcf", "card-b", "B", "other@example.test", "one")
	applyParticipantBindingCards(t, st, account, book, a, b)
	a = remoteResource(a.Href, "card-a", "A", "other-a@example.test", "two")
	b = remoteResource(b.Href, "card-b", "B", "alex@example.test", "two")
	a.SemanticHash, b.SemanticHash = "changed-a", "changed-b"
	applyParticipantBindingCards(t, st, account, book, a, b)
	assert.Empty(bindingCardPerson(t, st, book, a.Href).ParticipantIDs)
	assert.Equal([]int64{id}, bindingCardPerson(t, st, book, b.Href).ParticipantIDs)
	result, err := st.ReconcileCardDAVParticipantBindingsContext(t.Context(), store.CardDAVParticipantBindingOptions{DryRun: true})
	require.NoError(err)
	assert.Zero(result.Conflicts)
}

func TestCardDAVParticipantBindingSkipsUnreadableCard(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	st, account, book := newCardDAVResourceStore(t)
	goodID, err := st.EnsureParticipant("good@example.test", "Good", "example.test")
	require.NoError(err)
	badID, err := st.EnsureParticipant("bad@example.test", "Bad", "example.test")
	require.NoError(err)
	good := remoteResource(book.CanonicalURL+"good.vcf", "good", "Good", "good@example.test", "one")
	bad := remoteResource(book.CanonicalURL+"bad.vcf", "bad", "Bad", "bad@example.test", "one")
	bad.RemoteBody = []byte("BEGIN:VCARD\r\nVERSION:2.1\r\nUID:bad\r\nFN:Bad\r\nEMAIL;CHARSET=NO-SUCH-CHARSET:bad@example.test\r\nEND:VCARD\r\n")
	applyParticipantBindingCards(t, st, account, book, bad, good)
	assert.Equal([]int64{goodID}, bindingCardPerson(t, st, book, good.Href).ParticipantIDs)
	bound, err := st.PersonForParticipantsContext(t.Context(), []int64{badID})
	require.NoError(err)
	assert.Nil(bound)
	preview, err := st.ReconcileCardDAVParticipantBindingsContext(t.Context(), store.CardDAVParticipantBindingOptions{DryRun: true})
	require.NoError(err)
	assert.Equal(2, preview.CardsScanned)
}

func TestCardDAVParticipantBindingReconcilesReadableEmailWithUnreadablePhone(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	st, account, book := newCardDAVResourceStore(t)
	alex, err := st.EnsureParticipant("alex@example.test", "Alex", "example.test")
	require.NoError(err)
	other, err := st.EnsureParticipant("other@example.test", "Other", "example.test")
	require.NoError(err)
	phone, err := st.EnsureParticipantByPhone(t.Context(), "+12025550100", "Alex", "imessage")
	require.NoError(err)
	card := remoteResource(book.CanonicalURL+"alex.vcf", "alex", "Alex", "alex@example.test", "one")
	card.RemoteBody = []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:alex\r\nFN:Alex\r\nEMAIL:alex@example.test\r\nTEL:+12025550100\r\nEND:VCARD\r\n")
	applyParticipantBindingCards(t, st, account, book, card)
	require.ElementsMatch([]int64{alex, phone}, bindingCardPerson(t, st, book, card.Href).ParticipantIDs)

	card = remoteResource(card.Href, card.RemoteUID, "Alex Updated", "other@example.test", "two")
	card.SemanticHash = "updated"
	card.RemoteBody = []byte("BEGIN:VCARD\r\nVERSION:2.1\r\nUID:alex\r\nFN:Alex Updated\r\nEMAIL:other@example.test\r\nTEL;CHARSET=NO-SUCH-CHARSET:123\r\nEND:VCARD\r\n")
	applyParticipantBindingCards(t, st, account, book, card)
	updated := bindingCardPerson(t, st, book, card.Href)
	assert.ElementsMatch([]int64{other, phone}, updated.ParticipantIDs)
	require.NotNil(updated.DisplayName)
	assert.Equal("Alex Updated", *updated.DisplayName)
	person, err := st.PersonForParticipantsContext(t.Context(), []int64{alex})
	require.NoError(err)
	assert.Nil(person)
}

func TestCardDAVParticipantBindingRetainsMatchWhenAddressIsUnreadable(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	st, account, book := newCardDAVResourceStore(t)
	id, err := st.EnsureParticipant("alex@example.test", "Alex", "example.test")
	require.NoError(err)
	card := remoteResource(book.CanonicalURL+"alex.vcf", "alex", "Alex", "alex@example.test", "one")
	applyParticipantBindingCards(t, st, account, book, card)
	person := bindingCardPerson(t, st, book, card.Href)
	require.Equal([]int64{id}, person.ParticipantIDs)
	other, err := st.EnsureParticipant("other@example.test", "Other", "example.test")
	require.NoError(err)
	second := remoteResource(book.CanonicalURL+"other.vcf", "other", "Other", "other@example.test", "one")
	card.RemoteETag, card.SemanticHash = "two", "unreadable"
	card.RemoteBody = []byte("BEGIN:VCARD\r\nVERSION:2.1\r\nUID:alex\r\nFN:Alex\r\nEMAIL;CHARSET=NO-SUCH-CHARSET:alex@example.test\r\nEND:VCARD\r\n")
	applyParticipantBindingCards(t, st, account, book, card, second)
	assert.Equal([]int64{id}, bindingCardPerson(t, st, book, card.Href).ParticipantIDs)
	assert.Equal([]int64{other}, bindingCardPerson(t, st, book, second.Href).ParticipantIDs)
	second = remoteResource(second.Href, second.RemoteUID, "Other", "alex@example.test", "two")
	second.SemanticHash = "competing"
	applyParticipantBindingCards(t, st, account, book, second)
	assert.Equal([]int64{id}, bindingCardPerson(t, st, book, card.Href).ParticipantIDs)
	assert.Empty(bindingCardPerson(t, st, book, second.Href).ParticipantIDs)
	preview, err := st.ReconcileCardDAVParticipantBindingsContext(t.Context(), store.CardDAVParticipantBindingOptions{DryRun: true})
	require.NoError(err)
	require.Len(preview.ConflictDetails, 1)
	assert.Equal("shared_address", preview.ConflictDetails[0].Reason)
	card.RemoteETag, card.SemanticHash = "three", "removed"
	card.RemoteBody = []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:alex\r\nFN:Alex\r\nEND:VCARD\r\n")
	card.Emails = nil
	applyParticipantBindingCards(t, st, account, book, card)
	assert.Empty(bindingCardPerson(t, st, book, card.Href).ParticipantIDs)
}

func recordBindingConflict(t *testing.T, st *store.Store, bookID int64, card store.CardDAVRemoteResource, personID int64, remoteBody []byte) {
	t.Helper()
	resource, err := st.GetCardDAVResourceContext(t.Context(), bookID, card.Href)
	Require.NoError(t, err)
	_, err = st.RecordCardDAVConflictContext(t.Context(), store.CardDAVConflictCapture{
		AddressBookID: bookID, Href: card.Href, ExpectedPersonID: personID,
		ExpectedMappingRevision: resource.MappingRevision, BaseLocalHash: resource.LocalHash,
		LocalHash: resource.LocalHash, BaseRemoteHash: resource.RemoteSemanticHash,
		BaseRemoteETag: resource.RemoteETag, RemoteETag: "two", LocalBody: card.RemoteBody,
		RemoteBody: remoteBody,
	})
	Require.NoError(t, err)
}

func TestCardDAVParticipantBindingDisputedAddresses(t *testing.T) {
	t.Run("shared address", func(t *testing.T) {
		require := Require.New(t)
		assert := Assert.New(t)
		st, account, book := newCardDAVResourceStore(t)
		id, err := st.EnsureParticipant("shared@example.test", "Shared", "example.test")
		require.NoError(err)
		localOnly, err := st.EnsureParticipant("local@example.test", "Local", "example.test")
		require.NoError(err)
		remoteOnly, err := st.EnsureParticipant("remote@example.test", "Remote", "example.test")
		require.NoError(err)
		private, err := st.EnsureParticipant("private@example.test", "Private", "example.test")
		require.NoError(err)
		first := remoteResource(book.CanonicalURL+"first.vcf", "first", "First", "shared@example.test", "one")
		first.Emails = []string{"shared@example.test", "local@example.test", "private@example.test", "new@example.test"}
		first.RemoteBody = []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:first\r\nFN:First\r\nEMAIL:shared@example.test\r\nEMAIL:local@example.test\r\nEMAIL:private@example.test\r\nEMAIL:new@example.test\r\nEND:VCARD\r\n")
		second := remoteResource(book.CanonicalURL+"second.vcf", "second", "Second", "shared@example.test", "one")
		second.Emails = []string{"shared@example.test", "local@example.test", "remote@example.test"}
		second.RemoteBody = []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:second\r\nFN:Second\r\nEMAIL:shared@example.test\r\nEMAIL:local@example.test\r\nEMAIL:remote@example.test\r\nEND:VCARD\r\n")
		applyParticipantBindingCards(t, st, account, book, first, second)
		firstPerson := bindingCardPerson(t, st, book, first.Href)
		secondPerson := bindingCardPerson(t, st, book, second.Href)
		require.NotEqual(firstPerson.ID, secondPerson.ID)
		preview, err := st.ReconcileCardDAVParticipantBindingsContext(t.Context(), store.CardDAVParticipantBindingOptions{DryRun: true})
		require.NoError(err)
		assert.Contains(preview.ConflictDetails, store.CardDAVParticipantBindingConflict{ParticipantID: id, PersonIDs: []int64{firstPerson.ID, secondPerson.ID}, Reason: "shared_address"})
		require.Equal([]int64{private}, firstPerson.ParticipantIDs)
		newParticipant, err := st.EnsureParticipant("new@example.test", "New", "example.test")
		require.NoError(err)
		recordBindingConflict(t, st, book.ID, first, firstPerson.ID, []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:first\r\nFN:Changed Name\r\nEMAIL:shared@example.test\r\nEMAIL:remote@example.test\r\nEMAIL:private@example.test\r\nEMAIL:new@example.test\r\nEND:VCARD\r\n"))
		alias, err := st.EnsureParticipant("alias@example.test", "Alias", "example.test")
		require.NoError(err)
		owner, _, err := st.CreatePersonFromParticipant(alias)
		require.NoError(err)
		require.NoError(st.SetParticipantIdentifier(alias, "email", "private@example.test"))
		result, err := st.ReconcileCardDAVParticipantBindingsContext(t.Context(), store.CardDAVParticipantBindingOptions{})
		require.NoError(err)
		assert.ElementsMatch([]store.CardDAVParticipantBindingConflict{
			{ParticipantID: id, PersonIDs: []int64{firstPerson.ID, secondPerson.ID}, Reason: "shared_address"},
			{ParticipantID: localOnly, PersonIDs: []int64{firstPerson.ID, secondPerson.ID}, Reason: "shared_address"},
			{ParticipantID: remoteOnly, PersonIDs: []int64{firstPerson.ID, secondPerson.ID}, Reason: "shared_address"},
			{ParticipantID: private, PersonIDs: []int64{firstPerson.ID, owner.ID}, Reason: "existing_person"},
		}, result.ConflictDetails)
		bound, err := st.PersonForParticipantsContext(t.Context(), []int64{id})
		require.NoError(err)
		assert.Nil(bound)
		bound, err = st.PersonForParticipantsContext(t.Context(), []int64{newParticipant})
		require.NoError(err)
		assert.Nil(bound)
		bound, err = st.PersonForParticipantsContext(t.Context(), []int64{private})
		require.NoError(err)
		assert.Nil(bound)
		owner, err = st.GetPersonContext(t.Context(), owner.ID)
		require.NoError(err)
		assert.Equal([]int64{alias}, owner.ParticipantIDs)
		for _, participant := range []int64{localOnly, remoteOnly} {
			bound, err = st.PersonForParticipantsContext(t.Context(), []int64{participant})
			require.NoError(err)
			assert.Nil(bound)
		}
	})
	t.Run("linked addresses", func(t *testing.T) {
		require := Require.New(t)
		assert := Assert.New(t)
		st, account, book := newCardDAVResourceStore(t)
		first := remoteResource(book.CanonicalURL+"first.vcf", "first", "First", "first@example.test", "one")
		second := remoteResource(book.CanonicalURL+"second.vcf", "second", "Second", "second@example.test", "one")
		applyParticipantBindingCards(t, st, account, book, first, second)
		firstPerson := bindingCardPerson(t, st, book, first.Href)
		secondPerson := bindingCardPerson(t, st, book, second.Href)
		recordBindingConflict(t, st, book.ID, first, firstPerson.ID, []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:first\r\nFN:Changed Name\r\nEMAIL:first@example.test\r\nEND:VCARD\r\n"))
		firstID, err := st.EnsureParticipant("first@example.test", "Sender", "example.test")
		require.NoError(err)
		secondID, err := st.EnsureParticipant("second@example.test", "Sender", "example.test")
		require.NoError(err)
		_, err = st.LinkParticipants(firstID, secondID)
		require.NoError(err)
		result, err := st.ReconcileCardDAVParticipantBindingsContext(t.Context(), store.CardDAVParticipantBindingOptions{})
		require.NoError(err)
		if assert.Len(result.ConflictDetails, 1) {
			assert.ElementsMatch([]int64{firstPerson.ID, secondPerson.ID}, result.ConflictDetails[0].PersonIDs)
		}
		for _, id := range []int64{firstID, secondID} {
			bound, err := st.PersonForParticipantsContext(t.Context(), []int64{id})
			require.NoError(err)
			assert.Nil(bound)
		}
	})
}
