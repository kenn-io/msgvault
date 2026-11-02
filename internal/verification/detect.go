// Package verification identifies credential-like values in archived messages.
// It performs no I/O and does not establish sender authenticity. Findings are
// claims derived from untrusted content, not proof that a link is safe to open.
package verification

import (
	"net/mail"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
)

// Input contains complete canonical content, before excerpts or pagination.
// MessageType is deliberately not an allowlist: new channels and long direct
// messages must receive the same detection as email.
type Input struct {
	MessageType, Subject, Snippet, BodyText, BodyHTML string
	SenderEmail, SenderPhone, ListID                  string
	IsFromMe                                          bool
	SentAt, ReceivedAt                                time.Time
}

// EffectiveReceivedAt returns receipt time when available, falling back to
// sent time for archives and providers that do not record it.
func (in Input) EffectiveReceivedAt() time.Time {
	if !in.ReceivedAt.IsZero() {
		return in.ReceivedAt
	}
	return in.SentAt
}

type Secret struct {
	Kind  string // code or link
	Value string
	Host  string
}

// Finding is internal detector output. Values must never be serialized in a
// masked response. Neither expiry nor elapsed time makes a value safe to reveal.
type Finding struct {
	Kind       string
	Confidence string
	Signals    []string
	Values     []Secret
	ExpiresIn  time.Duration
}

type cue struct {
	text string
	kind string
}

const kindOTPCode = "otp_code"

var cues = []cue{
	{"verification code", kindOTPCode}, {"security code", kindOTPCode},
	{"one-time", kindOTPCode}, {"one time", kindOTPCode}, {"otp", kindOTPCode},
	{"passcode", kindOTPCode}, {"your code", kindOTPCode}, {"login code", kindOTPCode},
	{"sign-in code", kindOTPCode}, {"sign in code", kindOTPCode},
	{"authentication code", kindOTPCode}, {"do not share this code", kindOTPCode},
	{"is your", kindOTPCode},
	{"magic link", "magic_link"}, {"sign in", "magic_link"}, {"sign-in", "magic_link"},
	{"log in", "magic_link"}, {"login link", "magic_link"},
	{"reset your password", "password_reset"}, {"password reset", "password_reset"},
	{"reset password", "password_reset"},
	{"confirm your subscription", "double_opt_in"}, {"confirm subscription", "double_opt_in"},
	{"confirm your email", "account_confirmation"}, {"verify your email", "account_confirmation"},
	{"confirm your account", "account_confirmation"}, {"activate your account", "account_confirmation"},
	{"verification link", "account_confirmation"},
	{"code de vérification", kindOTPCode}, {"code de sécurité", kindOTPCode},
	{"bestätigungscode", kindOTPCode}, {"sicherheitscode", kindOTPCode},
	{"código de verificación", kindOTPCode}, {"código de seguridad", kindOTPCode},
	{"codice di verifica", kindOTPCode}, {"код подтверждения", kindOTPCode},
	{"код безопасности", kindOTPCode}, {"验证码", kindOTPCode}, {"驗證碼", kindOTPCode},
	{"認証コード", kindOTPCode}, {"인증 코드", kindOTPCode},
}

var (
	codePattern     = regexp.MustCompile(`(?:[A-Z]-[0-9]{4,8}|[0-9]{3}[ -][0-9]{3}|[A-Z0-9]{4,8})`)
	urlPattern      = regexp.MustCompile(`(?i)https?://[^\s\p{Z}\x{0085}\v<>"'\x60]+`)
	tokenPattern    = regexp.MustCompile(`^[A-Za-z0-9_-]{16,}$`)
	codeWordPattern = regexp.MustCompile(`\bcode\b`)
	expiryPattern   = regexp.MustCompile(`(?i)(?:expires? in|valid for)\s+([0-9]{1,5})\s+(minutes?|mins?|hours?|hrs?|seconds?|secs?)\b`)
)

var knownDomains = []string{
	"accounts.google.com", "accountprotection.microsoft.com", "apple.com",
	"github.com", "google.com", "microsoft.com",
}

var cuePatterns = func() []*regexp.Regexp {
	patterns := make([]*regexp.Regexp, len(cues))
	for i, c := range cues {
		words := strings.Fields(c.text)
		for j, word := range words {
			words[j] = regexp.QuoteMeta(word)
		}
		patterns[i] = regexp.MustCompile(strings.Join(words, `[\s\p{Z}]+`))
	}
	return patterns
}()

type cueMatch struct {
	start, end int
	maxEnd     int // latest end among this match and all preceding matches
	kind       string
}

func cueMatches(text string) []cueMatch {
	// Lowercasing can change byte widths (for example U+0130); all offsets
	// below refer to the normalized string, never to the original content.
	var found []cueMatch
	urls := urlPattern.FindAllStringIndex(text, -1)
	for index, c := range cues {
		urlIndex := 0
		for _, loc := range cuePatterns[index].FindAllStringIndex(text, -1) {
			i, end := loc[0], loc[1]
			for urlIndex < len(urls) && urls[urlIndex][1] <= i {
				urlIndex++
			}
			// A path such as /sign-in is not independent textual context.
			if urlIndex < len(urls) && end > urls[urlIndex][0] {
				continue
			}
			if wordBoundary(text, i, end) {
				// "is your" alone is ordinary prose, not a credential cue.
				if c.text != "is your" || hasCodeAfter(text, end, urls) {
					found = append(found, cueMatch{start: i, end: end, kind: c.kind})
				}
			}
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].start < found[j].start })
	for i := range found {
		found[i].maxEnd = found[i].end
		if i > 0 {
			found[i].maxEnd = max(found[i].maxEnd, found[i-1].maxEnd)
		}
	}
	return found
}

func hasCodeAfter(text string, start int, urls [][]int) bool {
	for _, loc := range codeWordPattern.FindAllStringIndex(text[start:min(len(text), start+60)], -1) {
		begin, end := start+loc[0], start+loc[1]
		if !wordBoundary(text, begin, end) {
			continue
		}
		index := sort.Search(len(urls), func(i int) bool { return urls[i][1] > begin })
		if index < len(urls) && end > urls[index][0] {
			continue
		}
		return true
	}
	return false
}

func wordBoundary(text string, start, end int) bool {
	// CJK cues have no whitespace boundaries. ASCII/European words do.
	first, _ := utf8.DecodeRuneInString(text[start:end])
	last, _ := utf8.DecodeLastRuneInString(text[start:end])
	isWord := func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' }
	if first < 0x3000 && start > 0 {
		previous, _ := utf8.DecodeLastRuneInString(text[:start])
		if previous < 0x3000 && isWord(previous) {
			return false
		}
	}
	if last < 0x3000 && end < len(text) {
		next, _ := utf8.DecodeRuneInString(text[end:])
		if next < 0x3000 && isWord(next) {
			return false
		}
	}
	return true
}

func nearCue(text string, start, end int, matches []cueMatch) bool {
	// A rune occupies at most four bytes. Skip earlier cues only when their
	// ends are distant: whitespace-expanded cues can start far before a value.
	first := sort.Search(len(matches), func(i int) bool { return matches[i].maxEnd >= start-320 })
	for _, c := range matches[first:] {
		if c.start > end+320 {
			break
		}
		if c.end < start {
			if start-c.end > 320 || utf8.RuneCountInString(text[c.end:start]) > 80 {
				continue
			}
		} else if end < c.start && utf8.RuneCountInString(text[end:c.start]) > 80 {
			continue
		}
		return true
	}
	return false
}

// normalizedOffsets maps increasing original offsets into their lowercased
// positions without repeatedly normalizing the entire preceding body.
func normalizedOffsets(original string) func(int) int {
	previous, normalized := 0, 0
	return func(offset int) int {
		normalized += len(strings.ToLower(original[previous:offset]))
		previous = offset
		return normalized
	}
}

// Credential words must describe a complete endpoint component or query key,
// rather than occur inside an article slug (for example "author"). Compound
// names such as password-reset and auth_token retain their meaning.
func credentialEndpoint(component string) bool {
	component = strings.ToLower(component)
	switch component {
	case "sign-in", "sign_in", "log-in", "log_in":
		return true
	}
	found := false
	for _, word := range strings.FieldsFunc(component, func(r rune) bool { return r == '-' || r == '_' }) {
		switch word {
		case "verify", "verification", "confirm", "confirmation", "activate", "activation", "magic", "login", "signin", "reset", "token", "otp", "auth", "authenticate", "authentication", "authorize", "authorization", "oauth", "oauth2":
			found = true
		case "password", "email", "account", "link", "code", "access":
			// Qualifiers do not establish a credential endpoint by themselves.
		default:
			return false
		}
	}
	return found
}

func credentialURL(u *url.URL) (endpoint, opaque, payload bool) {
	endpoint, opaque, payload = credentialURLParts(u.Path, u.Query())
	// Fragments can carry either query parameters (#token=...) or a client
	// route (#/login?token=...). Apply the same rules without following links.
	path, query, hasQuery := strings.Cut(u.Fragment, "?")
	if !hasQuery && strings.Contains(path, "=") {
		query, path = path, ""
	}
	values, _ := url.ParseQuery(query)
	fragmentEndpoint, fragmentOpaque, fragmentPayload := credentialURLParts(path, values)
	return endpoint || fragmentEndpoint, opaque || fragmentOpaque, payload || fragmentPayload
}

func credentialURLParts(path string, query url.Values) (endpoint, opaque, payload bool) {
	for component := range strings.SplitSeq(path, "/") {
		if credentialEndpoint(component) {
			endpoint = true
		} else if tokenPattern.MatchString(component) {
			opaque = true
		} else if loc := codePattern.FindStringIndex(component); loc != nil && loc[0] == 0 && loc[1] == len(component) && codeShape(component) {
			payload = true
		}
	}
	for key, values := range query {
		credentialKey := credentialEndpoint(key)
		endpoint = endpoint || credentialKey
		credentialKey = credentialKey || slices.Contains([]string{"code", "id", "ticket", "key", "nonce", "state"}, strings.ToLower(key))
		for _, value := range values {
			opaque = opaque || tokenPattern.MatchString(value)
			payload = payload || credentialKey && strings.TrimSpace(value) != ""
		}
	}
	return endpoint, opaque, payload || opaque
}

type htmlLink struct {
	value      string
	start, end int
}

// HTML attributes are not visible codes. Keep href values separate from the
// rendered text, with their anchor's position for nearby-cue checks.
func htmlContent(raw string) (string, []htmlLink) {
	if raw == "" {
		return "", nil
	}
	var text strings.Builder
	var links []htmlLink
	var anchors []int
	hidden, hiddenDepth := "", 0
	space := false
	write := func(value string) {
		for _, r := range value {
			if unicode.IsSpace(r) {
				space = true
				continue
			}
			if space && text.Len() > 0 {
				text.WriteByte(' ')
			}
			space = false
			text.WriteRune(r)
		}
	}
	// Tokenization has no DOM nesting limit: deeply nested mail must not lose
	// its credentials when a tree parser rejects it.
	tokens := html.NewTokenizer(strings.NewReader(raw))
	for {
		kind := tokens.Next()
		if kind == html.ErrorToken {
			for _, index := range anchors {
				if index >= 0 {
					links[index].end = text.Len()
				}
			}
			return text.String(), links
		}
		token := tokens.Token()
		if hidden != "" {
			if token.Data == hidden {
				switch kind {
				case html.StartTagToken:
					hiddenDepth++
				case html.EndTagToken:
					hiddenDepth--
					if hiddenDepth == 0 {
						hidden = ""
					}
				default:
					// Other token kinds do not change hidden-element nesting.
				}
			}
			continue
		}
		if kind == html.TextToken {
			write(token.Data)
			continue
		}
		if kind != html.StartTagToken && kind != html.SelfClosingTagToken && kind != html.EndTagToken {
			continue
		}
		switch token.Data {
		case "head":
			// Head can close implicitly before visible body content. Only its
			// non-visible children below need suppression.
		case "script", "style", "template", "title":
			if kind != html.EndTagToken {
				hidden, hiddenDepth = token.Data, 1
			}
		case "address", "article", "aside", "blockquote", "body", "center", "details", "dialog", "dir", "div", "dl", "dt", "dd",
			"fieldset", "figcaption", "figure", "footer", "form", "header", "hgroup", "html", "legend", "main", "menu", "nav", "search", "section", "summary",
			"p", "pre", "listing", "plaintext", "xmp", "h1", "h2", "h3", "h4", "h5", "h6", "br", "hr", "li", "ul", "ol",
			"table", "caption", "colgroup", "tbody", "thead", "tfoot", "tr", "td", "th":
			space = true
		case "a":
			if kind != html.EndTagToken {
				index := -1
				for _, attr := range token.Attr {
					if attr.Key == "href" {
						index = len(links)
						links = append(links, htmlLink{attr.Val, text.Len(), text.Len()})
						break
					}
				}
				anchors = append(anchors, index)
			} else if kind == html.EndTagToken && len(anchors) > 0 {
				index := anchors[len(anchors)-1]
				anchors = anchors[:len(anchors)-1]
				if index >= 0 {
					links[index].end = text.Len()
				}
			}
		}
	}
}

// Detect examines all canonical fields. There is no channel-specific early
// return and no body truncation: a code after a long chat preamble still needs
// protection. The result is deterministic and independent of the wall clock.
func Detect(in Input) *Finding {
	visibleHTML, hrefs := htmlContent(in.BodyHTML)
	fields := []string{in.Subject, in.Snippet, in.BodyText, visibleHTML}
	f := &Finding{Confidence: "medium"}
	seen := make(map[string]bool)
	add := func(s Secret) {
		if !seen[s.Value] {
			seen[s.Value] = true
			f.Values = append(f.Values, s)
		}
	}
	for fieldIndex, original := range fields {
		prefix := 0
		// A subject often supplies the cue while the body starts with only
		// the value. Keep that context when inspecting the other fields.
		if fieldIndex > 0 && in.Subject != "" {
			original = in.Subject + "\n" + original
			prefix = len(in.Subject) + 1
		}
		text := strings.ToLower(original)
		matches := cueMatches(text)
		var linkMatches []cueMatch
		for _, c := range matches {
			if c.kind != kindOTPCode {
				c.maxEnd = c.end
				if len(linkMatches) > 0 {
					c.maxEnd = max(c.maxEnd, linkMatches[len(linkMatches)-1].maxEnd)
				}
				linkMatches = append(linkMatches, c)
			}
			if f.Kind == "" || kindRank(c.kind) > kindRank(f.Kind) {
				f.Kind = c.kind
			}
		}
		if len(matches) > 0 {
			signal := "body_cue"
			if fieldIndex == 0 {
				signal = "subject_cue"
			}
			if !slices.Contains(f.Signals, signal) {
				f.Signals = append(f.Signals, signal)
			}
		}
		// Scan original casing for code shape, and normalize each prefix for
		// cue offsets rather than assuming Unicode lowercasing preserves bytes.
		urls := urlPattern.FindAllStringIndex(original, -1)
		urlIndex := 0
		codeOffset := normalizedOffsets(original)
		for _, loc := range codePattern.FindAllStringIndex(original, -1) {
			for urlIndex < len(urls) && urls[urlIndex][1] <= loc[0] {
				urlIndex++
			}
			if urlIndex < len(urls) && loc[1] > urls[urlIndex][0] {
				continue
			}
			value := original[loc[0]:loc[1]]
			if !wordBoundary(original, loc[0], loc[1]) || !codeShape(value) {
				continue
			}
			start := codeOffset(loc[0])
			end := start + len(strings.ToLower(value))
			if nearCue(text, start, end, matches) {
				add(Secret{Kind: "code", Value: value})
			}
		}
		candidates := make([]htmlLink, 0, len(urls)+len(hrefs))
		for _, loc := range urls {
			candidates = append(candidates, htmlLink{strings.TrimRight(original[loc[0]:loc[1]], ".,;!)]}"), loc[0], loc[1]})
		}
		if fieldIndex == 3 {
			for _, link := range hrefs {
				candidates = append(candidates, htmlLink{link.value, link.start + prefix, link.end + prefix})
			}
		}
		sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].start < candidates[j].start })
		urlOffset := normalizedOffsets(original)
		for _, link := range candidates {
			value := link.value
			u, err := url.Parse(value)
			if err != nil || u.Hostname() == "" || u.Scheme != "http" && u.Scheme != "https" {
				continue
			}
			endpoint, opaque, payload := credentialURL(u)
			if !payload {
				continue
			}
			start := urlOffset(link.start)
			end := start + len(strings.ToLower(original[link.start:link.end]))
			// Explicit link context can identify an opaque credential URL even
			// when its endpoint is generic. An OTP cue alone is not link context.
			if !endpoint && (!opaque || !nearCue(text, start, end, linkMatches)) {
				continue
			}
			if !opaque && !nearCue(text, start, end, matches) {
				continue
			}
			add(Secret{Kind: "link", Value: value, Host: strings.ToLower(u.Hostname())})
			if f.Kind == "" {
				f.Kind = "magic_link"
			}
		}
		if f.ExpiresIn == 0 {
			if m := expiryPattern.FindStringSubmatch(text); len(m) > 0 {
				n, _ := strconv.Atoi(m[1])
				unit := time.Minute
				if strings.HasPrefix(m[2], "h") {
					unit = time.Hour
				} else if strings.HasPrefix(m[2], "s") {
					unit = time.Second
				}
				f.ExpiresIn = time.Duration(n) * unit
			}
		}
	}
	if f.Kind == "" {
		return nil
	}
	if len(f.Values) > 0 {
		f.Confidence = "high"
		// A numeric code in a generic sign-in message is still an OTP.
		if f.Kind == "magic_link" && slices.ContainsFunc(f.Values, func(value Secret) bool { return value.Kind == "code" }) {
			f.Kind = kindOTPCode
		}
	}
	address := in.SenderEmail
	if parsed, err := mail.ParseAddress(address); err == nil {
		address = parsed.Address
	}
	local, domain, _ := strings.Cut(strings.ToLower(address), "@")
	if slices.Contains([]string{"noreply", "no-reply", "verify", "security", "auth", "account", "notifications"}, local) {
		f.Signals = append(f.Signals, "sender_local:"+local)
	}
	if slices.Contains(knownDomains, domain) {
		f.Signals = append(f.Signals, "domain:known")
	}
	if in.ListID != "" {
		f.Signals = append(f.Signals, "list_id")
	}
	return f
}

func codeShape(s string) bool {
	digits, letters := 0, 0
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case r >= 'A' && r <= 'Z':
			letters++
		}
	}
	return digits >= 4 && letters <= 1 || digits > 0 && letters > 0 && len(s) >= 6 && len(s) <= 8
}

func kindRank(kind string) int {
	switch kind {
	case "password_reset":
		return 5
	case "double_opt_in":
		return 4
	case "account_confirmation":
		return 3
	case kindOTPCode:
		return 2
	default:
		return 1
	}
}
