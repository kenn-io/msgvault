package embed

// IMPORTANT: any change in this file that would shift Preprocess()'s
// output for an unchanged PreprocessConfig (new regex, modified
// existing regex, new entry in trackingParams, new default-on
// transform) MUST be paired with a bump of preprocessVersion in
// internal/vector/config.go. That constant feeds the generation
// fingerprint so flipping it forces operators to --full-rebuild rather
// than silently embedding new messages under a different policy from
// the already-active vectors.

import (
	"html"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

// PreprocessConfig controls pre-embedding transformations.
//
// MaxBodyRunes, when > 0, applies a rune-count cap on the body
// *between* the cheap pollution-removal pass (CRLF normalize +
// StripBase64) and the heavier regex transforms (StripHTML,
// StripURLTracking, quote/signature, whitespace collapse). The
// ordering matters: a body whose first megabyte is an inline base64
// PNG would be capped to "just the base64" if the cap fired on raw
// input, losing every meaningful word that follows the blob. Doing
// the cheap strip first reclaims that space for the prose tail
// before the cap looks at the result.
type PreprocessConfig struct {
	StripQuotes        bool
	StripSignatures    bool
	StripHTML          bool
	StripBase64        bool
	StripURLTracking   bool
	CollapseWhitespace bool
	MaxBodyRunes       int
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
	// Structure:
	//   `</?`                          opening or closing slash
	//   `[a-z][a-zA-Z0-9-]*`           tag name (lowercase-leading)
	//   `(?:\s+ATTR)*`                 zero or more attributes
	//   `\s*/?>`                       optional self-close + `>`
	//
	// where ATTR is `name=value` with a real `=` and a quoted or
	// unquoted value:
	//   `[a-zA-Z_:][a-zA-Z0-9_:.-]*\s*=\s*(?:"…"|'…'|[^\s>"']+)`
	//
	// Two constraints exist to keep angle-bracketed prose alive:
	//
	//   - The tag name must start with a lowercase ASCII letter. HTML
	//     emails overwhelmingly use lowercase tags (<p>, <table>,
	//     <br/>), while mail-merge / placeholder conventions are Title
	//     Case (<First Name>, <Company>, <Aug 6, 2026>). Restricting
	//     the leading char to lowercase preserves the placeholders at
	//     the cost of leaving rare uppercase `<P>`-style legacy HTML
	//     alone. The body of the name still allows mixed case so SVG
	//     and custom elements like `viewBox` or `myCustomElement` work.
	//
	//   - Attributes REQUIRE a literal `=value`. Bareword attribute
	//     names alone would match phrases like `<First Last>` — the
	//     `=` requirement is the only thing distinguishing
	//     `<tag attr=val>` from `<two words>`. Tradeoff: bareword bool
	//     attributes like `<input disabled>` no longer strip. Rare in
	//     body_text.
	//
	// Survival examples:
	//   <john@example.com> – `@` not a name body char, fails
	//   <https://x.com>    – `:` not a name body char, fails
	//   x < 3 and y > 4    – digit after `<`, fails the name-start
	//   <First Name>       – capital `F`, fails the lowercase-leading rule
	//   <Company>          – capital `C`, fails likewise
	//   <Aug 6, 2026>      – capital `A`, fails likewise
	//
	// `{0,400}` / `{1,400}` caps on the value bodies bound the maximum
	// tag length to a few KB per attribute, so a stray unmatched quote
	// inside a long body cannot swallow the rest of the message.
	reHTMLTag = regexp.MustCompile(
		`</?[a-z][a-zA-Z0-9-]*` +
			`(?:\s+[a-zA-Z_:][a-zA-Z0-9_:.-]*\s*=\s*` +
			`(?:"[^"]{0,400}"|'[^']{0,400}'|[^\s>"']{1,400}))*` +
			`\s*/?>`,
	)

	// reDataURI matches data:...;base64,XXXX blobs (typically inline
	// images that leaked into body_text). These can be tens of KB each
	// and contribute zero semantic value to an email's vector. The
	// {0,128} content-type cap is defensive; the base64 trailing chunk
	// is greedy to remove the whole payload. (?i) for the case-insensitive
	// `data:` / `base64` tokens per RFC 2397.
	reDataURI = regexp.MustCompile(`(?i)data:[a-zA-Z0-9./+\-]{0,128};base64,[A-Za-z0-9+/]+={0,2}`)
	// reBase64Blob and reBase64BlobWithSlash together catch bare base64
	// payloads (no `data:` prefix) that leaked into body_text. The
	// split exists because '/' is in both the base64 alphabet AND in
	// every URL path, so a single regex cannot use one length threshold
	// for both cases without either eating URL paths or missing real
	// base64 with slashes:
	//
	//   - reBase64Blob (no '/', 200+ chars). Catches dense letter+digit
	//     runs that prose never produces. Stays slash-free so URL paths
	//     between '/' separators (which are individually short — every
	//     '/' resets the run) are never matched as a whole.
	//   - reBase64BlobWithSlash (with '/', 300+ chars). Catches real
	//     base64 binary residue where '/' appears at the alphabet's
	//     natural ~1/64 frequency. The higher threshold is the lever
	//     that keeps URL paths safe: even long signed S3 / CloudFront
	//     URLs and GitHub blob paths rarely reach 300 unbroken
	//     base64-alphabet chars without a `.`, `?`, `&`, `_`, `-`, or
	//     `~` breaking the run, while embedded images or PDFs encoded
	//     inline routinely produce thousands of chars.
	//
	// Padding `={0,2}` mops up the standard base64 tail. Both patterns
	// are unbounded on the upper side; RE2 keeps the scan linear.
	reBase64Blob          = regexp.MustCompile(`[A-Za-z0-9+]{200,}={0,2}`)
	reBase64BlobWithSlash = regexp.MustCompile(`[A-Za-z0-9+/]{300,}={0,2}`)

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
		// Keys are stored lowercase because the lookup at the call site
		// normalises the query-param name to lowercase first (so
		// "?HsCtaTracking=" matches the same key as "?hsctatracking=").
		"_hsenc": true, "_hsmi": true, "hsctatracking": true,
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
	// then sweep up. This pass is also "cheap pollution removal"
	// relative to MaxBodyRunes — by running it before the cap, a
	// body whose noise dominates the first megabyte gets its prose
	// tail preserved instead of chopped off with the blob.
	if cfg.StripBase64 {
		s = reDataURI.ReplaceAllString(s, " ")
		s = reBase64Blob.ReplaceAllString(s, " ")
		s = reBase64BlobWithSlash.ReplaceAllString(s, " ")
	}
	bodyTruncated := false
	if cfg.MaxBodyRunes > 0 {
		walked := 0
		for i := range s {
			if walked >= cfg.MaxBodyRunes {
				s = s[:i]
				bodyTruncated = true
				break
			}
			walked++
		}
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
		return combined, bodyTruncated
	}
	// Fast path: if every byte is ASCII, len == rune count and we
	// can skip the scan. Otherwise walk runes forward to find the
	// cut point at rune boundary maxChars.
	if len(combined) <= maxChars {
		return combined, bodyTruncated
	}
	byteOffset, runes := 0, 0
	for byteOffset < len(combined) && runes < maxChars {
		_, size := utf8.DecodeRuneInString(combined[byteOffset:])
		byteOffset += size
		runes++
	}
	if runes < maxChars {
		// Fewer runes than the cap — nothing to truncate.
		return combined, bodyTruncated
	}
	if byteOffset == len(combined) {
		// Exactly maxChars runes and no more — no truncation.
		return combined, bodyTruncated
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
