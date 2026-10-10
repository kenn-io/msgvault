package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestAuthorizedPersonAttributeWritesLockCatalogBeforePerson(t *testing.T) {
	for _, supersede := range []bool{false, true} {
		name := "set"
		if supersede {
			name = "supersede"
		}
		t.Run(name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			st := testutil.NewTestStore(t)
			if !st.IsPostgreSQL() {
				t.Skip("PostgreSQL catalog and row lock ordering")
			}
			participant, err := st.EnsureParticipant("attribute-lock@example.test", "Attribute Lock Example", "example.test")
			requirements.NoError(err)
			person, _, err := st.CreatePersonFromParticipant(participant)
			requirements.NoError(err)
			definition, err := st.CreateAttributeDefinitionContext(t.Context(), personTextDefinition("synthetic_lock"))
			requirements.NoError(err)
			input := store.PersonAttributeValueInput{PersonID: person.ID, DefinitionSlug: definition.Slug, Value: store.AttributeValue{Type: store.AttributeValueText, Text: new("Synthetic lock value")}, Source: store.ProvenanceUser}
			_, err = st.SetPersonAttributeValueContext(t.Context(), input)
			requirements.NoError(err)
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			catalog, err := st.DB().BeginTx(ctx, nil)
			requirements.NoError(err)
			defer func() { _ = catalog.Rollback() }()
			_, err = catalog.ExecContext(ctx, "LOCK TABLE attribute_definitions IN EXCLUSIVE MODE")
			requirements.NoError(err)
			var blockerPID int
			requirements.NoError(catalog.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&blockerPID))
			finished := make(chan error, 1)
			go func() {
				authorize := func(context.Context, *store.IdentityGrantSelection) error { return nil }
				var writeErr error
				if supersede {
					_, writeErr = st.SupersedePersonAttributeValueAuthorizedContext(ctx, store.PersonAttributeSupersedeInput{PersonID: person.ID, DefinitionSlug: definition.Slug}, authorize)
				} else {
					_, writeErr = st.SetPersonAttributeValueAuthorizedContext(ctx, input, authorize)
				}
				finished <- writeErr
			}()
			var observationErr error
			requirements.Eventually(func() bool {
				var waiting bool
				observationErr = st.DB().QueryRowContext(ctx, st.Rebind(`SELECT EXISTS (
 SELECT 1 FROM pg_locks l WHERE l.relation = 'attribute_definitions'::regclass
 AND NOT l.granted AND ? = ANY(pg_blocking_pids(l.pid)))`), blockerPID).Scan(&waiting)
				return observationErr == nil && waiting
			}, 15*time.Second, 10*time.Millisecond)
			requirements.NoError(observationErr)
			// Catalog exposure locks this person after its catalog lock. A blocked
			// attribute writer must leave the row available to that transaction.
			var lockedPerson int64
			err = catalog.QueryRowContext(ctx, st.Rebind("SELECT id FROM persons WHERE id = ? FOR UPDATE NOWAIT"), person.ID).Scan(&lockedPerson)
			assertions.NoError(err)
			if err == nil {
				assertions.Equal(person.ID, lockedPerson)
			}
			requirements.NoError(catalog.Rollback())
			requirements.NoError(<-finished)
		})
	}
}

func TestAuthorizedPersonAttributeWritesUseNativeScopeAndRollback(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	participant, err := st.EnsureParticipant("authorized-attribute@example.test", "Attribute Example", "example.test")
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participant)
	requirements.NoError(err)
	definition, err := st.CreateAttributeDefinitionContext(t.Context(), personTextDefinition("synthetic_preference"))
	requirements.NoError(err)
	seeded, err := st.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{PersonID: person.ID, DefinitionSlug: definition.Slug, Value: store.AttributeValue{Type: store.AttributeValueText, Text: new("Initial synthetic value")}, Source: store.ProvenanceUser})
	requirements.NoError(err)
	requirements.NotNil(seeded.Value)
	allowed := true
	bookURL := "https://contacts.example.test/books/synthetic-attributes/"
	_, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{
		BaseURL: "https://contacts.example.test/dav", Username: "synthetic-attribute-owner", PrincipalURL: "https://contacts.example.test/principal/", HomeURL: "https://contacts.example.test/books/",
		Books: []store.CardDAVDiscoveredBook{{CanonicalURL: bookURL, DisplayName: "Synthetic Attribute Contacts", CanCreate: &allowed, CanUpdate: &allowed, CanDelete: &allowed}},
	})
	requirements.NoError(err)
	requirements.Len(books, 1)
	requirements.NoError(st.SetCardDAVBookRolesContext(t.Context(), books[0].ID, store.CardDAVBookRoles{IsWriteTarget: true, IsSubscribed: true, IsLookupSource: true}))
	local, err := st.LoadPersonVCardSnapshotContext(t.Context(), person.ID)
	requirements.NoError(err)
	_, err = st.PrepareCardDAVPublicationContext(t.Context(), store.CardDAVPublicationPlan{PersonID: person.ID, Desired: true, AddressBookID: books[0].ID, Href: bookURL + person.VCardUID + ".vcf", LocalHash: local.Fingerprint, OutgoingBody: []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:" + person.VCardUID + "\r\nFN:Attribute Example\r\nEND:VCARD\r\n"), OutgoingSemanticHash: "synthetic-attribute-publication"})
	requirements.NoError(err)
	selected, err := st.PersonProfileEditScopeContext(t.Context(), person.ID)
	requirements.NoError(err)
	requirements.Len(selected.AddressBooks, 1)
	assertions.Equal(books[0].ID, selected.AddressBooks[0].BookID)
	assertions.NotEmpty(selected.AddressBooks[0].OwnershipFingerprint)
	before, err := st.GetPerson(person.ID)
	requirements.NoError(err)
	history, err := st.ListPersonAttributeValuesContext(t.Context(), person.ID, store.PersonAttributeQuery{DefinitionSlug: definition.Slug, IncludeHistory: true})
	requirements.NoError(err)
	denied := errors.New("synthetic attribute scope denial")
	calls := 0
	expectedRevision := before.Revision
	authorize := func(_ context.Context, scope *store.IdentityGrantSelection) error {
		calls++
		requirements.NotNil(scope)
		requirements.Len(scope.Persons, 1)
		assertions.Equal(person.ID, scope.Persons[0].ID)
		assertions.Equal(person.VCardUID, scope.Persons[0].UID)
		assertions.Equal(expectedRevision, scope.Persons[0].Revision)
		assertions.Equal(selected.AddressBooks, scope.AddressBooks)
		return denied
	}
	input := store.PersonAttributeValueInput{PersonID: person.ID, DefinitionSlug: definition.Slug, Value: store.AttributeValue{Type: store.AttributeValueText, Text: new("Changed synthetic value")}, Source: store.ProvenanceUser, ExpectedValueID: &seeded.Value.ID}
	for _, dryRun := range []bool{false, true} {
		input.DryRun = dryRun
		_, err := st.SetPersonAttributeValueAuthorizedContext(t.Context(), input, authorize)
		requirements.ErrorIs(err, denied)
		after, err := st.GetPerson(person.ID)
		requirements.NoError(err)
		assertions.Equal(before, after)
		afterHistory, err := st.ListPersonAttributeValuesContext(t.Context(), person.ID, store.PersonAttributeQuery{DefinitionSlug: definition.Slug, IncludeHistory: true})
		requirements.NoError(err)
		assertions.Equal(history, afterHistory)
	}
	assertions.Positive(calls)
	input.DryRun = false
	changed, err := st.SetPersonAttributeValueAuthorizedContext(t.Context(), input, func(context.Context, *store.IdentityGrantSelection) error { return nil })
	requirements.NoError(err)
	requirements.NotNil(changed.Value)
	requirements.NotNil(changed.Superseded)
	assertions.Equal(seeded.Value.ID, changed.Superseded.ID)
	assertions.Equal(new("Changed synthetic value"), changed.Value.Value.Text)
	beforeClear, err := st.GetPerson(person.ID)
	requirements.NoError(err)
	expectedRevision = beforeClear.Revision
	supersedeInput := store.PersonAttributeSupersedeInput{PersonID: person.ID, DefinitionSlug: definition.Slug, ExpectedValueID: &changed.Value.ID}
	_, err = st.SupersedePersonAttributeValueAuthorizedContext(t.Context(), supersedeInput, authorize)
	requirements.ErrorIs(err, denied)
	afterClear, err := st.GetPerson(person.ID)
	requirements.NoError(err)
	assertions.Equal(beforeClear, afterClear)
	current, err := st.ListPersonAttributeValuesContext(t.Context(), person.ID, store.PersonAttributeQuery{DefinitionSlug: definition.Slug})
	requirements.NoError(err)
	requirements.Len(current, 1)
	assertions.Equal(changed.Value.ID, current[0].ID)
	cleared, err := st.SupersedePersonAttributeValueAuthorizedContext(t.Context(), supersedeInput, func(context.Context, *store.IdentityGrantSelection) error { return nil })
	requirements.NoError(err)
	requirements.NotNil(cleared.Superseded)
	assertions.Equal(changed.Value.ID, cleared.Superseded.ID)
	current, err = st.ListPersonAttributeValuesContext(t.Context(), person.ID, store.PersonAttributeQuery{DefinitionSlug: definition.Slug})
	requirements.NoError(err)
	assertions.Empty(current)
}
