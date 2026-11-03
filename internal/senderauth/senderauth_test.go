package senderauth

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestEvaluate(t *testing.T) {
	const pass = "mx.google.com; spf=pass smtp.mailfrom=sender@example.test; dkim=pass header.d=example.test; dmarc=pass header.from=example.test"
	for _, tc := range []struct {
		name, from, displayed, results string
		want                           Verdict
	}{
		{"folded comments", "Sender <sender@example.test>", "sender@example.test", "mx.google.com (receiver);\r\n\tspf=pass (sender (nested)) smtp.mailfrom=sender@example.test;\r\n\tdkim=pass header.d=example.test; dmarc=pass (policy) header.from=example.test", Verdict{true, "example.test", "mx.google.com", "pass", "pass", "pass"}},
		{"windows-1252 name", "=?windows-1252?Q?Jos=E9?= <sender@example.test>", "sender@example.test", pass, Verdict{true, "example.test", "mx.google.com", "pass", "pass", "pass"}},
		{"quoted reason", "sender@example.test", "sender@example.test", `mx.google.com; dmarc=pass reason="reported header.from=example.test detail"`, Verdict{AuthservID: "mx.google.com", DMARC: "pass"}},
		{"quoted policy note", "sender@example.test", "sender@example.test", `mx.google.com; dmarc=pass header.from=other.test policy.note="details header.from=example.test here"`, Verdict{Domain: "other.test", AuthservID: "mx.google.com", DMARC: "pass"}},
		{"case insensitive pin", "sender@example.test", "sender@example.test", "MX.GOOGLE.COM; dmarc=pass header.from=EXAMPLE.TEST", Verdict{Verified: true, Domain: "example.test", AuthservID: "MX.GOOGLE.COM", DMARC: "pass"}},
		{"untrusted topmost", "sender@example.test", "sender@example.test", "untrusted.example.test; dmarc=pass header.from=example.test\r\nAuthentication-Results: " + pass, Verdict{}},
		{"lower same ID", "sender@example.test", "sender@example.test", "mx.google.com; spf=pass\r\nAuthentication-Results: " + pass, Verdict{AuthservID: "mx.google.com", SPF: "pass"}},
		{"wrong reported domain", "sender@example.test", "sender@example.test", "mx.google.com; dmarc=pass header.from=other.test", Verdict{Domain: "other.test", AuthservID: "mx.google.com", DMARC: "pass"}},
		{"wrong displayed domain", "sender@example.test", "sender@other.test", pass, Verdict{Domain: "example.test", AuthservID: "mx.google.com", SPF: "pass", DKIM: "pass", DMARC: "pass"}},
		{"two senders", "sender@example.test, other@example.test", "sender@example.test", pass, Verdict{Domain: "example.test", AuthservID: "mx.google.com", SPF: "pass", DKIM: "pass", DMARC: "pass"}},
		{"comment injection", "sender@example.test", "sender@example.test", "mx.google.com; spf=pass (ignored; dmarc=pass header.from=example.test)", Verdict{AuthservID: "mx.google.com", SPF: "pass"}},
		{"quoted semicolon", "sender@example.test", "sender@example.test", `mx.google.com; spf=pass smtp.mailfrom="a; dmarc=pass header.from=example.test"`, Verdict{AuthservID: "mx.google.com", SPF: "pass"}},
		{"policy and first results", "sender@example.test", "sender@example.test", "mx.google.com; dkim=policy; dkim=pass; dmarc=fail header.from=example.test; dmarc=pass header.from=example.test", Verdict{Domain: "example.test", AuthservID: "mx.google.com", DKIM: "policy", DMARC: "fail"}},
		{"first empty result", "sender@example.test", "sender@example.test", "mx.google.com; dmarc= header.from=other.test; dmarc=pass header.from=example.test", Verdict{Domain: "other.test", AuthservID: "mx.google.com"}},
		{"quoted local part", `"a@b c"@example.test`, "a@b c@example.test", pass, Verdict{true, "example.test", "mx.google.com", "pass", "pass", "pass"}},
		{"UTF-8 and mailbox comment", "Séndér <sender@example.test>", "sender@example.test", "mx.google.com; spf=pass smtp.mailfrom=sender(comment)@example.test; dmarc=pass header.from=example.test", Verdict{Verified: true, Domain: "example.test", AuthservID: "mx.google.com", SPF: "pass", DMARC: "pass"}},
		{"unterminated comment", "sender@example.test", "sender@example.test", pass + " (", Verdict{}},
		{"missing results", "sender@example.test", "sender@example.test", "", Verdict{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			block := "From: " + tc.from + "\r\n"
			if tc.results != "" {
				block += "Authentication-Results: " + tc.results + "\r\n"
			}
			block += "\r\n"
			assert.Equal(t, &tc.want, Evaluate([]byte(block), []string{"mx.google.com"}, tc.displayed))
		})
	}
}
