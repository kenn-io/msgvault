package store_test

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func assertPersonMergeIdentityFixture(t *testing.T, detail *store.PersonMergeDetail, kind string) {
	t.Helper()
	addresses := []store.ParticipantAddress{}
	for _, address := range detail.Labels.Addresses {
		for _, participant := range detail.Participants {
			if participant.OriginSide == "absorbed" && participant.ParticipantID == address.ParticipantID {
				addresses = append(addresses, address)
			}
		}
	}
	data, err := json.Marshal(addresses)
	require.NoError(t, err)
	fixture, err := os.ReadFile("../../web/src/lib/directory/" + kind + "-identities.test-fixture.json")
	require.NoError(t, err)
	require.JSONEq(t, string(fixture), string(data))
}

func beeperPhoneMergePeople(t *testing.T, st *store.Store) (*store.Person, *store.Person) {
	t.Helper()
	require := require.New(t)
	// Model an unscoped legacy archive with a phone identifier stored in its original form.
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
	// Model an unscoped legacy archive whose Beeper identifier is a raw mailbox.
	var participants []int64
	for _, value := range []string{"Reader@Example.org", "reader@example.org"} {
		participant, err := st.EnsureParticipant(value, "Reader", "example.org")
		require.NoError(err)
		participants = append(participants, participant)
	}
	_, err := st.LinkParticipants(participants[0], participants[1])
	require.NoError(err)
	require.NoError(st.SetParticipantIdentifierContext(t.Context(), participants[0], "beeper", "Reader@Example.org"))
	guest, _, err := st.CreatePersonFromParticipant(participants[0])
	require.NoError(err)
	return mustPromotedPerson(t.Context(), t, st, "owner@example.org", "Owner"), guest
}
