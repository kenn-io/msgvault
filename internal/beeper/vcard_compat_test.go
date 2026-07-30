package beeper

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vcard"
)

func TestVCardContactUpgradesBeeperParticipantByPhone(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	resolver := newParticipantResolver(st)

	weakID, err := resolver.resolveID("@alice:beeper.local", "")
	require.NoError(err)

	path := filepath.Join(t.TempDir(), "beeper-contact.vcf")
	require.NoError(os.WriteFile(path, []byte(
		"BEGIN:VCARD\r\n"+
			"VERSION:4.0\r\n"+
			"FN:Alice Example\r\n"+
			"TEL;VALUE=uri:tel:+1-202-555-0123\r\n"+
			"EMAIL:Alice@Example.com\r\n"+
			"END:VCARD\r\n",
	), 0o600))

	contacts, err := vcard.ParseFile(path)
	require.NoError(err)
	require.Len(contacts, 1)
	require.Len(contacts[0].Phones, 1)
	require.Len(contacts[0].Emails, 1)

	richID, err := resolver.resolveUser(&User{
		ID:          "@alice:beeper.local",
		PhoneNumber: contacts[0].Phones[0],
		Email:       contacts[0].Emails[0],
		FullName:    contacts[0].FullName,
	})
	require.NoError(err)
	assert.NotEqual(weakID, richID)

	resolvedAgain, err := resolver.resolveID("@alice:beeper.local", "")
	require.NoError(err)
	assert.Equal(richID, resolvedAgain)
}

func TestVCardEmailReusesExistingBeeperParticipant(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	existingID, err := st.EnsureParticipant(
		"bob@example.com",
		"Existing Mail Contact",
		"example.com",
	)
	require.NoError(err)
	resolver := newParticipantResolver(st)

	path := filepath.Join(t.TempDir(), "email-contact.vcf")
	require.NoError(os.WriteFile(path, []byte(
		"BEGIN:VCARD\r\n"+
			"VERSION:4.0\r\n"+
			"FN:Bob Example\r\n"+
			"EMAIL:Bob@Example.com\r\n"+
			"END:VCARD\r\n",
	), 0o600))

	contacts, err := vcard.ParseFile(path)
	require.NoError(err)
	require.Len(contacts, 1)
	require.Empty(contacts[0].Phones)
	require.Equal([]string{"bob@example.com"}, contacts[0].Emails)

	resolvedID, err := resolver.resolveUser(&User{
		ID:       "@bob:beeper.local",
		Email:    contacts[0].Emails[0],
		FullName: contacts[0].FullName,
	})
	require.NoError(err)
	assert.Equal(t, existingID, resolvedID)
}
