package cmd

import (
	"context"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/api"
)

var _ api.PersonIdentityStore = (*storeAPIAdapter)(nil)

// ListPersonIdentitiesContext lists the archived participant identities
// currently bound to the person. Curated contact points and postal addresses
// are not archived participant identities, so they never appear here.
func (a *storeAPIAdapter) ListPersonIdentitiesContext(
	ctx context.Context, personID int64,
) ([]api.PersonIdentity, error) {
	person, err := a.store.GetPersonContext(ctx, personID)
	if err != nil {
		return nil, fmt.Errorf("load person %d: %w", personID, err)
	}
	identity, err := a.store.GetParticipantIdentityContext(ctx, person.ParticipantIDs)
	if err != nil {
		return nil, fmt.Errorf("load identities for person %d: %w", personID, err)
	}
	rows := make([]api.PersonIdentity, 0)
	supported := make(map[string]bool)
	unsupported := make(map[[2]string]bool)
	add := func(kind, value string) {
		if strings.TrimSpace(value) == "" {
			return
		}
		if kind == "email" {
			if address, key, err := parseStoredMailbox(value); err == nil {
				if !supported[key] {
					supported[key] = true
					// String keeps local-part quoting so the listed value parses back as --to.
					mailbox := address.String()
					rows = append(rows, api.PersonIdentity{Kind: kind, Value: mailbox[1 : len(mailbox)-1], Supported: true})
				}
				return
			}
		}
		// Only email compares case-insensitively; chat IDs such as Matrix IDs are case-sensitive.
		key := [2]string{kind, value}
		if kind == "email" {
			key[1] = strings.ToLower(value)
		}
		if !unsupported[key] {
			unsupported[key] = true
			rows = append(rows, api.PersonIdentity{Kind: kind, Value: value})
		}
	}
	for _, member := range identity.Members {
		add("email", member.Email)
		add("phone", member.Phone)
	}
	for _, identifier := range identity.Identifiers {
		add(identifier.Type, identifier.Value)
	}
	return rows, nil
}
