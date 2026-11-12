package x

import (
	"context"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

// RecordAuthor records the provider response time, never a replay timestamp.
func RecordAuthor(ctx context.Context, st *store.Store, sourceID int64, user User, fetchedAt time.Time) (int64, error) {
	if err := ValidateID(user.ID); err != nil {
		return 0, err
	}
	return st.RecordXAuthorContext(ctx, normalizedXAuthorInput(sourceID, user, fetchedAt))
}

// normalizedXAuthorInput is shared by standalone observations and the writer.
// Unknown protection is ID-only evidence, never a new public profile fact.
func normalizedXAuthorInput(sourceID int64, user User, fetchedAt time.Time) store.XAuthorInput {
	input := store.XAuthorInput{SourceID: sourceID, UserID: user.ID, FetchedAt: fetchedAt}
	if user.ProtectedKnown || user.Protected {
		input.Username = user.Username
		input.DisplayName = user.Name
		input.Protected = user.Protected
	}
	return input
}
