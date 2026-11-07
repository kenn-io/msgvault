package mime

import (
	"strings"
	"unicode"

	"go.kenn.io/msgvault/internal/textutil"
)

// NormalizeMessageID returns the canonical form used for RFC822 Message-ID
// comparison and storage. It unwraps one structurally valid angle-bracket pair,
// rejects malformed bracket structure, and replaces invalid UTF-8 bytes.
// It intentionally validates only bracket structure: historical archives
// contain useful bare IDs that do not satisfy the full RFC grammar. Legacy
// SQLite comparison keys can include NUL; new writes use OmitNULIdentities
// instead of changing their historical extraction and comparison forms.
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

// NormalizeLegacyMessageID recovers an ID from historical header values with
// a missing closing bracket or trailing text after the closing bracket. It is
// for IMAP legacy identity recovery; NormalizeMessageID remains the
// storage/parser contract. A bracketed ID must start the value, and an
// unbracketed value is accepted only whole.
func NormalizeLegacyMessageID(value string) string {
	id := strings.TrimSpace(value)
	if strings.HasPrefix(id, "<") {
		id = id[1:]
		if end := strings.IndexByte(id, '>'); end >= 0 {
			if strings.ContainsAny(id[end+1:], "<>") {
				return ""
			}
			id = id[:end]
		}
	}
	if id == "" || strings.ContainsAny(id, "<>") || strings.IndexFunc(id, unicode.IsSpace) >= 0 {
		return ""
	}
	return textutil.SanitizeUTF8(id)
}

// LegacyMessageIDMatchKey compares recoverable historical IDs without changing
// their stored spelling. Only the domain is case insensitive.
func LegacyMessageIDMatchKey(value string) string {
	id := NormalizeLegacyMessageID(value)
	if at := strings.LastIndexByte(id, '@'); at >= 0 {
		id = id[:at+1] + strings.ToLower(id[at+1:])
	}
	return id
}

// ParseMessageIDs extracts canonical message and reply IDs from the top-level
// headers without decoding attachments or accepting header-shaped body text.
func ParseMessageIDs(raw []byte) (messageID, inReplyTo string) {
	messageID, inReplyTo, _ = ParseThreadingHeaders(raw)
	return messageID, inReplyTo
}

// ParseThreadingHeaders reads email identifiers and the ordered References
// chain from top-level headers without decoding bodies or attachments.
func ParseThreadingHeaders(raw []byte) (messageID, inReplyTo string, references []string) {
	headers := tokenizeHeaders(raw)
	messageID = NormalizeMessageID(firstHeader(headers, "message-id"))
	inReplyTo = NormalizeMessageID(firstHeader(headers, "in-reply-to"))
	return messageID, inReplyTo, parseReferences(firstHeader(headers, "references"))
}

// ReplyParentID returns the canonical parent Message-ID named by an
// In-Reply-To value. A single bare or bracketed ID is normalized as is. A
// msg-id list (several parents, or an ID followed by a comment such as
// "<id> (message of ...)") yields its first bracketed ID, the same entry IMAP
// conversation threading uses. A value containing NUL names no parent.
func ReplyParentID(inReplyTo string) string {
	if strings.ContainsRune(inReplyTo, 0) {
		return ""
	}
	if id := NormalizeMessageID(inReplyTo); id != "" {
		return id
	}
	rest := inReplyTo
	for {
		open := strings.IndexByte(rest, '<')
		if open < 0 {
			return ""
		}
		end := strings.IndexByte(rest[open+1:], '>')
		if end < 0 {
			return ""
		}
		if id := NormalizeMessageID(rest[open : open+end+2]); id != "" {
			return id
		}
		rest = rest[open+end+2:]
	}
}

// ReplyParentFromHeaders reads the parent Message-ID from the top-level
// In-Reply-To header without decoding bodies or attachments.
func ReplyParentFromHeaders(raw []byte) string {
	return ReplyParentID(firstHeader(tokenizeHeaders(raw), "in-reply-to"))
}
