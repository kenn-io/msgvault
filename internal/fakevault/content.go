package fakevault

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"math/rand/v2"
	"strings"
)

// fakeSources is the fixed source set every generated vault carries: two
// email accounts and one chat account, so both the email path (bodies, raw
// MIME, labels) and the chat path (no subject, no raw) are represented.
var fakeSources = []struct {
	typ, identifier, name, msgType string
}{
	{"gmail", "alice@example.com", "Alice Example", "email"},
	{"gmail", "bob@example.org", "Bob Example", "email"},
	{"whatsapp", "+15550100001", "Alice Phone", "whatsapp"},
}

var fakeLabels = []string{
	"INBOX", "SENT", "IMPORTANT", "STARRED", "SPAM", "TRASH",
	"DRAFT", "UNREAD", "CATEGORY_PERSONAL", "CATEGORY_UPDATES",
	"work", "family",
}

var domains = []string{
	"example.com", "example.org", "example.net", "mail.test",
	"corp.test", "school.test", "club.test", "shop.test",
}

// wordList feeds all generated text. Natural-language-shaped words matter:
// message bodies must compress the way real prose does (roughly 3x under
// zstd), because pack sizes and compression CPU are what the generated
// vaults exist to measure.
var wordList = []string{
	"about", "after", "again", "agenda", "almost", "always", "answer",
	"anyone", "around", "attach", "before", "better", "between", "bring",
	"budget", "call", "change", "check", "coffee", "coming", "confirm",
	"could", "customer", "deadline", "detail", "dinner", "document",
	"draft", "early", "email", "evening", "every", "family", "feedback",
	"figure", "final", "finish", "follow", "forward", "friday", "friend",
	"getting", "great", "happy", "having", "hello", "hoping", "hours",
	"house", "idea", "invoice", "issue", "know", "later", "launch",
	"leave", "letter", "little", "looking", "lunch", "makes", "maybe",
	"meeting", "message", "minutes", "monday", "money", "month", "morning",
	"needs", "never", "night", "notes", "number", "office", "order",
	"other", "people", "phone", "photo", "picture", "place", "planning",
	"please", "point", "problem", "project", "question", "quick", "ready",
	"really", "reminder", "report", "review", "right", "schedule", "school",
	"season", "second", "sending", "share", "should", "signed", "since",
	"sorry", "sounds", "start", "status", "still", "story", "summer",
	"sunday", "support", "sure", "team", "thanks", "these", "thing",
	"think", "those", "three", "ticket", "today", "together", "tomorrow",
	"tonight", "topic", "travel", "trying", "update", "vacation", "version",
	"visit", "waiting", "wanted", "weekend", "welcome", "where", "which",
	"while", "working", "would", "write", "yesterday",
}

func word(r *rand.Rand) string { return wordList[r.IntN(len(wordList))] }

// sentence returns n words, capitalized and terminated like prose.
func sentence(r *rand.Rand, n int) string {
	var b strings.Builder
	for i := range n {
		if i > 0 {
			b.WriteByte(' ')
		}
		w := word(r)
		if i == 0 {
			w = strings.ToUpper(w[:1]) + w[1:]
		}
		b.WriteString(w)
	}
	b.WriteByte('.')
	return b.String()
}

// paragraphs returns n paragraphs of 2-6 sentences each.
func paragraphs(r *rand.Rand, n int) string {
	var b strings.Builder
	for i := range n {
		if i > 0 {
			b.WriteString("\n\n")
		}
		for s, count := 0, 2+r.IntN(5); s < count; s++ {
			if s > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(sentence(r, 6+r.IntN(9)))
		}
	}
	return b.String()
}

func snippetOf(body string) string {
	const maxLen = 120
	if len(body) <= maxLen {
		return body
	}
	return body[:maxLen]
}

func htmlBody(body string) string {
	var b strings.Builder
	b.WriteString("<html><body>\n")
	for para := range strings.SplitSeq(body, "\n\n") {
		b.WriteString("<p>")
		b.WriteString(para)
		b.WriteString("</p>\n")
	}
	b.WriteString("</body></html>\n")
	return b.String()
}

// compressibleBytes returns size bytes of word-stream text, which zstd
// compresses roughly like real documents.
func compressibleBytes(r *rand.Rand, size int64) []byte {
	var b bytes.Buffer
	b.Grow(int(size) + 16)
	for int64(b.Len()) < size {
		b.WriteString(word(r))
		b.WriteByte(' ')
	}
	return b.Bytes()[:size]
}

// participantIdentity derives participant i's stable identity fields.
func participantIdentity(i int64) (email, name, domain string) {
	r := rand.New(rand.NewPCG(uint64(i), 0x9e3779b97f4a7c15)) //nolint:gosec // deterministic fake data
	domain = domains[i%int64(len(domains))]
	email = fmt.Sprintf("person%d@%s", i, domain)
	first, last := word(r), word(r)
	name = strings.ToUpper(first[:1]) + first[1:] + " " +
		strings.ToUpper(last[:1]) + last[1:]
	return email, name, domain
}

// compressedMIME builds a minimal RFC822-shaped message and zlib-compresses
// it exactly the way store.UpsertMessageRaw does ('mime' format, 'zlib'
// compression), so generated message_raw rows match what sync writes.
func compressedMIME(msgID int64, subject any, body, sent string) ([]byte, error) {
	subj := ""
	if s, ok := subject.(string); ok {
		subj = s
	}
	raw := fmt.Sprintf("From: alice@example.com\r\nTo: bob@example.org\r\n"+
		"Subject: %s\r\nDate: %s\r\nMessage-ID: <fake-%d@fake.local>\r\n"+
		"MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s\r\n",
		subj, sent, msgID, body)
	var compressed bytes.Buffer
	w := zlib.NewWriter(&compressed)
	if _, err := w.Write([]byte(raw)); err != nil {
		return nil, fmt.Errorf("fakevault: compressing raw MIME: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("fakevault: closing raw MIME compressor: %w", err)
	}
	return compressed.Bytes(), nil
}
