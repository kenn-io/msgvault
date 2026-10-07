package emailattribution

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const sink = "inbox@example.net"

func TestSinkWaitsBehindVisibleRecipients(t *testing.T) {
	e := Evidence{Delivered: []string{sink}, Visible: []string{"work@example.com"}}
	got := Attribute(e, []string{sink, "work@example.com"}, sink, false)
	assert.Equal(t, "work@example.com", got.Address)
	got = Attribute(e, []string{sink}, sink, false)
	assert.Equal(t, sink, got.Address)
}

func TestConflictStopsLowerTiers(t *testing.T) {
	e := Evidence{
		Original: []string{"work@example.com", "mask@example.org"},
		Visible:  []string{"second@example.com"},
	}
	got := Attribute(e, []string{sink, "work@example.com", "mask@example.org", "second@example.com"}, sink, false)
	assert.Empty(t, got.Address)
}

func TestSentUsesUniqueConfirmedSender(t *testing.T) {
	assert := assert.New(t)
	candidates := []string{sink, "work@example.com", "second@example.com"}
	e := Evidence{Sender: []string{"work@example.com"}, Delivered: []string{sink}}
	assert.Equal("work@example.com", Attribute(e, candidates, sink, true).Address)
	e.Sender = []string{"work@example.com", "WORK@example.com"}
	assert.Equal("work@example.com", Attribute(e, candidates, sink, true).Address)
	e.Sender = []string{"work@example.com", "second@example.com"}
	assert.Empty(Attribute(e, candidates, sink, true).Address, "two confirmed senders are ambiguous")
	e.Sender = nil
	assert.Empty(Attribute(e, candidates, sink, true).Address, "sent copies never take the source default")
}

func TestMixedCaseInputsCompareNormalized(t *testing.T) {
	e := Evidence{Visible: []string{" WORK@Example.com "}}
	got := Attribute(e, []string{"Work@EXAMPLE.com"}, "Inbox@Example.NET", false)
	assert.Equal(t, "work@example.com", got.Address)
	got = Attribute(Evidence{}, []string{"Inbox@Example.NET"}, "INBOX@example.net", false)
	assert.Equal(t, sink, got.Address)
}

func TestMalformedValueBesideValidHeader(t *testing.T) {
	assert := assert.New(t)
	headers, malformed, err := ParseHeaders([]byte("X-Delivered-To: <<bad\r\nX-Original-To: Work@Example.org\r\nDelivered-To: a@example.net, b@example.net\r\n\r\n"))
	require.NoError(t, err)
	assert.True(malformed)
	assert.Equal([]string{"work@example.org"}, headers.Original)
	assert.Equal([]string{"a@example.net", "b@example.net"}, headers.Delivered)
}

func TestParseHeadersRejectsBrokenBlock(t *testing.T) {
	assert := assert.New(t)
	_, _, err := ParseHeaders([]byte("To: work@example.org\r\ninvalid header line\r\n\r\n"))
	require.Error(t, err)
	headers, malformed, err := ParseHeaders([]byte("Subject: " + strings.Repeat("x", 1024) + "\r\n\r\n"))
	require.NoError(t, err)
	assert.False(malformed)
	assert.Empty(headers.Original)
}

func TestSinkInOriginalHeadersWaitsBehindUpstream(t *testing.T) {
	candidates := []string{sink, "work@example.com"}
	for _, tc := range []struct {
		name  string
		block string
	}{
		{"fastmail inbox", "Delivered-To: work@example.com\r\nX-Delivered-To: inbox@example.net\r\n" +
			"X-Resolved-To: inbox@example.net\r\nDelivered-To: inbox@example.net\r\n\r\n"},
		{"postfix inbox", "Delivered-To: inbox@example.net\r\nX-Original-To: inbox@example.net\r\n" +
			"Delivered-To: work@example.com\r\n\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers, _, err := ParseHeaders([]byte(tc.block))
			require.NoError(t, err)
			got := Attribute(Evidence{Original: headers.Original, Delivered: headers.Delivered}, candidates, sink, false)
			assert.Equal(t, Result{Address: "work@example.com"}, got)
		})
	}
	got := Attribute(Evidence{Original: []string{sink}}, candidates, sink, false)
	assert.Equal(t, Result{Address: sink}, got, "the sink alone is still the final inbox")
}

func TestConfirmedSenderWithoutInboundEvidenceIsSent(t *testing.T) {
	assert := assert.New(t)
	candidates := []string{sink, "work@example.com"}
	got := Attribute(Evidence{Sender: []string{sink}, NoSentFolder: true}, candidates, sink, false)
	assert.Equal(Result{Address: sink, Sent: true}, got, "a sent copy never falls back to received")

	got = Attribute(Evidence{Sender: []string{sink}}, candidates, sink, false)
	assert.Equal(Result{Address: sink}, got, "a source with a Sent folder already decided direction")

	got = Attribute(Evidence{Sender: []string{sink}, Delivered: []string{sink}, NoSentFolder: true}, candidates, sink, false)
	assert.Equal(Result{Address: sink}, got, "delivery evidence means the account received its own mail")

	got = Attribute(Evidence{Sender: []string{"work@example.com"}, Visible: []string{sink}, NoSentFolder: true},
		candidates, sink, false)
	assert.Equal(Result{Address: sink}, got, "mail one identity sent to another was received")

	got = Attribute(Evidence{Sender: []string{"someone@example.org"}, NoSentFolder: true}, candidates, sink, false)
	assert.Equal(Result{Address: sink}, got, "an unconfirmed sender keeps the source default")
}
