package senderauth

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name     string
		headers  []Header
		trust    TrustSet
		from     string
		verified bool
		dmarc    Status
		basis    Basis
	}{
		{"gmail", []Header{{"Authentication-Results", "mx.example.test; spf=pass; dkim=pass; dmarc=pass header.from=sender.example.test"}}, TrustSet{"mx.example.test"}, "sender.example.test", true, Pass, ProviderHeader},
		{"fastmail block", []Header{{"Authentication-Results", "mx1.provider.test; x-ptr=pass"}, {"Authentication-Results", "mx2.provider.test; spf=pass"}, {"Authentication-Results", "mx3.provider.test; dkim=pass"}, {"Authentication-Results", "mx4.provider.test; dmarc=pass header.from=sender.test"}}, TrustSet{"*.provider.test"}, "sender.test", true, Pass, ProviderHeader},
		{"proton folded comments", []Header{{"Authentication-Results", "MAIL.provider.test (comment); spf=pass"}, {"Authentication-Results", "mail.provider.test;\r\n\tDMARC=PASS (ok) header.from=Sender.Test"}}, TrustSet{"mail.provider.test"}, "sender.test", true, Pass, ProviderHeader},
		{"forged", []Header{{"Authentication-Results", "evil.test; dmarc=pass header.from=sender.test"}}, TrustSet{"mx.provider.test"}, "sender.test", false, Unknown, Untrusted},
		{"boundary", []Header{{"Authentication-Results", "mx.provider.test; spf=pass"}, {"Authentication-Results", "evil.test; none"}, {"Authentication-Results", "mx.provider.test; dmarc=pass header.from=sender.test"}}, TrustSet{"mx.provider.test"}, "sender.test", false, Unknown, ProviderHeader},
		{"mismatch", []Header{{"Authentication-Results", "mx.provider.test; dmarc=pass header.from=other.test"}}, TrustSet{"mx.provider.test"}, "sender.test", false, Pass, FromMismatch},
		{"unknown", []Header{{"Authentication-Results", "mx.provider.test; dmarc=surprise header.from=sender.test"}}, TrustSet{"mx.provider.test"}, "sender.test", false, Unknown, ProviderHeader},
		{"empty", nil, TrustSet{"mx.provider.test"}, "sender.test", false, Unknown, NoHeader},
		{"malformed", []Header{{"Authentication-Results", "mx.provider.test; dmarc=pass header.from=sender.test ("}}, TrustSet{"mx.provider.test"}, "sender.test", false, Unknown, Unparseable},
		{"arc only", []Header{{"ARC-Authentication-Results", "i=1; mx.provider.test; dmarc=pass header.from=sender.test"}}, TrustSet{"mx.provider.test"}, "sender.test", false, Unknown, NoHeader},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Parse(tc.headers, tc.trust, tc.from, false)
			assert.Equal(t, tc.verified, got.Verified)
			assert.Equal(t, tc.dmarc, got.DMARC)
			assert.Equal(t, tc.basis, got.Basis)
		})
	}
}

func TestTruncated(t *testing.T) {
	got := Parse([]Header{{"Authentication-Results", "mx.test; dmarc=pass header.from=sender.test"}}, TrustSet{"mx.test"}, "sender.test", true)
	assert.False(t, got.Verified)
	assert.Equal(t, HeaderTruncated, got.Basis)
}

func FuzzParse(f *testing.F) {
	f.Add("mx.test; dmarc=pass header.from=sender.test")
	f.Add(`mx.test; spf=pass reason="; dmarc=pass header.from=sender.test"`)
	f.Add(`mx.test; dmarc=pass header.from=sender.test header.b="a\"; dmarc=fail"`)
	f.Fuzz(func(t *testing.T, value string) {
		got := Parse([]Header{{"Authentication-Results", value}}, TrustSet{"mx.test"}, "sender.test", false)
		if got.Verified {
			assert.Equal(t, Pass, got.DMARC)
			assert.Equal(t, ProviderHeader, got.Basis)
			assert.NotEmpty(t, got.AuthservID)
		}
		assert.Contains(t, []Status{Unknown, Pass, Fail, Softfail, Neutral, None, Temperror, Permerror}, got.DMARC)
	})
}

func TestTrustAndMalformedInputs(t *testing.T) {
	tests := []struct {
		name, value string
		trust       TrustSet
		from        string
	}{
		{"wildcard excludes apex", "provider.test; dmarc=pass header.from=sender.test", TrustSet{"*.provider.test"}, "sender.test"},
		{"wildcard excludes lookalike", "evilprovider.test; dmarc=pass header.from=sender.test", TrustSet{"*.provider.test"}, "sender.test"},
		{"subdomain is not aligned", "mx.test; dmarc=pass header.from=sender.test", TrustSet{"mx.test"}, "sub.sender.test"},
		{"missing from", "mx.test; dmarc=pass", TrustSet{"mx.test"}, "sender.test"},
		{"duplicate from", "mx.test; dmarc=pass header.from=sender.test header.from=evil.test", TrustSet{"mx.test"}, "sender.test"},
		{"comment injection", "mx.test; spf=pass (dmarc=pass header.from=sender.test)", TrustSet{"mx.test"}, "sender.test"},
		{"quoted injection", "mx.test; spf=pass reason=\"; dmarc=pass header.from=sender.test\"", TrustSet{"mx.test"}, "sender.test"},
		{"broken fold", "mx.test; dmarc=pass\n header.from=sender.test", TrustSet{"mx.test"}, "sender.test"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.False(t, Parse([]Header{{"Authentication-Results", tc.value}}, tc.trust, tc.from, false).Verified)
		})
	}
}

func TestARCRemainsAnUntrustedObservation(t *testing.T) {
	arc := "i=1; mx.test; dmarc=pass header.from=sender.test"
	got := Parse([]Header{{"ARC-Authentication-Results", arc}}, TrustSet{"mx.test"}, "sender.test", false)
	assert.Equal(t, []string{arc}, got.ARC)
	assert.False(t, got.Verified)
}

func TestPropertyValuesAndTrustedBoundary(t *testing.T) {
	const trusted = "mx.test; dmarc=pass header.from=sender.test"
	tests := []struct {
		name     string
		headers  []Header
		verified bool
		dmarc    Status
		basis    Basis
	}{
		{"quoted property", []Header{{"Authentication-Results", trusted + ` header.b="abc="`}}, true, Pass, ProviderHeader},
		{"IP property", []Header{{"Authentication-Results", trusted + " smtp.remote-ip=192.0.2.1"}}, true, Pass, ProviderHeader},
		{"IPv6 property", []Header{{"Authentication-Results", trusted + " smtp.remote-ip=2001:db8::1"}}, true, Pass, ProviderHeader},
		{"quoted semicolon is not a method", []Header{{"Authentication-Results", `mx.test; spf=pass reason="; dmarc=pass header.from=sender.test"`}}, false, Unknown, ProviderHeader},
		{"escaped quote and slash", []Header{{"Authentication-Results", trusted + ` header.b="a\"; dmarc=fail \\ end"`}}, true, Pass, ProviderHeader},
		{"unclosed quote", []Header{{"Authentication-Results", trusted + ` header.b="abc`}}, false, Unknown, Unparseable},
		{"unclosed comment", []Header{{"Authentication-Results", trusted + " (oops"}}, false, Unknown, Unparseable},
		{"quoted from", []Header{{"Authentication-Results", `mx.test; dmarc=pass header.from="sender.test"`}}, true, Pass, ProviderHeader},
		{"escaped quoted from", []Header{{"Authentication-Results", `mx.test; dmarc=pass header.from="sen\der.test"`}}, true, Pass, ProviderHeader},
		{"quoted from injection", []Header{{"Authentication-Results", `mx.test; dmarc=pass header.from="sender.test; dmarc=pass"`}}, false, Unknown, Unparseable},
		{"quoted from whitespace", []Header{{"Authentication-Results", `mx.test; dmarc=pass header.from=" sender.test "`}}, false, Unknown, Unparseable},
		{"junk property rejected", []Header{{"Authentication-Results", trusted + " smtp.remote-ip=:::"}}, false, Unknown, Unparseable},
		{"junk after quote rejected", []Header{{"Authentication-Results", trusted + ` header.b="ok"junk`}}, false, Unknown, Unparseable},
		{"first dmarc result wins", []Header{{"Authentication-Results", `mx.test; dmarc=fail header.b="; dmarc=pass"; dmarc=pass header.from=sender.test`}}, false, Fail, ProviderHeader},
		{"invalid from rejected", []Header{{"Authentication-Results", `mx.test; dmarc=pass header.from=sender.test:123`}}, false, Unknown, Unparseable},
		{"trusted prefix survives malformed untrusted boundary", []Header{{"Authentication-Results", trusted}, {"Authentication-Results", `evil.test; broken="`}}, true, Pass, ProviderHeader},
		{"untrusted boundary with comment", []Header{{"Authentication-Results", trusted}, {"Authentication-Results", `evil.test (a; b); broken="`}}, true, Pass, ProviderHeader},
		{"untrusted boundary blocks forged trusted header", []Header{{"Authentication-Results", `evil.test; broken="`}, {"Authentication-Results", trusted}}, false, Unknown, Untrusted},
		{"malformed trusted header clears prefix", []Header{{"Authentication-Results", trusted}, {"Authentication-Results", `mx.test; broken="`}}, false, Unknown, Unparseable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Parse(tc.headers, TrustSet{"mx.test"}, "sender.test", false)
			assert.Equal(t, tc.verified, got.Verified)
			assert.Equal(t, tc.dmarc, got.DMARC)
			assert.Equal(t, tc.basis, got.Basis)
		})
	}
}

func TestAssignmentBoundaries(t *testing.T) {
	for _, value := range []string{
		"mx.test; spf=pass smtp.mailfrom=a=b@sender.test; dmarc=pass header.from=sender.test",
		"mx.test; spf=pass smtp.mailfrom=a%b@sender.test; dmarc=pass header.from=sender.test",
		"mx.test; spf=pass smtp.mailfrom=o'hara@sender.test; dmarc=pass header.from=sender.test",
		"mx.test; dmarc = pass header . from = sender.test",
		"mx.test; dmarc(comment)=(comment)pass header(comment).(comment)from = sender.test",
		"mx.test; dmarc\r\n\t=\r\n pass header.from=sender.test",
	} {
		t.Run(value, func(t *testing.T) {
			assert.True(t, Parse([]Header{{"Authentication-Results", value}}, TrustSet{"mx.test"}, "sender.test", false).Verified)
		})
	}
	for _, value := range []string{"mx.test; dmarc= passheader.from=sender.test", "mx.test; dmarc=pass header.from=sender.test junk", "mx.test; dmarc=pass header..from=sender.test"} {
		assert.False(t, Parse([]Header{{"Authentication-Results", value}}, TrustSet{"mx.test"}, "sender.test", false).Verified)
	}
}

func TestUnidentifiableHeaderStopsTrustedBlock(t *testing.T) {
	trusted := Header{"Authentication-Results", "mx.test; dmarc=pass header.from=sender.test"}
	for _, unknown := range []string{"; dmarc=pass", "broken", "(unterminated; dmarc=pass"} {
		t.Run(unknown, func(t *testing.T) {
			boundary := Header{"Authentication-Results", unknown}
			assert.True(t, Parse([]Header{trusted, boundary}, TrustSet{"mx.test"}, "sender.test", false).Verified)
			assert.False(t, Parse([]Header{boundary, trusted}, TrustSet{"mx.test"}, "sender.test", false).Verified)
		})
	}
}

func TestVersionedMethodsAndQuotedMailboxes(t *testing.T) {
	for _, value := range []string{
		`mx.test; dkim/1=pass; dmarc=pass header.from=sender.test`,
		`mx.test; dkim / 1 = pass; dmarc/1=pass header.from=sender.test`,
		`mx.test; dkim/9=pass; dmarc=pass header.from=sender.test`,
		`mx.test; spf=pass smtp.mailfrom="a b"@sender.test; dmarc=pass header.from=sender.test`,
		`mx.test; spf=pass smtp.mailfrom="a; dmarc=fail" @ sender.test; dmarc=pass header.from=sender.test`,
		`"mx.test"; dmarc=pass header.from=sender.test`,
	} {
		t.Run(value, func(t *testing.T) {
			assert.True(t, Parse([]Header{{"Authentication-Results", value}}, TrustSet{"mx.test"}, "sender.test", false).Verified)
		})
	}
	for _, value := range []string{
		`mx.test; dmarc/9=pass header.from=sender.test`,
		`mx.test; dmarc/=pass header.from=sender.test`,
		`mx.test; spf=pass smtp.mailfrom="a b"@; dmarc=pass header.from=sender.test`,
		`mx.test; spf=pass smtp.mailfrom="a; dmarc=pass header.from=sender.test"@sender.test`,
		`mx.test; none; dmarc=pass header.from=sender.test`,
	} {
		assert.False(t, Parse([]Header{{"Authentication-Results", value}}, TrustSet{"mx.test"}, "sender.test", false).Verified)
	}
}
