package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestPhoneParticipantContextResolvesAliasesConservatively(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := storetest.New(t).Store
	survivor, err := st.EnsureParticipantByPhone("+16045550100", "Synthetic Survivor", "imessage")
	require.NoError(err)
	absorbed, err := st.EnsureParticipantByPhone("+16045550101", "Synthetic Absorbed", "imessage")
	require.NoError(err)
	require.NoError(st.MergeParticipants(absorbed, survivor))

	got, err := st.PhoneParticipantContext(t.Context(), "+16045550100")
	require.NoError(err)
	assert.Equal(survivor, got, "primary number")
	got, err = st.PhoneParticipantContext(t.Context(), "+16045550101")
	require.NoError(err)
	assert.Equal(survivor, got, "a merge keeps the absorbed number as an alias")
	got, err = st.PhoneParticipantContext(t.Context(), "+16045550199")
	require.NoError(err)
	assert.Zero(got, "unknown numbers create nothing")

	other, err := st.EnsureParticipantByPhone("+16045550102", "Synthetic Other", "imessage")
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`INSERT INTO participant_identifiers
		(participant_id, identifier_type, identifier_value, is_primary) VALUES (?, 'whatsapp', '+16045550100', FALSE)`), other)
	require.NoError(err)
	got, err = st.PhoneParticipantContext(t.Context(), "+16045550100")
	require.NoError(err)
	assert.Zero(got, "a number held by two participants is ambiguous")
}

func TestEmailParticipantContextResolvesAliasesConservatively(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := storetest.New(t).Store
	primary, err := st.EnsureParticipant("attendee@example.com", "", "example.com")
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`INSERT INTO participant_identifiers
		(participant_id, identifier_type, identifier_value, is_primary) VALUES (?, 'email', 'Alias@Example.com', FALSE)`), primary)
	require.NoError(err)

	got, err := st.EmailParticipantContext(t.Context(), "attendee@example.com")
	require.NoError(err)
	assert.Equal(primary, got)
	got, err = st.EmailParticipantContext(t.Context(), "alias@example.com")
	require.NoError(err)
	assert.Equal(primary, got, "aliases match without regard to case")
	got, err = st.EmailParticipantContext(t.Context(), "missing@example.com")
	require.NoError(err)
	assert.Zero(got)

	other, err := st.EnsureParticipant("other@example.com", "", "example.com")
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`INSERT INTO participant_identifiers
		(participant_id, identifier_type, identifier_value, is_primary) VALUES (?, 'apple_id', 'attendee@example.com', FALSE)`), other)
	require.NoError(err)
	got, err = st.EmailParticipantContext(t.Context(), "attendee@example.com")
	require.NoError(err)
	assert.Zero(got, "an address held by two participants is ambiguous")
}
