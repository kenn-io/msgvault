package msgsegment

import (
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHTMLMarkers(t *testing.T) {
	for _, tc := range []struct{ name, body, authored, basis string }{
		{"gmail", `<p>New π</p><div class="gmail_quote"><p>Old</p></div>`, `<p>New π</p>`, "gmail_quote"},
		{"cite", `Hello<blockquote type="cite">Old</blockquote>After`, `HelloAfter`, "blockquote_cite"},
		{"thunderbird", `<p>New</p><blockquote TYPE="cite">Old</blockquote>`, `<p>New</p>`, "blockquote_cite"},
		{"outlook", `<p>New</p><div id="divRplyFwdMsg">From: Sender<br>Sent: Tuesday<br>To: Other</div><p>Old</p>`, `<p>New</p>`, "outlook_header"},
		{"outlook negative", `<div id="appendonsend">My answer</div>`, `<div id="appendonsend">My answer</div>`, ""},
		{"trailing bare", `<p>On Tue A wrote:</p><blockquote>Old</blockquote>`, `<p>On Tue A wrote:</p>`, "trailing_blockquote"},
		{"authored quotation", `<p>A literary quote:</p><blockquote>Text</blockquote>`, `<p>A literary quote:</p><blockquote>Text</blockquote>`, ""},
		{"interleaved bare", `<p>On Tue A wrote:</p><blockquote>Old</blockquote><p>New</p>`, `<p>On Tue A wrote:</p><blockquote>Old</blockquote><p>New</p>`, ""},
		{"unclosed", `New<div class="gmail_quote">Old`, `New`, "gmail_quote"},
		{"script", `<script>const x = '<div class="gmail_quote">';</script>New`, `<script>const x = '<div class="gmail_quote">';</script>New`, ""},
		{"incomplete", `New <div class="gmail_quote`, `New <div class="gmail_quote`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := Split(tc.body, HTML, Options{})
			assertTiling(t, tc.body, r)
			assert.Equal(t, tc.authored, r.AuthoredText())
			if tc.basis != "" {
				var bases []string
				for _, s := range r.Segments {
					bases = append(bases, s.Basis)
				}
				assert.Contains(t, bases, tc.basis)
			}
		})
	}
}
func TestHTMLReviewRegressions(t *testing.T) {
	for _, tc := range []struct {
		name, body, authored, basis string
	}{
		{
			name:     "outlook div state ends with its enclosing quote",
			body:     "<blockquote type='cite'><p>Old</p><div id='divRplyFwdMsg'><p>From: Pat</p><p>Sent: Tuesday</p><p>To: Other</p></div></blockquote><p>New reply</p>",
			authored: "<p>New reply</p>",
			basis:    "outlook_header",
		},
		{
			name:     "outlook hr context ends with its enclosing quote",
			body:     "<blockquote type='cite'><p>Old</p><hr><p>From: Pat</p><p>Sent: Tuesday</p><p>To: Other</p></blockquote><p>New reply</p>",
			authored: "<p>New reply</p>",
			basis:    "outlook_header",
		},
		{
			name:     "nested quote ends when its ancestor closes",
			body:     `<div class="gmail_quote"><blockquote type="cite"><p>Old</p></div><p>New reply</p>`,
			authored: `<p>New reply</p>`,
			basis:    "blockquote_cite",
		},
		{
			name:     "outlook lookahead stops at intervening prose and quotes",
			body:     "<hr><p>My note</p><blockquote><p>From: Pat</p><p>Sent: Tuesday</p><p>To: Other</p></blockquote><p>More authored text</p>",
			authored: "<hr><p>My note</p><blockquote><p>From: Pat</p><p>Sent: Tuesday</p><p>To: Other</p></blockquote><p>More authored text</p>",
		},
		{
			name:     "outlook lookahead allows a recognized relay marker",
			body:     "<p>New</p><hr><p>Begin forwarded message:</p><p>From: Pat</p><p>Sent: Tuesday</p><p>To: Other</p><p>Old</p>",
			authored: "<p>New</p>",
			basis:    "outlook_header",
		},
		{
			name:     "formatted attribution spans inline elements",
			body:     "<p>On Tue <strong>Pat</strong> wrote:</p><blockquote>Old</blockquote>",
			authored: "<p>On Tue <strong>Pat</strong> wrote:</p>",
			basis:    "trailing_blockquote",
		},
		{
			name:     "linked attribution spans inline elements",
			body:     "<p>On Tue <a href='mailto:pat@example.test'>Pat</a> wrote:</p><blockquote>Old</blockquote>",
			authored: "<p>On Tue <a href='mailto:pat@example.test'>Pat</a> wrote:</p>",
			basis:    "trailing_blockquote",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := Split(tc.body, HTML, Options{})
			assertTiling(t, tc.body, r)
			assert.Equal(t, tc.authored, r.AuthoredText())
			if tc.basis != "" {
				var bases []string
				for _, s := range r.Segments {
					bases = append(bases, s.Basis)
				}
				assert.Contains(t, bases, tc.basis)
			}
		})
	}
}

func TestHTMLOutlookForwardKeepsClaimsForSiblingBody(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	body := `<div id="divRplyFwdMsg"><p>Begin forwarded message:</p><p>From: Vendor &lt;vendor@example.test&gt;</p><p>Date: Monday</p><p>Subject: Invoice</p><p>To: buyer@example.test</p></div><p>Pay here</p><p>More details</p>`
	r := Split(body, HTML, Options{})
	assertTiling(t, body, r)
	assert.Empty(r.AuthoredText())

	position := strings.Index(body, "Pay here")
	var bodySegment *Segment
	for i := range r.Segments {
		if r.Segments[i].Start <= position && position < r.Segments[i].End {
			bodySegment = &r.Segments[i]
			break
		}
	}
	require.NotNil(bodySegment)
	assert.Equal(Forwarded, bodySegment.Kind)
	require.NotNil(bodySegment.Claimed)
	assert.Equal("Vendor <vendor@example.test>", bodySegment.Claimed.From)
	assert.Equal("Monday", bodySegment.Claimed.Date)
}

func TestHTMLOutlookScopesEndWithImplicitlyClosedQuote(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{
			name: "forward header div",
			body: `<div class="gmail_quote"><blockquote type="cite"><div id="divRplyFwdMsg"><p>Begin forwarded message:</p><p>From: Vendor &lt;vendor@example.test&gt;</p><p>Date: Monday</p><p>Subject: Invoice</p><p>To: buyer@example.test</p></div><p>Forwarded body</p></div><p>New reply</p>`,
		},
		{
			name: "nested hr marker",
			body: `<div class="gmail_quote"><blockquote type="cite"><p>Old</p><hr><p>Begin forwarded message:</p><p>From: Vendor &lt;vendor@example.test&gt;</p><p>Date: Monday</p><p>Subject: Invoice</p><p>To: buyer@example.test</p><p>Forwarded body</p></div><p>New reply</p>`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			r := Split(tc.body, HTML, Options{})
			assertTiling(t, tc.body, r)
			assert.Equal(`<p>New reply</p>`, r.AuthoredText())

			position := strings.Index(tc.body, "Forwarded body")
			var bodySegment *Segment
			for i := range r.Segments {
				if r.Segments[i].Start <= position && position < r.Segments[i].End {
					bodySegment = &r.Segments[i]
					break
				}
			}
			require.NotNil(bodySegment)
			assert.Equal(Forwarded, bodySegment.Kind)
			require.NotNil(bodySegment.Claimed)
			assert.Equal("Vendor <vendor@example.test>", bodySegment.Claimed.From)
		})
	}
}

func TestHTMLForwardClaimsContinuePastGmailAttribution(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	body := `<div class="gmail_quote"><div class="gmail_attr"><p>Begin forwarded message:</p><p>From: Outer &lt;outer@example.test&gt;</p><p>Date: Monday</p><p>Subject: Outer invoice</p></div><p>Forwarded body</p></div><p>New reply</p>`
	r := Split(body, HTML, Options{})
	assertTiling(t, body, r)
	assert.Equal(`<p>New reply</p>`, r.AuthoredText())

	position := strings.Index(body, "Forwarded body")
	var bodySegment *Segment
	for i := range r.Segments {
		if r.Segments[i].Start <= position && position < r.Segments[i].End {
			bodySegment = &r.Segments[i]
			break
		}
	}
	require.NotNil(bodySegment)
	assert.Equal(Forwarded, bodySegment.Kind)
	require.NotNil(bodySegment.Claimed)
	assert.Equal("Outer <outer@example.test>", bodySegment.Claimed.From)
	assert.Equal("Monday", bodySegment.Claimed.Date)
}

func TestHTMLNestedForwardReplacesInheritedClaims(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	body := `<div class="gmail_quote"><p>Begin forwarded message:</p><p>From: Outer &lt;outer@example.test&gt;</p><p>Date: Monday</p><p>Subject: Outer invoice</p><div class="gmail_quote"><p>Begin forwarded message:</p><p>From: Inner &lt;inner@example.test&gt;</p><p>Inner body</p></div><p>Outer body</p></div><p>New reply</p>`
	r := Split(body, HTML, Options{})
	assertTiling(t, body, r)
	assert.Equal(`<p>New reply</p>`, r.AuthoredText())

	segmentAt := func(needle string) *Segment {
		position := strings.Index(body, needle)
		for i := range r.Segments {
			if r.Segments[i].Start <= position && position < r.Segments[i].End {
				return &r.Segments[i]
			}
		}
		return nil
	}
	inner := segmentAt("Inner body")
	require.NotNil(inner)
	assert.Equal(Forwarded, inner.Kind)
	require.NotNil(inner.Claimed)
	assert.Equal("Inner <inner@example.test>", inner.Claimed.From)
	assert.Empty(inner.Claimed.Date)
	assert.Empty(inner.Claimed.Subject)

	outer := segmentAt("Outer body")
	require.NotNil(outer)
	assert.Equal(Forwarded, outer.Kind)
	require.NotNil(outer.Claimed)
	assert.Equal("Outer <outer@example.test>", outer.Claimed.From)
	assert.Equal("Monday", outer.Claimed.Date)
	assert.Equal("Outer invoice", outer.Claimed.Subject)
}

func TestHTMLNestedForwardDoesNotPromoteOuterQuote(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	body := `<div class="gmail_quote"><div class="gmail_quote"><p>Begin forwarded message:</p><p>From: Vendor &lt;vendor@example.test&gt;</p><p>Date: Monday</p><p>Subject: Invoice</p></div><p>Reply about the forwarded message.</p></div><p>New reply</p>`
	r := Split(body, HTML, Options{})
	assertTiling(t, body, r)
	assert.Equal(`<p>New reply</p>`, r.AuthoredText())

	position := strings.Index(body, "Reply about")
	var replySegment *Segment
	for i := range r.Segments {
		if r.Segments[i].Start <= position && position < r.Segments[i].End {
			replySegment = &r.Segments[i]
			break
		}
	}
	require.NotNil(replySegment)
	assert.Equal(Quoted, replySegment.Kind)
	assert.Nil(replySegment.Claimed)

	position = strings.Index(body, "Begin forwarded message")
	var forwardSegment *Segment
	for i := range r.Segments {
		if r.Segments[i].Start <= position && position < r.Segments[i].End {
			forwardSegment = &r.Segments[i]
			break
		}
	}
	require.NotNil(forwardSegment)
	assert.Equal(Forwarded, forwardSegment.Kind)
	require.NotNil(forwardSegment.Claimed)
	assert.Equal("Vendor <vendor@example.test>", forwardSegment.Claimed.From)
}

func TestHTMLDepthAndBounds(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	body := `New<div class="gmail_quote">Old<blockquote type="cite">Older</blockquote>Old</div>New`
	r := Split(body, HTML, Options{})
	assertTiling(t, body, r)
	assert.Equal("NewNew", r.AuthoredText())
	var depths []int
	for _, s := range r.Segments {
		depths = append(depths, s.Depth)
	}
	assert.Contains(depths, 2)
	body = strings.Repeat("<b>x</b>", maxHTMLTokens)
	r = Split(body, HTML, Options{})
	assertTiling(t, body, r)
	require.True(r.Truncated)
	assert.Equal("unexamined_tail", r.Segments[len(r.Segments)-1].Basis)
}

func TestHTMLAncestorCloseBoundsImplicitDescendants(t *testing.T) {
	body := `<div class="gmail_quote"><blockquote type="cite"><p>Old</div><p>New</p>`
	r := Split(body, HTML, Options{})
	assertTiling(t, body, r)
	assert.Equal(t, `<p>New</p>`, r.AuthoredText())
}

func TestHTMLForwardClaims(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	body := `FYI<div class="gmail_quote"><p>---------- Forwarded message ---------</p><p>From: Vendor &lt;billing@example.com&gt;</p><p>Date: Monday</p><p>Subject: Invoice</p><p>To: buyer@example.com</p><p>Pay here</p></div>`
	r := Split(body, HTML, Options{})
	assertTiling(t, body, r)
	assert.Equal("FYI", r.AuthoredText())
	require.Len(r.Segments, 2)
	s := r.Segments[1]
	assert.Equal(Forwarded, s.Kind)
	require.NotNil(s.Claimed)
	assert.Equal("Vendor <billing@example.com>", s.Claimed.From)
	assert.Equal("Monday", s.Claimed.Date)
	assert.Equal("untrusted_text", s.Claimed.ContentTrust)
}

func TestHTMLNestedForwardDoesNotPromoteEnclosingQuote(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	body := `<p>New</p><div class="gmail_quote"><blockquote type="cite"><p>Begin forwarded message:</p><p>From: Nested &lt;nested@example.test&gt;</p><p>Date: Monday</p><p>Subject: Inside</p><p>To: Other</p><p>Nested body</p></blockquote><p>Outer reply</p></div><p>New follow-up</p>`
	r := Split(body, HTML, Options{})
	assertTiling(t, body, r)
	assert.Equal(`<p>New</p><p>New follow-up</p>`, r.AuthoredText())

	outerAt := strings.Index(body, `<p>Outer reply</p>`)
	var outer Segment
	outerFound := false
	for _, s := range r.Segments {
		if s.Start <= outerAt && outerAt < s.End {
			outer, outerFound = s, true
			break
		}
	}
	require.True(outerFound, "outer reply should be covered by a segment")
	assert.Equal(Quoted, outer.Kind)
	assert.Nil(outer.Claimed)

	nestedAt := strings.Index(body, `<p>Nested body</p>`)
	var nested Segment
	nestedFound := false
	for _, s := range r.Segments {
		if s.Start <= nestedAt && nestedAt < s.End {
			nested, nestedFound = s, true
			break
		}
	}
	require.True(nestedFound, "nested body should be covered by a segment")
	assert.Equal(Forwarded, nested.Kind)
	require.NotNil(nested.Claimed)
	assert.Equal("Nested <nested@example.test>", nested.Claimed.From)
}

func TestHTMLForwardMarkerStaysWithinItsContainer(t *testing.T) {
	for _, body := range []string{
		`New<div class="gmail_quote">Old reply</div><p>Begin forwarded message:</p><p>From: outside@example.com</p><p>Date: Monday</p>`,
		`New<div class="gmail_quote">On Tuesday A wrote:<br>Old reply<br>Begin forwarded message:<br>From: nested@example.com<br>Date: Monday</div>`,
	} {
		r := Split(body, HTML, Options{})
		assertTiling(t, body, r)
		for _, s := range r.Segments {
			assert.NotEqual(t, Forwarded, s.Kind)
			assert.Nil(t, s.Claimed)
		}
	}
}

func TestHTMLForwardClaimLimitPreservesUnicode(t *testing.T) {
	for _, symbol := range []string{"π", "漢", "🦉"} {
		for prefix := range 4 {
			value := strings.Repeat("x", prefix) + strings.Repeat(symbol, 3000)
			t.Run(strings.Repeat("x", prefix)+symbol, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				body := `<div class="gmail_quote">Begin forwarded message:<br>From: ` + value + `</div>`
				r := Split(body, HTML, Options{})
				assertTiling(t, body, r)
				require.Len(r.Segments, 1)
				s := r.Segments[0]
				require.NotNil(s.Claimed)
				assert.True(strings.HasPrefix(value, s.Claimed.From))
				_, err := json.Marshal(r.Segments)
				assert.NoError(err)
			})
		}
	}
}
