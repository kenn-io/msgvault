package sensitive

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScan(t *testing.T) {
	for _, tc := range []struct{ name, text, kind string }{
		{"visa", "4111 1111 1111 1111", "payment_card"},
		{"amex", "378282246310005", "payment_card"},
		{"mastercard", "5555555555554444", "payment_card"},
		{"mastercard new range", "2221000000000009", "payment_card"},
		{"discover", "6011111111111117", "payment_card"},
		{"iban", "DE89 3704 0044 0532 0130 00", "iban"},
		{"british iban", "GB82WEST12345698765432", "iban"},
		{"ssn", "123-45-6789", "us_ssn"},
		{"spaced ssn", "123 45 6789", "us_ssn"},
		{"prefixed key", "ghp_" + strings.Repeat("aB12", 9), "api_key"},
		{"openai key", "sk-" + strings.Repeat("synthetic", 4), "api_key"},
		{"stripe key", "sk_live_" + strings.Repeat("synthetic", 4), "api_key"},
		{"github fine grained", "github_pat_" + strings.Repeat("synthetic", 4), "api_key"},
		{"slack key", "xoxb-" + strings.Repeat("synthetic", 4), "api_key"},
		{"aws key", "AKIA" + strings.Repeat("A1", 8), "api_key"},
		{"google key", "AIza" + strings.Repeat("synthetic", 4), "api_key"},
		{"jwt", "eyJ" + strings.Repeat("a", 12) + ".eyJ" + strings.Repeat("b", 12) + "." + strings.Repeat("c", 12), "api_key"},
		{"private key", "-----BEGIN RSA PRIVATE KEY-----", "api_key"},
		{"anchored token", "token: aB1_cD2-eF3_gH4-iJ5_kL6-mN7_oP8-qR9", "api_key"}, // betterleaks:allow -- synthetic detector fixture
		{"password", "password: synthetic-password", "password"},
		{"password is", "password is synthetic-password", "password"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			findings := Scan("Before\n" + tc.text + "\nAfter")
			require.Len(t, findings, 1)
			assert.Equal(tc.kind, findings[0].Kind)
			assert.Equal("authored", findings[0].Origin)
			assert.NotContains(findings[0].Hint, tc.text)
			assert.Contains(tc.text, ("Before\n" + tc.text + "\nAfter")[findings[0].Start:findings[0].End])
		})
	}
}

func TestIBANFollowedByUppercaseProse(t *testing.T) {
	findings := Scan("AT61 1904 3002 3457 3201 PAID TODAY")
	require.Len(t, findings, 1)
	assert.Equal(t, "iban", findings[0].Kind)
	assert.Equal(t, "IBAN ending 3201", findings[0].Hint)
}

func TestCardChecksumRejectsChangedCheckDigit(t *testing.T) {
	for _, card := range []string{"4111111111111111", "378282246310005", "5555555555554444", "2221000000000009", "6011111111111117"} {
		for digit := byte('0'); digit <= '9'; digit++ {
			if digit == card[len(card)-1] {
				continue
			}
			assert.Empty(t, Scan(card[:len(card)-1]+string(digit)))
		}
	}
}

func TestCardNumericFields(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		want       []string
	}{
		{"expiry", "4111 1111 1111 1111 12/27", []string{"4111 1111 1111 1111"}},
		{"security code", "4111111111111111 123", []string{"4111111111111111"}},
		{"adjacent cards", "4111111111111111 5555555555554444", []string{"4111111111111111", "5555555555554444"}},
		{"adjacent grouped cards", "4111 1111 1111 1111 5555 5555 5555 4444", []string{"4111 1111 1111 1111", "5555 5555 5555 4444"}},
		{"unbroken oversized run", "41111111111111111234", nil},
		{"unbroken adjacent cards", "41111111111111115555555555554444", nil},
		{"invalid checksum", "4111 1111 1111 1112 12/27", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			text := "Review: " + tc.text
			var values []string
			for _, finding := range Scan(text) {
				assert.Equal("payment_card", finding.Kind)
				values = append(values, text[finding.Start:finding.End])
			}
			assert.Equal(tc.want, values)
		})
	}
}

func TestAnchoredTokenContext(t *testing.T) {
	token := strings.Join([]string{"aB1", "cD2", "eF3", "gH4", "iJ5", "kL6", "mN7", "oP8", "qR9"}, "_")
	for _, prefix := range []string{
		"https://example.com/doc\ntoken: ",
		"/var/cache/doc\ntoken: ",
		"Call /api with token: ",
		"key /api token: ",
		"token: /api\ntoken: ",
	} {
		t.Run(prefix, func(t *testing.T) {
			assert := assert.New(t)
			findings := Scan(prefix + token)
			require.Len(t, findings, 1)
			assert.Equal("api_key", findings[0].Kind)
			assert.Equal(len(prefix), findings[0].Start)
			assert.Equal(len(prefix)+len(token), findings[0].End)
		})
	}
	for _, prefix := range []string{"token: https://example.com/", "token: /var/cache/", "token: C:\\cache\\"} {
		assert.Empty(t, Scan(prefix+token))
	}
}

func TestScanIgnoresOrdinaryText(t *testing.T) {
	for _, text := range []string{
		"Please reset your password", "password: <redacted>", "pwd: ******", "password is your password",
		"password: [redacted]", "password: redacted", "password: password", "passwordless: example123",
		"4111111111111112", "14111111111111111111", "41111111111111111111", "1111111111111111",
		"order_4111111111111111", "2026-10-31 13:58:40", "+1-415-555-0123",
		"DE88 3704 0044 0532 0130 00", "000-12-3456", "666-12-3456", "900-12-3456",
		"123-00-4567", "123-45-0000", "123456789", "0123-45-6789", "123-45-67890",
		"sk-short", "key: 0123456789abcdef0123456789abcdef", "secret: 550e8400-e29b-41d4-a716-446655440000",
		"https://example.com/aB1_cD2-eF3_gH4-iJ5_kL6-mN7_oP8-qR9",
		"token: https://example.com/aB1_cD2-eF3_gH4-iJ5_kL6-mN7_oP8-qR9",
		"aB1_cD2-eF3_gH4-iJ5_kL6-mN7_oP8-qR9",
	} {
		t.Run(text, func(t *testing.T) { assert.Empty(t, Scan(text)) })
	}
}

func FuzzScan(f *testing.F) {
	for _, seed := range []string{"", "password: synthetic-password", "4111 1111 1111 1111", "DE89 3704 0044 0532 0130 00", "\xff\x00"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		findings := Scan(text)
		assert.Equal(t, findings, Scan(text))
		last := 0
		for _, finding := range findings {
			require.GreaterOrEqual(t, finding.Start, last)
			require.Greater(t, finding.End, finding.Start)
			require.LessOrEqual(t, finding.End, len(text))
			assert.NotContains(t, finding.Hint, text[finding.Start:finding.End])
			last = finding.Start
		}
	})
}

func TestAnchoredTokensAcrossOtherFindings(t *testing.T) {
	assert := assert.New(t)
	token := strings.Join([]string{"aB1", "cD2", "eF3", "gH4", "iJ5", "kL6", "mN7", "oP8", "qR9"}, "_")
	known := "ghp_" + strings.Repeat("aB12", 9)
	parts := []string{"token: " + token, "token: " + known, "123-45-6789", "4111 1111 1111 1111", "token: " + token, "token: " + known}
	findings := Scan(strings.Join(parts, "\n"))
	require.Len(t, findings, 6)
	assert.Equal([]string{"api_key", "api_key", "us_ssn", "payment_card", "api_key", "api_key"}, []string{findings[0].Kind, findings[1].Kind, findings[2].Kind, findings[3].Kind, findings[4].Kind, findings[5].Kind})
	assert.Equal("anchored secret token", findings[0].Hint)
	assert.Equal("key prefix ghp_", findings[1].Hint)
	assert.Equal("anchored secret token", findings[4].Hint)
	assert.Equal("key prefix ghp_", findings[5].Hint)
}

func BenchmarkScanAnchoredTokens(b *testing.B) {
	token := strings.Join([]string{"aB1", "cD2", "eF3", "gH4", "iJ5", "kL6", "mN7", "oP8", "qR9"}, "_")
	for _, count := range []int{1000, 10000, 100000} {
		b.Run(strconv.Itoa(count), func(b *testing.B) {
			text := strings.Repeat("token: "+token+"\n", count)
			b.SetBytes(int64(len(text)))
			b.ReportAllocs()
			for b.Loop() {
				require.Len(b, Scan(text), count)
			}
		})
	}
}
