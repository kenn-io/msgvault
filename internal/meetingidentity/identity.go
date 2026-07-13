// Package meetingidentity resolves the confirmed account identities used to
// attribute meeting organizers to the local account.
package meetingidentity

import (
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/store"
)

// Set contains normalized confirmed identities for one meeting source.
type Set map[string]struct{}

// ForSource loads every confirmed identity for sourceID and unions the
// configured primary account email. Email comparison is case-insensitive and
// ignores surrounding whitespace.
func ForSource(s *store.Store, sourceID int64, primaryEmail string) (Set, error) {
	stored, err := s.GetIdentitiesForScope([]int64{sourceID})
	if err != nil {
		return nil, fmt.Errorf("load account identities: %w", err)
	}
	identities := make(Set, len(stored)+1)
	for address := range stored {
		if normalized := normalize(address); normalized != "" {
			identities[normalized] = struct{}{}
		}
	}
	if normalized := normalize(primaryEmail); normalized != "" {
		identities[normalized] = struct{}{}
	}
	return identities, nil
}

// Contains reports whether email belongs to the confirmed identity set.
func (s Set) Contains(email string) bool {
	_, ok := s[normalize(email)]
	return ok
}

func normalize(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}
