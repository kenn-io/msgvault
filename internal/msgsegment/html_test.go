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
