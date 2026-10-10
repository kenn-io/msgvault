package agentgrant

import (
	"errors"
	"net/url"
	"slices"
	"strings"
)

// PersonRef binds an explicit archive person ID to its durable identity. An ID
// reused after deletion does not inherit the former person's grant.
type PersonRef struct {
	ID  int64
	UID string
}

// AddressBookRef binds native account/book identity and account ownership.
// The native issuer resolves these fields; clients select book IDs only.
type AddressBookRef struct {
	AccountID            int64
	BookID               int64
	CanonicalURL         string
	OwnershipFingerprint string
}

type ResourceScopes struct {
	Sources      []SourceRef
	Persons      []PersonRef
	AddressBooks []AddressBookRef
}

func (g Grant) AllowsPerson(permission Permission, person PersonRef) bool {
	if !g.HasPermission(permission) || !validResourceID(person.ID) || !boundedResourceValue(person.UID) {
		return false
	}
	return slices.Contains(g.Persons, person)
}

func (g Grant) AllowsAddressBook(permission Permission, book AddressBookRef) bool {
	if !g.HasPermission(permission) || !validResourceID(book.AccountID) || !validResourceID(book.BookID) || !boundedResourceValue(book.CanonicalURL) || !boundedResourceValue(book.OwnershipFingerprint) {
		return false
	}
	return slices.Contains(g.AddressBooks, book)
}

// IssueScoped issues an immutable, bounded selection without expanding sources
// or people as the archive changes. Legacy Issue retains its source requirement.
func (r *Registry) IssueScoped(label string, permissions []Permission, scope ResourceScopes) (id, secret string, grant Grant, err error) {
	count := len(scope.Sources) + len(scope.Persons) + len(scope.AddressBooks)
	if count < 1 || count > 100 {
		return "", "", Grant{}, errors.New("agentgrant: select between 1 and 100 explicit resources")
	}
	for _, source := range scope.Sources {
		if !validResourceID(source.ID) {
			return "", "", Grant{}, errors.New("agentgrant: source ID must be an exact positive JSON integer")
		}
	}
	seenPersons := make(map[int64]bool, len(scope.Persons))
	for _, person := range scope.Persons {
		if !validResourceID(person.ID) || !boundedResourceValue(person.UID) || seenPersons[person.ID] {
			return "", "", Grant{}, errors.New("agentgrant: invalid or duplicate person scope")
		}
		seenPersons[person.ID] = true
	}
	seenBooks := make(map[int64]bool, len(scope.AddressBooks))
	for _, book := range scope.AddressBooks {
		parsed, parseErr := url.Parse(book.CanonicalURL)
		if !validResourceID(book.AccountID) || !validResourceID(book.BookID) || !boundedResourceValue(book.CanonicalURL) || !boundedResourceValue(book.OwnershipFingerprint) || seenBooks[book.BookID] || parseErr != nil || parsed.Host == "" || parsed.User != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") {
			return "", "", Grant{}, errors.New("agentgrant: invalid or duplicate address-book scope")
		}
		seenBooks[book.BookID] = true
	}
	return r.issue(label, permissions, scope, true)
}

func validResourceID(id int64) bool {
	return id > 0 && id <= 9_007_199_254_740_991
}

func boundedResourceValue(value string) bool {
	return strings.TrimSpace(value) != "" && len(value) <= 2048
}
