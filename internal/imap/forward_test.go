package imap

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildForwardPreservesMIME(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	note := "distinctive current note"
	draft, err := BuildForward(ForwardOptions{
		From: "sender@example.test", To: []string{"recipient@example.test"},
		Subject: "Original subject", Body: note, QuotedHeader: "From: Parent <parent@example.test>\r\nSubject: Original subject",
		QuotedText: "quoted text", QuotedHTML: `<p>quoted <img src="cid:logo@example.test"></p>`,
		Attachments: []ForwardAttachment{{Filename: "logo.png", ContentType: "image/png", ContentID: "logo@example.test", IsInline: true, Content: []byte("png")}},
	}, time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC), "forward@example.test")
	requirements.NoError(err)
	assertions.Contains(string(draft.Raw), "X-Msgvault-Forward: 1")
	assertions.Equal(1, strings.Count(draft.Parsed.BodyText, note))
	assertions.Equal(1, strings.Count(draft.Parsed.BodyHTML, note))
	assertions.Contains(draft.Parsed.BodyText, "quoted text")
	assertions.Contains(draft.Parsed.BodyHTML, "quoted")
	assertions.Equal(1, strings.Count(draft.Parsed.BodyHTML, "cid:logo@example.test"))
	requirements.Len(draft.Parsed.Attachments, 1)
	assertions.Equal("logo.png", draft.Parsed.Attachments[0].Filename)
	assertions.Equal([]byte("png"), draft.Parsed.Attachments[0].Content)
	message, err := mail.ReadMessage(bytes.NewReader(draft.Raw))
	requirements.NoError(err)
	_, mixedParams, err := mime.ParseMediaType(message.Header.Get("Content-Type"))
	requirements.NoError(err)
	mixed := multipart.NewReader(message.Body, mixedParams["boundary"])
	_, err = mixed.NextPart()
	requirements.NoError(err)
	relatedPart, err := mixed.NextPart()
	requirements.NoError(err)
	_, relatedParams, err := mime.ParseMediaType(relatedPart.Header.Get("Content-Type"))
	requirements.NoError(err)
	related := multipart.NewReader(relatedPart, relatedParams["boundary"])
	root, err := related.NextPart()
	requirements.NoError(err)
	rootType, _, err := mime.ParseMediaType(root.Header.Get("Content-Type"))
	requirements.NoError(err)
	assertions.Equal("multipart/alternative", rootType)
}

func TestBuildForwardWrapsAttachmentBase64(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	inline := bytes.Repeat([]byte{'i'}, 200)
	regular := bytes.Repeat([]byte{'a'}, 201)
	draft, err := BuildForward(ForwardOptions{
		From: "sender@example.test", To: []string{"recipient@example.test"},
		QuotedHTML: "<p>quoted</p>", Attachments: []ForwardAttachment{
			{Filename: "inline.png", ContentType: "image/png", ContentID: "inline@example.test", IsInline: true, Content: inline},
			{Filename: "regular.bin", ContentType: "application/octet-stream", Content: regular},
		},
	}, time.Now(), "forward@example.test")
	requirements.NoError(err)
	requirements.Len(draft.Parsed.Attachments, 2)
	for _, content := range [][]byte{inline, regular} {
		encoded := base64.StdEncoding.EncodeToString(content)
		assertions.NotContains(string(draft.Raw), encoded)
		assertions.Contains(string(draft.Raw), encoded[:76]+"\r\n"+encoded[76:152])
	}
	contents := make(map[string][]byte, len(draft.Parsed.Attachments))
	for _, attachment := range draft.Parsed.Attachments {
		contents[attachment.Filename] = attachment.Content
	}
	assertions.Equal(inline, contents["inline.png"])
	assertions.Equal(regular, contents["regular.bin"])
}

func TestRewriteForwardAlternativeWrapsBase64HTML(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	plainHeader := textproto.MIMEHeader{"Content-Type": {"text/plain; charset=utf-8"}}
	plain, err := writer.CreatePart(plainHeader)
	requirements.NoError(err)
	_, err = plain.Write([]byte("quoted plain"))
	requirements.NoError(err)
	htmlHeader := textproto.MIMEHeader{"Content-Type": {"text/html; charset=utf-8"}, "Content-Transfer-Encoding": {"base64"}}
	htmlPart, err := writer.CreatePart(htmlHeader)
	requirements.NoError(err)
	quoted := strings.Repeat("<p>quoted html</p>", 20)
	_, err = htmlPart.Write([]byte(base64.StdEncoding.EncodeToString([]byte(forwardHTMLBody("", quoted, "old note")))))
	requirements.NoError(err)
	requirements.NoError(writer.Close())

	replacement, boundary, err := rewriteForwardAlternative(body.Bytes(), writer.Boundary(), "new note")
	requirements.NoError(err)
	reader := multipart.NewReader(bytes.NewReader(replacement), boundary)
	_, err = reader.NextRawPart()
	requirements.NoError(err)
	rewrittenHTML, err := reader.NextRawPart()
	requirements.NoError(err)
	encoded, err := io.ReadAll(rewrittenHTML)
	requirements.NoError(err)
	for line := range strings.SplitSeq(string(encoded), "\r\n") {
		assertions.LessOrEqual(len(line), 76)
	}
	decoded, err := decodeTransfer(encoded, "base64")
	requirements.NoError(err)
	assertions.Contains(string(decoded), "new note")
	assertions.Contains(string(decoded), quoted)
	assertions.NotContains(string(decoded), "old note")
}

func TestEncodeTransferBase64LineLengths(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	for _, size := range []int{0, 1, 2, 3, 57, 58, 300} {
		content := bytes.Repeat([]byte{'x'}, size)
		encoded, err := encodeTransfer(content, "base64")
		requirements.NoError(err)
		for line := range strings.SplitSeq(string(encoded), "\r\n") {
			assertions.LessOrEqual(len(line), 76, "content size %d", size)
		}
		assertions.NotContains(strings.ReplaceAll(string(encoded), "\r\n", ""), "\n")
		decoded, err := decodeTransfer(encoded, "base64")
		requirements.NoError(err)
		assertions.Equal(content, decoded)
	}
}

func TestBuildForwardPlainParentRetainsInlineFile(t *testing.T) {
	draft, err := BuildForward(ForwardOptions{
		From: "sender@example.test", To: []string{"recipient@example.test"},
		QuotedText: "plain parent", Attachments: []ForwardAttachment{{
			Filename: "photo.jpg", ContentType: "image/jpeg", ContentID: "photo@example.test",
			IsInline: true, Content: []byte("photo"),
		}},
	}, time.Now(), "forward@example.test")
	require.NoError(t, err)
	require.Len(t, draft.Parsed.Attachments, 1)
	assert.Equal(t, []byte("photo"), draft.Parsed.Attachments[0].Content)
}

func TestBuildForwardVisibleCIDWord(t *testing.T) {
	_, err := BuildForward(ForwardOptions{
		From: "sender@example.test", To: []string{"recipient@example.test"},
		QuotedHTML: "<p>Lucid: ideas. Use cid:logo to reference an image.</p><!-- cid:missing -->",
	}, time.Now(), "forward@example.test")
	require.NoError(t, err)
}

func TestBuildForwardUnicodeBeforeCIDReference(t *testing.T) {
	for _, quotedHTML := range []string{
		`<p>İstanbul</p><img src="cid:logo@example.test">`,
		`<style>.hero { background: url(cid:logo@example.test) }</style><p>İstanbul</p>`,
	} {
		draft, err := BuildForward(ForwardOptions{
			From: "sender@example.test", To: []string{"recipient@example.test"},
			QuotedHTML:  quotedHTML,
			Attachments: []ForwardAttachment{{Filename: "logo.png", ContentType: "image/png", ContentID: "logo@example.test", IsInline: true, Content: []byte("logo")}},
		}, time.Now(), "forward@example.test")
		require.NoError(t, err)
		require.Len(t, draft.Parsed.Attachments, 1)
	}
}

func TestBuildForwardReplacementPreservesParts(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	initialNote := "distinctive initial note"
	currentNote := "distinctive current note"
	draft, err := BuildForward(ForwardOptions{
		From: "sender@example.test", To: []string{"recipient@example.test"},
		Subject: "Original", Body: initialNote, QuotedHeader: "Subject: Original",
		QuotedText: "distinctive quoted text", QuotedHTML: `<p>distinctive quoted html <img src="cid:logo@example.test"></p>`,
		Attachments: []ForwardAttachment{{Filename: "logo.png", ContentType: "image/png", ContentID: "logo@example.test", IsInline: true, Content: []byte("file")}},
	}, time.Now(), "forward@example.test")
	requirements.NoError(err)
	replacement, err := BuildIMAPDraftReplacement(draft.Raw, currentNote, time.Now(), "replacement@example.test")
	requirements.NoError(err)
	assertions.Equal(1, strings.Count(replacement.Parsed.BodyText, currentNote))
	assertions.Equal(1, strings.Count(replacement.Parsed.BodyHTML, currentNote))
	assertions.NotContains(replacement.Parsed.BodyText, initialNote)
	assertions.Contains(replacement.Parsed.BodyText, "distinctive quoted text")
	assertions.Contains(replacement.Parsed.BodyHTML, "distinctive quoted html")
	assertions.Equal(1, strings.Count(replacement.Parsed.BodyHTML, "cid:logo@example.test"))
	requirements.Len(replacement.Parsed.Attachments, 1)
	assertions.Equal([]byte("file"), replacement.Parsed.Attachments[0].Content)
}

func TestBuildForwardRejectsUnsupportedParts(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	_, err := BuildIMAPDraftReplacement([]byte("From: a@example.test\r\nTo: b@example.test\r\nContent-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\n"), "changed", time.Now(), "")
	requirements.Error(err)
	assertions.True(strings.Contains(err.Error(), "multipart") || strings.Contains(err.Error(), "forward"))
	_, err = BuildIMAPDraftReplacement([]byte("From: a@example.test\r\nTo: b@example.test\r\nX-Msgvault-Forward: 1\r\nContent-Type: multipart/signed; boundary=x\r\n\r\n--x--\r\n"), "changed", time.Now(), "")
	requirements.ErrorContains(err, "does not support")
}

func TestBuildIMAPDraftReplacementWithoutContentType(t *testing.T) {
	raw := []byte("From: sender@example.test\r\nTo: recipient@example.test\r\nSubject: Draft\r\n\r\nOriginal body\r\n")
	draft, err := BuildIMAPDraftReplacement(raw, "Updated body", time.Now(), "updated@example.test")
	require.NoError(t, err)
	assert.Contains(t, draft.Parsed.BodyText, "Updated body")
}

func TestBuildForwardKeepsUnresolvedParentCID(t *testing.T) {
	draft, err := BuildForward(ForwardOptions{
		From: "sender@example.test", To: []string{"recipient@example.test"},
		QuotedHTML: `<img src="cid:missing@example.test">`,
	}, time.Now(), "forward@example.test")
	require.NoError(t, err)
	assert.Contains(t, draft.Parsed.BodyHTML, "cid:missing@example.test")
}
