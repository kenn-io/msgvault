package verification

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIs(t *testing.T) {
	type testCase struct {
		name    string
		content Content
		want    bool
	}
	cases := []testCase{
		// Short base64 segments require normalization; no segment qualifies on its own.
		{"encoded base64 reset token", Content{Text: "Reset your password: https://example.test/reset?token=Q7r%2BNy9%2FDe2%2BWv5%2FAp8%2BKs3%2FJm6%3D"}, true},
		{"encoded base64 reset code", Content{Text: "Reset your password: https://example.test/reset?code=Q7r%2BNy9%2FDe2%2BWv5%2FAp8%2BKs3%2FJm6%3D"}, true},
		{"encoded next path", Content{Text: "Sign in: https://example.test/reset?next=%2Faccount%2Fsettings%2Fnotifications2"}, false},
		{"alphabetic magic link", Content{Text: "Sign in: https://example.test/api/auth/magic-link/verify?token=AbCdEfGhIjKlMnOpQrStUvWxYzAbCdEf&callbackURL=%2Fdashboard"}, true},
		{"alphabetic code key", Content{Text: "https://example.test/verify?code=AbCdEfGhIjKlMnOpQrStUvWxYzAbCdEf"}, true},
		{"short alphabetic token", Content{Text: "Sign in: https://example.test/verify?token=AbCdEfGhIjKlMnOpQrStUvWxYzAbCdE"}, false},
		{"lowercase alphabetic token", Content{Text: "Sign in: https://example.test/verify?token=abcdefghijklmnopqrstuvwxyzabcdefghijklmn"}, true},
		{"alphabetic unsubscribe token", Content{Text: "https://example.test/unsubscribe?token=AbCdEfGhIjKlMnOpQrStUvWxYzAbCdEf"}, false},
		{"long hyphenated slug", Content{Text: "Sign in: https://example.test/articles/AbCdEfGhIjKlMnOpQrStUvWxYz-AbCdEfGhIjKlMnOpQrStUvWxYz"}, false},
		{"long hyphenated campaign", Content{Text: "Sign in: https://example.test/articles?utm_campaign=AbCdEfGhIjKlMnOpQrStUvWxYz-AbCdEfGhIjKlMnOpQrStUvWxYz"}, false},
		{"alphabetic verification code", Content{Text: "Your verification code is ABCDEF"}, true},
		{"alphabetic security code", Content{Text: "Your security code: ABCDEFG"}, true},
		{"hyphenated alphabetic confirmation code", Content{Text: "Confirmation code: ABC-DEF"}, false},
		{"short alphabetic promo code", Content{Text: "Promo code SAVE"}, false},
		{"short alphabetic verification code", Content{Text: "Use the verification code ABC at checkout"}, false},
		{"pending verification code", Content{Text: "Your verification code is pending"}, false},
		{"lowercase login code", Content{Text: "Your login code: 7f3a9c"}, true},
		{"lowercase sign-in code", Content{Text: "Your sign-in code is kd8fq2"}, true},
		{"lowercase alphabetic verification code", Content{Text: "Your verification code is hjkmqp"}, false},
		{"invalid login code", Content{Text: "Your login code is invalid"}, false},
		{"lowercase order code", Content{Text: "Order code a8f3k2 shipped"}, false},
		{"invalid security code", Content{Text: "Your security code is invalid"}, false},
		{"alphabetic order code", Content{Text: "Order code ABCDEF shipped"}, false},
		{"German lautet", Content{Text: "Ihr Bestätigungscode lautet 482913"}, true},
		{"verification code was", Content{Text: "Your verification code was 482913"}, true},
		{"German war", Content{Text: "Ihr Sicherheitscode war 482913"}, true},
		{"code was", Content{Text: "Your code was 482913"}, true},
		{"verification code sent yesterday", Content{Text: "Your verification code was sent yesterday"}, false},
		{"order code updated count", Content{Text: "Order code was updated for 482913 accounts"}, false},
		{"security code after as", Content{Text: "Use 482913 as your Dropbox security code"}, true},
		{"verify login after code", Content{Text: "Use 482913 to verify your login"}, true},
		{"confirm identity after code", Content{Text: "Enter 482913 on the Uber app to confirm it's you"}, true},
		{"reference after as", Content{Text: "Use 482913 as your reference"}, false},
		{"enter on form", Content{Text: "Enter 482913 on the form"}, false},
		{"uppercase HTTPS reset link", Content{Text: "Reset your password: HTTPS://example.test/reset?token=abcDEF1234567890"}, true},
		{"hash code", Content{Text: "Your verification code is #482913"}, true},
		{"quoted code", Content{Text: `Your verification code is "482913"`}, true},
		{"italic code", Content{Text: "Your verification code is *482913*"}, true},
		{"bracketed code", Content{Text: "Your code: [482913]"}, true},
		{"bold code", Content{Text: "Your code is: **482913**"}, true},
		{"parenthesized code", Content{Text: "Your code is (482913)"}, true},
		{"curly quoted code", Content{Text: "Your code is “482913”"}, true},
		{"angle bracketed code", Content{Text: "Your code is <482913>"}, true},
		{"HTML angle bracketed code", Content{Text: "View this email in a browser", HTML: "<p>Your verification code is <482913></p>"}, true},
		{"HTML bare less-than", Content{HTML: "<p>Orders <5 items ship free.</p><p>Your verification code is <b>482913</b></p>"}, true},
		{"narrow no-break spaced code", Content{Text: "Your code is 482\u202f913"}, true},
		{"thin spaced code", Content{Text: "Your code is 482\u2009913"}, true},
		{"hash order number", Content{Text: "Your order number is #482913"}, false},
		{"bracketed password reset count", Content{Text: "Password reset requested for [482913] accounts"}, false},
		{"verify account", Content{Text: "Use 482913 to verify your account"}, true},
		{"enter this code", Content{Text: "Enter this code to verify: 482913"}, true},
		{"code below", Content{Text: "Enter this code below: 482913"}, true},
		{"build verification prose", Content{Text: "The code to verify the build is in the repo, 482913 files were scanned"}, false},
		{"code to continue", Content{Text: "Enter this code to continue: 482913"}, true},
		{"code aside", Content{Text: "Verification code (valid 10 min): 482913"}, true},
		{"code for sender", Content{Text: "Your code for Acme is 482913"}, true},
		{"type to verify", Content{Text: "Type 482913 to verify your identity"}, true},
		{"code for ticket", Content{Text: "The code for ticket 482913 is ready for review"}, false},
		{"bar code reads", Content{Text: "Bar code on the box reads 0123456789"}, false},
		{"ship to verify", Content{Text: "Ship 482913 units to verify demand"}, false},
		{"four line breaks", Content{Text: "Your one-time passcode:\n\n\n\n482913"}, true},
		{"newline blank", Content{Text: "Your verification code is:\n\n482913"}, true},
		{"newline", Content{Text: "Your code:\n482913"}, true},
		{"CRLF", Content{Text: "Your code:\r\n482913"}, true},
		{"CRLF digits", Content{Text: "Your code:\r\n4\r\n8\r\n2\r\n9\r\n1\r\n3"}, true},
		{"CRLF number list", Content{Text: "Order quantities:\r\n4\r\n8\r\n2\r\n9\r\n1\r\n3"}, false},
		{"cue sentence", Content{Text: "Here is your verification code.\n482913"}, true},
		{"reverse newline", Content{Text: "Order 123456 shipped.\nEnter your code below"}, false},
		{"promo newline", Content{Text: "Promo code\n\nOrder 1234567 shipped"}, false},
		{"same line sentence", Content{Text: "Here is your verification code. 482913"}, true},
		{"same line sentence before thanks", Content{Text: "Here is your verification code. 482913\nThanks"}, true},
		{"same line cue sentence", Content{Text: "Your verification code. 482913"}, true},
		{"same line sentence closing period", Content{Text: "Here is your verification code. 482913."}, true},
		{"same line code shipped count", Content{Text: "Use the code. 4821 items shipped"}, false},
		{"same line promo sold count", Content{Text: "Promo code. 482913 units sold today"}, false},
		{"same line order shipped count", Content{Text: "Your order code. 4821 items shipped today"}, false},
		{"many lines", Content{Text: "Your code:\n\n\n\n\n482913"}, true},
		{"six blank lines", Content{Text: "Your verification code is:\n\n\n\n\n\n 123456"}, true},
		{"blank lines outside window", Content{Text: "Your verification code:" + strings.Repeat("\n", 61) + "123456"}, false},
		{"long prose gap", Content{Text: "Your verification code is below.\n\nPlease do not share it with anyone, ever, for any reason whatsoever including support staff members.\n\n123456"}, false},
		{"reverse line break", Content{Text: "482913\nis your verification code"}, false},
		{"title context", Content{Title: "Your verification code", Text: "482913"}, true},
		{"filename context", Content{Text: "Your verification code", Filenames: []string{"482913.txt"}}, true},
		{"bare title cue", Content{Title: "Your code", Text: "482913"}, false},
		{"bare filename cue", Content{Text: "Your code", Filenames: []string{"482913.txt"}}, false},
		{"unrelated title", Content{Title: "Code review", Filenames: []string{"123456.txt"}}, false},
		{"unrelated trailing cue", Content{Text: "Order 123456 shipped. Read about your verification code"}, false},
		{"whole token", Content{Text: "Your code is 1234AB56"}, true},
		{"sign-in mixed code", Content{Text: "Your sign-in code is K7MQPZ"}, true},
		{"verification mixed code", Content{Text: "Your verification code: AB12CD"}, true},
		{"five character login code", Content{Text: "Login code: F7KQ2"}, true},
		{"invalid verification status", Content{Text: "Your verification code is INVALID"}, false},
		{"disabled OTP status", Content{Text: "OTP DISABLED"}, false},
		{"qualified code at window limit", Content{Text: "Verification code" + strings.Repeat("\u00a0", 60) + "K7MQPZ"}, true},
		{"qualified code outside window", Content{Text: "Verification code" + strings.Repeat("\u00a0", 61) + "K7MQPZ"}, false},
		{"code after numeric report", Content{Text: "Report code. " + strings.Repeat("Order 1234567 shipped. ", 100) + "Your code is 482913"}, true},
		{"long spaced number", Content{Text: "Your code is " + strings.Repeat("4 ", 600)}, false},
		{"unqualified mixed code", Content{Text: "Your code: AB12CD"}, false},
		{"separate qualified mixed code", Content{Subject: "Your verification code", Text: "AB12CD"}, false},
		{"reverse qualified mixed code", Content{Text: "AB12CD is your verification code"}, false},
		{"short qualified mixed code", Content{Text: "Login code: A1BC"}, false},
		{"long qualified mixed code", Content{Text: "Login code: A1BCDEFGHIJ"}, false},
		{"SAVE20", Content{Text: "Use code SAVE20"}, false},
		{"promo 482913", Content{Text: "promo code is 482913"}, false},
		{"promo 2024", Content{Text: "promo code is 2024"}, false},
		{"promo with verification", Content{Text: "promo code 887766. Your verification code is 482913"}, true},
		{"sign in", Content{Text: "Use 482913 to sign in to Acme"}, true},
		{"temporary password", Content{Text: "Your temporary password is 482913"}, true},
		{"temporary password mixed", Content{Text: "Your temporary password is K7MQ2P"}, true},
		{"temporary password one digit", Content{Text: "Your temporary password is TR0UB4DOR"}, true},
		{"temporary password status", Content{Text: "Your temporary password is INVALID"}, false},
		{"2FA numeric", Content{Text: "2FA: 482913"}, true},
		{"login password numeric", Content{Text: "Your login password is 482913"}, true},
		{"2FA year", Content{Text: "2FA: 2026"}, true},
		{"separate 2FA year", Content{Subject: "Your 2FA", Text: "© 2026"}, false},
		{"order number", Content{Text: "Your order number is 482913"}, false},
		{"password reset count", Content{Text: "Password reset requested for 482913 accounts"}, false},
		{"folded passcode", Content{Text: "Your paſſcode is 482913"}, true},
		{"folded sign in", Content{Text: "Uſe 482913 to ſign in"}, true},
		{"folded localized cue", Content{Text: "Votre code de ſécurité: 482913"}, true},
		{"verification number", Content{Text: "Your verification number is 482913"}, true},
		{"fullwidth", Content{Text: "Your code is ４８２９１３"}, true},
		{"zero width cue", Content{Text: "Your verification c\u200bode is 482913"}, true},
		{"zero width code", Content{Text: "Your code is 4829\u200b13"}, true},
		{"zero width HTML entity", Content{HTML: "<p>Your code is 48&#8203;2913</p>"}, true},
		{"zero width order number", Content{Text: "Your order number is 482\u200b913"}, false},
		{"code review", Content{Subject: "Code review: PR 123456 merged"}, false},
		{"pin items", Content{Text: "Pin your favorite items. Order 123456 shipped"}, false},
		{"pin channel messages", Content{Text: "Can you pin 2024 messages in the channel"}, false},
		{"please pin messages", Content{Text: "Please pin 2024 messages"}, false},
		{"your PIN", Content{Text: "Your PIN 4829"}, true},
		{"your pin is", Content{Text: "Your pin is 4829"}, true},
		{"PIN colon", Content{Text: "PIN: 4829"}, true},
		{"Google", Content{Subject: "G-482913 is your Google verification code"}, true},
		{"hyphenated digits", Content{Text: "Your code is 4-8-2-9"}, true},
		{"hyphenated four digits", Content{Text: "Your verification code: 4-829"}, true},
		{"prefixed order", Content{Text: "Order G-482913 shipped"}, false},
		{"hyphenated room", Content{Text: "Room 4-829 is on the left"}, false},
		{"split", Content{Text: "Your code is 482 913"}, true},
		{"entity repeat", Content{HTML: "<p>Your code is &#52;&#56;&#50;&#57;&#49;&#51;</p>"}, true},
		{"nbsp repeat", Content{HTML: "<p>Your code is 4&nbsp;8&nbsp;2&nbsp;9&nbsp;1&nbsp;3</p>"}, true},
		{"cells", Content{HTML: "Your verification code is <td>4</td><td>8</td><td>2</td><td>9</td><td>1</td><td>3</td>"}, true},
		{"long cell number", Content{HTML: "Your verification code is " + strings.Repeat("<td>4</td>", 11)}, false},
		{"mixed", Content{Text: "Your code is K7M2-9QXA"}, true},
		{"bare subject cue", Content{Subject: "Your code", Text: "482913"}, false},
		{"PIN year", Content{Text: "Your code is 2026"}, true},
		{"PIN year after colon", Content{Text: "Your verification code is:\n\n2026"}, true},
		{"PIN year inline colon", Content{Text: "Your code is: 2026"}, true},
		{"PIN year this", Content{Text: "Enter this code to verify: 2026"}, true},
		{"spaced PIN year", Content{Text: "Your code is 2 0 2 6"}, true},
		{"PIN year cells", Content{HTML: "<p>Your verification code</p><table><tr><td>2</td><td>0</td><td>2</td><td>6</td></tr></table>"}, true},
		{"code changed year", Content{Text: "The code changed in 2026"}, false},
		{"spaced release year", Content{Text: "Release 2 0 2 6 of the code"}, false},
		{"reverse", Content{Text: "482913 is your verification code"}, true},
		{"Unicode window", Content{Text: "code " + strings.Repeat("\u00a0", 59) + "482913"}, true},
		{"outside window", Content{Text: "code " + strings.Repeat("\u00a0", 61) + "482913"}, false},
		{"French copula", Content{Text: "Votre code de vérification est 482913"}, true},
		{"German copula", Content{Text: "Ihr Bestätigungscode ist 482913"}, true},
		{"Spanish copula", Content{Text: "Tu código de verificación es 482913"}, true},
		{"Italian copula", Content{Text: "Il tuo codice di verifica è 482913"}, true},
		{"Chinese copula", Content{Text: "您的验证码是482913"}, true},
		{"Chinese wei", Content{Text: "您的验证码为482913"}, true},
		{"traditional Chinese copula", Content{Text: "您的驗證碼是482913"}, true},
		{"Japanese particle", Content{Text: "認証コードは482913です"}, true},
		{"expired Chinese code", Content{Text: "验证码已过期"}, false},
		{"Japanese code prose", Content{Text: "認証コードについて"}, false},
		{"UUID", Content{Text: "Reset your password https://example.test/r/12345678-1234-4321-1234-123456789abc"}, true},
		{"Firebase", Content{Text: "https://example.test/reset?oobCode=Qm9_xK3-abc1234567890"}, true},
		{"encoded base64 token", Content{Text: "Reset your password: https://example.test/reset?email=a%40example.test&token=CfDJ8Ab%2Fcd3EfGh%2BIjK4lMnOpQr5StUvWx%2Fyz6Ab%3D%3D"}, true},
		{"encoded base64 code key", Content{Text: "https://example.test/Account/ResetPassword?userId=42&code=CfDJ8Ab%2Fcd3EfGh%2BIjK4lMnOpQr5StUvWx%2Fyz6Ab%3D%3D"}, true},
		{"Supabase", Content{Text: "https://example.test/login?token=0123456789abcdef0123456789abcdef0123456789abcdef01234567"}, true},
		{"Supabase invite hash", Content{HTML: "<a href=\"https://example.test/auth/v1/verify?token_hash=pkce_0123456789abcdef0123456789abcdef&type=invite\">Accept invitation</a>"}, true},
		{"access token", Content{Text: "https://example.test/r?access_token=abcDEF1234567890"}, true},
		{"reset token", Content{Text: "https://example.test/r?reset_token=abcDEF1234567890"}, true},
		{"long page", Content{Text: "https://example.test/r?page=abcDEF1234567890"}, false},
		{"Slack", Content{Text: "Sign in https://example.test/login/z-app-1234567890-0987654321-abcdef123456"}, true},
		{"fragment", Content{Text: "https://example.test/r#token=abcDEF1234567890"}, true},
		{"href", Content{HTML: "<a href=\"https://example.test/r?a=b&amp;token=abcDEF1234567890\">Sign in</a>"}, true},
		{"padded href", Content{HTML: "<a href=\" \thttps://example.test/r?token=abcDEF1234567890 \r\n\">Sign in</a>"}, true},
		{"parenthesized link", Content{Text: "Sign in (https://example.test/login?token=abcDEF1234567890)"}, true},
		{"bracketed link", Content{Text: "Reset your password [https://example.test/r/12345678-1234-4321-1234-123456789abc]"}, true},
		{"link before comma", Content{Text: "Your magic link is https://example.test/m#token=abcDEF1234567890, valid for 10 minutes"}, true},
		{"parenthesized HTML text link", Content{HTML: "<p>Sign in (https://example.test/login?token=abcDEF1234567890)</p>"}, true},
		{"angle bracketed link", Content{Text: "Sign in <https://example.test/login?token=abcDEF1234567890>"}, true},
		{"parenthesized article", Content{Text: "Sign in to read more (https://example.test/articles/interesting-article-title)"}, false},
		{"parenthesized unsubscribe", Content{Text: "Sign in (https://example.test/unsubscribe?token=abcDEF1234567890)"}, false},
		{"copyright", Content{Text: "© 2026"}, false},
		{"cue copyright", Content{Subject: "Your verification code", Text: "© 2026"}, false},
		{"order", Content{Text: "Order 1234567 shipped. Log in to track it."}, false},
		{"help line", Content{Text: "Sign in help line 1800555"}, false},
		{"article", Content{Text: "Sign in to read more https://example.test/articles/interesting-article-title"}, false},
		{"booking reference", Content{Subject: "Your trip", Text: "Confirmation code: KXPL2M"}, false},
		{"promo sale", Content{Subject: "Sale", Text: "Use promo code SAVE2024 for 20% off. Order 55512345"}, false},
		{"unsubscribe", Content{Text: "Sign in https://example.test/unsubscribe?token=abcDEF1234567890"}, false},
		{"preferences", Content{Text: "Sign in https://example.test/preferences/abcDEF1234567890"}, false},
		{"tracked sign-in", Content{Subject: "Sign in to Acme", HTML: `<a href="https://u1234567.ct.sendgrid.net/ls/click?upn=u001.Kq3vZ8xY2mN7pL4wR9tB6cD1fG5hJ0kS">Sign in</a>`}, true},
		{"tracked reset", Content{Text: "Reset your password: https://mandrillapp.com/track/click/30123456/app.acme.test?p=eyJzIjoiYWJjMTIzNDU2Nzg5MCJ9"}, true},
		{"tracked newsletter", Content{Subject: "Weekly digest", HTML: `<a href="https://u1234567.ct.sendgrid.net/ls/click?upn=u001.Kq3vZ8xY2mN7pL4wR9tB6cD1fG5hJ0kS">Read more</a>`}, false},
		{"long number", Content{Text: "Your code is 12345678901"}, false},
		{"oversized", Content{Text: strings.Repeat("x", MaxFieldBytes)}, true},
		{"HTML block boundary", Content{Subject: "Your verification code", HTML: "<p>482913</p><p>5550100</p>"}, true},
		{"HTML inline code", Content{HTML: "Your verification code is <b>482</b>913"}, true},
	}
	for _, cue := range []string{"verification code", "security PIN", "login password", "sign-in code", "sign in code", "authentication code", "one-time code", "one-time PIN", "one time password", "OTP", "passcode", "2FA"} {
		cases = append(cases, testCase{cue + " mixed", Content{Text: cue + ": A1BCD"}, true})
	}
	for _, cue := range []string{"verification code", "security code", "login code", "sign-in code", "confirmation code", "authentication code", "one-time code", "passcode", "OTP", "2FA"} {
		cases = append(cases, testCase{cue, Content{Subject: "Your " + cue, Text: "482913"}, true})
	}
	for _, cue := range []string{
		"Bestätigungscode", "Sicherheitscode", "code de vérification", "code de sécurité",
		"código de verificación", "código de seguridad", "codice di verifica",
		"Код подтверждения", "Код безопасности", "验证码", "驗證碼", "認証コード", "인증 코드",
	} {
		gaps := []string{" "}
		if cue == "Bestätigungscode" {
			gaps = append(gaps, "：", ": ")
		}
		for _, gap := range gaps {
			cases = append(cases, testCase{cue + gap, Content{Text: cue + gap + "482913"}, true})
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, Is(tc.content))
		})
	}
}

func BenchmarkIsCueDense(b *testing.B) {
	content := Content{Text: strings.Repeat("code 123456 ", 2000)}
	for b.Loop() {
		Is(content)
	}
}

func BenchmarkIsLargeNumericText(b *testing.B) {
	const size = 400 * 1024
	body := "Report code\n" + strings.Repeat("id,1234567,2024\n", size/len("id,1234567,2024\n")+1)
	content := Content{Text: body[:size]}
	b.SetBytes(int64(len(content.Text)))
	for b.Loop() {
		Is(content)
	}
}

func BenchmarkIsLargeProse(b *testing.B) {
	const size = 400 * 1024
	const sentence = "The report recorded 1234567 items. Another 7654321 items shipped today. "
	body := "The code review is complete. " + strings.Repeat(sentence, size/len(sentence)+1)
	content := Content{Text: body[:size]}
	b.SetBytes(int64(len(content.Text)))
	for b.Loop() {
		Is(content)
	}
}
