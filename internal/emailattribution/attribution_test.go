package emailattribution

import (
	"bytes"
	"net/mail"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const sink = "inbox@example.net"

// fixtureEvidence builds Evidence the way the store does: delivery addresses
// from ParseHeaders, visible and sender addresses from the envelope.
func fixtureEvidence(t *testing.T, raw []byte) (Evidence, bool) {
	t.Helper()
	end := len(raw)
	for _, sep := range [][]byte{[]byte("\r\n\r\n"), []byte("\n\n")} {
		if i := bytes.Index(raw, sep); i >= 0 && i+len(sep) < end {
			end = i + len(sep)
		}
	}
	headers, malformed, err := ParseHeaders(raw[:end])
	require.NoError(t, err)
	msg, err := mail.ReadMessage(bytes.NewReader(raw[:end]))
	require.NoError(t, err)
	envelope := func(names ...string) []string {
		var out []string
		for _, name := range names {
			list, err := msg.Header.AddressList(name)
			if err != nil {
				continue
			}
			for _, a := range list {
				out = append(out, a.Address)
			}
		}
		return out
	}
	return Evidence{
		Original:  headers.Original,
		Delivered: headers.Delivered,
		Visible:   envelope("To", "Cc"),
		Sender:    envelope("From"),
	}, malformed
}

func TestForwardingPaths(t *testing.T) {
	candidates := []string{sink, "work@example.com", "mask@example.org", "second@example.com"}
	for _, tc := range []struct{ name, address string }{
		{"gmail", "work@example.com"},
		{"gmail-visible", "work@example.com"},
		{"pop", "work@example.com"},
		{"workspace", "work@example.com"},
		{"fastmail", "mask@example.org"},
		{"generic", "work@example.com"},
		{"bcc", "mask@example.org"},
		{"list", "mask@example.org"},
		{"ambiguous", ""},
		{"conflicting", ""},
		{"nested", sink},
		{"malformed-mime", "work@example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := os.ReadFile("testdata/" + tc.name + ".eml")
			require.NoError(t, err)
			e, _ := fixtureEvidence(t, raw)
			assert.Equal(t, tc.address, Attribute(e, candidates, sink, false).Address)
		})
	}
}

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
