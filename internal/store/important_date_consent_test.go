package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestImportantDateConsentIsBoundToDestination(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	st := storetest.New(t).Store
	ctx := t.Context()
	personID := newTestPerson(t, st)
	person, err := st.GetPerson(personID)
	must.NoError(err)
	subject := "person/" + person.VCardUID
	active, err := st.HasImportantDateConsentContext(ctx, subject, "private-example", "owner/agent")
	must.NoError(err)
	checks.False(active)
	must.NoError(st.SetImportantDateConsentContext(ctx, subject, "private-example", "owner/agent", true))
	active, err = st.HasImportantDateConsentContext(ctx, subject, "private-example", "owner/agent")
	must.NoError(err)
	checks.True(active)
	active, err = st.HasImportantDateConsentContext(ctx, subject, "other-project", "owner/agent")
	must.NoError(err)
	checks.False(active)
	active, err = st.HasImportantDateConsentContext(ctx, subject, "private-example", "other-agent")
	must.NoError(err)
	checks.False(active)
	must.NoError(st.SetImportantDateConsentContext(ctx, subject, "private-example", "owner/agent", false))
	active, err = st.HasImportantDateConsentContext(ctx, subject, "private-example", "owner/agent")
	must.NoError(err)
	checks.False(active)
}

func TestImportantDateConsentDispatchRequiresGrant(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	st := storetest.New(t).Store
	ctx := t.Context()
	personID := newTestPerson(t, st)
	person, err := st.GetPerson(personID)
	must.NoError(err)
	subject := "person/" + person.VCardUID
	calls := 0
	dispatch := func(fenced context.Context) error {
		active, err := st.HasImportantDateConsentContext(fenced, subject, "private-example", "owner/agent")
		must.NoError(err)
		checks.True(active)
		calls++
		return nil
	}
	active, err := st.WithImportantDateConsentContext(ctx, subject, subject, "private-example", "owner/agent", dispatch)
	must.NoError(err)
	checks.False(active)
	checks.Zero(calls)
	must.NoError(st.SetImportantDateConsentContext(ctx, subject, "private-example", "owner/agent", true))
	active, err = st.WithImportantDateConsentContext(ctx, subject, subject, "private-example", "owner/agent", dispatch)
	must.NoError(err)
	checks.True(active)
	checks.Equal(1, calls)
}

func TestImportantDateConsentRefusesUnknownSubject(t *testing.T) {
	must := require.New(t)
	st := storetest.New(t).Store
	err := st.SetImportantDateConsentContext(t.Context(), "person/missing-example", "private-example", "owner/agent", true)
	must.Error(err)
}

// PostgreSQL must keep both the subject and its parent locked throughout the
// external-write callback, because relationship and cadence writers update
// those person rows in their transactions.
func TestImportantDateDispatchLocksEligibilityPeoplePostgreSQL(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	st := storetest.New(t).Store
	if !st.IsPostgreSQL() {
		t.Skip("requires PostgreSQL row locks")
	}
	ctx := t.Context()
	makePerson := func(address string) *store.Person {
		id, err := st.EnsureParticipantByIdentifier("email", address, "Example Person")
		must.NoError(err)
		person, _, err := st.CreatePersonFromParticipantContext(ctx, id)
		must.NoError(err)
		return person
	}
	parent := makePerson("parent-lock@example.test")
	child := makePerson("child-lock@example.test")
	_, err := st.AddPersonRelationshipContext(ctx, store.PersonRelationshipInput{SourcePersonID: parent.ID, TargetPersonID: child.ID, TypeSlug: "parent", Source: store.ProvenanceUser, Actor: "test"})
	must.NoError(err)
	subject := "person/" + child.VCardUID
	must.NoError(st.SetImportantDateConsentContext(ctx, subject, "private-example", "owner/agent", true))
	active, err := st.WithImportantDateConsentContext(ctx, subject, "person/"+parent.VCardUID, "private-example", "owner/agent", func(context.Context) error {
		for _, personID := range []int64{child.ID, parent.ID} {
			var id int64
			err := st.DB().QueryRowContext(ctx, `SELECT id FROM persons WHERE id = $1 FOR UPDATE NOWAIT`, personID).Scan(&id)
			must.ErrorContains(err, "could not obtain lock")
		}
		return nil
	})
	must.NoError(err)
	checks.True(active)
}

func TestPersonDateMutationsRevokeImportantDateConsent(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	st := storetest.New(t).Store
	ctx := t.Context()
	personID := newTestPerson(t, st)
	person, err := st.GetPerson(personID)
	must.NoError(err)
	subject := "person/" + person.VCardUID
	must.NoError(st.SetImportantDateConsentContext(ctx, subject, "private-example", "owner/agent", true))
	date, err := st.AddPersonDateContext(ctx, personID, store.PersonDateInput{DateKind: store.PersonDateBirthday, Date: store.PartialDate{Month: new(10), Day: new(23)}, Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
	must.NoError(err)
	active, err := st.HasImportantDateConsentContext(ctx, subject, "private-example", "owner/agent")
	must.NoError(err)
	checks.False(active)
	must.NoError(st.SetImportantDateConsentContext(ctx, subject, "private-example", "owner/agent", true))
	must.NoError(st.SupersedePersonDateContext(ctx, personID, date.Envelope.ID, nil))
	active, err = st.HasImportantDateConsentContext(ctx, subject, "private-example", "owner/agent")
	must.NoError(err)
	checks.False(active)
}
