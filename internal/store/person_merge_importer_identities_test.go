package store_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// TestWebMergeIdentityFixturesMatchStore keeps the Web History fixtures in sync
// with merge labels returned by the store.
func TestWebMergeIdentityFixturesMatchStore(t *testing.T) {
	for _, tc := range []struct {
		kind  string
		setup func(*testing.T, *store.Store) (*store.Person, *store.Person)
	}{
		{"beeper-phone", func(t *testing.T, st *store.Store) (*store.Person, *store.Person) {
			t.Helper()
			require := require.New(t)
			// Match Beeper's email resolution followed by its stronger phone resolution.
			email, err := st.EnsureParticipant("Reader@Example.org", "Reader", "example.org")
			require.NoError(err)
			phone, err := st.EnsureParticipantByPhone(t.Context(), "+1 (202) 555-0123", "Reader", "beeper")
			require.NoError(err)
			require.NoError(st.MergeParticipantsContext(t.Context(), email, phone))
			require.NoError(st.SetParticipantIdentifierContext(t.Context(), phone, "phone", "+1 (202) 555-0123"))
			guest, _, err := st.CreatePersonFromParticipant(phone)
			require.NoError(err)
			return mustPromotedPerson(t.Context(), t, st, "owner@example.org", "Owner"), guest
		}},
		{"beeper-migrated", func(t *testing.T, st *store.Store) (*store.Person, *store.Person) {
			t.Helper()
			owner := mustPromotedPerson(t.Context(), t, st, "owner@example.org", "Owner")
			participant, _ := migratedBeeperParticipant(t.Context(), t, st)
			guest, _, err := st.CreatePersonFromParticipant(participant)
			require.NoError(t, err)
			return owner, guest
		}},
		{"mailbox-case", func(t *testing.T, st *store.Store) (*store.Person, *store.Person) {
			t.Helper()
			require := require.New(t)
			var participants []int64
			for _, value := range []string{"Reader@Example.org", "reader@example.org"} {
				participant, err := st.EnsureParticipant(value, "Reader", "example.org")
				require.NoError(err)
				participants = append(participants, participant)
			}
			_, err := st.LinkParticipants(participants[0], participants[1])
			require.NoError(err)
			guest, _, err := st.CreatePersonFromParticipant(participants[0])
			require.NoError(err)
			return mustPromotedPerson(t.Context(), t, st, "owner@example.org", "Owner"), guest
		}},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := storetest.New(t).Store
			ctx := t.Context()
			owner, guest := tc.setup(t, st)
			if tc.kind == "beeper-phone" {
				_, err := st.AddPersonContactPointContext(ctx, guest.ID, store.PersonContactPointInput{AddressKind: store.ContactAddressPhone, OriginalValue: "+12025550123", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
				require.NoError(err)
				guest, err = st.GetPersonContext(ctx, guest.ID)
				require.NoError(err)
			}
			fixtureName := tc.kind + "-identities.test-fixture.json"
			merged, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{SurvivorID: owner.ID, AbsorbedID: guest.ID, ExpectedSurvivorRevision: owner.Revision, ExpectedAbsorbedRevision: guest.Revision, IdempotencyKey: tc.kind, Actor: "test"})
			require.NoError(err)
			detail, err := st.GetPersonMergeContext(ctx, merged.Merge.ID)
			require.NoError(err)
			addresses := []store.ParticipantAddress{}
			for _, address := range detail.Labels.Addresses {
				for _, participant := range detail.Participants {
					if participant.OriginSide == "absorbed" && participant.ParticipantID == address.ParticipantID {
						addresses = append(addresses, address)
					}
				}
			}
			data, err := json.Marshal(addresses)
			require.NoError(err)
			fixture, err := os.ReadFile("../../web/src/lib/directory/" + fixtureName)
			require.NoError(err)
			require.JSONEq(string(fixture), string(data))
			page, err := st.ListPersonMergesContext(ctx, owner.ID)
			require.NoError(err)
			require.Len(page, 1)
			assert.Equal(detail.Labels.Addresses, page[0].Labels.Addresses)
			assert.Equal(detail.Labels.AbsorbedAddressCount, page[0].Labels.AbsorbedAddressCount)
			if tc.kind == "mailbox-case" {
				assert.Equal(1, detail.Labels.AbsorbedAddressCount)
			}
			if tc.kind == "beeper-phone" {
				assert.Equal(2, detail.Labels.AbsorbedAddressCount)
				points, err := st.ListPersonContactPointsContext(ctx, owner.ID, true)
				require.NoError(err)
				require.Len(points, 1)
				assert.Equal(addresses[0].IdentityKey, points[0].IdentityKey)
				assert.Equal(addresses[0].IdentityKey, detail.Labels.Profiles[1].ContactPoints[0].IdentityKey)
				require.NoError(st.SetParticipantIdentifierContext(ctx, guest.ParticipantIDs[0], "beeper", "+12025550123"))
				alias, err := st.GetPersonMergeContext(ctx, merged.Merge.ID)
				require.NoError(err)
				assert.Equal(detail.Labels.Addresses, alias.Labels.Addresses)
				service, err := st.ResolveCommunicationServiceContext(ctx, "whatsapp")
				require.NoError(err)
				require.NoError(st.ClassifyParticipantIdentifierServiceContext(ctx, "beeper", "+12025550123", &service.ID, new("account"), new("garden")))
				scoped, err := st.GetPersonMergeContext(ctx, merged.Merge.ID)
				require.NoError(err)
				assert.Equal(3, scoped.Labels.AbsorbedAddressCount)
				assert.Contains(scoped.Labels.Addresses, store.ParticipantAddress{ParticipantID: guest.ParticipantIDs[0], Type: "beeper", Value: "+12025550123", DisplayValue: "+12025550123", NormalizedValue: "+12025550123", ServiceSlug: "whatsapp", ServiceLabel: "WhatsApp", ScopeKind: "account", ScopeValue: "garden", ParticipantDisplayName: "Reader", IdentityKey: `["whatsapp","account","garden","+12025550123"]`})
				require.NoError(st.SetParticipantIdentifierContext(ctx, guest.ParticipantIDs[0], "beeper", "+1 (202) 555-0123"))
				require.NoError(st.ClassifyParticipantIdentifierServiceContext(ctx, "beeper", "+1 (202) 555-0123", &service.ID, new("account"), new("work")))
				scoped, err = st.GetPersonMergeContext(ctx, merged.Merge.ID)
				require.NoError(err)
				assert.Equal(4, scoped.Labels.AbsorbedAddressCount)
			}
			if tc.kind == "beeper-migrated" {
				alias := strings.ToUpper(addresses[0].Value)
				require.NoError(st.SetParticipantIdentifierContext(ctx, guest.ParticipantIDs[0], "beeper", alias))
				service, err := st.ResolveCommunicationServiceContext(ctx, "whatsapp")
				require.NoError(err)
				require.NoError(st.ClassifyParticipantIdentifierServiceContext(ctx, "beeper", alias, &service.ID, new("account"), new("garden-account")))
				opaque, err := st.GetPersonMergeContext(ctx, merged.Merge.ID)
				require.NoError(err)
				assert.Equal(2, opaque.Labels.AbsorbedAddressCount)
			}
		})
	}
}

func TestPersonMergeKnownEmptyAddresses(t *testing.T) {
	f := newPersonMergeInspectionFixture(t, "empty-addresses")
	ctx := t.Context()
	require := require.New(t)
	assert := assert.New(t)
	_, err := f.store.DB().ExecContext(ctx, `DELETE FROM participant_identifiers`)
	require.NoError(err)
	_, err = f.store.DB().ExecContext(ctx, `UPDATE participants SET email_address = NULL, phone_number = NULL`)
	require.NoError(err)
	detail, err := f.store.GetPersonMergeContext(ctx, f.merge.Merge.ID)
	require.NoError(err)
	assert.Len(detail.Labels.ParticipantIDs, 3)
	assert.Empty(detail.Labels.Addresses)
	assert.False(detail.Labels.Incomplete)
	assert.Zero(detail.Labels.AbsorbedAddressCount)
	page, err := f.store.ListPersonMergesContext(ctx, f.person.ID)
	require.NoError(err)
	require.Len(page, 1)
	assert.Equal(detail.Labels.ParticipantIDs, page[0].Labels.ParticipantIDs)
	assert.Empty(page[0].Labels.Addresses)
	assert.False(page[0].Labels.Incomplete)
}
