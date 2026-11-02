package verification

import (
	"html"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	CodePlaceholder    = "[verification code masked]"
	LinkPlaceholder    = "[verification link masked]"
	ContentPlaceholder = "[verification content masked]"
)

// Metadata exposes derived claims without detector values. Masked and
// MaskedFields describe server behavior; the remaining fields are untrusted
// claims about archived content. Expiry never authorizes automatic reveal.
type Metadata struct {
	Kind         string     `json:"kind"`
	Confidence   string     `json:"confidence"`
	Signals      []string   `json:"signals,omitempty"`
	Sender       string     `json:"sender,omitempty"`
	ReceivedAt   time.Time  `json:"received_at"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
	Expired      bool       `json:"expired"`
	Hosts        []string   `json:"hosts,omitempty"`
	Masked       bool       `json:"masked"`
	MaskedFields []string   `json:"masked_fields,omitempty"`
	Reveal       string     `json:"reveal"`
}

type replacement struct {
	pattern     *regexp.Regexp
	placeholder string
}

// Masker is immutable and safe for reuse. Callers must apply it to explicit
// human-content fields of copied DTOs before deriving excerpts or paging.
// Structural identifiers and timestamps are deliberately outside this API.
type Masker struct {
	replacements []replacement
	withhold     bool
	withholdHTML bool
}

func NewMasker(f *Finding) *Masker {
	m := &Masker{}
	if f == nil {
		return m
	}
	m.withholdHTML = f.Kind != "" || len(f.Values) > 0
	// Bound regexp construction and repeated full-field scans before any
	// cloning or compilation. Dense credential content is withheld in full.
	if len(f.Values) > 64 {
		m.withhold = true
		return m
	}
	remaining := 8 * 1024
	for _, secret := range f.Values {
		if len(secret.Value) > remaining {
			m.withhold = true
			return m
		}
		remaining -= len(secret.Value)
	}
	values := slices.Clone(f.Values)
	for _, secret := range f.Values {
		if secret.Kind == "link" {
			for _, payload := range linkPayloads(secret.Value) {
				// A payload inside the placeholder would make repeated masking
				// unstable. Withhold the field rather than expose short tokens.
				if strings.Contains(CodePlaceholder+LinkPlaceholder+ContentPlaceholder, payload) {
					m.withhold = true
					return m
				}
				values = append(values, Secret{Kind: "link", Value: payload})
				if len(values) > 64 || len(payload) > remaining {
					m.withhold = true
					return m
				}
				remaining -= len(payload)
			}
		}
	}
	sort.SliceStable(values, func(i, j int) bool { return len(values[i].Value) > len(values[j].Value) })
	seen := make(map[string]bool)
	for _, secret := range values {
		if secret.Value == "" || seen[secret.Value] {
			continue
		}
		seen[secret.Value] = true
		placeholder := CodePlaceholder
		if secret.Kind == "link" {
			placeholder = LinkPlaceholder
		}
		forms := []string{secret.Value, html.EscapeString(secret.Value), url.QueryEscape(secret.Value), url.PathEscape(secret.Value)}
		var alternatives []string
		for _, form := range forms {
			alternatives = append(alternatives, encodedPattern(form))
		}
		// Grouped codes share literal and encoded forms across MIME variants,
		// including alphanumeric values split by HTML or whitespace.
		compact := strings.NewReplacer(" ", "", "-", "").Replace(secret.Value)
		if len(secret.Value) > 2 && secret.Value[0] >= 'A' && secret.Value[0] <= 'Z' && secret.Value[1] == '-' && allDigits(compact[1:]) {
			compact = compact[1:]
		}
		if allDigits(compact) || credentialCodeComponent(compact) {
			parts := make([]string, 0, len(compact))
			for _, r := range compact {
				parts = append(parts, encodedPattern(string(r)))
			}
			alternatives = append(alternatives, strings.Join(parts, numericSeparatorPattern))
		}
		pattern, err := regexp.Compile(strings.Join(alternatives, "|"))
		if err != nil {
			m.withhold = true
			return m
		}
		m.replacements = append(m.replacements, replacement{pattern, placeholder})
	}
	return m
}

// linkPayloads follows the detector's URL payload rules without treating
// ordinary path components or navigation parameters as credentials.
func linkPayloads(value string) []string {
	return CredentialLinkSearchValues(value)
}

// Grouping can mix literal, HTML-entity and percent-encoded whitespace.
// Build the fixed separator expression once, rather than normalizing caller
// content and changing unrelated text. HTML fragments may split copied digits.
var numericSeparatorPattern = func() string {
	alternatives := []string{`[\s\p{Z}\x{0085}\v+-]`, `&(?:nbsp|Tab|NewLine);`, `<[^>]*>`}
	for _, separator := range "- \t\n\r\v\f\u0085\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000" {
		alternatives = append(alternatives, encodedPattern(string(separator)))
	}
	return "(?:" + strings.Join(alternatives, "|") + ")*"
}()

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// encodedPattern accepts mixed-case percent escapes while preserving the case
// of literal token characters. HTML entities can also use decimal/hex forms.
func encodedPattern(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '%' && i+2 < len(s) {
			if _, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
				b.WriteString(`(?i:` + regexp.QuoteMeta(s[i:i+3]) + `)`)
				i += 3
				continue
			}
		}
		if strings.HasPrefix(s[i:], "&amp;") {
			b.WriteString(`(?:&amp;|&#0*38;|&#[xX]0*26;)`)
			i += len("&amp;")
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		// HTML bodies may mix literal and numeric character references.
		// Percent-encoded bytes can likewise appear inside a copied value.
		b.WriteString("(?:" + regexp.QuoteMeta(string(r)) + "|&#0*" + strconv.Itoa(int(r)) + ";|&#[xX]0*(?i:" + strconv.FormatInt(int64(r), 16) + ");")
		var percent strings.Builder
		for _, value := range []byte(string(r)) {
			percent.WriteByte('%')
			if value < 16 {
				percent.WriteByte('0')
			}
			percent.WriteString(strconv.FormatUint(uint64(value), 16))
		}
		b.WriteString("|(?i:" + percent.String() + "))")
		i += size
	}
	return b.String()
}

// Text replaces all occurrences, including values repeated outside their cue.
func (m *Masker) Text(text string) string {
	if m.withhold && text != "" {
		return ContentPlaceholder
	}
	for _, r := range m.replacements {
		text = r.pattern.ReplaceAllString(text, r.placeholder)
	}
	return text
}

// HTML withholds markup from detected messages. Tags and attributes can split
// values across nodes, so literal replacements cannot reliably protect it.
func (m *Masker) HTML(markup string) string {
	if markup != "" && m.withholdHTML {
		return ContentPlaceholder
	}
	return markup
}

func (m *Masker) Strings(values []string) []string {
	if values == nil {
		return nil
	}
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = m.Text(value)
	}
	return out
}

// Describe projects safe metadata using an injected clock. Sender and host
// claims are scrubbed with the same values as the message itself.
func Describe(in Input, f *Finding, masked bool, fields []string, now time.Time) *Metadata {
	if f == nil {
		return nil
	}
	m := NewMasker(f)
	metadata := &Metadata{
		Kind: f.Kind, Confidence: f.Confidence, Signals: slices.Clone(f.Signals),
		Sender: m.Text(in.SenderEmail), ReceivedAt: in.EffectiveReceivedAt(), Masked: masked,
		MaskedFields: slices.Clone(fields),
		Reveal:       "requires verification.read on the message source or an owner session started with --unmask-verification",
	}
	if metadata.Sender == "" {
		metadata.Sender = m.Text(in.SenderPhone)
	}
	if f.ExpiresIn > 0 && !in.SentAt.IsZero() {
		expires := in.SentAt.Add(f.ExpiresIn)
		metadata.ExpiresAt = &expires
		metadata.Expired = !now.Before(expires)
	}
	for _, value := range f.Values {
		if value.Kind == "link" && value.Host != "" {
			host := m.Text(value.Host)
			if !slices.Contains(metadata.Hosts, host) {
				metadata.Hosts = append(metadata.Hosts, host)
			}
		}
	}
	return metadata
}

// ScrubText is best-effort for isolated text without canonical context. It is
// unsuitable as the protection boundary for cropped excerpts or paged bodies.
func ScrubText(text string) string {
	return NewMasker(Detect(Input{BodyText: text})).Text(text)
}
