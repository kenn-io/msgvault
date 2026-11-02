package msgsegment

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func assertTiling(t *testing.T, body string, r Result) {
	t.Helper()
	end := 0
	var rebuilt strings.Builder
	for _, s := range r.Segments {
		require.Equal(t, end, s.Start)
		require.Greater(t, s.End, s.Start)
		require.LessOrEqual(t, s.End, len(body))
		rebuilt.WriteString(body[s.Start:s.End])
		end = s.End
	}
	assert.Equal(t, len(body), end)
	assert.Equal(t, body, rebuilt.String())
}
func TestPlainText(t *testing.T) {
	for _, tc := range []struct {
		name, body, authored string
		kinds                []Kind
	}{
		{"empty", "", "", nil},
		{"plain", "Hello π\r\n", "Hello π\r\n", []Kind{Authored}},
		{"inline", "Answer\n> question\nMore\n>> older\n", "Answer\nMore\n", []Kind{Authored, Quoted, Authored, Quoted}},
		{"attribution", "Yes\nOn Tue, 3 Mar 2026, A wrote:\n\n> Older\n", "Yes\n", []Kind{Authored, Quoted, Quoted}},
		{"wrapped", "Yes\nOn Tue, 3 Mar 2026,\nA wrote:\n> Older\n", "Yes\n", []Kind{Authored, Quoted, Quoted}},
		{"dated authored sentence", "On Monday we agreed to ship.\nSomeone wrote:\n> Older\n", "On Monday we agreed to ship.\nSomeone wrote:\n", []Kind{Authored, Quoted}},
		{"dated authored fragment", "On May 1, 2026 we agreed to ship\nSomeone wrote:\n> Older\n", "On May 1, 2026 we agreed to ship\nSomeone wrote:\n", []Kind{Authored, Quoted}},
		{"signature", "Hello\n-- \nName\n", "Hello\n", []Kind{Authored, Signature}},
		{"signature before quote", "Hello\n-- \nName\nOn Tuesday A wrote:\n> Older\nMy inline reply\n", "Hello\nMy inline reply\n", []Kind{Authored, Signature, Quoted, Quoted, Authored}},
		{"signature before forward", "FYI\n-- \nName\nBegin forwarded message:\nFrom: Other\nDate: Monday\nOld\n", "FYI\n", []Kind{Authored, Signature, Forwarded}},
		{"signature before headers", "Hello\n-- \nName\nFrom: Other\nDate: Monday\nOld\n", "Hello\n", []Kind{Authored, Signature, Quoted}},
		{"footer", "Hello\nSent from my iPhone", "Hello\n", []Kind{Authored, Signature}},
		{"fence", "```go\n> sample\nFrom: team\nDate: today\n```\n", "```go\n> sample\nFrom: team\nDate: today\n```\n", []Kind{Authored}},
		{"inline backtick span", "```example```\n> Old\n", "```example```\n", []Kind{Authored, Quoted}},
		{"backticks in fence info", "```go `example`\n> Old\n", "```go `example`\n", []Kind{Authored, Quoted}},
		{"backticks in tilde fence info", "~~~`example`\n> code\n~~~\n> Old\n", "~~~`example`\n> code\n~~~\n", []Kind{Authored, Quoted}},
		{"long backtick fence", "````go\n```\n> code\n```` trailing text\n> still code\n`````\n> Older\n", "````go\n```\n> code\n```` trailing text\n> still code\n`````\n", []Kind{Authored, Quoted}},
		{"long tilde fence", "~~~~text\n~~~\n> code\n~~~~ trailing text\n> still code\n~~~~\n> Older\n", "~~~~text\n~~~\n> code\n~~~~ trailing text\n> still code\n~~~~\n", []Kind{Authored, Quoted}},
		{"prose", "I wrote: this is fine\nFrom: the team\nThanks\n", "I wrote: this is fine\nFrom: the team\nThanks\n", []Kind{Authored}},
		{"no quoted successor", "On Tue, 3 Mar 2026, A wrote:\nMy reply\n", "On Tue, 3 Mar 2026, A wrote:\nMy reply\n", []Kind{Authored}},
		{"outlook", "Reply\nFrom: Sender <sender@example.com>\nSent: Tuesday\nTo: receiver@example.com\nSubject: Topic\nOld text\n", "Reply\n", []Kind{Authored, Quoted}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := Split(tc.body, Text, Options{})
			assertTiling(t, tc.body, r)
			assert.Equal(t, tc.authored, r.AuthoredText())
			var kinds []Kind
			for _, s := range r.Segments {
				kinds = append(kinds, s.Kind)
			}
			assert.Equal(t, tc.kinds, kinds)
		})
	}
}
func TestAttributionLocales(t *testing.T) {
	for _, s := range []string{"On Tuesday A wrote:", "Am Dienstag schrieb A:", "Le mardi A a écrit :", "El martes A escribió:", "Il martedì A ha scritto:", "Em terça A escreveu:", "Op dinsdag schreef A:", "Den tisdag A skrev:", "A 来信:", "A 書きました:", "A 写道：", "Во вторник пользователь A написал:"} {
		t.Run(s, func(t *testing.T) {
			body := "New\n" + s + "\n> Old\n"
			r := Split(body, Text, Options{})
			assertTiling(t, body, r)
			assert.Equal(t, "New\n", r.AuthoredText())
		})
	}
}
func TestForwardHeaders(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	body := "FYI\n---------- Forwarded message ---------\nFrom: Vendor <billing@example.com>\nDate: Monday\nSubject: Invoice\nTo: buyer@example.com\n\nPay here\n"
	r := Split(body, Text, Options{})
	assertTiling(t, body, r)
	require.Len(r.Segments, 2)
	s := r.Segments[1]
	assert.Equal(Forwarded, s.Kind)
	assert.Equal("relayed", s.Trust)
	require.NotNil(s.Claimed)
	assert.Equal("Vendor <billing@example.com>", s.Claimed.From)
	assert.Equal("Monday", s.Claimed.Date)
	assert.Equal("Invoice", s.Claimed.Subject)
	assert.Equal("buyer@example.com", s.Claimed.To)
	assert.Equal("untrusted_text", s.Claimed.ContentTrust)
	withSignature := Split("FYI\n-- \nName\n"+strings.TrimPrefix(body, "FYI\n"), Text, Options{})
	require.Len(withSignature.Segments, 3)
	assert.Equal(Signature, withSignature.Segments[1].Kind)
	assert.Equal(Forwarded, withSignature.Segments[2].Kind)
	assert.Equal(s.Claimed, withSignature.Segments[2].Claimed)
	for _, subject := range []string{"FW: Topic", "WG: Topic", "TR: Topic", "RV: Topic", "I: Topic", "ENC: Topic", "VS: Topic"} {
		r := Split("New\n-----Original Message-----\nFrom: Other\nDate: Monday\nOld", Text, Options{Subject: subject})
		assert.Equal(Forwarded, r.Segments[1].Kind)
	}
}
func TestBoundsAndChat(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	body := strings.Repeat("π", MaxBodyBytes/2+3)
	r := Split(body, Text, Options{})
	assertTiling(t, body, r)
	assert.True(r.Truncated)
	assert.Equal("unexamined_tail", r.Segments[len(r.Segments)-1].Basis)
	body = "> quoted\n-- \nfooter"
	r = Split(body, Chat, Options{})
	assertTiling(t, body, r)
	require.Len(r.Segments, 1)
	assert.Equal(Authored, r.Segments[0].Kind)
	r = Split("New\n> Old\n", Text, Options{})
	assert.Equal([]Kind{Authored, Quoted}, r.KindsOverlapping(0, 9))
	assert.Nil(r.KindsOverlapping(-1, 2))
	assert.Nil(r.KindsOverlapping(2, 2))
}
func FuzzSplitTiling(f *testing.F) {
	for _, body := range []string{"", "> a\r\nAnswer\n", "-- \nfooter", "Begin forwarded message:\nFrom: x\nDate: y\n"} {
		f.Add(body)
	}
	f.Fuzz(func(t *testing.T, body string) {
		for _, format := range []Format{Text, Chat, HTML} {
			assertTiling(t, body, Split(body, format, Options{}))
		}
	})
}
