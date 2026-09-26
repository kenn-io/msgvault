package muesli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// ContactsState reports how much of the macOS Contacts directory a sync could
// read.
type ContactsState string

const (
	// ContactsComplete means every Contacts store opened with the expected
	// schema, so an email that matches one card is unambiguous.
	ContactsComplete ContactsState = "complete"
	// ContactsPartial means some stores could not be read. Identifier lookups
	// still work; email lookups are disabled because a skipped store could
	// hold another card with the same email.
	ContactsPartial ContactsState = "partial"
	// ContactsUnavailable means no store could be read, for example without
	// Full Disk Access.
	ContactsUnavailable ContactsState = "unavailable"
	// ContactsOff means enrichment is disabled in the configuration.
	ContactsOff ContactsState = "off"
)

const addressBookFile = "AddressBook-v22.abcddb"

// DefaultContactsPath is where macOS keeps the Contacts stores.
func DefaultContactsPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "Application Support", "AddressBook")
}

// ContactCard is one person in Contacts: a card, or every card linked to it
// across accounts. Phones are as typed in Contacts; see NormalizeContactPhone.
type ContactCard struct {
	// GroupKey is the card's link ID, or its unique ID when it is not linked.
	// It never leaves msgvault's memory unhashed.
	GroupKey string
	Emails   []string
	Phones   []string
}

// Contacts is an in-memory, read-only snapshot of the macOS Contacts stores.
type Contacts struct {
	state      ContactsState
	byUniqueID map[string]string
	byLinkID   map[string]string
	byEmail    map[string]map[string]bool
	cards      map[string]*ContactCard
}

// DisabledContacts is the Contacts snapshot used when enrichment is off.
func DisabledContacts() *Contacts {
	return &Contacts{state: ContactsOff}
}

// OpenContacts reads the Contacts stores under root (the root store and each
// Sources/<account>/ store) into memory. Stores are opened query-only and
// never modified. Unreadable stores lower the state instead of failing, so a
// sync without Full Disk Access still archives meetings. Only context
// cancellation is returned as an error.
func OpenContacts(ctx context.Context, root string) (*Contacts, error) {
	contacts := newContacts()
	paths, complete := addressBookStores(root)
	read := 0
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Each store is read into its own snapshot and merged only when the
		// whole store was read, so a store that fails partway contributes
		// no half-populated cards.
		store := newContacts()
		if err := store.readStore(ctx, path); err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			complete = false
			continue
		}
		contacts.merge(store)
		read++
	}
	switch {
	case read == 0:
		contacts.state = ContactsUnavailable
	case complete:
		contacts.state = ContactsComplete
	default:
		contacts.state = ContactsPartial
	}
	return contacts, nil
}

func newContacts() *Contacts {
	return &Contacts{
		state:      ContactsUnavailable,
		byUniqueID: map[string]string{},
		byLinkID:   map[string]string{},
		byEmail:    map[string]map[string]bool{},
		cards:      map[string]*ContactCard{},
	}
}

// merge adds another store's snapshot. Cards linked across stores share a
// group key and combine their identities.
func (c *Contacts) merge(other *Contacts) {
	maps.Copy(c.byUniqueID, other.byUniqueID)
	maps.Copy(c.byLinkID, other.byLinkID)
	for email, groups := range other.byEmail {
		if c.byEmail[email] == nil {
			c.byEmail[email] = map[string]bool{}
		}
		maps.Copy(c.byEmail[email], groups)
	}
	for key, card := range other.cards {
		existing := c.cards[key]
		if existing == nil {
			c.cards[key] = card
			continue
		}
		for _, email := range card.Emails {
			if !slices.Contains(existing.Emails, email) {
				existing.Emails = append(existing.Emails, email)
			}
		}
		for _, phone := range card.Phones {
			if !slices.Contains(existing.Phones, phone) {
				existing.Phones = append(existing.Phones, phone)
			}
		}
		slices.Sort(existing.Emails)
		slices.Sort(existing.Phones)
	}
}

// addressBookStores lists the store files under root. The second result is
// false when a directory that should be listed could not be read.
func addressBookStores(root string) ([]string, bool) {
	if root == "" {
		return nil, false
	}
	var paths []string
	complete := true
	rootStore := filepath.Join(root, addressBookFile)
	if _, err := os.Stat(rootStore); err == nil {
		paths = append(paths, rootStore)
	} else if !errors.Is(err, os.ErrNotExist) {
		complete = false
	}
	entries, err := os.ReadDir(filepath.Join(root, "Sources"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		complete = false
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		store := filepath.Join(root, "Sources", entry.Name(), addressBookFile)
		if _, err := os.Stat(store); err == nil {
			paths = append(paths, store)
		} else if !errors.Is(err, os.ErrNotExist) {
			complete = false
		}
	}
	return paths, complete
}

// State reports how much of the Contacts directory was read.
func (c *Contacts) State() ContactsState {
	if c == nil {
		return ContactsOff
	}
	return c.state
}

// Resolve finds the Contacts person for a Muesli participant: first by the
// stored Contacts identifier (a card's unique ID, with or without its
// ":ABPerson" suffix, or a unified contact's link ID), then by exact email.
// Email lookup needs a complete read and a single matching person. Names are
// never used.
func (c *Contacts) Resolve(contactID, email string) (ContactCard, bool) {
	if c == nil || c.cards == nil {
		return ContactCard{}, false
	}
	if contactID = strings.TrimSpace(contactID); contactID != "" {
		bare := strings.TrimSuffix(contactID, ":ABPerson")
		for _, candidate := range []string{contactID, bare + ":ABPerson"} {
			if key, ok := c.byUniqueID[candidate]; ok {
				return c.card(key), true
			}
		}
		for _, candidate := range []string{contactID, bare} {
			if key, ok := c.byLinkID[candidate]; ok {
				return c.card(key), true
			}
		}
	}
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" || c.state != ContactsComplete {
		return ContactCard{}, false
	}
	groups := c.byEmail[email]
	if len(groups) != 1 {
		return ContactCard{}, false
	}
	for key := range groups {
		return c.card(key), true
	}
	return ContactCard{}, false
}

func (c *Contacts) card(key string) ContactCard {
	card := c.cards[key]
	return ContactCard{
		GroupKey: card.GroupKey,
		Emails:   slices.Clone(card.Emails),
		Phones:   slices.Clone(card.Phones),
	}
}

func (c *Contacts) readStore(ctx context.Context, path string) error {
	db, err := openQueryOnly(path)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	for _, required := range []struct{ table, column string }{
		{"Z_PRIMARYKEY", "Z_ENT"}, {"Z_PRIMARYKEY", "Z_NAME"},
		{"ZABCDRECORD", "Z_PK"}, {"ZABCDRECORD", "Z_ENT"}, {"ZABCDRECORD", "ZUNIQUEID"},
		{"ZABCDEMAILADDRESS", "ZOWNER"}, {"ZABCDEMAILADDRESS", "ZADDRESS"},
		{"ZABCDPHONENUMBER", "ZOWNER"}, {"ZABCDPHONENUMBER", "ZFULLNUMBER"},
	} {
		columns, err := tableColumns(ctx, db, required.table)
		if err != nil {
			return err
		}
		if !columns[required.column] {
			return fmt.Errorf("contacts store %s lacks %s.%s", filepath.Base(filepath.Dir(path)), required.table, required.column)
		}
	}
	recordColumns, err := tableColumns(ctx, db, "ZABCDRECORD")
	if err != nil {
		return err
	}
	linkColumn := "NULL"
	if recordColumns["ZLINKID"] {
		linkColumn = "r.ZLINKID"
	}

	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	owners, err := c.readRecords(ctx, tx, linkColumn)
	if err != nil {
		return err
	}
	if err := c.readAddresses(ctx, tx, owners, `SELECT ZOWNER, ZADDRESS FROM ZABCDEMAILADDRESS`, true); err != nil {
		return err
	}
	return c.readAddresses(ctx, tx, owners, `SELECT ZOWNER, ZFULLNUMBER FROM ZABCDPHONENUMBER`, false)
}

// readRecords loads the store's person cards and returns their group keys by
// record primary key.
func (c *Contacts) readRecords(ctx context.Context, tx *sql.Tx, linkColumn string) (map[int64]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT r.Z_PK, r.ZUNIQUEID, `+linkColumn+`
		FROM ZABCDRECORD r
		WHERE r.Z_ENT IN (SELECT Z_ENT FROM Z_PRIMARYKEY
			WHERE Z_NAME IN ('ABCDContact', 'ABCDSubscribedContact'))`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	owners := map[int64]string{}
	for rows.Next() {
		var pk int64
		var uniqueID, linkID sql.NullString
		if err := rows.Scan(&pk, &uniqueID, &linkID); err != nil {
			return nil, err
		}
		unique := strings.TrimSpace(uniqueID.String)
		link := strings.TrimSpace(linkID.String)
		key := link
		if key == "" {
			key = unique
		}
		if key == "" {
			continue
		}
		owners[pk] = key
		if unique != "" {
			c.byUniqueID[unique] = key
		}
		if link != "" {
			c.byLinkID[link] = key
		}
		if c.cards[key] == nil {
			c.cards[key] = &ContactCard{GroupKey: key}
		}
	}
	return owners, rows.Err()
}

func (c *Contacts) readAddresses(ctx context.Context, tx *sql.Tx, owners map[int64]string, query string, email bool) error {
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var owner sql.NullInt64
		var value sql.NullString
		if err := rows.Scan(&owner, &value); err != nil {
			return err
		}
		key, ok := owners[owner.Int64]
		address := strings.TrimSpace(value.String)
		if !ok || address == "" {
			continue
		}
		card := c.cards[key]
		if email {
			address = strings.ToLower(address)
			if !slices.Contains(card.Emails, address) {
				card.Emails = append(card.Emails, address)
				slices.Sort(card.Emails)
			}
			if c.byEmail[address] == nil {
				c.byEmail[address] = map[string]bool{}
			}
			c.byEmail[address][key] = true
			continue
		}
		if !slices.Contains(card.Phones, address) {
			card.Phones = append(card.Phones, address)
			slices.Sort(card.Phones)
		}
	}
	return rows.Err()
}
