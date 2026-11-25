package meetingimport

import (
	"context"
	"strings"

	"go.kenn.io/msgvault/internal/store"
)

// RecorderIdentityMatch reports whether an account-email identity exists and matches.
// Callers retain provider-specific registration and display-name policy.
func RecorderIdentityMatch(ctx context.Context, st *store.Store, sourceID int64, email string) (has, matches bool, err error) {
	identities, err := st.ListAccountIdentitiesContext(ctx, sourceID)
	if err != nil {
		return false, false, err
	}
	for _, identity := range identities {
		if strings.Contains(","+identity.SourceSignal+",", ",account-email,") {
			has = true
			matches = matches || store.EqualIdentifier(identity.Address, email)
		}
	}
	return has, matches, nil
}
