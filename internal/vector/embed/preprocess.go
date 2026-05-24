package embed

import (
	"html"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

// PreprocessConfig controls pre-embedding transformations.
type PreprocessConfig struct {
	StripQuotes        bool
	StripSignatures    bool
	StripHTML          bool
	StripBase64        bool
	StripURLTracking   bool
	CollapseWhitespace bool
}

var (
	// reReplyPreamble matches "On <date>, <name> wrote:" followed by one
	// or more quoted lines. A quoted line starts with one or more `>`
	// characters, optionally followed by a single space or tab, so nested
	// quotes (">>", ">>>") and clients that omit the space after `>` are
	// both recognised.
	reReplyPreamble = regexp.MustCompile(`(?m)^On [^\n]+wrote:\s*\n(?:>+[ \t]?.*\n?)+`)
	// reSigDelim matches a signature delimiter "\n-- \n" (or "\n--\n") through
	// end of string.
	reSigDelim = regexp.MustCompile(`\n--\s*\n[\s\S]*\z`)
	// reQuoteLine matches standalone quoted lines: one or more `>`
	// characters followed by optional whitespace and the rest of the
	// line. Handles ">", "> foo", ">foo", ">> nested", ">>>deep".
	reQuoteLine = regexp.MustCompile(`(?m)^>+[ \t]?.*\n?`)

	// reStyleBlock and reScriptBlock match <style>...</style> and
	// <script>...</script> including their CSS/JS contents. Applied
	// before the generic HTML-tag stripper so the wrapped contents do
	// not survive to be embedded as gibberish. (?is) = case-insensitive
	// + dotall so the body can span newlines.
	reStyleBlock  = regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	reScriptBlock = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	// reHTMLTag matches a single HTML tag.
	//
	// The leading `</?` allows opening or closing tags. The tag-name
	// rule (`[a-zA-Z][a-zA-Z0-9-]*`) is intentionally strict so the
	// stripper does NOT eat angle-bracketed prose that resembles a tag
	// but isn't one:
	//
	//   <john@example.com>          – inline email address, `@` rejects
	//   <https://example.com>       – URL, the `:` rejects
	//   2 < 3 and 5 > 4             – math, space-then-digit rejects
	//   <Aug 6, 2026>               – date placeholder, space rejects
	//
	// The optional whitespace-then-attributes group `(?:\s[^>]{0,400})?`
	// only fires when an attribute would actually follow — every real
	// HTML tag puts whitespace between the name and the first
	// attribute. The {0,400} cap inside the attribute body bounds
	// backtracking on adversarial input. The trailing `/?>` covers
	// self-closing tags like <br/> and <img .../>.
	reHTMLTag = regexp.MustCompile(`</?[a-zA-Z][a-zA-Z0-9-]*(?:\s[^>]{0,400})?\s*/?>`)

	// reDataURI matches data:...;base64,XXXX blobs (typically inline
	// images that leaked into body_text). These can be tens of KB each
	// and contribute zero semantic value to an email's vector. The
	// {0,128} content-type cap is defensive; the base64 trailing chunk
	// is greedy to remove the whole payload. (?i) for the case-insensitive
	// `data:` / `base64` tokens per RFC 2397.
	reDataURI = regexp.MustCompile(`(?i)data:[a-zA-Z0-9./+\-]{0,128};base64,[A-Za-z0-9+/]+={0,2}`)
	// reBase64Blob matches a run of base64-ish characters 200+ long with
	// no whitespace. Empirically this only matches embedded binary data
	// (inline images, embedded PDFs, S/MIME blocks) — real prose breaks
	// up well before 200 unbroken letters.
	//
	// '/' is deliberately excluded from the character class so the
	// pattern does NOT match URL paths (e.g. signed S3 URLs and
	// DocuSign envelope IDs can run 200+ chars with no whitespace and
	// they contain plenty of '/'). Real base64 streams without slashes
	// are still by far the dominant pollution pattern in body_text.
	reBase64Blob = regexp.MustCompile(`[A-Za-z0-9+]{200,}={0,2}`)

	// reURL matches an http(s) URL up to the next whitespace, quote, or
	// angle bracket. Used as the seed for tracking-param stripping; the
	// real parsing happens via net/url.
	reURL = regexp.MustCompile(`https?://[^\s"'<>)]+`)

	// trackingParams is the set of query-string keys that exist purely
	// for analytics/attribution and carry no semantic content for a
	// search corpus. Stripping them collapses 30 visually-distinct copies
	// of the same campaign URL into one canonical URL.
	trackingParams = map[string]bool{
		"utm_source": true, "utm_medium": true, "utm_campaign": true,
		"utm_term": true, "utm_content": true, "utm_id": true,
		"utm_name": true, "utm_brand": true, "utm_social": true,
		"fbclid": true, "gclid": true, "dclid": true, "gbraid": true,
		"wbraid": true, "msclkid": true, "yclid": true, "twclid": true,
		"mc_cid": true, "mc_eid": true, "ml_subscriber": true,
		"_hsenc": true, "_hsmi": true, "hsCtaTracking": true,
		"vero_conv": true, "vero_id": true, "ck_subscriber_id": true,
		"_branch_match_id": true, "ref": true, "ref_src": true,
		"s_cid": true, "icid": true, "spm": true,
	}

	// reTrailingHWS strips trailing horizontal whitespace at end of
	// each line. Run before reMultiNewline so a "\n  \n" sequence (a
	// blank line that happens to contain spaces) collapses with its
	// neighbours.
	reTrailingHWS = regexp.MustCompile(`(?m)[ \t]+$`)
	// reMultiNewline collapses 3+ consecutive newlines to two. Two
	// preserves paragraph breaks; more is purely whitespace bloat from
	// HTML→text conversion.
	reMultiNewline = regexp.MustCompile(`\n{3,}`)
	// reHorizontalRun collapses runs of spaces/tabs to a single space.
	// Stops at newlines so indentation across lines is unaffected.
	reHorizontalRun = regexp.MustCompile(`[ \t]{2,}`)
)

// Preprocess produces the string fed to the embedder. It optionally strips
// reply quotes and signature blocks, trims whitespace, prepends a
// "Subject: <subject>\n\n" prefix when subject is non-empty, and truncates
// the result to maxChars runes (not bytes — embedders count characters, and
// a byte-based cap would shortchange multi-byte scripts like CJK). Returns
// the preprocessed string and a boolean indicating whether truncation
// occurred. A maxChars value <= 0 disables truncation.
func Preprocess(subject, body string, maxChars int, cfg PreprocessConfig) (string, bool) {
	// Normalize CRLF → LF up front so the line-oriented regexes below
	// (reTrailingHWS, reMultiNewline) and the [ \t] horizontal-whitespace
	// matchers behave the same regardless of mail-client line endings.
	s := strings.ReplaceAll(body, "\r\n", "\n")
	// Strip base64 / data: URIs before HTML so an oversized
	// `<img src="data:image/...;base64,...">` (which can exceed
	// reHTMLTag's 500-char bound and slip past the tag stripper) loses
	// its payload first, leaving a small enough tag for reHTMLTag to
	// then sweep up.
	if cfg.StripBase64 {
		s = reDataURI.ReplaceAllString(s, " ")
		s = reBase64Blob.ReplaceAllString(s, " ")
	}
	if cfg.StripHTML {
		s = reStyleBlock.ReplaceAllString(s, " ")
		s = reScriptBlock.ReplaceAllString(s, " ")
		s = reHTMLTag.ReplaceAllString(s, " ")
		s = html.UnescapeString(s)
	}
	if cfg.StripURLTracking {
		s = stripTrackingParams(s)
	}
	if cfg.StripQuotes {
		s = reReplyPreamble.ReplaceAllString(s, "")
		s = reQuoteLine.ReplaceAllString(s, "")
	}
	if cfg.StripSignatures {
		s = reSigDelim.ReplaceAllString(s, "")
	}
	if cfg.CollapseWhitespace {
		s = reTrailingHWS.ReplaceAllString(s, "")
		s = reHorizontalRun.ReplaceAllString(s, " ")
		s = reMultiNewline.ReplaceAllString(s, "\n\n")
	}
	s = strings.TrimSpace(s)

	var prefix string
	if subject != "" {
		prefix = "Subject: " + subject + "\n\n"
	}
	combined := prefix + s

	if maxChars <= 0 {
		return combined, false
	}
	// Fast path: if every byte is ASCII, len == rune count and we
	// can skip the scan. Otherwise walk runes forward to find the
	// cut point at rune boundary maxChars.
	if len(combined) <= maxChars {
		return combined, false
	}
	byteOffset, runes := 0, 0
	for byteOffset < len(combined) && runes < maxChars {
		_, size := utf8.DecodeRuneInString(combined[byteOffset:])
		byteOffset += size
		runes++
	}
	if runes < maxChars {
		// Fewer runes than the cap — nothing to truncate.
		return combined, false
	}
	if byteOffset == len(combined) {
		// Exactly maxChars runes and no more — no truncation.
		return combined, false
	}
	return combined[:byteOffset], true
}

// stripTrackingParams rewrites every http(s) URL in s, removing any query
// parameter whose key is in trackingParams. Non-URL text is untouched.
// Malformed URLs are returned as-is so we never lose user content to a
// parse failure.
func stripTrackingParams(s string) string {
	return reURL.ReplaceAllStringFunc(s, func(raw string) string {
		// Trailing punctuation ("...visit https://x.com/path.") is
		// common in prose. Peel it off so net/url doesn't reject the
		// URL, then re-attach. We only peel chars that are never
		// valid as the final character of a URL.
		trailing := ""
		for len(raw) > 0 {
			c := raw[len(raw)-1]
			if c == '.' || c == ',' || c == ';' || c == ':' || c == '!' || c == '?' || c == ')' || c == ']' {
				trailing = string(c) + trailing
				raw = raw[:len(raw)-1]
				continue
			}
			break
		}
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return raw + trailing
		}
		q := u.Query()
		dropped := false
		for k := range q {
			if trackingParams[strings.ToLower(k)] {
				q.Del(k)
				dropped = true
			}
		}
		if !dropped {
			return raw + trailing
		}
		u.RawQuery = q.Encode()
		return u.String() + trailing
	})
}
