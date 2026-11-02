package query

import (
	"bytes"
	"compress/zlib"
	"crypto/rand"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/senderauth"
)

func TestSenderAuthHeaders(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	const good = "From: Sender <sender@example.test>\r\nAuthentication-Results: mx.test; dmarc=pass header.from=example.test\r\n\r\n"
	for _, tc := range []struct {
		name, raw, from string
		count           int
		trust           senderauth.TrustSet
		verified        bool
		basis           senderauth.Basis
	}{
		{"trusted", good, "sender@example.test", 1, senderauth.TrustSet{"mx.test"}, true, senderauth.ProviderHeader},
		{"quoted local part", "From: \"a@b\"@example.test\r\nAuthentication-Results: mx.test; dmarc=pass header.from=example.test\r\n\r\n", `"a@b"@example.test`, 1, senderauth.TrustSet{"mx.test"}, true, senderauth.ProviderHeader},
		{"empty pins", good, "sender@example.test", 1, nil, false, senderauth.Untrusted},
		{"display mismatch", good, "other@other.test", 1, senderauth.TrustSet{"mx.test"}, false, senderauth.FromMismatch},
		{"ambiguous displayed from", good, "sender@example.test", 2, senderauth.TrustSet{"mx.test"}, false, senderauth.FromMismatch},
		{"multiple raw From", "From: second@example.test\r\n" + good, "sender@example.test", 1, senderauth.TrustSet{"mx.test"}, false, senderauth.FromMismatch},
		{"truncated", "From: sender@example.test\r\n", "sender@example.test", 1, senderauth.TrustSet{"mx.test"}, false, senderauth.HeaderTruncated},
		{"forged boundary", "From: sender@example.test\r\nAuthentication-Results: evil.test; none\r\nAuthentication-Results: mx.test; dmarc=pass header.from=example.test\r\n\r\n", "sender@example.test", 1, senderauth.TrustSet{"mx.test"}, false, senderauth.Untrusted},
		{"no results", "From: sender@example.test\r\n\r\n", "sender@example.test", 1, senderauth.TrustSet{"mx.test"}, false, senderauth.NoHeader},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checks := assert.New(t)
			v := senderAuthFromRaw([]byte(tc.raw), "none", tc.from, tc.count, tc.trust)
			checks.Equal(tc.verified, v.Verified)
			checks.Equal(tc.basis, v.Basis)
		})
	}
	// The stored prefix is deliberately not a complete zlib stream. Its header
	// must remain usable without reading or allocating the random attachment.
	body := make([]byte, 2*1024*1024)
	_, err := rand.Read(body)
	must.NoError(err)
	var compressed bytes.Buffer
	zw := zlib.NewWriter(&compressed)
	_, err = zw.Write(append([]byte(good), body...))
	must.NoError(err)
	must.NoError(zw.Close())
	must.Greater(compressed.Len(), senderAuthPrefixBytes)
	v := senderAuthFromRaw(compressed.Bytes()[:senderAuthPrefixBytes], "zlib", "sender@example.test", 1, senderauth.TrustSet{"mx.test"})
	checks.True(v.Verified)
	oversized := []byte("From: sender@example.test\r\nX-Large: " + string(bytes.Repeat([]byte("x"), senderAuthHeaderBytes)) + "\r\n\r\n")
	checks.Equal(senderauth.HeaderTruncated, senderAuthFromRaw(oversized, "none", "sender@example.test", 1, nil).Basis)
}

func FuzzSenderAuthHeaders(f *testing.F) {
	f.Add([]byte("From: sender@example.test\r\n\r\n"), "none")
	f.Fuzz(func(t *testing.T, data []byte, compression string) {
		checks := assert.New(t)
		v := senderAuthFromRaw(data, compression, "sender@example.test", 1, senderauth.TrustSet{"mx.test"})
		if v.Verified {
			checks.Equal(senderauth.Pass, v.DMARC)
			checks.Equal(senderauth.ProviderHeader, v.Basis)
			checks.Equal("example.test", v.VerifiedDomain)
		}
	})
}

func TestSenderAuthDisplayedSender(t *testing.T) {
	checks := assert.New(t)
	original := &senderauth.Verdict{Kind: "email", DMARC: senderauth.Pass, Verified: true, VerifiedDomain: "example.test", Basis: senderauth.ProviderHeader}
	checks.True(SenderAuthForAddresses(original, []Address{{Email: "sender@example.test"}}).Verified)
	for _, from := range [][]Address{nil, {{Email: "sender@evil.test"}}, {{Email: "sender@example.test"}, {Email: "other@example.test"}}} {
		verdict := SenderAuthForAddresses(original, from)
		checks.False(verdict.Verified)
		checks.Equal(senderauth.FromMismatch, verdict.Basis)
	}
	checks.True(original.Verified, "binding a cached row must not mutate shared receipt evidence")
}
