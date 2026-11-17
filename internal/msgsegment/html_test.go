package msgsegment

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/msgvault/internal/mime"
)

func TestHTMLText(t *testing.T) {
	for _, tc := range []struct {
		name, html, want string
	}{
		{
			"no quote containers keeps StripHTML output",
			"<p>Hello&nbsp;there</p><p>Second   line</p>",
			mime.StripHTML("<p>Hello&nbsp;there</p><p>Second   line</p>"),
		},
		{
			"Gmail reply",
			`<div dir="ltr">Sounds good.</div><br><div class="gmail_quote"><div dir="ltr" class="gmail_attr">On Mon, Jan 5, 2026 at 9:00 AM Alice &lt;alice@example.com&gt; wrote:<br></div><blockquote class="gmail_quote" style="margin:0"><div>Lunch at noon?</div><div>Second line</div></blockquote></div>`,
			"Sounds good.\n\n> On Mon, Jan 5, 2026 at 9:00 AM Alice <alice@example.com> wrote:\n\n> Lunch at noon?\n\n> Second line",
		},
		{
			"nested blockquotes",
			"<p>New</p><blockquote><p>Old</p><blockquote>Older</blockquote><p>Old again</p></blockquote><p>After</p>",
			"New\n\n> Old\n\n>> Older\n\n> Old again\n\nAfter",
		},
		{
			"Gmail quote inside a blockquote keeps the outer depth",
			`<p>New</p><blockquote><p>Old</p><div class="gmail_quote">On Mon, Jan 5, 2026 at 9:00 AM Bob wrote:<blockquote>Older</blockquote></div></blockquote>`,
			"New\n\n> Old\n\n> On Mon, Jan 5, 2026 at 9:00 AM Bob wrote:\n>> Older",
		},
		{
			"unrelated divs and unclosed quote",
			`<div class="other"><div>New</div></div><blockquote>Old<div>still old`,
			"New\n\n> Old\n> still old",
		},
		{
			"markers already in the text are dropped",
			"<p>a﷐b</p><blockquote>c&#xFDD1;</blockquote>",
			"ab\n\n> c",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, HTMLText(tc.html))
		})
	}
}

func TestSplitHTMLText(t *testing.T) {
	text := HTMLText(`<div>Thanks, see below.</div><div class="gmail_quote"><div class="gmail_attr">Le lun. 5 janv. 2026, Alice a écrit :</div><blockquote class="gmail_quote">Old <b>news</b><blockquote>Older</blockquote></blockquote></div><div>-- <br>Sender</div>`)
	assert.Equal(t, "Thanks, see below.\n\n> Le lun. 5 janv. 2026, Alice a écrit :\n\n> Old news\n>> Older\n\n-- \nSender", text)
	quoteStart := strings.Index(text, "> Le")
	olderStart := strings.Index(text, ">> Older")
	signatureStart := strings.Index(text, "-- ")
	assert.Equal(t, []Segment{
		{Authored, 0, quoteStart, 0},
		{Quoted, quoteStart, olderStart, 1},
		{Quoted, olderStart, signatureStart, 2},
		{Signature, signatureStart, len(text), 0},
	}, Split(text).Segments)
}
