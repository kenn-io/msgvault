package muesli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// addressBookDDL is the subset of the macOS Contacts Core Data store
// (AddressBook-v22.abcddb) that msgvault reads.
const addressBookDDL = `
CREATE TABLE Z_PRIMARYKEY (Z_ENT INTEGER PRIMARY KEY, Z_NAME VARCHAR, Z_SUPER INTEGER, Z_MAX INTEGER);
INSERT INTO Z_PRIMARYKEY (Z_ENT, Z_NAME) VALUES (19, 'ABCDGroup'), (22, 'ABCDContact');
CREATE TABLE ZABCDRECORD (
    Z_PK INTEGER PRIMARY KEY, Z_ENT INTEGER, ZUNIQUEID VARCHAR, ZLINKID VARCHAR,
    ZFIRSTNAME VARCHAR, ZLASTNAME VARCHAR, ZORGANIZATION VARCHAR
);
CREATE TABLE ZABCDEMAILADDRESS (
    Z_PK INTEGER PRIMARY KEY, ZOWNER INTEGER, ZADDRESS VARCHAR,
    ZADDRESSNORMALIZED VARCHAR, ZORDERINGINDEX INTEGER
);
CREATE TABLE ZABCDPHONENUMBER (
    Z_PK INTEGER PRIMARY KEY, ZOWNER INTEGER, ZFULLNUMBER VARCHAR, ZORDERINGINDEX INTEGER
);
`

type fixtureCard struct {
	uniqueID string
	linkID   string
	emails   []string
	phones   []string
	group    bool
}

// newAddressBookStore writes one Contacts store with the given cards.
func newAddressBookStore(t *testing.T, path string, cards ...fixtureCard) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	db := newFixtureDB(t, path, addressBookDDL)
	for _, card := range cards {
		entity := 22
		if card.group {
			entity = 19
		}
		var linkID any
		if card.linkID != "" {
			linkID = card.linkID
		}
		owner := insertRow(t, db, "ZABCDRECORD", map[string]any{
			"Z_ENT": entity, "ZUNIQUEID": card.uniqueID, "ZLINKID": linkID,
		})
		for i, email := range card.emails {
			insertRow(t, db, "ZABCDEMAILADDRESS", map[string]any{
				"ZOWNER": owner, "ZADDRESS": email, "ZORDERINGINDEX": i,
			})
		}
		for i, phone := range card.phones {
			insertRow(t, db, "ZABCDPHONENUMBER", map[string]any{
				"ZOWNER": owner, "ZFULLNUMBER": phone, "ZORDERINGINDEX": i,
			})
		}
	}
}
