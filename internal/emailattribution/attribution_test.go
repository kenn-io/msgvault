package emailattribution

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestForwardingPaths(t *testing.T) {
	candidates := []string{"inbox@example.net", "work@example.com", "mask@example.org", "second@example.com"}
	for _, tc := range []struct{ name, address, basis string }{
		{"gmail", "work@example.com", "delivery-chain"},
		{"gmail-visible", "work@example.com", "recipient-headers"},
		{"pop", "work@example.com", "delivery-chain"},
		{"workspace", "work@example.com", "original-recipient"},
		{"fastmail", "mask@example.org", "original-recipient"},
		{"generic", "work@example.com", "original-recipient"},
		{"bcc", "mask@example.org", "original-recipient"},
		{"list", "mask@example.org", "original-recipient"},
		{"ambiguous", "", "ambiguous"},
		{"conflicting", "", "ambiguous"},
		{"nested", "inbox@example.net", "source-default"},
		{"malformed-mime", "work@example.com", "recipient-headers"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			raw, err := os.ReadFile("testdata/" + tc.name + ".eml")
			require.NoError(err)
			e := Parse(raw)
			got := Attribute(e, candidates, "inbox@example.net", false)
			assert.Equal(tc.address, got.Address)
			assert.Equal(tc.basis, got.Basis)
			assert.Equal("inbound", got.Path)
		})
	}
}

func TestSentAndCandidateChanges(t *testing.T) {
	assert := assert.New(t)
	e := Parse([]byte("From: mask@example.org\r\nTo: work@example.com\r\nX-Delivered-To: mask@example.org\r\n\r\nbody"))
	got := Attribute(e, []string{"inbox@example.net"}, "inbox@example.net", false)
	assert.Equal("inbox@example.net", got.Address)
	assert.Contains(e.Mentions(), "mask@example.org", "unconfirmed header occurrences must be indexed")
	got = Attribute(e, []string{"inbox@example.net", "mask@example.org"}, "inbox@example.net", false)
	assert.Equal("mask@example.org", got.Address)
	got = Attribute(e, []string{"inbox@example.net", "mask@example.org"}, "inbox@example.net", true)
	assert.Equal("sent", got.Path)
	assert.Equal("sent-from", got.Basis)
	got = Attribute(e, []string{"inbox@example.net"}, "inbox@example.net", true)
	assert.Empty(got.Address)
	assert.Equal("unconfirmed-sender", got.Basis)
}

func TestBoundedOuterHeadersAndAddressSpelling(t *testing.T) {
	assert := assert.New(t)
	e := Parse([]byte("To: Work+tag@Example.com\r\nCc: work@example.com\r\nBcc: second@example.com\r\n\r\nDelivered-To: mask@example.org"))
	got := Attribute(e, []string{"work+tag@example.com"}, "", false)
	assert.Equal("work+tag@example.com", got.Address)
	assert.NotContains(e.Mentions(), "mask@example.org")
	got = Attribute(e, []string{"second@example.com"}, "", false)
	assert.Empty(got.Address, "archived Bcc is diagnostic only")
	assert.Equal("missing-evidence", got.Basis)
}

func TestHeaderRecoveryAndConservativeSender(t *testing.T) {
	candidates := []string{"inbox@example.net", "work+tag@example.org", "second@example.org"}
	for _, tc := range []struct {
		name, raw, address, basis string
		sent                      bool
	}{
		{"folded-cc", "Delivered-To: inbox@example.net\r\nCc: Work+tag@EXAMPLE.org,\r\n external@example.test\r\n\r\nbody", "work+tag@example.org", "recipient-headers", false},
		{"duplicate-delivery", "Delivered-To: work+tag@example.org\r\nDelivered-To: WORK+TAG@EXAMPLE.ORG\r\n\r\nbody", "work+tag@example.org", "delivery-chain", false},
		{"diagnostic-only", "Bcc: second@example.org\r\nX-Forwarded-To: second@example.org\r\n\r\nbody", "inbox@example.net", "source-default", false},
		{"owned-incoming-from", "From: work+tag@example.org\r\nTo: second@example.org\r\n\r\nbody", "second@example.org", "recipient-headers", false},
		{"missing-sent-from", "To: second@example.org\r\n\r\nbody", "", "missing-sender", true},
		{"multiple-from", "From: work+tag@example.org, second@example.org\r\n\r\nbody", "", "ambiguous", true},
		{"duplicate-from-list", "From: WORK+TAG@EXAMPLE.ORG, Work <work+tag@example.org>\r\n\r\nbody", "work+tag@example.org", "sent-from", true},
		{"duplicate-from-fields", "From: work+tag@example.org\r\nFrom: WORK+TAG@EXAMPLE.ORG\r\n\r\nbody", "work+tag@example.org", "sent-from", true},
		{"multiple-from-unconfirmed", "From: work+tag@example.org, external@example.test\r\n\r\nbody", "", "ambiguous", true},
		{"broken-header", "To: work+tag@example.org\r\ninvalid header line\r\n\r\nbody", "inbox@example.net", "source-default", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			e := Parse([]byte(tc.raw))
			from := append([]string(nil), e.From...)
			got := Attribute(e, candidates, "inbox@example.net", tc.sent)
			assert.Equal(from, e.From, "classification preserves compact evidence for later recomputation")
			assert.Equal(tc.address, got.Address)
			assert.Equal(tc.basis, got.Basis)
		})
	}
	assert := assert.New(t)
	e := Parse([]byte("Subject: " + strings.Repeat("x", MaxHeaderBytes) + "\r\nTo: work+tag@example.org\r\n\r\nbody"))
	assert.True(e.Malformed)
	assert.Empty(e.Mentions())
	e = Parse([]byte("To: work+tag@example.org\r\n\r\n" + strings.Repeat("x", MaxHeaderBytes*2)))
	assert.Equal([]string{"work+tag@example.org"}, e.Visible)
}
