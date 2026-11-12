package sensitive

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestScan(t *testing.T) {
	token := "aB1_cD2-eF3_gH4-iJ5_kL6-mN7_oP8-qR9"
	key := "ghp_" + strings.Repeat("aB12", 9)
	stripe := "sk_" + "live_" + strings.Repeat("aB12", 6)
	restricted := "rk_" + "live_" + strings.Repeat("aB12", 6)
	for _, tc := range []struct {
		name, text string
		want       []Finding
	}{
		{"visa", "4111 1111 1111 1111", []Finding{{"payment_card", 0, 19, "ending 1111"}}},
		{"mastercard", "5555555555554444", []Finding{{"payment_card", 0, 16, "ending 4444"}}},
		{"new mastercard", "2221000000000009", []Finding{{"payment_card", 0, 16, "ending 0009"}}},
		{"amex", "378282246310005", []Finding{{"payment_card", 0, 15, "ending 0005"}}},
		{"discover", "6011111111111117", []Finding{{"payment_card", 0, 16, "ending 1117"}}},
		{"jcb", "3566002020360505", []Finding{{"payment_card", 0, 16, "ending 0505"}}},
		{"diners 16", "3056930009020004", []Finding{{"payment_card", 0, 16, "ending 0004"}}},
		{"diners 14", "36227206271667", []Finding{{"payment_card", 0, 14, "ending 1667"}}},
		{"unionpay", "6200000000000005", []Finding{{"payment_card", 0, 16, "ending 0005"}}},
		{"unionpay 81", "8100000000000002", []Finding{{"payment_card", 0, 16, "ending 0002"}}},
		{"unionpay 19", "6205500000000000004", []Finding{{"payment_card", 0, 19, "ending 0004"}}},
		{"trailing digits", "4111 1111 1111 1111 123", []Finding{{"payment_card", 0, 19, "ending 1111"}}},
		{"expiry", "4111 1111 1111 1111 12/27", []Finding{{"payment_card", 0, 19, "ending 1111"}}},
		{"leading year", "2024 4111 1111 1111 1111", []Finding{{"payment_card", 5, 24, "ending 1111"}}},
		{"adjacent cards", "4111111111111111 5555555555554444", []Finding{{"payment_card", 0, 16, "ending 1111"}, {"payment_card", 17, 33, "ending 4444"}}},
		{"hyphenated card", "4111-1111-1111-1111", []Finding{{"payment_card", 0, 19, "ending 1111"}}},
		{"israeli iban", "IL620108000000099999999", []Finding{{"iban", 0, 23, "ending 9999"}}},
		{"british iban", "GB82WEST12345698765432", []Finding{{"iban", 0, 22, "ending 5432"}}},
		{"iban prose", "DE89 3704 0044 0532 0130 00 THANKS", []Finding{{"iban", 0, 27, "ending 3000"}}},
		{"iban after rejected start", "Reference AB12 DE89370400440532013000", []Finding{{"iban", 15, 37, "ending 3000"}}},
		{"adjacent ibans", "NO9386011117947 NO9386011117947", []Finding{{"iban", 0, 15, "ending 7947"}, {"iban", 16, 31, "ending 7947"}}},
		{"ssn", "123-45-6789", []Finding{{"us_ssn", 0, 11, ""}}},
		{"spaced ssn", "123 45 6789", []Finding{{"us_ssn", 0, 11, ""}}},
		{"stripe", stripe, []Finding{{"api_key", 0, 32, "starts sk_l"}}},
		{"restricted stripe", restricted, []Finding{{"api_key", 0, 32, "starts rk_l"}}},
		{"github", key, []Finding{{"api_key", 0, 40, "starts ghp_"}}},
		{"openai", "sk-" + strings.Repeat("aB12", 5), []Finding{{"api_key", 0, 23, "starts sk-a"}}},
		{"github pat", "github_" + "pat_" + strings.Repeat("aB12", 6), []Finding{{"api_key", 0, 35, "starts gith"}}},
		{"slack", "xoxb" + "-" + strings.Repeat("aB12", 5), []Finding{{"api_key", 0, 25, "starts xoxb"}}},
		{"aws", "AK" + "IA" + strings.Repeat("AB12", 4), []Finding{{"api_key", 0, 20, "starts AKIA"}}},
		{"google", "AI" + "za" + strings.Repeat("aB12", 9), []Finding{{"api_key", 0, 40, "starts AIza"}}},
		{"jwt", "ey" + "Jabcdefgh.ey" + "Jabcdefgh.abcdefgh", []Finding{{"api_key", 0, 32, "starts eyJa"}}},
		{"pem", "-----BEGIN RSA PRIVATE KEY-----", []Finding{{"api_key", 0, 31, ""}}},
		{"password", "password: abcDef123!", []Finding{{"password", 10, 20, ""}}},
		{"json password", `{"password": "abcDef123!"}`, []Finding{{"password", 14, 24, ""}}},
		{"json password escaped quote", `{"password": "abc\"Def123!"}`, []Finding{{"password", 14, 26, ""}}},
		{"yaml password", "'password': 'abcDef123!'", []Finding{{"password", 13, 23, ""}}},
		{"env password", "PASSWORD=abcDef123!", []Finding{{"password", 9, 19, ""}}},
		{"passphrase", `password: "my secret passphrase"`, []Finding{{"password", 11, 31, ""}}},
		{"single quoted phrase", "pwd: 'my secret passphrase'", []Finding{{"password", 6, 26, ""}}},
		{"period password", "password: P@ss.word1", []Finding{{"password", 10, 20, ""}}},
		{"colon password", "pwd: abc:Def123!", []Finding{{"password", 5, 16, ""}}},
		{"api key", "api_key=" + token, []Finding{{"api_key", 8, 43, ""}}},
		{"json key", `{"api_key": "` + token + `"}`, []Finding{{"api_key", 13, 48, ""}}},
		{"hyphen label", "x-api-key: " + token, []Finding{{"api_key", 11, 46, ""}}},
		{"camel label", "secretKey=" + token, []Finding{{"api_key", 10, 45, ""}}},
		{"acronym key label", "APIKey: abcDef123!", []Finding{{"api_key", 8, 18, ""}}},
		{"acronym password label", "DBPassword: abcDef123!", []Finding{{"password", 12, 22, ""}}},
		{"env token", "ACCESS_TOKEN=" + token, []Finding{{"api_key", 13, 48, ""}}},
		{"bearer", "Authorization: Bearer " + token, []Finding{{"api_key", 22, 57, ""}}},
		{"hex token", "TWILIO_AUTH_TOKEN=0123456789abcdef0123456789abcdef", []Finding{{"api_key", 18, 50, ""}}},
		{"prefixed overlap", "api_key=" + stripe, []Finding{{"api_key", 8, 40, "starts sk_l"}}},
		{"prefixed overlap trailing comma", "token: sk-" + strings.Repeat("aB12", 5) + ",", []Finding{{"api_key", 7, 30, "starts sk-a"}}},
		{"forgotten", "password: forgotten", nil},
		{"forgotten period", "password: forgotten.", nil},
		{"key underscore only", "api_key: abcdefg_", nil},
		{"password hyphen only", "password: abcdefg-", nil},
		{"required", "Password: Required", nil},
		{"url", "password: https://example.com/reset", nil},
		{"windows path", `pwd: C:\Users\bob`, nil},
		{"next line", "password:\nabcDef123!", nil},
		{"masked", "password: ********", nil},
		{"template", "password: <your-password>", nil},
		{"changeme", "password: changeme", nil},
		{"redacted", "password: redacted", nil},
		{"short", "password: abcd12!", nil},
		{"epoch ms", "1793712345678", nil},
		{"invalid luhn", "4111 1111 1111 1112", nil},
		{"oversized card", "41111111111111111234", nil},
		{"embedded card", "order_4111111111111111", nil},
		{"bare sha", "0123456789abcdef0123456789abcdef01234567", nil},
		{"hotkey", "Hotkey: Ctrl+Shift+9", nil},
		{"keyword", "keyword: abc12345", nil},
		{"monkey", "monkey=" + token, nil},
		{"secretary", "Secretary: Jane-Doe2", nil},
		{"passwordless", "passwordless: abcDef123!", nil},
		{"key phrase", `{"key": "Main office hours"}`, nil},
		{"secret phrase", `secret: "don't tell anyone"`, nil},
		{"token phrase with digits", `token: "two words 123"`, nil},
		{"embedded key", "prefix_" + key, nil},
		{"unknown iban country", "ZZ22370400440532013000", nil},
		{"oversized iban", "DE813704004405320130000", nil},
		{"bad iban checksum", "DE88370400440532013000", nil},
		{"mixed ssn separators", "123-45 6789", nil},
		{"ssn area zero", "000-12-3456", nil},
		{"ssn area 666", "666-12-3456", nil},
		{"ssn area nine", "900-12-3456", nil},
		{"ssn group zero", "123-00-4567", nil},
		{"ssn serial zero", "123-45-0000", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			findings := Scan(tc.text)
			assert.Equal(tc.want, findings)
			for _, finding := range findings {
				assert.NotContains(finding.Hint, tc.text[finding.Start:finding.End])
			}
		})
	}
}
