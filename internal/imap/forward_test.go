package imap

import (
	"bytes"
	"mime"
	"mime/multipart"
	"net/mail"
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
		QuotedHTML: "<p>Lucid: ideas</p>",
	}, time.Now(), "forward@example.test")
	require.NoError(t, err)
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
	_, err := BuildIMAPDraftReplacement([]byte("From: a@example.test\r\nTo: b@example.test\r\nContent-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\n"), "changed", time.Now(), "")
	assertions.Error(err)
	assertions.True(strings.Contains(err.Error(), "multipart") || strings.Contains(err.Error(), "forward"))
	_, err = BuildForward(ForwardOptions{
		From: "sender@example.test", To: []string{"recipient@example.test"},
		Subject: "Original", QuotedHTML: `<img src="cid:missing@example.test">`,
	}, time.Now(), "forward@example.test")
	assertions.Error(err)
	assertions.Contains(err.Error(), "Content-ID")
	_, err = BuildIMAPDraftReplacement([]byte("From: a@example.test\r\nTo: b@example.test\r\nX-Msgvault-Forward: 1\r\nContent-Type: multipart/signed; boundary=x\r\n\r\n--x--\r\n"), "changed", time.Now(), "")
	assertions.ErrorContains(err, "does not support")
}
