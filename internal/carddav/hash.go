package carddav

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/vcard"
)

var serverOwnedProperties = map[string]bool{
	"PRODID": true, "REV": true, "SOURCE": true,
	"CREATED": true, "LAST-MODIFIED": true,
}

// SemanticHash hashes the parsed vCard rather than its wire formatting. The
// five properties CardDAV servers conventionally own are deliberately absent,
// so their churn cannot masquerade as a user edit.
func SemanticHash(body []byte) (string, error) {
	envelope, err := vcard.ParseResourceEnvelope(body)
	if err != nil {
		return "", fmt.Errorf("parse vCard for semantic hash: %w", err)
	}
	properties := make([]vcard.SemanticProperty, 0, len(envelope.PropertyTree))
	for _, occurrence := range envelope.PropertyTree {
		property := occurrence.Property
		name := strings.ToUpper(property.Name)
		if serverOwnedProperties[name] {
			continue
		}
		properties = append(properties, vcard.NormalizeSemanticProperty(envelope.RenderMetadata.StoredVersion, property))
	}
	slices.SortFunc(properties, vcard.CompareSemanticProperties)
	encoded, err := json.Marshal(properties)
	if err != nil {
		return "", fmt.Errorf("encode semantic vCard: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}
