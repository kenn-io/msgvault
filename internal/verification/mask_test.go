package verification

import (
	"encoding/json"
	"fmt"
	"html"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMaskVariants(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	link := "https://example.com/verify?token=abcdEFGH12345678&next=a+b"
	f := &Finding{Kind: "otp_code", Values: []Secret{{Kind: "code", Value: "482913"}, {Kind: "link", Value: link}}}
	before, err := json.Marshal(f)
	require.NoError(err)
	m := NewMasker(f)
	for _, text := range []string{
		"482913 482 913 482-913 4 8 2 9 1 3",
		link, html.EscapeString(link), url.QueryEscape(link), url.PathEscape(link),
		strings.ReplaceAll(url.QueryEscape(link), "%3A", "%3a"),
		strings.ReplaceAll(html.EscapeString(link), "&amp;", "&#38;"),
		"&#52;82&#x39;13",
		"%34%38%32%39%31%33",
		"<b>482</b><span>913</span>",
		"&#52;82 <span>9&#x31;3</span>",
		"界 " + link + " 482913 ü",
	} {
		masked := m.Text(text)
		assert.NotContains(masked, "482913")
		assert.NotContains(masked, "482")
		assert.NotContains(masked, "913")
		assert.NotContains(masked, "482 913")
		assert.NotContains(masked, "482-913")
		assert.NotContains(masked, "abcdEFGH12345678")
		assert.NotContains(masked, url.QueryEscape(link))
		assert.Equal(masked, m.Text(masked))
		assert.NotEqual(text, masked)
	}
	after, err := json.Marshal(f)
	require.NoError(err)
	assert.Equal(before, after)
	assert.Equal("Harmless 123456 and 4829", m.Text("Harmless 123456 and 4829"))
	original := []string{"482913", "safe"}
	assert.Equal([]string{CodePlaceholder, "safe"}, m.Strings(original))
	assert.Equal([]string{"482913", "safe"}, original)
}

func TestMaskKnownCodeWithWhitespace(t *testing.T) {
	finding := Detect(Input{BodyText: "Your verification code is 482913"})
	require.NotNil(t, finding)
	masker := NewMasker(finding)
	for name, separator := range map[string]string{
		"newline": "\n", "CRLF": "\r\n", "tab": "\t", "vertical tab": "\v",
		"form feed": "\f", "next line": "\u0085", "nonbreaking space": "\u00a0",
		"line separator": "\u2028", "paragraph separator": "\u2029", "ideographic space": "\u3000",
	} {
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			text := "Repeated: 482" + separator + "913"
			masked := masker.Text(text)
			assert.Equal("Repeated: "+CodePlaceholder, masked)
			assert.Equal(masked, masker.Text(masked))
		})
	}
}

func TestMaskGroupedAlphanumericAcrossMIMEVariants(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	finding := Detect(Input{
		BodyText: "Your verification code is AB4 C9D",
		BodyHTML: "<p>Your verification code is <b>AB4C9D</b></p>",
	})
	requirements.NotNil(finding)
	masker := NewMasker(finding)

	masked := masker.Text("Copied: AB4 C9D")
	assertions.Equal("Copied: "+CodePlaceholder, masked)
	assertions.Equal(masked, masker.Text(masked))
}

func TestMaskRepeatedDecodedCredentialLinkPayload(t *testing.T) {
	assertions := assert.New(t)
	link := "https://example.com/verify?code=%2534%2538%2532%2539%2531%2533"
	masker := NewMasker(&Finding{Kind: "magic_link", Values: []Secret{{Kind: "link", Value: link}}})

	masked := masker.Text("Uncued repeated value: 482913")
	assertions.Equal("Uncued repeated value: "+LinkPlaceholder, masked)
	assertions.Equal(masked, masker.Text(masked))
}

func TestMaskRepeatedEncodedPathCredentialPayload(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	link := "https://example.com/auth%2Fabcdefghijklmnop"
	values := CredentialLinkSearchValues(link)
	assertions.Contains(values, "abcdefghijklmnop")

	body := link + " Repeated abcdefghijklmnop"
	finding := Detect(Input{BodyText: body})
	requirements.NotNil(finding)
	masked := NewMasker(finding).Text(body)
	assertions.NotContains(masked, "abcdefghijklmnop")
	assertions.Contains(masked, LinkPlaceholder)
}

func TestMaskOversizedValueFailsClosed(t *testing.T) {
	value := "https://example.com/login?token=" + strings.Repeat("a", 65*1024)
	m := NewMasker(&Finding{Values: []Secret{{Kind: "link", Value: value}}})
	assert.Equal(t, ContentPlaceholder, m.Text(value))
	assert.Equal(t, ContentPlaceholder, m.Text("subject"))
	assert.Empty(t, m.Text(""))
}

func TestMaskFragmentedHTML(t *testing.T) {
	assert := assert.New(t)
	markup := "<p>Your verification code is <b>482</b><span>913</span></p>"
	m := NewMasker(&Finding{Values: []Secret{{Kind: "code", Value: "482913"}}})
	assert.Equal(ContentPlaceholder, m.HTML(markup))
	assert.Equal(ContentPlaceholder, m.HTML(m.HTML(markup)))
	assert.Empty(m.HTML(""))
	assert.Equal(markup, NewMasker(nil).HTML(markup))
	assert.Equal(markup, NewMasker(&Finding{}).HTML(markup))
	assert.Equal(ContentPlaceholder, NewMasker(&Finding{Kind: "otp_code"}).HTML(markup))
}

func TestMaskValueBudgetFailsClosed(t *testing.T) {
	t.Run("many codes", func(t *testing.T) {
		assert := assert.New(t)
		f := &Finding{}
		for i := range 64 {
			f.Values = append(f.Values, Secret{Kind: "code", Value: fmt.Sprint(10000000 + i)})
		}
		within := NewMasker(f)
		assert.Equal("subject", within.Text("subject"))
		assert.Equal(CodePlaceholder, within.Text(f.Values[0].Value))
		f.Values = append(f.Values, Secret{Kind: "code", Value: "10000064"})
		m := NewMasker(f)
		assert.Equal(ContentPlaceholder, m.Text("subject"))
		assert.Equal(ContentPlaceholder, m.Text(f.Values[0].Value))
		assert.Empty(m.Text(""))
	})
	t.Run("combined link bytes", func(t *testing.T) {
		prefix := "https://example.com/reset?token="
		m := NewMasker(&Finding{Values: []Secret{
			{Kind: "link", Value: prefix + strings.Repeat("a", 5000)},
			{Kind: "link", Value: prefix + strings.Repeat("b", 5000)},
		}})
		assert.Equal(t, ContentPlaceholder, m.Text("subject"))
	})
}

func FuzzMaskCanonicalValues(f *testing.F) {
	for _, seed := range []string{
		"Your verification code is 482913. 482 913.",
		`<a href="https://example.com/login?token=abcdefghijklmnop&amp;x=y">Sign in</a>`,
		"验证码是482913",
		"\xff\x00",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		finding := Detect(Input{BodyText: text, BodyHTML: text})
		masker := NewMasker(finding)
		masked := masker.Text(text)
		assert.Equal(t, masked, masker.Text(masked))
		if finding == nil {
			return
		}
		for _, secret := range finding.Values {
			assert.NotContains(t, masked, secret.Value)
		}
	})
}

func TestMaskLongestFirst(t *testing.T) {
	f := &Finding{Values: []Secret{
		{Kind: "code", Value: "482913"},
		{Kind: "link", Value: "https://example.com/login?token=482913abcdefghijk"},
		{Kind: "code", Value: "482913"},
	}}
	m := NewMasker(f)
	assert.Equal(t, LinkPlaceholder+" "+CodePlaceholder, m.Text(f.Values[1].Value+" 482913"))
	assert.Equal(t, "safe", NewMasker(nil).Text("safe"))
}

func TestSafeMetadata(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	sent := time.Date(2026, time.October, 31, 10, 0, 0, 0, time.UTC)
	in := Input{BodyText: "Your code is 482913, valid for 10 minutes", SenderEmail: "482913@example.com", SentAt: sent}
	f := Detect(in)
	require.NotNil(f)
	metadata := Describe(in, f, true, []string{"body_text"}, sent.Add(time.Hour))
	require.NotNil(metadata)
	assert.True(metadata.Masked)
	assert.True(metadata.Expired)
	require.NotNil(metadata.ExpiresAt)
	assert.Equal(sent.Add(10*time.Minute), *metadata.ExpiresAt)
	encoded, err := json.Marshal(metadata)
	require.NoError(err)
	assert.NotContains(string(encoded), "482913")
	assert.NotContains(string(encoded), "Values")
	assert.Nil(Describe(in, nil, true, nil, sent))
	assert.Equal("Your code is "+CodePlaceholder+", valid for 10 minutes", ScrubText(in.BodyText))
}

func FuzzMask(f *testing.F) {
	f.Add("Your verification code is 482913", "abc 482913 xyz")
	f.Add("Sign in: https://example.com/login?token=abcdefghijklmnop", "Repeated abcdefghijklmnop")
	f.Fuzz(func(t *testing.T, canonical, text string) {
		finding := Detect(Input{BodyText: canonical, BodyHTML: canonical})
		m := NewMasker(finding)
		masked := m.Text(text)
		assert.Equal(t, masked, m.Text(masked))
		if finding != nil {
			for _, secret := range finding.Values {
				assert.NotContains(t, masked, secret.Value)
			}
		}
	})
}

func TestMaskRepeatedLinkPayloads(t *testing.T) {
	for name, link := range map[string]string{
		"query":           "https://example.com/login?token=abcDEF1234567890",
		"path":            "https://example.com/login/abcDEF1234567890",
		"fragment query":  "https://example.com/#token=abcDEF1234567890",
		"fragment route":  "https://example.com/#/login?token=abcDEF1234567890",
		"short payload":   "https://example.com/login?code=AB4C9D",
		"encoded payload": "https://example.com/login?token=" + url.QueryEscape("abcDEF+1234567890"),
	} {
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			finding := Detect(Input{BodyText: "Sign in using " + link})
			require.NotNil(finding)
			masker := NewMasker(finding)
			payload := "abcDEF1234567890"
			if name == "short payload" {
				payload = "AB4C9D"
			}
			if name == "encoded payload" {
				payload = "abcDEF+1234567890"
			}
			text := link + " Repeated " + payload + " " + url.QueryEscape(payload)
			masked := masker.Text(text)
			assert.NotContains(masked, payload)
			assert.NotContains(masked, url.QueryEscape(payload))
			assert.Contains(masked, LinkPlaceholder)
			assert.Equal(masked, masker.Text(masked))
		})
	}
}

func TestMaskShortLinkPayloadFailsClosed(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	finding := Detect(Input{BodyText: "Sign in: https://example.com/login?token=a"})
	require.NotNil(finding)
	masker := NewMasker(finding)
	assert.Equal(ContentPlaceholder, masker.Text("Repeated a"))
	assert.Equal(ContentPlaceholder, masker.Text(ContentPlaceholder))
}

func TestMaskGroupedNumericLinkPayload(t *testing.T) {
	finding := Detect(Input{BodyText: "Sign in: https://example.com/login?code=482913"})
	require.NotNil(t, finding)
	masker := NewMasker(finding)
	for _, repeated := range []string{"482 913", "482-913", "4 8 2 9 1 3", "482\n913", "482\u00a0913"} {
		t.Run(repeated, func(t *testing.T) {
			assertions := assert.New(t)
			masked := masker.Text("Uncued field: " + repeated)
			assertions.Equal("Uncued field: "+LinkPlaceholder, masked)
			assertions.Equal(masked, masker.Text(masked))
		})
	}
}

func TestMaskCombinedEncodingAndGrouping(t *testing.T) {
	for _, canonical := range []string{
		"Your verification code is 482913",
		"Your verification code is G-482913",
		"Sign in: https://example.com/login?code=482913",
	} {
		t.Run(canonical, func(t *testing.T) {
			require := require.New(t)
			finding := Detect(Input{BodyText: canonical})
			require.NotNil(finding)
			masker := NewMasker(finding)
			placeholder := CodePlaceholder
			if finding.Values[0].Kind == "link" {
				placeholder = LinkPlaceholder
			}
			for _, repeated := range []string{
				"482913", "482 913", "482-913", "482+913", "482&#32;913", "482&#x20;913",
				"482&nbsp;913", "482%20%20%20913", "482&#160;913", "482%C2%A0913",
				"482&Tab;913", "482%09913", "482&NewLine;913", "482%0A913", "482&#x000A;913",
				"&#52;%38&#50;&#32;%39&#49;3",
			} {
				t.Run(repeated, func(t *testing.T) {
					assert := assert.New(t)
					text := "Uncued field: " + repeated
					masked := masker.Text(text)
					assert.Equal("Uncued field: "+placeholder, masked)
					assert.Equal(masked, masker.Text(masked))
				})
			}
		})
	}
}
