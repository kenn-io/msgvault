package gvoice

import (
	"strings"
	"testing"
	"time"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
)

func TestParseVCF(t *testing.T) {
	vcf := `BEGIN:VCARD
VERSION:3.0
FN:
N:;;;;
item1.TEL:+17026083638
item1.X-ABLabel:Google Voice
TEL;TYPE=CELL:+15753222266
END:VCARD
`
	phones, err := parseVCF([]byte(vcf))
	requirepkg.NoError(t, err, "parseVCF")
	assertpkg.Equal(t, "+17026083638", phones.GoogleVoice)
	assertpkg.Equal(t, "+15753222266", phones.Cell)
}

func TestParseVCF_MissingGV(t *testing.T) {
	vcf := `BEGIN:VCARD
VERSION:3.0
TEL;TYPE=CELL:+15551234567
END:VCARD
`
	_, err := parseVCF([]byte(vcf))
	requirepkg.Error(t, err, "expected error for missing GV number")
}

func TestClassifyFile(t *testing.T) {
	tests := []struct {
		filename string
		wantName string
		wantType fileType
		wantErr  bool
	}{
		{
			filename: "Keith Stern - Text - 2020-02-03T17_37_45Z.html",
			wantName: "Keith Stern",
			wantType: fileTypeText,
		},
		{
			filename: "Keith Stern - Received - 2020-02-05T23_26_28Z.html",
			wantName: "Keith Stern",
			wantType: fileTypeReceived,
		},
		{
			filename: "Kicy Motley - Placed - 2020-02-03T20_05_20Z.html",
			wantName: "Kicy Motley",
			wantType: fileTypePlaced,
		},
		{
			filename: "John Doe - Missed - 2020-03-15T10_30_00Z.html",
			wantName: "John Doe",
			wantType: fileTypeMissed,
		},
		{
			filename: "Jane - Voicemail - 2020-04-01T12_00_00Z.html",
			wantName: "Jane",
			wantType: fileTypeVoicemail,
		},
		{
			filename: "Group Conversation - 2020-02-05T17_16_14Z.html",
			wantName: "",
			wantType: fileTypeGroup,
		},
		{
			// Filename without type keyword (some call files lack explicit type)
			filename: "Kicy Motley - 2020-02-03T20_05_20Z.html",
			wantName: "Kicy Motley",
			wantType: fileTypePlaced, // defaults to placed, caller overrides from HTML
		},
		{
			// Timestamp without trailing Z
			filename: "Someone - Text - 2020-01-15T08_30_00.html",
			wantName: "Someone",
			wantType: fileTypeText,
		},
		{
			// Phone number as contact name
			filename: "+12025551234 - Text - 2020-06-01T09_00_00Z.html",
			wantName: "+12025551234",
			wantType: fileTypeText,
		},
		{
			filename: "photo.jpg",
			wantErr:  true,
		},
		{
			filename: "Bills.html",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.filename, func(t *testing.T) {
			require := requirepkg.New(t)
			assert := assertpkg.New(t)
			name, ft, err := classifyFile(tt.filename)
			if tt.wantErr {
				require.Error(err)
				return
			}
			require.NoError(err)
			assert.Equal(tt.wantName, name, "name")
			assert.Equal(tt.wantType, ft, "type")
		})
	}
}

const sampleTextHTML = `<?xml version="1.0" ?>
<!DOCTYPE html PUBLIC "-//W3C//DTD XHTML 1.0 Strict//EN" "http://www.w3.org/TR/xhtml1/DTD/xhtml1-strict.dtd">
<html xmlns="http://www.w3.org/1999/xhtml"><head>
<meta http-equiv="Content-Type" content="text/html; charset=UTF-8" />
<title>Keith Stern</title></head>
<body><div class="hChatLog hfeed">
<div class="message"><abbr class="dt" title="2020-02-03T11:37:45.632-06:00">Feb 3, 2020</abbr>:
<cite class="sender vcard"><a class="tel" href="tel:+12023065386"><span class="fn">Keith Stern</span></a></cite>:
<q>Cara says you&#39;re coming in tonight? Awesome.</q>
</div> <div class="message"><abbr class="dt" title="2020-02-03T11:59:08.554-06:00">Feb 3, 2020</abbr>:
<cite class="sender vcard"><a class="tel" href="tel:+15753222266"><abbr class="fn" title="">Me</abbr></a></cite>:
<q>I&#39;m looking at a bus getting in 815ish.</q>
</div></div></body></html>`

func TestParseTextHTML(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	messages, groupPar, err := parseTextHTML(strings.NewReader(sampleTextHTML))
	require.NoError(err, "parseTextHTML")

	assert.Empty(groupPar, "expected no group participants")

	require.Len(messages, 2, "expected 2 messages")

	// First message: from Keith
	m0 := messages[0]
	assert.Equal("+12023065386", m0.SenderPhone)
	assert.Equal("Keith Stern", m0.SenderName)
	assert.False(m0.IsMe, "m0.IsMe should be false")
	assert.Contains(m0.Body, "Cara says")
	// HTML entity should be decoded
	assert.Contains(m0.Body, "you're", "expected HTML entities to be decoded")

	// Timestamp
	expectedTime := time.Date(2020, 2, 3, 17, 37, 45, 632000000, time.UTC)
	assert.True(m0.Timestamp.Equal(expectedTime), "m0.Timestamp = %v, want %v", m0.Timestamp, expectedTime)

	// Second message: from Me
	m1 := messages[1]
	assert.True(m1.IsMe, "m1.IsMe should be true")
	assert.Equal("Me", m1.SenderName)
}

const sampleGroupHTML = `<?xml version="1.0" ?>
<html xmlns="http://www.w3.org/1999/xhtml"><head>
<title>Group Conversation</title></head>
<body><div class="hChatLog hfeed"><div class="participants">Group conversation with:
<cite class="sender vcard"><a class="tel" href="tel:+12022712272"><span class="fn">Cara Morris Stern</span></a></cite>, <cite class="sender vcard"><a class="tel" href="tel:+12023065386"><span class="fn">Keith Stern</span></a></cite></div>
<div class="message"><abbr class="dt" title="2020-02-05T11:16:14.368-06:00">Feb 5, 2020</abbr>:
<cite class="sender vcard"><a class="tel" href="tel:+12022712272"><span class="fn">Cara Morris Stern</span></a></cite>:
<q>Check this out<br></q>
</div> <div class="message"><abbr class="dt" title="2020-02-05T11:17:38.249-06:00">Feb 5, 2020</abbr>:
<cite class="sender vcard"><a class="tel" href="tel:+12023065386"><span class="fn">Keith Stern</span></a></cite>:
<q>Cool<br></q>
</div></div></body></html>`

func TestParseTextHTML_Group(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	messages, groupPar, err := parseTextHTML(strings.NewReader(sampleGroupHTML))
	require.NoError(err, "parseTextHTML")

	require.Len(groupPar, 2, "expected 2 group participants")
	assert.Equal("+12022712272", groupPar[0])
	assert.Equal("+12023065386", groupPar[1])

	require.Len(messages, 2, "expected 2 messages")

	// Trailing <br> should be stripped
	assert.False(strings.HasSuffix(messages[0].Body, "\n"), "body should not end with newline: %q", messages[0].Body)
}

const sampleMMS = `<?xml version="1.0" ?>
<html xmlns="http://www.w3.org/1999/xhtml"><head>
<title>Test</title></head>
<body><div class="hChatLog hfeed">
<div class="message"><abbr class="dt" title="2020-02-05T19:30:44.602-06:00">Feb 5, 2020</abbr>:
<cite class="sender vcard"><a class="tel" href="tel:+12022712272"><span class="fn">Test User</span></a></cite>:
<q></q>
<div><a class="video" href="Group Conversation - 2020-02-05T17_16_14Z-7-1">Video attachment</a></div></div></div></body></html>`

func TestParseTextHTML_MMS(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	messages, _, err := parseTextHTML(strings.NewReader(sampleMMS))
	require.NoError(err, "parseTextHTML")

	require.Len(messages, 1, "expected 1 message")

	require.Len(messages[0].Attachments, 1, "expected 1 attachment")

	att := messages[0].Attachments[0]
	assert.Equal("video", att.MediaType)
	assert.Equal("Group Conversation - 2020-02-05T17_16_14Z-7-1", att.HrefInHTML)
}

const sampleReceivedCallHTML = `<?xml version="1.0" ?>
<html xmlns="http://www.w3.org/1999/xhtml"><head>
<title>Received call from
Keith Stern</title></head>
<body><div class="haudio"><span class="album">Call Log for
</span>
<span class="fn">Received call from
Keith Stern</span>
<div class="contributor vcard">Received call from
<a class="tel" href="tel:+12023065386"><span class="fn">Keith Stern</span></a></div>
<abbr class="published" title="2020-02-05T17:26:28.000-06:00">Feb 5, 2020</abbr>
<br />
<abbr class="duration" title="PT1M23S">(00:01:23)</abbr>
<div class="tags">Labels:
<a rel="tag" href="http://www.google.com/voice#received">Received</a></div>
</div></body></html>`

func TestParseCallHTML_Received(t *testing.T) {
	assert := assertpkg.New(t)
	record, err := parseCallHTML(strings.NewReader(sampleReceivedCallHTML))
	requirepkg.NoError(t, err, "parseCallHTML")

	assert.Equal(fileTypeReceived, record.CallType)
	assert.Equal("+12023065386", record.Phone)
	assert.Equal("Keith Stern", record.Name)
	assert.Equal("PT1M23S", record.Duration)

	expectedTime := time.Date(2020, 2, 5, 23, 26, 28, 0, time.UTC)
	assert.True(record.Timestamp.Equal(expectedTime), "Timestamp = %v, want %v", record.Timestamp, expectedTime)
}

const samplePlacedCallHTML = `<?xml version="1.0" ?>
<html xmlns="http://www.w3.org/1999/xhtml"><head>
<title>Placed call to
Kicy Motley</title></head>
<body><div class="haudio"><span class="album">Call Log for
</span>
<span class="fn">Placed call to
Kicy Motley</span>
<div class="contributor vcard">Placed call to
<a class="tel" href="tel:+17188096446"><span class="fn">Kicy Motley</span></a></div>
<abbr class="published" title="2020-02-03T14:05:20.000-06:00">Feb 3, 2020</abbr>
<br />
<abbr class="duration" title="PT5M8S">(00:05:08)</abbr>
<div class="tags">Labels:
<a rel="tag" href="http://www.google.com/voice#placed">Placed</a></div>
</div></body></html>`

func TestParseCallHTML_Placed(t *testing.T) {
	record, err := parseCallHTML(strings.NewReader(samplePlacedCallHTML))
	requirepkg.NoError(t, err, "parseCallHTML")

	assertpkg.Equal(t, fileTypePlaced, record.CallType)
	assertpkg.Equal(t, "+17188096446", record.Phone)
}

func TestComputeMessageID(t *testing.T) {
	id1 := computeMessageID("+12023065386", "2020-02-03T11:37:45Z", "Hello")
	id2 := computeMessageID("+12023065386", "2020-02-03T11:37:45Z", "Hello")
	id3 := computeMessageID("+12023065386", "2020-02-03T11:37:45Z", "Goodbye")

	assertpkg.Equal(t, id1, id2, "same inputs should produce same ID")
	assertpkg.NotEqual(t, id1, id3, "different inputs should produce different IDs")
	assertpkg.Len(t, id1, 16)
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"PT1M23S", "1m 23s"},
		{"PT5M8S", "5m 8s"},
		{"PT0S", "0s"},
		{"PT1H2M3S", "1h 2m 3s"},
		{"PT30S", "30s"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := formatDuration(tt.input)
			assertpkg.Equal(t, tt.want, got, "formatDuration(%q)", tt.input)
		})
	}
}

func TestComputeThreadID(t *testing.T) {
	// 1:1 text uses other party's phone
	tid := computeThreadID(fileTypeText, "+12023065386", nil)
	assertpkg.Equal(t, "+12023065386", tid, "1:1 threadID")

	// Group uses sorted participants
	tid = computeThreadID(fileTypeGroup, "", []string{"+12023065386", "+12022712272"})
	assertpkg.Equal(t, "group:+12022712272,+12023065386", tid, "group threadID")

	// Call uses calls: prefix
	tid = computeThreadID(fileTypeReceived, "+12023065386", nil)
	assertpkg.Equal(t, "calls:+12023065386", tid, "call threadID")
}

func TestSnippet(t *testing.T) {
	long := strings.Repeat("a", 200)
	s := snippet(long)
	assertpkg.Len(t, s, 100)

	s = snippet("short")
	assertpkg.Equal(t, "short", s)

	// Whitespace normalization
	s = snippet("  hello   world  ")
	assertpkg.Equal(t, "hello world", s)
}
