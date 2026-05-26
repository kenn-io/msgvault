package embed

import (
	"strings"
	"testing"
	"unicode/utf8"

	assertpkg "github.com/stretchr/testify/assert"
)

func TestPreprocess(t *testing.T) {
	tests := []struct {
		name      string
		subject   string
		body      string
		maxChars  int
		cfg       PreprocessConfig
		checkWant bool // if true, assert out == want (even when want is "")
		want      string
		lenLE     int  // 0 = no check; N = require len(out) <= N
		wantValid bool // require utf8.ValidString(out)
		wantTrunc bool
	}{
		{
			name:      "PlainBody",
			subject:   "Hello",
			body:      "Hi there,\n\nLet's chat tomorrow.",
			maxChars:  1000,
			cfg:       PreprocessConfig{StripQuotes: true, StripSignatures: true},
			checkWant: true,
			want:      "Subject: Hello\n\nHi there,\n\nLet's chat tomorrow.",
			wantTrunc: false,
		},
		{
			name:      "StripsQuotedPreamble",
			subject:   "Re: plan",
			body:      "My reply.\n\nOn 2026-01-01, alice wrote:\n> previous message\n> more quote",
			maxChars:  1000,
			cfg:       PreprocessConfig{StripQuotes: true, StripSignatures: true},
			checkWant: true,
			want:      "Subject: Re: plan\n\nMy reply.",
			wantTrunc: false,
		},
		{
			name:      "StripsStandaloneQuoteLines",
			subject:   "",
			body:      "> nested quote 1\n> nested quote 2\nActual content.",
			maxChars:  1000,
			cfg:       PreprocessConfig{StripQuotes: true, StripSignatures: false},
			checkWant: true,
			want:      "Actual content.",
			wantTrunc: false,
		},
		{
			name:      "StripsSignature",
			subject:   "Hi",
			body:      "Body here.\n-- \nBob\nPhone: ...",
			maxChars:  1000,
			cfg:       PreprocessConfig{StripQuotes: true, StripSignatures: true},
			checkWant: true,
			want:      "Subject: Hi\n\nBody here.",
			wantTrunc: false,
		},
		{
			name:      "SignatureWithoutTrailingSpace",
			subject:   "Hi",
			body:      "Body.\n--\nBob",
			maxChars:  1000,
			cfg:       PreprocessConfig{StripQuotes: false, StripSignatures: true},
			checkWant: true,
			want:      "Subject: Hi\n\nBody.",
			wantTrunc: false,
		},
		{
			name:      "Truncates",
			subject:   "S",
			body:      strings.Repeat("x", 2000),
			maxChars:  100,
			cfg:       PreprocessConfig{},
			lenLE:     100,
			wantTrunc: true,
		},
		{
			// maxChars is a rune count, so 10 snowmen (3 bytes
			// each) should produce a 30-byte result.
			name:      "TruncateAtRuneBoundary",
			subject:   "",
			body:      strings.Repeat("☃", 50),
			maxChars:  10,
			cfg:       PreprocessConfig{},
			lenLE:     30,
			wantValid: true,
			wantTrunc: true,
		},
		{
			name:      "EmptySubjectAndBody",
			subject:   "",
			body:      "",
			maxChars:  1000,
			cfg:       PreprocessConfig{},
			checkWant: true,
			want:      "",
			wantTrunc: false,
		},
		{
			name:      "NoConfigPreservesEverything",
			subject:   "Hi",
			body:      "Hello\n> not stripped\n-- \nsig kept",
			maxChars:  1000,
			cfg:       PreprocessConfig{StripQuotes: false, StripSignatures: false},
			checkWant: true,
			want:      "Subject: Hi\n\nHello\n> not stripped\n-- \nsig kept",
			wantTrunc: false,
		},
		{
			name:      "MaxCharsZeroIsUnlimited",
			subject:   "S",
			body:      strings.Repeat("x", 100),
			maxChars:  0,
			cfg:       PreprocessConfig{},
			checkWant: true,
			want:      "Subject: S\n\n" + strings.Repeat("x", 100),
			wantTrunc: false,
		},
		{
			// Each snowman is 3 bytes. 100 bytes of snowmen = ~33
			// runes, well under the 100-rune cap, so no truncation
			// despite byte length comfortably exceeding maxChars
			// under the old byte-counting rule.
			name:      "MultiByteRunesUnderCap",
			subject:   "",
			body:      strings.Repeat("☃", 33),
			maxChars:  100,
			cfg:       PreprocessConfig{},
			wantTrunc: false,
			lenLE:     100,
			wantValid: true,
		},
		{
			name:      "StripsNestedQuotes",
			subject:   "",
			body:      ">> deep quote\n>>> deeper quote\nReal content.",
			maxChars:  1000,
			cfg:       PreprocessConfig{StripQuotes: true},
			checkWant: true,
			want:      "Real content.",
			wantTrunc: false,
		},
		{
			name:      "StripsQuoteWithoutSpace",
			subject:   "",
			body:      ">no space after caret\n>another\nKept content.",
			maxChars:  1000,
			cfg:       PreprocessConfig{StripQuotes: true},
			checkWant: true,
			want:      "Kept content.",
			wantTrunc: false,
		},
		{
			name:      "StripsPreambleWithNestedQuotes",
			subject:   "Re: topic",
			body:      "My reply.\n\nOn 2026-04-18, bob wrote:\n>> prior reply\n> and response\n>> more",
			maxChars:  1000,
			cfg:       PreprocessConfig{StripQuotes: true},
			checkWant: true,
			want:      "Subject: Re: topic\n\nMy reply.",
			wantTrunc: false,
		},
		{
			// <p>, <br>, attributes and entities are routinely seen in
			// body_text when MIME parsing accepts a "text/html" part as
			// plaintext. The stripper should drop the tags and decode
			// common entities so the prose survives intact.
			name:      "StripHTMLDropsTagsAndDecodesEntities",
			subject:   "",
			body:      `<p style="color:red">Hello &amp; goodbye</p><br/>End.`,
			maxChars:  1000,
			cfg:       PreprocessConfig{StripHTML: true},
			checkWant: true,
			want:      "Hello & goodbye  End.",
			wantTrunc: false,
		},
		{
			// Regression: roborev #322 flagged that the original
			// `<[^>]{0,500}>` pattern ate any angle-bracketed prose. An
			// inline RFC-style email address must survive — `@` is
			// rejected by the strict tag-name pattern.
			name:      "StripHTMLPreservesAngleBracketEmailAddress",
			subject:   "",
			body:      "Please CC John <john@example.com> on the next reply.",
			maxChars:  1000,
			cfg:       PreprocessConfig{StripHTML: true},
			checkWant: true,
			want:      "Please CC John <john@example.com> on the next reply.",
			wantTrunc: false,
		},
		{
			// Regression: an angle-bracket URL (the markdown autolink
			// convention) must survive. The `:` after the scheme breaks
			// the tag-name pattern.
			name:      "StripHTMLPreservesAngleBracketURL",
			subject:   "",
			body:      "See <https://example.com/page>.",
			maxChars:  1000,
			cfg:       PreprocessConfig{StripHTML: true},
			checkWant: true,
			want:      "See <https://example.com/page>.",
			wantTrunc: false,
		},
		{
			// Regression: math/comparison prose passes through. `< 3`
			// and `> 4` each have a non-letter immediately after the
			// bracket, so the tag-name pattern rejects them.
			name:      "StripHTMLPreservesMathPunctuation",
			subject:   "",
			body:      "Show me rows where x < 3 and y > 4 but z != 0.",
			maxChars:  1000,
			cfg:       PreprocessConfig{StripHTML: true},
			checkWant: true,
			want:      "Show me rows where x < 3 and y > 4 but z != 0.",
			wantTrunc: false,
		},
		{
			// Regression: roborev medium follow-up. A date placeholder
			// like `<Aug 6, 2026>` starts with a letter and contains a
			// space, which an earlier looser regex treated as
			// `tag-name + attributes`. The attribute branch now requires
			// `name=value` with a literal `=`, so `<Aug 6, 2026>`
			// (no `=`) falls through and survives.
			name:      "StripHTMLPreservesAngleBracketDatePlaceholder",
			subject:   "",
			body:      "Schedule for <Aug 6, 2026> still good?",
			maxChars:  1000,
			cfg:       PreprocessConfig{StripHTML: true},
			checkWant: true,
			want:      "Schedule for <Aug 6, 2026> still good?",
			wantTrunc: false,
		},
		{
			// Regression: mail-merge placeholders like `<First Name>`,
			// `<Last Name>`, `<Company>` are ubiquitous in templated
			// emails. Same fix path as the date placeholder above —
			// no `=` in the body means no attribute match, the tag-
			// matching falls through, and the prose survives.
			name:      "StripHTMLPreservesMailMergePlaceholder",
			subject:   "",
			body:      "Hi <First Name>, welcome to <Company>.",
			maxChars:  1000,
			cfg:       PreprocessConfig{StripHTML: true},
			checkWant: true,
			want:      "Hi <First Name>, welcome to <Company>.",
			wantTrunc: false,
		},
		{
			// Single-quoted attribute values must strip the same as
			// double-quoted, otherwise pollution like
			// `<a href='https://campaign'>` would survive.
			name:      "StripHTMLDropsSingleQuotedAttrValue",
			subject:   "",
			body:      "Click <a href='https://campaign.example.com'>here</a> now.",
			maxChars:  1000,
			cfg:       PreprocessConfig{StripHTML: true},
			checkWant: true,
			want:      "Click  here  now.",
			wantTrunc: false,
		},
		{
			// Unquoted attribute values must strip too — HTML5 allows
			// them, and email-generator templates occasionally emit
			// `<img src=cid:abc width=1 height=1>` for tracking pixels.
			name:      "StripHTMLDropsUnquotedAttrValues",
			subject:   "",
			body:      "Pixel <img src=cid:abc width=1 height=1> end.",
			maxChars:  1000,
			cfg:       PreprocessConfig{StripHTML: true},
			checkWant: true,
			want:      "Pixel   end.",
			wantTrunc: false,
		},
		{
			// <style>…</style> wraps CSS that should never reach the
			// embedder. The whole block (tags + body) must be removed
			// before the generic HTML-tag stripper would otherwise leave
			// raw CSS behind.
			name:      "StripHTMLDropsStyleBlock",
			subject:   "",
			body:      "Hello\n<style type=\"text/css\">body{margin:0;color:#000}</style>\nWorld",
			maxChars:  1000,
			cfg:       PreprocessConfig{StripHTML: true, CollapseWhitespace: true},
			checkWant: true,
			want:      "Hello\n\nWorld",
			wantTrunc: false,
		},
		{
			// <script>…</script> is treated identically to <style>: the
			// entire block disappears so embedded JS doesn't pollute the
			// vector.
			name:      "StripHTMLDropsScriptBlock",
			subject:   "",
			body:      "Pre\n<script>var x = 1; alert(x);</script>\nPost",
			maxChars:  1000,
			cfg:       PreprocessConfig{StripHTML: true, CollapseWhitespace: true},
			checkWant: true,
			want:      "Pre\n\nPost",
			wantTrunc: false,
		},
		{
			// data: URIs are how inline images leak into body_text.
			// A single base64 PNG can run tens of KB; stripping it
			// reclaims the entire input budget for actual prose.
			name:    "StripBase64DropsDataURI",
			subject: "",
			body: "Before image " +
				"data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==" +
				" After image",
			maxChars:  1000,
			cfg:       PreprocessConfig{StripBase64: true, CollapseWhitespace: true},
			checkWant: true,
			want:      "Before image After image",
			wantTrunc: false,
		},
		{
			// Long unbroken base64-y runs (200+ chars) are stripped even
			// without a data: prefix — embedded MIME parts and other
			// binary residue often appear bare.
			name:      "StripBase64DropsBareBlob",
			subject:   "",
			body:      "Hello " + strings.Repeat("A", 250) + " world",
			maxChars:  1000,
			cfg:       PreprocessConfig{StripBase64: true, CollapseWhitespace: true},
			checkWant: true,
			want:      "Hello world",
			wantTrunc: false,
		},
		{
			// Realistic bare base64 includes '/' at the alphabet's
			// natural ~1/64 frequency, so the original slash-free
			// 200+ regex split such payloads into short subruns and
			// missed them entirely. The slash-allowing companion regex
			// (300+ threshold) catches them. The fixture below has '/'
			// every 11 chars over 330 chars — much denser than real
			// base64 yet still representative of "the slashes break my
			// 200-char run" failure mode the reviewer flagged.
			name:      "StripBase64DropsBareBlobWithSlashes",
			subject:   "",
			body:      "Before " + strings.Repeat("abcdefghij/", 30) + " After",
			maxChars:  1000,
			cfg:       PreprocessConfig{StripBase64: true, CollapseWhitespace: true},
			checkWant: true,
			want:      "Before After",
			wantTrunc: false,
		},
		{
			// Short base64-looking strings (URLs, hashes, tokens) must
			// survive — only 200+ char runs are treated as embedded
			// blobs.
			name:      "StripBase64KeepsShortAlphanumeric",
			subject:   "",
			body:      "Token=abc123XYZ check this hash 0123456789abcdef0123456789abcdef.",
			maxChars:  1000,
			cfg:       PreprocessConfig{StripBase64: true},
			checkWant: true,
			want:      "Token=abc123XYZ check this hash 0123456789abcdef0123456789abcdef.",
			wantTrunc: false,
		},
		{
			// utm_* / fbclid / gclid carry no semantic content. Stripping
			// them lets the embedder treat campaign-tagged URLs as the
			// same canonical link.
			name:      "StripURLTrackingDropsKnownParams",
			subject:   "",
			body:      "Visit https://example.com/page?utm_source=newsletter&utm_medium=email&fbclid=xyz&id=42 today",
			maxChars:  1000,
			cfg:       PreprocessConfig{StripURLTracking: true},
			checkWant: true,
			want:      "Visit https://example.com/page?id=42 today",
			wantTrunc: false,
		},
		{
			// Regression: HubSpot's hsCtaTracking param is stored in the
			// trackingParams map but the lookup lowercases the query key
			// at the call site. With the map key in its canonical lower
			// form ("hsctatracking"), an incoming "?hsCtaTracking=" must
			// still be stripped. Originally the key was stored as
			// camelCase and silently never matched.
			name:      "StripURLTrackingHandlesHubSpotMixedCase",
			subject:   "",
			body:      "Click https://hub.example.com/cta?hsCtaTracking=abc&id=7 now",
			maxChars:  1000,
			cfg:       PreprocessConfig{StripURLTracking: true},
			checkWant: true,
			want:      "Click https://hub.example.com/cta?id=7 now",
			wantTrunc: false,
		},
		{
			// Trailing prose punctuation must come back after the URL —
			// we peel it for net/url parsing, then re-attach.
			name:      "StripURLTrackingPreservesTrailingPunctuation",
			subject:   "",
			body:      "See https://example.com/x?utm_source=foo&keep=1.",
			maxChars:  1000,
			cfg:       PreprocessConfig{StripURLTracking: true},
			checkWant: true,
			want:      "See https://example.com/x?keep=1.",
			wantTrunc: false,
		},
		{
			// Non-tracking params survive verbatim; URLs with no tracking
			// params at all are returned untouched.
			name:      "StripURLTrackingLeavesCleanURLAlone",
			subject:   "",
			body:      "See https://example.com/page?id=42 and ftp://elsewhere/path",
			maxChars:  1000,
			cfg:       PreprocessConfig{StripURLTracking: true},
			checkWant: true,
			want:      "See https://example.com/page?id=42 and ftp://elsewhere/path",
			wantTrunc: false,
		},
		{
			// Runs of blank lines (HTML→text conversion artifact) and
			// horizontal whitespace must collapse so the input cap is
			// spent on content, not padding.
			name:      "CollapseWhitespaceShrinksRuns",
			subject:   "",
			body:      "Line one\n\n\n\nLine two with    big    gaps",
			maxChars:  1000,
			cfg:       PreprocessConfig{CollapseWhitespace: true},
			checkWant: true,
			want:      "Line one\n\nLine two with big gaps",
			wantTrunc: false,
		},
		{
			// Counterpart guarantee for the slash-allowing regex
			// (reBase64BlobWithSlash): a long URL path that *looks*
			// base64-ish (240 unbroken chars of "a/b/a/b/...") must
			// survive. The slash-allowing regex's 300-char threshold
			// is the lever that protects URL paths; this fixture pins
			// a representative path right under that threshold.
			name:      "StripBase64KeepsLongURLPath",
			subject:   "",
			body:      "https://example.com/" + strings.Repeat("a/b", 80) + "/end",
			maxChars:  2000,
			cfg:       PreprocessConfig{StripBase64: true},
			checkWant: true,
			want:      "https://example.com/" + strings.Repeat("a/b", 80) + "/end",
			wantTrunc: false,
		},
		{
			// Regression: an oversized inline-image tag (long enough that
			// reHTMLTag's 500-char ceiling does not match the whole tag)
			// must still come out clean. The fix relies on stripping the
			// base64 payload *before* the HTML pass so reHTMLTag has a
			// small carcass left to sweep.
			name:    "StripPipelineSweepsOversizedImgTag",
			subject: "",
			body: "Before " +
				`<img alt="x" src="data:image/png;base64,` + strings.Repeat("AAAA", 600) + `">` +
				" After",
			maxChars: 5000,
			cfg: PreprocessConfig{
				StripHTML: true, StripBase64: true, CollapseWhitespace: true,
			},
			checkWant: true,
			want:      "Before After",
			wantTrunc: false,
		},
		{
			// Regression: CRLF line endings must collapse identically to
			// LF. Before the normalization at the top of Preprocess,
			// `\r\n\r\n\r\n` left every `\r` in place and the multi-
			// newline collapse never fired because the `\n`s were not
			// consecutive.
			name:      "CollapseWhitespaceNormalizesCRLF",
			subject:   "",
			body:      "Line one   \r\n\r\n\r\n\r\nLine two",
			maxChars:  1000,
			cfg:       PreprocessConfig{CollapseWhitespace: true},
			checkWant: true,
			want:      "Line one\n\nLine two",
			wantTrunc: false,
		},
		{
			// End-to-end: an HTML-polluted email with a base64 image,
			// tracking-laden link and runs of whitespace shrinks to its
			// prose alone when every transform is enabled.
			name:    "PipelineRemovesPollutionEndToEnd",
			subject: "Newsletter",
			body: "<p>Hello</p>\n\n\n" +
				"data:image/gif;base64,R0lGODlhAQABAAAAACw= " +
				"\n\n\nClick " +
				"https://example.com/?utm_source=x&keep=y" +
				"\n\n-- \nSig",
			maxChars: 1000,
			cfg: PreprocessConfig{
				StripQuotes: true, StripSignatures: true,
				StripHTML: true, StripBase64: true,
				StripURLTracking: true, CollapseWhitespace: true,
			},
			checkWant: true,
			want:      "Subject: Newsletter\n\nHello\n\nClick https://example.com/?keep=y",
			wantTrunc: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assertpkg.New(t)
			out, tr := Preprocess(tt.subject, tt.body, tt.maxChars, tt.cfg)
			if tt.checkWant {
				assert.Equal(tt.want, out)
			}
			if tt.lenLE > 0 {
				assert.LessOrEqual(len(out), tt.lenLE)
			}
			if tt.wantValid {
				assert.True(utf8.ValidString(out), "out is not valid UTF-8")
			}
			assert.Equal(tt.wantTrunc, tr, "truncated")
		})
	}
}
