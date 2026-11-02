package verification

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDetectCorpus(t *testing.T) {
	tests := []struct {
		name, body, kind, value string
		expiry                  time.Duration
	}{
		{"verification", "Your verification code is 482913", "otp_code", "482913", 0},
		{"security", "Security code: 482913. Expires in 10 minutes.", "otp_code", "482913", 10 * time.Minute},
		{"one-time", "One-time code 482913", "otp_code", "482913", 0},
		{"one-time cue wraps before code", "One-time\ncode: 482913", "otp_code", "482913", 0},
		{"one time", "One time password 482913", "otp_code", "482913", 0},
		{"generic sign-in code", "Sign in: 482913", "otp_code", "482913", 0},
		{"otp", "OTP: 482913", "otp_code", "482913", 0},
		{"passcode", "Passcode: 4829", "otp_code", "4829", 0},
		{"your code", "Your code is 482913", "otp_code", "482913", 0},
		{"reverse", "482913 is your sign-in code", "otp_code", "482913", 0},
		{"prefixed", "G-482913 is your verification code", "otp_code", "G-482913", 0},
		{"space grouped", "Verification code: 482 913", "otp_code", "482 913", 0},
		{"dash grouped", "Verification code: 482-913", "otp_code", "482-913", 0},
		{"alphanumeric", "Verification code: AB4C9D", "otp_code", "AB4C9D", 0},
		{"eight digits", "Security code: 48291376", "otp_code", "48291376", 0},
		{"expiry hours", "Your code: 482913, valid for 2 hours", "otp_code", "482913", 2 * time.Hour},
		{"expiry seconds", "Your code: 482913, expires in 30 seconds", "otp_code", "482913", 30 * time.Second},
		{"french", "Votre code de vérification est 482913", "otp_code", "482913", 0},
		{"german", "Ihr Bestätigungscode: 482913", "otp_code", "482913", 0},
		{"spanish", "Su código de verificación: 482913", "otp_code", "482913", 0},
		{"italian", "Il codice di verifica è 482913", "otp_code", "482913", 0},
		{"russian", "Ваш код подтверждения: 482913", "otp_code", "482913", 0},
		{"chinese", "您的验证码是482913", "otp_code", "482913", 0},
		{"traditional", "您的驗證碼是482913", "otp_code", "482913", 0},
		{"japanese", "認証コード: 482913", "otp_code", "482913", 0},
		{"korean", "인증 코드: 482913", "otp_code", "482913", 0},
		{"magic", "Your magic link: https://login.example.com/magic?token=abcdefghijklmnop", "magic_link", "https://login.example.com/magic?token=abcdefghijklmnop", 0},
		{"password reset", "Reset your password: https://example.com/reset?token=abcdefghijklmnop", "password_reset", "https://example.com/reset?token=abcdefghijklmnop", 0},
		{"double opt in", "Confirm your subscription: https://example.com/confirm?token=abcdefghijklmnop", "double_opt_in", "https://example.com/confirm?token=abcdefghijklmnop", 0},
		{"account", "Confirm your email: https://example.com/verify?token=abcdefghijklmnop", "account_confirmation", "https://example.com/verify?token=abcdefghijklmnop", 0},
		{"opaque link", "https://example.com/auth/abcdefghijklmnop", "magic_link", "https://example.com/auth/abcdefghijklmnop", 0},
		{"link userinfo", "Reset your password: https://user:password@example.com/reset?token=abcdefghijklmnop", "password_reset", "https://user:password@example.com/reset?token=abcdefghijklmnop", 0},
		{"cue adjacent link", "Log in: https://example.com/login?id=abc", "magic_link", "https://example.com/login?id=abc", 0},
		{"long DM", strings.Repeat("ordinary text ", 100) + "Verification code: 482913", "otp_code", "482913", 0},
		{"cue only", "Your verification code will arrive separately", "otp_code", "", 0},
		{"generic sign-in navigation", "Your order is confirmed. Sign in to view your order: https://shop.example.test/login?return=%2Forders", "", "", 0},
		{"generic sign-in footer near order UUID", "Your order is confirmed: https://shop.example.test/orders/550e8400-e29b-41d4-a716-446655440000. Sign in to view your account.", "", "", 0},
		{"generic sign-in date", "Order date: 2026-11-03. Sign in to view your order.", "", "", 0},
		{"one-time payment", "This is a one-time payment for your order.", "", "", 0},
		{"one time purchase", "Your one time purchase is confirmed.", "", "", 0},
		{"invoice", "Invoice 482913 total $154.00", "", "", 0},
		{"order", "Your order number is 482913", "", "", 0},
		{"tracking", "Tracking 1234567890123456", "", "", 0},
		{"year", "The year is 2026", "", "", 0},
		{"phone", "Call 555-010-1234", "", "", 0},
		{"source code", "Review the source code in issue 482913", "", "", 0},
		{"word boundary", "We adopted 482913 options", "", "", 0},
		{"ordinary reverse", "This is your invoice 482913", "", "", 0},
		{"ordinary URL", "Read https://example.com/news/abcdefghijklmnop", "", "", 0},
		{"article slug", "https://example.com/news/author-interview-2026", "", "", 0},
		{"article endpoint words", "https://example.com/news/password-reset-guide-2026?ref=abcdefghijklmnop", "", "", 0},
		{"query value is not endpoint", "https://example.com/news?topic=auth&slug=abcdefghijklmnop", "", "", 0},
		{"query key boundary", "https://example.com/news?author=abcdefghijklmnop", "", "", 0},
		{"ordinary login page", "https://example.com/login", "", "", 0},
		{"long number", "Your verification code is 123456789012", "otp_code", "", 0},
		{"distant code", "Verification code" + strings.Repeat("界", 81) + " 482913", "otp_code", "", 0},
		{"unicode distance", "Verification code" + strings.Repeat("界", 60) + " 482913", "otp_code", "482913", 0},
		{"unicode casing", "İ Verification code: 482913", "otp_code", "482913", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := Detect(Input{MessageType: "email", BodyText: tt.body})
			if tt.kind == "" {
				assert.Nil(f)
				return
			}
			require.NotNil(f)
			assert.Equal(tt.kind, f.Kind)
			assert.Equal(tt.expiry, f.ExpiresIn)
			if tt.value == "" {
				assert.Empty(f.Values)
				assert.Equal("medium", f.Confidence)
			} else {
				require.NotEmpty(f.Values)
				assert.Contains(f.Values, Secret{Kind: f.Values[0].Kind, Value: tt.value, Host: f.Values[0].Host})
				assert.Equal("high", f.Confidence)
			}
		})
	}
}

func TestAllChannelsAndCanonicalFields(t *testing.T) {
	for _, channel := range []string{"email", "sms", "imessage", "rcs", "whatsapp", "teams", "beeper", "chat", ""} {
		t.Run(channel, func(t *testing.T) {
			for _, long := range []bool{false, true} {
				body := "Your verification code is 482913"
				if long {
					body = strings.Repeat("A long message. ", 100) + body
				}
				f := Detect(Input{MessageType: channel, BodyText: body, IsFromMe: true})
				require.NotNil(t, f)
				assert.Contains(t, f.Values, Secret{Kind: "code", Value: "482913"})
			}
		})
	}
	for _, in := range []Input{
		{Subject: "Verification code 482913"},
		{Subject: "Your verification code", BodyText: "482913"},
		{Snippet: "Verification code 482913"},
		{BodyHTML: `<p>Verification <b>code</b>: 482913</p>`},
		{BodyHTML: `<a href="https://example.com/verify?token=abcdefghijklmnop&amp;a=b">Confirm your email</a>`},
	} {
		f := Detect(in)
		require.NotNil(t, f)
		assert.NotEmpty(t, f.Values)
	}
}

func TestHintsNeverCreateFinding(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	assert.Nil(Detect(Input{SenderEmail: "noreply@google.com", SenderPhone: "12345", ListID: "list.example.com", BodyText: "Invoice 482913"}))
	f := Detect(Input{SenderEmail: "Security <security@google.com>", ListID: "list.example.com", BodyText: "Confirm your subscription"})
	require.NotNil(f)
	assert.Contains(f.Signals, "sender_local:security")
	assert.Contains(f.Signals, "domain:known")
	assert.Contains(f.Signals, "list_id")
	assert.Empty(f.Values)
}

func TestDuplicateValuesAndHTML(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := Detect(Input{Subject: "Verification code: 482913", Snippet: "Your code is 482913", BodyText: "Your code is 482913", BodyHTML: "Your code is 482913"})
	require.NotNil(f)
	assert.Equal([]Secret{{Kind: "code", Value: "482913"}}, f.Values)
	f = Detect(Input{BodyHTML: `<a href="https://example.com/verify?token=abcdefghijklmnop&amp;a=b">Verify your email</a>`})
	require.NotNil(f)
	assert.Equal([]Secret{{Kind: "link", Value: "https://example.com/verify?token=abcdefghijklmnop&a=b", Host: "example.com"}}, f.Values)
}

func TestCredentialLinksDoNotCreateStandaloneCodes(t *testing.T) {
	for _, tc := range []struct{ name, prefix, url string }{
		{"login parameter", "Sign in: ", "https://example.com/login?id=482913"},
		{"magic parameter", "Magic link: ", "https://example.com/magic?token=482913"},
		{"opaque numeric path", "", "https://example.com/auth/0123456789012345"},
		{"compound endpoint and key", "", "https://example.com/password-reset?auth_token=abcdefghijklmnop"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := Detect(Input{BodyText: tc.prefix + tc.url})
			require.NotNil(f)
			assert.Equal("magic_link", f.Kind)
			require.Len(f.Values, 1)
			assert.Equal(Secret{Kind: "link", Value: tc.url, Host: "example.com"}, f.Values[0])
		})
	}
	f := Detect(Input{BodyText: "Your verification code is 482913. Sign in: https://example.com/login?id=729415"})
	require.NotNil(t, f)
	assert.Equal(t, "otp_code", f.Kind)
	assert.Equal(t, []Secret{
		{Kind: "code", Value: "482913"},
		{Kind: "link", Value: "https://example.com/login?id=729415", Host: "example.com"},
	}, f.Values)
}

func TestHTMLCodesPreserveInlineAdjacency(t *testing.T) {
	for _, tc := range []struct{ name, html, value string }{
		{"numeric spans", `<p>Your verification code is <span>4</span><span>8</span><span>2</span><span>9</span><span>1</span><span>3</span>.</p>`, "482913"},
		{"alphanumeric spans", `<p>Your verification code is <b>AB4</b><span>C9D</span>.</p>`, "AB4C9D"},
		{"entity spans", `<p>Your verification code is <span>&#52;</span><span>&#56;</span><span>2913</span>.</p>`, "482913"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := Detect(Input{BodyHTML: tc.html})
			require.NotNil(t, f)
			assert.Equal(t, "otp_code", f.Kind)
			assert.Contains(t, f.Values, Secret{Kind: "code", Value: tc.value})
		})
	}
}

func TestMultipleCodesAfterUnicodeNormalization(t *testing.T) {
	f := Detect(Input{BodyText: "İ Your verification code: 482913. İ Security code: 729415."})
	require.NotNil(t, f)
	assert.Equal(t, []Secret{{Kind: "code", Value: "482913"}, {Kind: "code", Value: "729415"}}, f.Values)
}

func BenchmarkDetectLongDM(b *testing.B) {
	in := Input{MessageType: "beeper", BodyText: strings.Repeat("Ordinary note 123456. ", 10_000) + "Your verification code is 482913"}
	b.SetBytes(int64(len(in.BodyText)))
	b.ReportAllocs()
	for b.Loop() {
		Detect(in)
	}
}

func FuzzDetect(f *testing.F) {
	for _, seed := range []string{"Your code is 482913", "验证码是482913", `<a href="https://example.com/reset?token=abcdefghijklmnop">Reset your password</a>`, "\xff\x00"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		assert := assert.New(t)
		in := Input{BodyText: text, BodyHTML: text}
		first, second := Detect(in), Detect(in)
		assert.Equal(first, second)
		if first == nil {
			return
		}
		seen := make(map[string]bool)
		for _, s := range first.Values {
			assert.NotEmpty(s.Value)
			assert.False(seen[s.Value])
			seen[s.Value] = true
			assert.Contains([]string{"code", "link"}, s.Kind)
		}
	})
}

func TestHTMLPresentationDoesNotCreateCodes(t *testing.T) {
	for _, tc := range []struct{ name, html string }{
		{"css color", `<p style="color:#123456">Your verification code will arrive shortly.</p>`},
		{"attribute value", `<p data-reference="482913">Your verification code will arrive shortly.</p>`},
		{"script and style", `<style>.code { color: #123456 }</style><script>const code = 482913;</script><p>Your verification code will arrive shortly.</p>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := Detect(Input{BodyHTML: tc.html})
			require.NotNil(t, f)
			assert.Empty(t, f.Values)
			assert.Equal(t, "medium", f.Confidence)
		})
	}
	f := Detect(Input{BodyHTML: `<p>Your verification code is <span>48</span><span>2913</span>.</p>`})
	require.NotNil(t, f)
	assert.Equal(t, []Secret{{Kind: "code", Value: "482913"}}, f.Values)
}

func TestHTMLLinkCuesAcrossInlineTags(t *testing.T) {
	for _, body := range []string{
		`<a href="https://example.com/login?id=482913">Sign <b>in</b></a>`,
		`<p>Sign <b>in</b>: <a href="https://example.com/login?id=482913">continue</a></p>`,
		`<a href="https://example.com/login?id=482913">Sign   <b>in</b></a>`,
		`<a href="https://example.com/login?id=482913"/>Sign <b>in</b></a>`,
	} {
		t.Run(body, func(t *testing.T) {
			f := Detect(Input{BodyHTML: body})
			require.NotNil(t, f)
			assert.Equal(t, "magic_link", f.Kind)
			assert.Equal(t, []Secret{{Kind: "link", Value: "https://example.com/login?id=482913", Host: "example.com"}}, f.Values)
		})
	}
}

func TestDeepHTMLRetainsCodesAndLinks(t *testing.T) {
	f := Detect(Input{BodyHTML: strings.Repeat("<div>", 600) + `Your verification code is 482913. <a href="https://example.com/login?id=729415">Sign <b>in</b></a>` + strings.Repeat("</div>", 600)})
	require.NotNil(t, f)
	assert.Equal(t, []Secret{
		{Kind: "code", Value: "482913"},
		{Kind: "link", Value: "https://example.com/login?id=729415", Host: "example.com"},
	}, f.Values)
}

func TestDetectUnclosedCredentialAnchorsWithLongBody(t *testing.T) {
	const anchorCount = 2048
	link := "https://example.test/login?token=abcdefghijklmnop"
	body := strings.Repeat(`<a href="`+link+`">`, anchorCount) + strings.Repeat("A", 32*1024) + " Sign in"
	f := Detect(Input{BodyHTML: body})
	require.NotNil(t, f)
	assert.Equal(t, []Secret{{Kind: "link", Value: link, Host: "example.test"}}, f.Values)
}

func TestCuesAllowWrappedWhitespace(t *testing.T) {
	for _, body := range []string{
		"Your verification\ncode is 482913",
		"Your verification   code is 482913",
		"Your verification\t\r\n code is 482913",
		"Your verification\u00a0code is 482913",
		"482913 is\n your code",
	} {
		t.Run(body, func(t *testing.T) {
			f := Detect(Input{BodyText: body})
			require.NotNil(t, f)
			assert.Equal(t, "otp_code", f.Kind)
			assert.Equal(t, []Secret{{Kind: "code", Value: "482913"}}, f.Values)
		})
	}
	f := Detect(Input{Subject: "Verification\r\n code", BodyText: "482913"})
	require.NotNil(t, f)
	assert.Equal(t, []Secret{{Kind: "code", Value: "482913"}}, f.Values)
}

func TestHTMLImplicitHeadClosing(t *testing.T) {
	for _, body := range []string{
		`<html><head><title>Example</title><body>Your verification code is 482913.</body></html>`,
		`<head><style>body { color: #123456 }</style><p>Your verification code is 482913.</p>`,
		`<head><title>Example</title>Your verification code is 482913.`,
	} {
		t.Run(body, func(t *testing.T) {
			f := Detect(Input{BodyHTML: body})
			require.NotNil(t, f)
			assert.Equal(t, []Secret{{Kind: "code", Value: "482913"}}, f.Values)
		})
	}
	assert.Nil(t, Detect(Input{BodyHTML: `<head><title>Your verification code is 482913</title></head><body>Ordinary message</body>`}))
}

func TestWrappedMagicLinkCue(t *testing.T) {
	f := Detect(Input{BodyText: "Sign\r\n in: https://example.com/login?id=482913"})
	require.NotNil(t, f)
	assert.Equal(t, "magic_link", f.Kind)
	assert.Equal(t, []Secret{{Kind: "link", Value: "https://example.com/login?id=482913", Host: "example.com"}}, f.Values)
}

func TestWhitespaceExpandedCuesRetainNearbyValues(t *testing.T) {
	for _, tc := range []struct{ name, gap string }{{"ascii", strings.Repeat(" ", 400)}, {"unicode", strings.Repeat("\u00a0", 400)}} {
		t.Run(tc.name, func(t *testing.T) {
			gap := tc.gap
			assert := assert.New(t)
			require := require.New(t)
			f := Detect(Input{BodyText: "Verification" + gap + "code: 482913"})
			require.NotNil(f)
			assert.Equal([]Secret{{Kind: "code", Value: "482913"}}, f.Values)
			f = Detect(Input{BodyText: "Sign" + gap + "in: https://example.com/login?id=482913"})
			require.NotNil(f)
			assert.Equal([]Secret{{Kind: "link", Value: "https://example.com/login?id=482913", Host: "example.com"}}, f.Values)
		})
	}
}

func TestURLsDoNotSupplyTheirOwnCue(t *testing.T) {
	for _, body := range []string{
		"https://example.com/sign-in",
		"https://example.com/sign-in?redirect=docs",
		"https://example.com/reset-your-password",
	} {
		t.Run(body, func(t *testing.T) { assert.Nil(t, Detect(Input{BodyText: body})) })
	}
	f := Detect(Input{BodyText: "https://example.com/sign-in?token=abcdefghijklmnop"})
	require.NotNil(t, f)
	assert.Equal(t, []Secret{{Kind: "link", Value: "https://example.com/sign-in?token=abcdefghijklmnop", Host: "example.com"}}, f.Values)
}

func TestUnicodeWhitespaceEndsURLs(t *testing.T) {
	for _, separator := range []string{"\u00a0", "\u0085", "\u2003", "\u2028", "\u3000", "\v"} {
		t.Run(separator, func(t *testing.T) {
			f := Detect(Input{BodyText: "Your verification code: https://example.com/login" + separator + "482913"})
			require.NotNil(t, f)
			assert.Contains(t, f.Values, Secret{Kind: "code", Value: "482913"})
		})
	}
}

func TestHTMLAdjacentBlocksPreserveBoundaries(t *testing.T) {
	for _, tc := range []struct{ name, html string }{
		{"centered mail", `<center>Your verification code</center><center>482913</center>`},
		{"definition list", `<dl><dt>Your verification code</dt><dd>482913</dd></dl>`},
		{"page sections", `<main>Your verification code</main><aside>482913</aside>`},
		{"figure caption", `<figure>Your verification code<figcaption>482913</figcaption></figure>`},
		{"fieldset legend", `<fieldset><legend>Your verification code</legend>482913</fieldset>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := Detect(Input{BodyHTML: tc.html})
			require.NotNil(t, f)
			assert.Equal(t, []Secret{{Kind: "code", Value: "482913"}}, f.Values)
		})
	}
}

func TestAnyCodePromotesGenericSignIn(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   Input
	}{
		{"snippet link then body code", Input{Snippet: "Sign in: https://example.com/login?token=abcdefghijklmnop", BodyText: "Sign in: 482913"}},
		{"equivalent body text", Input{BodyText: "Sign in: https://example.com/login?token=abcdefghijklmnop. Sign in: 482913"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := Detect(tc.in)
			require.NotNil(t, f)
			assert.Equal(t, "otp_code", f.Kind)
			assert.Contains(t, f.Values, Secret{Kind: "code", Value: "482913"})
		})
	}
}

func TestGenericSignInDoesNotPromoteOrderOrTrackingNumbers(t *testing.T) {
	for _, body := range []string{
		"Order number 482913. Sign in to view your order.",
		"Tracking number: 482913. Sign in to view delivery details.",
		"482913 is your order number. Sign in to view your order.",
		"482913 is your tracking number. Sign in to view delivery details.",
		"Sign in to view order number 482913.",
	} {
		t.Run(body, func(t *testing.T) {
			assert.Nil(t, Detect(Input{BodyText: body}))
		})
	}
}

func TestGenericSignInRepeatedNavigationKeepsOnlyNearbyCode(t *testing.T) {
	body := strings.Repeat("Sign in to view order 482913. ", 512) + "Sign in: 928374"
	finding := Detect(Input{BodyText: body})
	require.NotNil(t, finding)
	assert.Equal(t, "otp_code", finding.Kind)
	assert.Equal(t, []Secret{{Kind: "code", Value: "928374"}}, finding.Values)
}

func TestExplicitCredentialCuesExtractOpaqueLinks(t *testing.T) {
	for _, tc := range []struct {
		name, kind, value string
		in                Input
	}{
		{"reset path", "password_reset", "https://example.com/r/abcdefghijklmnop", Input{BodyText: "Reset your password: https://example.com/r/abcdefghijklmnop"}},
		{"magic query", "magic_link", "https://example.com/go?ticket=abcdefghijklmnop", Input{BodyText: "Your magic link: https://example.com/go?ticket=abcdefghijklmnop"}},
		{"sign in snippet", "magic_link", "https://example.com/r/abcdefghijklmnop", Input{Snippet: "Sign in: https://example.com/r/abcdefghijklmnop"}},
		{"sign in anchor", "magic_link", "https://example.com/r/abcdefghijklmnop", Input{BodyHTML: `<a href="https://example.com/r/abcdefghijklmnop">Sign in</a>`}},
		{"HTML confirmation", "account_confirmation", "https://example.com/r/abcdefghijklmnop", Input{BodyHTML: `<a href="https://example.com/r/abcdefghijklmnop">Confirm <b>your email</b></a>`}},
		{"subscription subject", "double_opt_in", "https://example.com/r/abcdefghijklmnop", Input{Subject: "Confirm your subscription", BodyText: "https://example.com/r/abcdefghijklmnop"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := Detect(tc.in)
			require.NotNil(t, f)
			assert.Equal(t, tc.kind, f.Kind)
			assert.Equal(t, []Secret{{Kind: "link", Value: tc.value, Host: "example.com"}}, f.Values)
		})
	}
	for _, tc := range []struct{ name, body string }{
		{"distant cue", "Reset your password" + strings.Repeat("界", 81) + " https://example.com/r/abcdefghijklmnop"},
		{"no opaque token", "Reset your password: https://example.com/r?id=abc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := Detect(Input{BodyText: tc.body})
			require.NotNil(t, f)
			assert.Empty(t, f.Values)
		})
	}
	f := Detect(Input{BodyText: "Your verification code is 482913. Read https://example.com/news/abcdefghijklmnop"})
	require.NotNil(t, f)
	assert.Equal(t, []Secret{{Kind: "code", Value: "482913"}}, f.Values)
}

func TestFragmentCredentialLinks(t *testing.T) {
	for _, tc := range []struct{ name, prefix, url string }{
		{"fragment parameters", "Magic link: ", "https://example.com/#token=abcdefghijklmnop"},
		{"fragment route", "", "https://example.com/#/login?token=abcdefghijklmnop"},
		{"fragment path token", "", "https://example.com/#/auth/abcdefghijklmnop"},
		{"bare fragment token", "Sign in: ", "https://example.com/#abcdefghijklmnop"},
		{"fragment numeric parameter", "Sign in: ", "https://example.com/#/login?id=482913"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, in := range []Input{
				{BodyText: tc.prefix + tc.url},
				{BodyHTML: `<a href="` + tc.url + `">` + tc.prefix + `continue</a>`},
			} {
				f := Detect(in)
				require.NotNil(t, f)
				assert.Equal(t, "magic_link", f.Kind)
				assert.Equal(t, []Secret{{Kind: "link", Value: tc.url, Host: "example.com"}}, f.Values)
			}
		})
	}
	for _, body := range []string{
		"https://example.com/#abcdefghijklmnop",
		"https://example.com/#/news/author-interview-2026",
		"https://example.com/#/login",
	} {
		assert.Nil(t, Detect(Input{BodyText: body}))
	}
}

func TestReverseCodeCueRequiresStandaloneNonURLCode(t *testing.T) {
	for _, body := range []string{
		"482913 is your barcode",
		"This is your invoice 482913 with its barcode below.",
		"482913 is your invoice: https://example.com/code",
		"482913 is your barcode: https://example.com/?code=invoice",
	} {
		t.Run(body, func(t *testing.T) {
			assert.Nil(t, Detect(Input{BodyText: body}))
		})
	}
	f := Detect(Input{BodyText: "482913 is your access code"})
	require.NotNil(t, f)
	assert.Equal(t, []Secret{{Kind: "code", Value: "482913"}}, f.Values)
}

func TestCredentialLinkRequiresPayload(t *testing.T) {
	for _, value := range []string{
		"https://example.com/login",
		"https://example.com/login?token=",
		"https://example.com/login?redirect=docs",
		"https://example.com/login/help",
		"https://example.com/login/help2026",
		"https://example.com/#/login?redirect=docs",
	} {
		t.Run(value, func(t *testing.T) {
			assert := assert.New(t)
			for _, in := range []Input{
				{BodyText: "Sign in: " + value},
				{BodyHTML: `<a href="` + value + `">Sign in</a>`},
			} {
				f := Detect(in)
				assert.Nil(f)
			}
		})
	}
	for _, value := range []string{
		"https://example.com/login?token=abc",
		"https://example.com/login?id=482913",
		"https://example.com/login/482913",
		"https://example.com/#/login?code=abc",
	} {
		t.Run(value, func(t *testing.T) {
			f := Detect(Input{BodyText: "Sign in: " + value})
			require.NotNil(t, f)
			assert.Equal(t, "high", f.Confidence)
			assert.Equal(t, []Secret{{Kind: "link", Value: value, Host: "example.com"}}, f.Values)
		})
	}
}
