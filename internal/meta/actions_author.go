package meta

import (
	"context"
	"strings"

	"go.kenn.io/msgvault/internal/beeperidentity"
	"go.kenn.io/msgvault/internal/store"
)

// restoreArchivedActionAuthor fills omitted raw fields from the archived
// sender's exact provider scope and the message's saved envelope name.
func restoreArchivedActionAuthor(ctx context.Context, st *store.Store, t *Target, senderID int64, kind, emptyIdentifier string) error {
	if t.AuthorID == "" && senderID != 0 {
		identity, err := st.GetParticipantIdentityContext(ctx, []int64{senderID})
		if err != nil {
			return err
		}
		prefix := strings.TrimSuffix(emptyIdentifier, beeperidentity.EncodePart(""))
		var authorID string
		for _, identifier := range identity.Identifiers {
			if identifier.Type != kind {
				continue
			}
			suffix, matches := strings.CutPrefix(identifier.Value, prefix)
			_, candidate, encoded := strings.Cut(suffix, ":")
			if !matches || !encoded || candidate == "" || suffix != beeperidentity.EncodePart(candidate) {
				continue
			}
			if authorID != "" && authorID != candidate {
				// A manually merged participant can have multiple identities
				// in this scope. Do not choose an author without evidence.
				authorID = ""
				break
			}
			authorID = candidate
		}
		t.AuthorID = authorID
	}
	if t.AuthorName == "" {
		message, err := st.GetMessageContext(ctx, t.MessageID)
		if err != nil {
			return err
		}
		t.AuthorName = message.FromName
	}
	return nil
}
