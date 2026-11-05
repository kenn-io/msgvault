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
		{"verify account", Content{Text: "Use 482913 to verify your account"}, true},
		{"enter this code", Content{Text: "Enter this code to verify: 482913"}, true},
		{"code below", Content{Text: "Enter this code below: 482913"}, true},
		{"build verification prose", Content{Text: "The code to verify the build is in the repo, 482913 files were scanned"}, false},
		{"four line breaks", Content{Text: "Your one-time passcode:\n\n\n\n482913"}, true},
		{"newline blank", Content{Text: "Your verification code is:\n\n482913"}, true},
		{"newline", Content{Text: "Your code:\n482913"}, true},
		{"CRLF", Content{Text: "Your code:\r\n482913"}, true},
		{"cue sentence", Content{Text: "Here is your verification code.\n482913"}, true},
		{"reverse newline", Content{Text: "Order 123456 shipped.\nEnter your code below"}, false},
		{"promo newline", Content{Text: "Promo code\n\nOrder 1234567 shipped"}, false},
		{"same line sentence", Content{Text: "Here is your verification code. 482913"}, false},
		{"too many lines", Content{Text: "Your code:\n\n\n\n\n482913"}, false},
		{"reverse line break", Content{Text: "482913\nis your verification code"}, false},
		{"title context", Content{Title: "Your verification code", Text: "482913"}, true},
		{"filename context", Content{Text: "Your verification code", Filenames: []string{"482913.txt"}}, true},
		{"bare title cue", Content{Title: "Your code", Text: "482913"}, false},
		{"bare filename cue", Content{Text: "Your code", Filenames: []string{"482913.txt"}}, false},
		{"unrelated title", Content{Title: "Code review", Filenames: []string{"123456.txt"}}, false},
		{"unrelated trailing cue", Content{Text: "Order 123456 shipped. Read about your verification code"}, false},
		{"whole token", Content{Text: "Your code is 1234AB56"}, true},
		{"sign in", Content{Text: "Use 482913 to sign in to Acme"}, true},
		{"temporary password", Content{Text: "Your temporary password is 482913"}, true},
		{"folded passcode", Content{Text: "Your paſſcode is 482913"}, true},
		{"folded sign in", Content{Text: "Uſe 482913 to ſign in"}, true},
		{"folded localized cue", Content{Text: "Votre code de ſécurité: 482913"}, true},
		{"verification number", Content{Text: "Your verification number is 482913"}, true},
		{"fullwidth", Content{Text: "Your code is ４８２９１３"}, true},
		{"code review", Content{Subject: "Code review: PR 123456 merged"}, false},
		{"pin items", Content{Text: "Pin your favorite items. Order 123456 shipped"}, false},
		{"Google", Content{Subject: "G-482913 is your Google verification code"}, true},
		{"split", Content{Text: "Your code is 482 913"}, true},
		{"entity repeat", Content{HTML: "<p>Your code is &#52;&#56;&#50;&#57;&#49;&#51;</p>"}, true},
		{"nbsp repeat", Content{HTML: "<p>Your code is 4&nbsp;8&nbsp;2&nbsp;9&nbsp;1&nbsp;3</p>"}, true},
		{"cells", Content{HTML: "Your verification code is <td>4</td><td>8</td><td>2</td><td>9</td><td>1</td><td>3</td>"}, true},
		{"long cell number", Content{HTML: "Your verification code is " + strings.Repeat("<td>4</td>", 11)}, false},
		{"mixed", Content{Text: "Your code is K7M2-9QXA"}, true},
		{"subject cue", Content{Subject: "Your verification code", Text: "482913"}, true},
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
		{"French", Content{Text: "code de vérification: 482913"}, true},
		{"French copula", Content{Text: "Votre code de vérification est 482913"}, true},
		{"German copula", Content{Text: "Ihr Bestätigungscode ist 482913"}, true},
		{"Spanish copula", Content{Text: "Tu código de verificación es 482913"}, true},
		{"Italian copula", Content{Text: "Il tuo codice di verifica è 482913"}, true},
		{"UUID", Content{Text: "Reset your password https://example.test/r/12345678-1234-4321-1234-123456789abc"}, true},
		{"Firebase", Content{Text: "https://example.test/reset?oobCode=Qm9_xK3-abc1234567890"}, true},
		{"Supabase", Content{Text: "https://example.test/login?token=0123456789abcdef0123456789abcdef0123456789abcdef01234567"}, true},
		{"Supabase invite hash", Content{HTML: "<a href=\"https://example.test/auth/v1/verify?token_hash=pkce_0123456789abcdef0123456789abcdef&type=invite\">Accept invitation</a>"}, true},
		{"access token", Content{Text: "https://example.test/r?access_token=abcDEF1234567890"}, true},
		{"reset token", Content{Text: "https://example.test/r?reset_token=abcDEF1234567890"}, true},
		{"long page", Content{Text: "https://example.test/r?page=abcDEF1234567890"}, false},
		{"Slack", Content{Text: "Sign in https://example.test/login/z-app-1234567890-0987654321-abcdef123456"}, true},
		{"fragment", Content{Text: "https://example.test/r#token=abcDEF1234567890"}, true},
		{"href", Content{HTML: "<a href=\"https://example.test/r?a=b&amp;token=abcDEF1234567890\">Sign in</a>"}, true},
		{"copyright", Content{Text: "© 2026"}, false},
		{"cue copyright", Content{Subject: "Your verification code", Text: "© 2026"}, false},
		{"order", Content{Text: "Order 1234567 shipped. Log in to track it."}, false},
		{"help line", Content{Text: "Sign in help line 1800555"}, false},
		{"article", Content{Text: "Sign in to read more https://example.test/articles/interesting-article-title"}, false},
		{"coupon", Content{Text: "Use code SAVE20"}, false},
		{"promo sale", Content{Subject: "Sale", Text: "Use promo code SAVE2024 for 20% off. Order 55512345"}, false},
		{"promo code", Content{Text: "Use promo code SAVE2024"}, false},
		{"unsubscribe", Content{Text: "Sign in https://example.test/unsubscribe?token=abcDEF1234567890"}, false},
		{"long number", Content{Text: "Your code is 12345678901"}, false},
		{"oversized", Content{Text: strings.Repeat("x", MaxFieldBytes)}, true},
		{"HTML block boundary", Content{Subject: "Your verification code", HTML: "<p>482913</p><p>5550100</p>"}, true},
		{"HTML inline code", Content{HTML: "Your verification code is <b>482</b>913"}, true},
		{"Promo code", Content{Subject: "Promo code", Text: "Order 1234567 shipped"}, false},
		{"Update the code", Content{Subject: "Update the code", Text: "Invoice 123456 attached"}, false},
		{"Re: fix the code", Content{Subject: "Re: fix the code", Text: "555 1234"}, false},
		{"Your PIN", Content{Subject: "Your PIN", Text: "Account ending 123456"}, false},
	}
	for _, prefix := range []string{"promo", "coupon", "discount", "voucher", "referral", "gift", "Zip", "postal", "area", "tracking", "order", "invoice", "product", "project"} {
		for _, value := range []string{"482913", "2024"} {
			cases = append(cases, testCase{prefix + " " + value, Content{Text: prefix + " code is " + value}, false})
		}
		cases = append(cases, testCase{prefix + " with verification", Content{Text: prefix + " code 887766. Your verification code is 482913"}, true})
	}
	for _, cue := range []string{"verification code", "security code", "login code", "sign-in code", "confirmation code", "authentication code", "one-time code", "passcode", "OTP", "2FA"} {
		cases = append(cases, testCase{cue, Content{Subject: "Your " + cue, Text: "482913"}, true})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { assert.Equal(t, tc.want, Is(tc.content)) })
	}
}

func BenchmarkIsCueDense(b *testing.B) {
	content := Content{Text: strings.Repeat("code 123456 ", 2000)}
	for b.Loop() {
		Is(content)
	}
}
