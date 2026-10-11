package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"go.kenn.io/msgvault/internal/vcard"
)

func (s *Store) cardDAVImportedPersonUIDTx(
	ctx context.Context, tx *loggedTx, resourceID int64, input CardDAVRemoteResource,
) (string, error) {
	if isUsableCardDAVUID(input.RemoteUID) &&
		cardDAVResourceHasOneUID(input.RemoteBody, input.RemoteUID) {
		var taken bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM persons WHERE vcard_uid = ?
			UNION ALL
			SELECT 1 FROM person_uid_aliases WHERE retired_uid = ?
			UNION ALL
			SELECT 1 FROM carddav_resources WHERE remote_uid = ? AND id <> ?
		)`, input.RemoteUID, input.RemoteUID, input.RemoteUID, resourceID).Scan(&taken); err != nil {
			return "", fmt.Errorf("check imported CardDAV UID availability: %w", err)
		}
		if !taken {
			return input.RemoteUID, nil
		}
	}
	uid, err := newVCardUID()
	if err != nil {
		return "", fmt.Errorf("mint CardDAV imported person UID: %w", err)
	}
	return uid, nil
}

func cardDAVResourceHasOneUID(body []byte, uid string) bool {
	envelope, err := vcard.ParseResourceEnvelope(body)
	if err != nil {
		return false
	}
	count := 0
	for _, occurrence := range envelope.PropertyTree {
		if !strings.EqualFold(occurrence.Property.Name, "UID") {
			continue
		}
		count++
		if strings.TrimSpace(occurrence.Property.RawValue) != uid {
			return false
		}
	}
	return count == 1
}

// isUsableCardDAVUID accepts only a canonical UUID, bare or as a urn:uuid:
// URI. Those values need no vCard escaping and are safe in a publication
// href, so the adopted UID round-trips unchanged through later publication.
func isUsableCardDAVUID(value string) bool {
	if len(value) > len(urnUUIDPrefix) && strings.EqualFold(value[:len(urnUUIDPrefix)], urnUUIDPrefix) {
		value = value[len(urnUUIDPrefix):]
	}
	if len(value) != canonicalUUIDLength {
		return false
	}
	_, err := uuid.Parse(value)
	return err == nil
}

const canonicalUUIDLength = 36
