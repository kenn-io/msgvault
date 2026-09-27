package muesli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func contactsRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "AddressBook")
	newAddressBookStore(t, filepath.Join(root, "AddressBook-v22.abcddb"))
	newAddressBookStore(t, filepath.Join(root, "Sources", "A", "AddressBook-v22.abcddb"),
		fixtureCard{uniqueID: "CARD-1:ABPerson", emails: []string{"Alex@Example.com"}, phones: []string{"+1 (604) 555-0100"}},
		fixtureCard{uniqueID: "CARD-2:ABPerson", linkID: "LINK-9", emails: []string{"jo@example.com"}},
		fixtureCard{uniqueID: "SHARED-1:ABPerson", emails: []string{"desk@example.com"}},
		fixtureCard{uniqueID: "GROUP-1:ABGroup", group: true, emails: []string{"team@example.com"}},
	)
	newAddressBookStore(t, filepath.Join(root, "Sources", "B", "AddressBook-v22.abcddb"),
		fixtureCard{uniqueID: "CARD-3:ABPerson", linkID: "LINK-9", emails: []string{"jo.work@example.com"}, phones: []string{"0044 20 7946 0000"}},
		fixtureCard{uniqueID: "SHARED-2:ABPerson", emails: []string{"desk@example.com"}},
	)
	return root
}

func TestContactsResolveByIdentifierAndEmail(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	contacts, err := OpenContacts(t.Context(), contactsRoot(t))
	require.NoError(err)
	assert.Equal(ContactsComplete, contacts.State())

	card, ok := contacts.Resolve("CARD-1:ABPerson", "")
	require.True(ok)
	assert.Equal([]string{"alex@example.com"}, card.Emails)
	assert.Equal([]string{"+1 (604) 555-0100"}, card.Phones)

	bare, ok := contacts.Resolve("CARD-1", "")
	require.True(ok, "a bare UUID matches its :ABPerson record")
	assert.Equal(card.GroupKey, bare.GroupKey)

	unified, ok := contacts.Resolve("LINK-9", "")
	require.True(ok, "a unified identifier resolves every linked card")
	assert.Equal([]string{"jo.work@example.com", "jo@example.com"}, unified.Emails)
	assert.Equal([]string{"0044 20 7946 0000"}, unified.Phones)
	linked, ok := contacts.Resolve("CARD-2:ABPerson", "")
	require.True(ok)
	assert.Equal(unified.GroupKey, linked.GroupKey)

	byEmail, ok := contacts.Resolve("", "ALEX@example.com")
	require.True(ok)
	assert.Equal(card.GroupKey, byEmail.GroupKey)

	_, ok = contacts.Resolve("", "desk@example.com")
	assert.False(ok, "an email on two unrelated cards is ambiguous")
	_, ok = contacts.Resolve("", "team@example.com")
	assert.False(ok, "groups are not people")
	_, ok = contacts.Resolve("MISSING", "nobody@example.com")
	assert.False(ok)
}

func TestContactsPartialReadDisablesEmailLookup(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	root := contactsRoot(t)
	broken := filepath.Join(root, "Sources", "C", "AddressBook-v22.abcddb")
	require.NoError(os.MkdirAll(filepath.Dir(broken), 0o755))
	newFixtureDB(t, broken, `CREATE TABLE unrelated (id INTEGER)`)

	contacts, err := OpenContacts(t.Context(), root)
	require.NoError(err)

	assert.Equal(ContactsPartial, contacts.State())
	_, ok := contacts.Resolve("", "alex@example.com")
	assert.False(ok, "a skipped store could hide another card with the same email")
	_, ok = contacts.Resolve("CARD-1:ABPerson", "")
	assert.True(ok, "identifier lookups still work")
}

func TestContactsUnavailable(t *testing.T) {
	require := require.New(t)
	missing, err := OpenContacts(t.Context(), filepath.Join(t.TempDir(), "absent"))
	require.NoError(err)
	assert.Equal(t, ContactsUnavailable, missing.State())
	_, ok := missing.Resolve("CARD-1:ABPerson", "alex@example.com")
	assert.False(t, ok)

	fileRoot := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(os.WriteFile(fileRoot, []byte("not a directory"), 0o600))
	denied, err := OpenContacts(t.Context(), fileRoot)
	require.NoError(err)
	assert.Equal(t, ContactsUnavailable, denied.State())
}

func TestContactsReadDoesNotChangeStores(t *testing.T) {
	require := require.New(t)
	root := contactsRoot(t)
	path := filepath.Join(root, "Sources", "A", "AddressBook-v22.abcddb")
	db, err := openQueryOnly(path)
	require.NoError(err)
	var before int
	require.NoError(db.QueryRow(`SELECT count(*) FROM ZABCDRECORD`).Scan(&before))
	require.NoError(db.Close())

	_, err = OpenContacts(t.Context(), root)
	require.NoError(err)

	db, err = openQueryOnly(path)
	require.NoError(err)
	defer func() { _ = db.Close() }()
	var after int
	require.NoError(db.QueryRow(`SELECT count(*) FROM ZABCDRECORD`).Scan(&after))
	assert.Equal(t, before, after)
}

func TestNormalizeContactPhone(t *testing.T) {
	for _, tt := range []struct {
		raw, country, want string
		ok                 bool
	}{
		{raw: "+1 (604) 555-0100", want: "+16045550100", ok: true},
		{raw: "0044 20 7946 0000", want: "+442079460000", ok: true},
		{raw: "(604) 555-0100", ok: false},
		{raw: "(604) 555-0100", country: "1", want: "+16045550100", ok: true},
		{raw: "1-604-555-0100", country: "1", want: "+16045550100", ok: true},
		{raw: "555-0100", country: "1", ok: false},
		{raw: "020 7946 0000", country: "44", want: "+442079460000", ok: true},
		{raw: "06 1234 5678", country: "39", want: "+390612345678", ok: true},
		{raw: "+1 604 555 0100 ext. 12", want: "+16045550100", ok: true},
		{raw: "+12", ok: false},
		{raw: "+44 (0)20 7946 0000", want: "+442079460000", ok: true},
		{raw: "+0044 20 7946 0000", ok: false},
		{raw: "+1234567890123456", ok: false},
		{raw: "call me", country: "1", ok: false},
	} {
		got, ok := NormalizeContactPhone(tt.raw, tt.country)
		assert.Equal(t, tt.ok, ok, "%q/%q", tt.raw, tt.country)
		assert.Equal(t, tt.want, got, "%q/%q", tt.raw, tt.country)
	}
}

func TestContactsDropStoreThatFailsMidRead(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	root := contactsRoot(t)
	path := filepath.Join(root, "Sources", "D", "AddressBook-v22.abcddb")
	require.NoError(os.MkdirAll(filepath.Dir(path), 0o755))
	db := newFixtureDB(t, path, `
CREATE TABLE Z_PRIMARYKEY (Z_ENT INTEGER PRIMARY KEY, Z_NAME VARCHAR);
INSERT INTO Z_PRIMARYKEY VALUES (22, 'ABCDContact');
CREATE TABLE ZABCDRECORD (Z_PK INTEGER PRIMARY KEY, Z_ENT INTEGER, ZUNIQUEID VARCHAR);
CREATE TABLE ZABCDEMAILADDRESS (Z_PK INTEGER PRIMARY KEY, ZOWNER INTEGER, ZADDRESS VARCHAR);
CREATE TABLE ZABCDPHONENUMBER (Z_PK INTEGER PRIMARY KEY, ZFULLNUMBER VARCHAR);
`)
	insertRow(t, db, "ZABCDRECORD", map[string]any{"Z_ENT": 22, "ZUNIQUEID": "HALF-1:ABPerson"})
	insertRow(t, db, "ZABCDEMAILADDRESS", map[string]any{"ZOWNER": 1, "ZADDRESS": "half@example.com"})

	contacts, err := OpenContacts(t.Context(), root)
	require.NoError(err)

	assert.Equal(ContactsPartial, contacts.State())
	_, ok := contacts.Resolve("HALF-1:ABPerson", "")
	assert.False(ok, "a store that could not be read completely contributes no cards")
}
