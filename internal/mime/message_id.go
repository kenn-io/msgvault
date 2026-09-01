package mime

import (
	"strings"

	"go.kenn.io/msgvault/internal/textutil"
)

// NormalizeMessageID returns the canonical form used for RFC822 Message-ID
// comparison and storage. It unwraps one structurally valid angle-bracket pair,
// rejects malformed bracket structure, and makes invalid bytes safe for SQL
// TEXT. It intentionally validates only bracket structure: historical archives
// contain useful bare IDs that do not satisfy the full RFC grammar.
func NormalizeMessageID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	if strings.ContainsAny(id, "<>") {
		if len(id) <= 2 || id[0] != '<' || id[len(id)-1] != '>' {
			return ""
		}
		id = id[1 : len(id)-1]
		if strings.TrimSpace(id) != id || strings.ContainsAny(id, "<>") {
			return ""
		}
	}
	return textutil.SanitizeUTF8(id)
}
