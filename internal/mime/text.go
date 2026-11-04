package mime

import (
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/textutil"
)

// normalizeText applies the archive's textual projection policy after MIME
// decoding, including header-only recovery. Original MIME, attachment content
// and the parsed part tree used for fingerprints never pass through it.
func (m *Message) normalizeText() {
	for _, field := range []*string{
		&m.Subject, &m.RawDateHeader, &m.BodyText, &m.BodyHTML,
		&m.AutoSubmitted, &m.Precedence,
	} {
		*field = textutil.ReplaceNUL(*field)
	}
	for _, addresses := range [][]Address{m.From, m.To, m.Cc, m.Bcc, m.ReplyTo} {
		for i := range addresses {
			addresses[i].Name = textutil.ReplaceNUL(addresses[i].Name)
		}
	}
	for i := range m.Attachments {
		part := &m.Attachments[i]
		part.Filename = textutil.ReplaceNUL(part.Filename)
		part.ContentType = textutil.ReplaceNUL(part.ContentType)
		part.Disposition = textutil.ReplaceNUL(part.Disposition)
	}
	for i := range m.Errors {
		m.Errors[i] = textutil.ReplaceNUL(m.Errors[i])
	}
}

// OmitNULIdentities prepares a newly ingested or repaired snapshot for storage
// without inventing identity matches by replacing characters in a key. Parse
// retains original identity evidence for historical comparisons and backfill.
func (m *Message) OmitNULIdentities() {
	m.MessageID = withoutNUL(m.MessageID)
	m.InReplyTo = withoutNUL(m.InReplyTo)
	m.ListID = withoutNUL(m.ListID)
	m.References = slices.DeleteFunc(m.References, func(ref string) bool {
		return strings.ContainsRune(ref, 0)
	})
	for i := range m.Attachments {
		m.Attachments[i].ContentID = withoutNUL(m.Attachments[i].ContentID)
	}
}

// withoutNUL leaves an unusable MIME identity unknown. Replacing or deleting
// NUL could make it compare equal to a different message, list or content ID.
func withoutNUL(value string) string {
	if strings.ContainsRune(value, 0) {
		return ""
	}
	return value
}
