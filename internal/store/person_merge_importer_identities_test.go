package store_test

import (
	"encoding/json"
	"os"
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
		{"beeper-phone", beeperPhoneMergePeople},
		{"beeper-migrated", func(t *testing.T, st *store.Store) (*store.Person, *store.Person) {
			t.Helper()
			owner := mustPromotedPerson(t.Context(), t, st, "owner@example.org", "Owner")
			participant, _ := migratedBeeperParticipant(t.Context(), t, st)
			guest, _, err := st.CreatePersonFromParticipant(participant)
			require.NoError(t, err)
			return owner, guest
		}},
		{"mailbox-case", mailboxCaseMergePeople},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			require := require.New(t)
			st := storetest.New(t).Store
			ctx := t.Context()
			owner, guest := tc.setup(t, st)
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
			assert.Equal(t, detail.Labels.Addresses, page[0].Labels.Addresses)
			assert.Equal(t, detail.Labels.AbsorbedAddressCount, page[0].Labels.AbsorbedAddressCount)
			if tc.kind == "mailbox-case" {
				assert.Equal(t, 1, detail.Labels.AbsorbedAddressCount)
			}
		})
	}
}

func beeperPhoneMergePeople(t *testing.T, st *store.Store) (*store.Person, *store.Person) {
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
}

func mailboxCaseMergePeople(t *testing.T, st *store.Store) (*store.Person, *store.Person) {
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
}
