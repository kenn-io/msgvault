package peoplesweep

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"unicode/utf8"
)

// AppreciationContributionKey identifies a native message independently of
// person membership, local database coordinates, and message content. Version 1
// uses the original JSON format, including HTML and JavaScript escaping. Changing it
// requires an archive migration that preserves existing dismissal tombstones.
func AppreciationContributionKey(sourceType, sourceIdentifier, sourceMessageID string) (string, error) {
	for _, value := range []string{sourceType, sourceIdentifier, sourceMessageID} {
		if value == "" || !utf8.ValidString(value) {
			return "", errors.New("appreciation contribution identity requires nonempty UTF-8 coordinates")
		}
	}
	identity := struct {
		Version          int    `json:"version"`
		SourceType       string `json:"source_type"`
		SourceIdentifier string `json:"source_identifier"`
		SourceMessageID  string `json:"source_message_id"`
	}{1, sourceType, sourceIdentifier, sourceMessageID}
	data, err := json.Marshal(identity, jsontext.EscapeForHTML(true), jsontext.EscapeForJS(true))
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}
